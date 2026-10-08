// step_approval_status_vocabulary_test.go — the two status columns that had no
// owning vocabulary until now, and the strings the writers actually write.
//
// steps.status and runs.approval_status were both bare string literals at their
// write sites (`internal/wiring/persist.go`, `internal/grpc/change_service.go`,
// `internal/template/clone.go`, `cmd/levee/cmd_new.go`). The BatchState* case
// showed what that costs: the reader judged "done" while the writer wrote
// "completed", both sides were green, and the regression was only visible on a
// live run. These tests seed the strings the WRITERS write, taken from the
// source as literals, so a constant that drifts away from its write site fails
// here rather than in production.
//
// The cross-package half lives with the writer: internal/wiring's
// step_persist_vocabulary_test.go and cmd/levee's equivalents assert the values
// that reach the store.
package state

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestStepStatusVocabularyMatchesWriters pins each StepStatus* constant against
// the literal the execution path writes. The literals are spelled here on
// purpose: asserting `state.StepStatusSuccess == state.StepStatusSuccess` would
// stay green through exactly the drift this guards.
func TestStepStatusVocabularyMatchesWriters(t *testing.T) {
	assert.Equal(t, "success", StepStatusSuccess,
		"persist.go writes this for a step whose action returned no error")
	assert.Equal(t, "failed", StepStatusFailed,
		"persist.go writes this for a step whose action returned an error")
	assert.Equal(t, "skipped", StepStatusSkipped,
		"persist.go writes this for a rollback step skipped by idempotency, and for resume-skipped rows")
	assert.Equal(t, "pending", StepStatusPending,
		"the schema comment documents it; internal/lock's busy-host probe reads it")
	assert.Equal(t, "running", StepStatusRunning,
		"the schema comment documents it; internal/lock's busy-host probe reads it")
}

// TestStepStatusFilterFindsWrittenRows checks the constants are usable as a
// filter — an owning vocabulary that cannot select the rows it names would be
// documentation, not a vocabulary. The rows are written with the literals the
// engine writes.
func TestStepStatusFilterFindsWrittenRows(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)
	seedRunForBatchTest(t, ctx, store, "r-step-vocab")
	require.NoError(t, store.CreateBatch(ctx, &Batch{
		ID: "sv-1", RunID: "r-step-vocab", BatchNo: 1,
		Status: BatchStateRunning, TotalHosts: 1,
	}))

	writes := []struct{ literal, wantConst string }{
		{"success", StepStatusSuccess},
		{"failed", StepStatusFailed},
		{"skipped", StepStatusSkipped},
	}
	for i, w := range writes {
		require.NoError(t, store.CreateStep(ctx, &Step{
			ID:       "sv-step-" + string(rune('a'+i)),
			RunID:    "r-step-vocab",
			BatchID:  "sv-1",
			Host:     "host-1",
			StepName: "deploy",
			Action:   "pkg.install",
			Status:   w.literal,
		}))
	}

	for _, w := range writes {
		got, err := store.ListSteps(ctx, StepFilter{RunID: "r-step-vocab", Status: w.wantConst})
		require.NoError(t, err)
		require.Len(t, got, 1, "filtering by %s must find the row written as %q", w.wantConst, w.literal)
		assert.Equal(t, w.literal, got[0].Status)
	}
}

// TestApprovalStatusVocabularyMatchesWriters pins the run-side approval verdict
// set against the writers.
//
// Note what is NOT here: `timeout` and `skipped`. The schema comment on
// runs.approval_status lists them, but they belong to the approvals TABLE's
// status column one schema line over, and a sweep of the writers finds neither
// on a run row. Including them would make the constant set describe a
// vocabulary no row carries.
func TestApprovalStatusVocabularyMatchesWriters(t *testing.T) {
	assert.Equal(t, "pending", ApprovalStatusPending,
		"set by CreateChange / InstantiateTemplate / CloneChange / cmd new")
	assert.Equal(t, "approved", ApprovalStatusApproved,
		"set by the approval settle path (change_service.go)")
	assert.Equal(t, "rejected", ApprovalStatusRejected,
		"set by the rejection path (change_service.go)")
}

// TestApprovalStatusRoundTripsThroughStore checks the verdict constants select
// real rows. RunFilter has no approval-status predicate, so this reads the rows
// back by id rather than pretending a filter exists.
func TestApprovalStatusRoundTripsThroughStore(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)

	verdicts := map[string]string{
		ApprovalStatusPending:  "pending",
		ApprovalStatusApproved: "approved",
		ApprovalStatusRejected: "rejected",
	}
	for c, literal := range verdicts {
		id := "run-" + literal
		require.NoError(t, store.CreateRun(ctx, &Run{
			ID: id, WorkflowName: "wf", TemplateName: "tpl", PlanHash: "h",
			Status: "pending", ApprovalStatus: c,
			CreatedAt: tnow(), UpdatedAt: tnow(), Creator: "alice",
		}))

		got, err := store.GetRun(ctx, id)
		require.NoError(t, err)
		require.NotNil(t, got)
		assert.Equal(t, literal, got.ApprovalStatus,
			"the constant %q must be the string the column holds", c)
	}
}
