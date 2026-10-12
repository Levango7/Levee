// priority_split_test.go — the boundary and persistence rules created by moving
// priority out of runs.approval_level (schema v9).
//
// Two invariants are tested here, and the second one was already broken before this
// change:
//
//  1. Each column carries one vocabulary. Priority is validated at the boundary and
//     lands in runs.priority; the tier column stays empty until a plan derives one.
//     Validation is what turns "not in the list" from a value that silently lives in
//     a tier-typed column into an error the caller sees.
//  2. What an RPC answers is what it stored. InstantiateTemplate computed a priority,
//     never wrote it into the run, and still returned it — so the caller was told
//     "urgent" while the row said nothing, and every later reader (board, detail,
//     audit export) saw the empty one. The echo-parity assertions below keep that
//     from coming back.
package grpc

import (
	"context"
	"testing"

	"github.com/nexus/levee/internal/grpc/pb"
	"github.com/nexus/levee/internal/state"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func createReq(priority string) *pb.CreateChangeRequest {
	return &pb.CreateChangeRequest{
		Label:        "prio-" + priority,
		Priority:     priority,
		WorkflowFile: "workflow.levee",
		TemplateName: "deploy-web",
		Environment:  "staging",
	}
}

func instantiateReq(templateName, priority string) *pb.InstantiateTemplateRequest {
	return &pb.InstantiateTemplateRequest{
		TemplateName: templateName,
		Label:        "from-" + templateName,
		Priority:     priority,
	}
}

func TestCreateChange_StoresPriorityInItsOwnColumn(t *testing.T) {
	svc, store := newTestChangeService(t)
	ctx := context.Background()

	for _, want := range state.PriorityNames() {
		resp, err := svc.CreateChange(ctx, createReq(want))
		require.NoError(t, err, "priority %q is declared, so the create path must accept it", want)

		run, err := store.GetRun(ctx, resp.GetId())
		require.NoError(t, err)
		assert.Equal(t, want, run.Priority, "priority %q did not reach runs.priority", want)
		assert.Empty(t, run.ApprovalLevel,
			"a create must not write the tier column; %q in there is the pre-v9 shape coming back", run.ApprovalLevel)
		assert.Equal(t, want, resp.GetPriority(), "the response and the stored row disagree")
	}
}

func TestCreateChange_BlankPriorityDefaultsToNormal(t *testing.T) {
	svc, store := newTestChangeService(t)
	ctx := context.Background()

	resp, err := svc.CreateChange(ctx, createReq(""))
	require.NoError(t, err)
	run, err := store.GetRun(ctx, resp.GetId())
	require.NoError(t, err)

	assert.Equal(t, state.PriorityNormal, run.Priority,
		`"" is documented as "unspecified" and lands as normal; storing it literally would put a value outside the vocabulary in the column`)
	assert.Equal(t, state.PriorityNormal, resp.GetPriority())
}

func TestCreateChange_RejectsValuesOutsideTheVocabulary(t *testing.T) {
	svc, store := newTestChangeService(t)
	ctx := context.Background()

	// "medium" and "critical" are the spellings this column used to swallow and
	// could never mean anything by: they belong to plan.RiskLevel and to alert
	// severity. "standard"/"emergency" are tiers — legal in the OTHER column, never
	// here. Case and space variants are refused because every downstream reader
	// compares these strings literally.
	for _, bad := range []string{"medium", "critical", "standard", "emergency", "HIGH", " urgent", "urgent ", "none", "0"} {
		resp, err := svc.CreateChange(ctx, createReq(bad))
		require.Error(t, err, "priority %q is not declared and must be refused", bad)
		assert.Nil(t, resp)
		assert.Equal(t, codes.InvalidArgument, status.Code(err), "wrong code for %q", bad)

		msg := err.Error()
		assert.Contains(t, msg, bad, "the rejection must name the value the caller sent")
		assert.Contains(t, msg, state.PriorityNamesJoined(),
			"the rejection must list what IS accepted, read from the same source the predicate consults")

		runs, err := store.ListRuns(ctx, state.RunFilter{})
		require.NoError(t, err)
		assert.Empty(t, runs, "a rejected create must not leave a run behind")
	}
}

func TestInstantiateTemplate_StoredPriorityMatchesTheResponse(t *testing.T) {
	svc := newTestTemplateService(t)
	ctx := context.Background()

	_, err := svc.CreateTemplate(ctx, &pb.CreateTemplateRequest{
		Name:            "deploy",
		WorkflowContent: "steps: []",
	})
	require.NoError(t, err)

	resp, err := svc.InstantiateTemplate(ctx, instantiateReq("deploy", state.PriorityUrgent))
	require.NoError(t, err)

	run, err := svc.store.GetRun(ctx, resp.GetId())
	require.NoError(t, err)

	// The bug this pins: the response carried "urgent" while the row carried nothing.
	assert.Equal(t, state.PriorityUrgent, run.Priority,
		"instantiate dropped the caller's priority claim; it must be stored, not just echoed")
	assert.Equal(t, run.Priority, resp.GetPriority(), "response and stored row disagree")
	assert.Empty(t, run.ApprovalLevel, "instantiate must not seed the tier column")
}

func TestInstantiateTemplate_RejectsValuesOutsideTheVocabulary(t *testing.T) {
	svc := newTestTemplateService(t)
	ctx := context.Background()

	_, err := svc.CreateTemplate(ctx, &pb.CreateTemplateRequest{
		Name:            "deploy2",
		WorkflowContent: "steps: []",
	})
	require.NoError(t, err)

	// Both create paths must agree: a value accepted by one and refused by the other
	// would leave the column's meaning depending on which door the change came in by.
	_, err = svc.InstantiateTemplate(ctx, instantiateReq("deploy2", "medium"))
	require.Error(t, err)
	assert.Equal(t, codes.InvalidArgument, status.Code(err))
}
