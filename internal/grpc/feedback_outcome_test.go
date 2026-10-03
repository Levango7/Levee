// feedback_outcome_test.go — the effect-learning producer on ChangeService:
// every apply verdict becomes a FixOutcome; successes synthesise knowledge
// base patterns, and the learner stays optional (nil = no-op).
package grpc

import (
	"context"
	"errors"
	"testing"

	"github.com/nexus/levee/internal/grpc/pb"
	"github.com/nexus/levee/internal/recommend"
	"github.com/nexus/levee/internal/recommend/feedback"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestApplyChange_RecordsFixOutcomeForLearner(t *testing.T) {
	engine := &racingEngine{runID: "exec-fb-succ", runSuccess: true}
	svc, store := newTestChangeServiceWithEngine(t, engine.adapter())
	kb := recommend.NewKnowledgeBase()
	learner := feedback.NewFeedbackLearner(feedback.FeedbackLearnerConfig{KnowledgeBase: kb})
	svc.SetFeedbackLearner(learner)

	ctx := context.Background()
	created, err := svc.CreateChange(ctx, &pb.CreateChangeRequest{Label: "fb-outcome"})
	require.NoError(t, err)
	persistPlanOnRun(t, store, created.GetId())
	// Name the workflow so the outcome has a non-empty fix action.
	run, err := store.GetRun(ctx, created.GetId())
	require.NoError(t, err)
	run.WorkflowName = "restart-nginx"
	require.NoError(t, store.UpdateRun(ctx, run))

	_, err = svc.ApplyChange(ctx, &pb.ApplyChangeRequest{ChangeId: created.GetId(), AutoApprove: true})
	require.NoError(t, err)

	stats := learner.GetStats()
	assert.Equal(t, 1, stats.TotalRecords, "a completed apply must be recorded as a fix outcome")
	assert.Equal(t, 1, stats.SuccessCount)
	assert.NotEmpty(t, learner.ExportPatterns(),
		"a successful outcome must synthesise a learned pattern")
}

func TestApplyChange_RecordsFailedOutcomeWithoutPattern(t *testing.T) {
	engine := &racingEngine{runID: "exec-fb-err", runErr: errors.New("disk on fire")}
	svc, store := newTestChangeServiceWithEngine(t, engine.adapter())
	learner := feedback.NewFeedbackLearner(feedback.FeedbackLearnerConfig{
		KnowledgeBase: recommend.NewKnowledgeBase(),
	})
	svc.SetFeedbackLearner(learner)

	ctx := context.Background()
	created, err := svc.CreateChange(ctx, &pb.CreateChangeRequest{Label: "fb-fail"})
	require.NoError(t, err)
	persistPlanOnRun(t, store, created.GetId())
	run, err := store.GetRun(ctx, created.GetId())
	require.NoError(t, err)
	run.WorkflowName = "restart-nginx"
	require.NoError(t, store.UpdateRun(ctx, run))

	_, err = svc.ApplyChange(ctx, &pb.ApplyChangeRequest{ChangeId: created.GetId(), AutoApprove: true})
	require.Error(t, err)

	stats := learner.GetStats()
	assert.Equal(t, 1, stats.TotalRecords, "a failed apply must also be recorded")
	assert.Equal(t, 1, stats.FailureCount)
	assert.Empty(t, learner.ExportPatterns(),
		"failures do not synthesise patterns (nothing to learn from an unlinked failure)")
}

func TestApplyChange_NoLearnerIsANoOp(t *testing.T) {
	// The producer must stay strictly optional: without a learner the apply
	// path behaves exactly as before.
	engine := &racingEngine{runID: "exec-fb-none", runSuccess: true}
	svc, store := newTestChangeServiceWithEngine(t, engine.adapter())

	ctx := context.Background()
	created, err := svc.CreateChange(ctx, &pb.CreateChangeRequest{Label: "fb-none"})
	require.NoError(t, err)
	persistPlanOnRun(t, store, created.GetId())

	_, err = svc.ApplyChange(ctx, &pb.ApplyChangeRequest{ChangeId: created.GetId(), AutoApprove: true})
	require.NoError(t, err)
}
