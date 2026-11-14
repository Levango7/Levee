package dbchange

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/nexus/levee/internal/audit"
	"github.com/nexus/levee/internal/dsl"
	"github.com/nexus/levee/internal/evidence"
	"github.com/nexus/levee/internal/plan"
	"github.com/nexus/levee/internal/state"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newStore(t *testing.T) state.Store {
	t.Helper()
	store, err := state.NewSQLiteStore(context.Background(), filepath.Join(t.TempDir(), "dbchange-test.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	return store
}

// seedPlan persists a run whose plan artifact carries the shape a real
// database change leaves behind: one mysql.query step with an undo, a run-level
// rollback policy, and the review summary stamped at plan time.
func seedPlan(t *testing.T, store state.Store, runID string) {
	t.Helper()
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)
	require.NoError(t, store.CreateRun(ctx, &state.Run{
		ID: runID, WorkflowName: "db-change.yaml", TemplateName: "mysql-ddl",
		Status: "approved", ApprovalStatus: "approved", ApprovalLevel: "high",
		Priority: "high", PlanHash: "v2:abc", CreatedAt: now, UpdatedAt: now,
	}))

	p := &plan.Plan{
		ID: runID,
		Batches: []plan.Batch{{
			Index:          0,
			Targets:        []string{"db-1"},
			MaxConcurrency: 2,
			Steps: []plan.PlanStep{{
				Name: "add-column", Module: "mysql", Action: "query",
				Args: map[string]any{"sql": "ALTER TABLE t ADD COLUMN c INT"},
				Rollback: &dsl.RollbackSpec{
					Strategy: "undo-action",
					Steps:    []dsl.Step{{Name: "drop-column", Module: "mysql", Action: "query"}},
				},
			}},
		}},
		TotalTargets:  1,
		ApprovalFloor: "high",
		Rollback:      &dsl.RollbackSpec{OnFailure: "auto", VerifyAfter: true, SnapshotPaths: []string{"/etc/app"}},
		SQLReview: &plan.SQLReviewSummary{
			Verdict: "warn", WarnCount: 1,
			Findings: []plan.SQLReviewFinding{{
				Step: "add-column", Rule: "SR-05", Severity: "warn", Statement: 1,
				Message: "ADD COLUMN without a default rewrites the table",
			}},
		},
	}
	raw, err := json.Marshal(p)
	require.NoError(t, err)
	require.NoError(t, store.UpdateRunPlan(ctx, runID, string(raw), "v2:abc", now))
}

func TestCollect_NotFound(t *testing.T) {
	store := newStore(t)
	_, err := Collect(context.Background(), store, "nope")
	require.ErrorIs(t, err, ErrNotFound)
}

// A change with no plan yet is a legitimate state (draft, or created but never
// planned): the ticket renders with empty SQL/rollback sections rather than
// failing, and says so through HasSQL / HasUndo.
func TestCollect_NoPlanYet(t *testing.T) {
	store := newStore(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)
	require.NoError(t, store.CreateRun(ctx, &state.Run{
		ID: "draft-1", Status: "draft", CreatedAt: now, UpdatedAt: now,
	}))

	tk, err := Collect(ctx, store, "draft-1")
	require.NoError(t, err)
	assert.Equal(t, "draft-1", tk.Change.ID)
	assert.False(t, tk.SQL.HasSQL)
	assert.False(t, tk.Rollback.HasUndo)
	assert.False(t, tk.Approval.HasApproval)
	assert.False(t, tk.Execution.HasEvidence)
}

func TestCollect_SQLReviewAndRollback(t *testing.T) {
	store := newStore(t)
	ctx := context.Background()
	seedPlan(t, store, "chg-1")

	tk, err := Collect(ctx, store, "chg-1")
	require.NoError(t, err)

	require.True(t, tk.SQL.HasSQL)
	require.Len(t, tk.SQL.Steps, 1)
	assert.Equal(t, "add-column", tk.SQL.Steps[0].Step)
	assert.Equal(t, "ALTER TABLE t ADD COLUMN c INT", tk.SQL.Steps[0].SQL)

	require.NotNil(t, tk.SQL.Review)
	assert.Equal(t, "warn", tk.SQL.Review.Verdict)
	assert.Equal(t, 1, tk.SQL.Review.WarnCount)
	require.Len(t, tk.SQL.Review.Findings, 1)
	assert.Equal(t, "SR-05", tk.SQL.Review.Findings[0].Rule)

	assert.Equal(t, "auto", tk.Rollback.OnFailure)
	assert.True(t, tk.Rollback.VerifyAfter)
	assert.Equal(t, []string{"/etc/app"}, tk.Rollback.SnapshotPaths)
	require.Len(t, tk.Rollback.StepUndos, 1)
	assert.Equal(t, "add-column", tk.Rollback.StepUndos[0].Step)
	assert.Equal(t, "undo-action", tk.Rollback.StepUndos[0].Strategy)
	assert.Equal(t, 1, tk.Rollback.StepUndos[0].UndoSteps)
}

func TestCollect_ApprovalChain(t *testing.T) {
	store := newStore(t)
	ctx := context.Background()
	seedPlan(t, store, "chg-2")
	now := time.Now().UTC().Truncate(time.Second)
	extra, err := json.Marshal(map[string]any{
		"approvers": []string{"alice", "bob"}, "min_approvers": 2,
		"decisions": []map[string]any{
			{"approver": "alice", "action": "approve", "reason": "looks safe", "at": now},
		},
	})
	require.NoError(t, err)
	require.NoError(t, store.CreateApproval(ctx, &state.Approval{
		ID: "appr-1", RunID: "chg-2", Level: "high", Approver: "alice",
		Status: "approved", Comment: string(extra), PlanHash: "v2:abc",
		ActedAt: &now,
	}))

	tk, err := Collect(ctx, store, "chg-2")
	require.NoError(t, err)
	require.True(t, tk.Approval.HasApproval)
	assert.Equal(t, "high", tk.Approval.Level)
	assert.Equal(t, "approved", tk.Approval.Status)
	assert.Equal(t, []string{"alice", "bob"}, tk.Approval.Approvers)
	assert.Equal(t, 1, tk.Approval.ApprovedCount)
	assert.True(t, tk.Approval.MatchesPlan, "the approval is bound to the run's current plan hash")
	require.Len(t, tk.Approval.Decisions, 1)
	assert.Equal(t, "alice", tk.Approval.Decisions[0].Approver)
	assert.Equal(t, "looks safe", tk.Approval.Decisions[0].Reason)
}

// A stale approval (one bound to a plan the change no longer carries) must be
// visible, not silently counted as a match.
func TestCollect_StaleApprovalIsReported(t *testing.T) {
	store := newStore(t)
	ctx := context.Background()
	seedPlan(t, store, "chg-3")
	now := time.Now().UTC().Truncate(time.Second)
	require.NoError(t, store.CreateApproval(ctx, &state.Approval{
		ID: "appr-old", RunID: "chg-3", Level: "high", Approver: "alice",
		Status: "approved", PlanHash: "v2:an-old-plan", ActedAt: &now,
	}))

	tk, err := Collect(ctx, store, "chg-3")
	require.NoError(t, err)
	assert.True(t, tk.Approval.HasApproval)
	assert.False(t, tk.Approval.MatchesPlan)
}

func TestCollect_ExecutionAndAudit(t *testing.T) {
	store := newStore(t)
	ctx := context.Background()
	seedPlan(t, store, "chg-4")
	rec, err := audit.NewTraceRecorder(store)
	require.NoError(t, err)
	_, err = rec.Record(ctx, audit.TraceRecord{
		RunID: "chg-4", Event: audit.EventStepExecute, Actor: "system", Target: "db-1",
	})
	require.NoError(t, err)

	tk, err := Collect(ctx, store, "chg-4")
	require.NoError(t, err)

	assert.True(t, tk.Audit.Verified)
	assert.True(t, tk.Audit.Valid)
	assert.Equal(t, 1, tk.Audit.Count)
	assert.False(t, tk.Audit.Unverifiable)

	// No batch/step rows yet: the evidence verdict must say so rather than
	// reading as "complete".
	assert.Equal(t, string(evidence.NotStarted), tk.Execution.Verdict)
	assert.False(t, tk.Execution.HasEvidence)
}

// A change with no trace rows has nothing to verify. The ticket must not
// render that as a passed chain.
func TestCollect_AuditUnverifiableWhenNoTraces(t *testing.T) {
	store := newStore(t)
	seedPlan(t, store, "chg-5")

	tk, err := Collect(context.Background(), store, "chg-5")
	require.NoError(t, err)
	assert.False(t, tk.Audit.Verified)
	assert.True(t, tk.Audit.Unverifiable)
	assert.False(t, tk.Audit.Valid)
}

// The window is a workflow fact the store does not persist; the ticket says so
// instead of inventing a value a reader would trust.
func TestCollect_WindowIsHonestAboutNotBeingPersisted(t *testing.T) {
	store := newStore(t)
	seedPlan(t, store, "chg-6")

	tk, err := Collect(context.Background(), store, "chg-6")
	require.NoError(t, err)
	assert.Equal(t, "plan", tk.Window.EnforcedAt)
	assert.False(t, tk.Window.PersistedPerRun)
	assert.NotEmpty(t, tk.Window.Note)
}

func TestCollect_UnparsablePlanDegradesNotFails(t *testing.T) {
	store := newStore(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)
	require.NoError(t, store.CreateRun(ctx, &state.Run{
		ID: "chg-7", Status: "approved", PlanHash: "v2:abc", CreatedAt: now, UpdatedAt: now,
	}))
	// A plan artifact that cannot be parsed is a real defect, but the ticket
	// should still render the rest of the change rather than refuse.
	require.NoError(t, store.UpdateRunPlan(ctx, "chg-7", "{not json", "v2:abc", now))

	tk, err := Collect(ctx, store, "chg-7")
	require.NoError(t, err)
	assert.Equal(t, "v2:abc", tk.Change.PlanHash, "a blessed plan is still reported")
	assert.False(t, tk.SQL.HasSQL, "its contents are not readable, and the section says so by being empty")
}

func TestCollect_NilStoreIsRefused(t *testing.T) {
	_, err := Collect(context.Background(), nil, "x")
	require.Error(t, err)
}

// The approval domain mapping is reused rather than re-implemented: a row whose
// metadata cannot be decoded must not take the ticket down.
func TestCollect_UnparsableApprovalMetadataSkipsThatRow(t *testing.T) {
	store := newStore(t)
	ctx := context.Background()
	seedPlan(t, store, "chg-8")
	now := time.Now().UTC().Truncate(time.Second)
	require.NoError(t, store.CreateApproval(ctx, &state.Approval{
		ID: "appr-bad", RunID: "chg-8", Level: "high", Approver: "alice",
		Status: "approved", Comment: "{broken", PlanHash: "v2:abc", ActedAt: &now,
	}))

	tk, err := Collect(ctx, store, "chg-8")
	require.NoError(t, err)
	assert.True(t, tk.Approval.HasApproval)
	assert.Equal(t, "high", tk.Approval.Level, "the raw row still carries the level")
	assert.Empty(t, tk.Approval.Decisions, "the undecodable metadata is skipped, not guessed")
}

// A plan artifact that exists but cannot be parsed must be reported as
// unreadable. Otherwise the ticket renders "no SQL / no rollback declared" —
// a claim about the change that the store cannot support — where the truth
// is that nobody knows.
func TestCollect_UnreadablePlanIsMarkedUnreadable(t *testing.T) {
	store := newStore(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)
	require.NoError(t, store.CreateRun(ctx, &state.Run{
		ID: "chg-9", Status: "approved", PlanHash: "v2:abc", CreatedAt: now, UpdatedAt: now,
	}))
	require.NoError(t, store.UpdateRunPlan(ctx, "chg-9", "{not json", "v2:abc", now))

	tk, err := Collect(ctx, store, "chg-9")
	require.NoError(t, err)
	assert.Equal(t, "unreadable", tk.Change.PlanState)
	assert.False(t, tk.SQL.HasSQL)
}

// A change with no plan at all is a different state from an unreadable one:
// there is nothing to read yet, and the ticket may say so plainly.
func TestCollect_NoPlanStateIsNone(t *testing.T) {
	store := newStore(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)
	require.NoError(t, store.CreateRun(ctx, &state.Run{
		ID: "chg-10", Status: "draft", CreatedAt: now, UpdatedAt: now,
	}))
	tk, err := Collect(ctx, store, "chg-10")
	require.NoError(t, err)
	assert.Equal(t, "none", tk.Change.PlanState)
}

// A legacy approval row carries no plan hash: it blesses no particular plan.
// state.Approval.MatchesPlan reports such a row as matching anything (a
// compatibility rule), and the ticket must not repeat that as "bound to the
// current plan".
func TestCollect_LegacyApprovalIsNotReportedAsBound(t *testing.T) {
	store := newStore(t)
	ctx := context.Background()
	seedPlan(t, store, "chg-11")
	now := time.Now().UTC().Truncate(time.Second)
	require.NoError(t, store.CreateApproval(ctx, &state.Approval{
		ID: "appr-legacy", RunID: "chg-11", Level: "high", Approver: "alice",
		Status: "approved", ActedAt: &now,
	}))

	tk, err := Collect(ctx, store, "chg-11")
	require.NoError(t, err)
	assert.True(t, tk.Approval.HasApproval)
	assert.False(t, tk.Approval.MatchesPlan, "a legacy row binds to no plan")
	assert.True(t, tk.Approval.LegacyApproval)
	assert.False(t, tk.Approval.StaleApproval)
}

// A row bound to a plan this run no longer carries is stale, and must be
// visible as such rather than silently folded into the counts.
func TestCollect_StaleApprovalIsReportedAsStale(t *testing.T) {
	store := newStore(t)
	ctx := context.Background()
	seedPlan(t, store, "chg-12")
	now := time.Now().UTC().Truncate(time.Second)
	require.NoError(t, store.CreateApproval(ctx, &state.Approval{
		ID: "appr-stale", RunID: "chg-12", Level: "high", Approver: "alice",
		Status: "approved", PlanHash: "v2:an-old-plan", ActedAt: &now,
	}))

	tk, err := Collect(ctx, store, "chg-12")
	require.NoError(t, err)
	assert.False(t, tk.Approval.MatchesPlan)
	assert.True(t, tk.Approval.StaleApproval)
	assert.False(t, tk.Approval.LegacyApproval)
}

// The ticket's headline promise is the audit chain's integrity, so a TAMPERED
// chain must render as invalid. Without this case, `t.Audit.Valid = true`
// passes the whole package unnoticed (measured: the mutation stayed green).
func TestCollect_TamperedChainIsInvalid(t *testing.T) {
	store := newStore(t)
	ctx := context.Background()
	seedPlan(t, store, "chg-13")
	// A trace row whose CurrHash was never built is a broken chain: the
	// verifier reports it as tampered rather than as a pass.
	require.NoError(t, store.CreateTrace(ctx, &state.Trace{
		ID: "trace-tampered", RunID: "chg-13", Event: "step_execute", Actor: "system",
		Timestamp: time.Now().UTC(),
	}))

	tk, err := Collect(ctx, store, "chg-13")
	require.NoError(t, err)
	assert.True(t, tk.Audit.Verified, "the chain was checked")
	assert.False(t, tk.Audit.Valid, "a tampered chain must not render as valid")
	assert.Equal(t, 1, tk.Audit.Count)
}

// A verification that could not be performed is not the same as an empty
// chain: the reader must be told which happened.
func TestCollect_VerifierFailureIsReportedAsAnError(t *testing.T) {
	base := newStore(t)
	ctx := context.Background()
	seedPlan(t, base, "chg-14")

	tk, err := Collect(ctx, &failingTraceStore{Store: base, failAt: 2}, "chg-14")
	require.NoError(t, err)
	assert.False(t, tk.Audit.Verified)
	assert.True(t, tk.Audit.Unverifiable)
	assert.NotEmpty(t, tk.Audit.VerifyError, "a broken audit store is not an empty chain")
}

// failingTraceStore fails the Nth trace listing. Collect lists traces twice
// (once for the evidence verdict, once for the chain verifier), so failAt=2
// breaks only the verification.
type failingTraceStore struct {
	state.Store
	calls  int
	failAt int
}

func (s *failingTraceStore) ListTraces(ctx context.Context, filter state.TraceFilter) ([]*state.Trace, error) {
	s.calls++
	if s.calls == s.failAt {
		return nil, errors.New("audit store is unreachable")
	}
	return s.Store.ListTraces(ctx, filter)
}
