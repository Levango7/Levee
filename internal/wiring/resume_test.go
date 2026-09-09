// resume_test.go pins the resumable-retry evidence model
// (docs/design-cluster-failover.md, "断点续跑 v1"): on a retry out of
// "interrupted", batches whose every (step, host) combination already has a
// "success" row from an idempotent module are skipped; the engine then runs a
// filtered plan and the skipped steps are recorded as "skipped" evidence.
package wiring

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/nexus/levee/internal/executor"
	_ "github.com/nexus/levee/internal/executor/modules/file"
	_ "github.com/nexus/levee/internal/executor/modules/pkg"
	_ "github.com/nexus/levee/internal/executor/modules/shell"
	_ "github.com/nexus/levee/internal/executor/modules/svc"
	_ "github.com/nexus/levee/internal/executor/modules/user"
	"github.com/nexus/levee/internal/plan"
	"github.com/nexus/levee/internal/state"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// seedRunRow inserts the minimal run row required for batch/step FKs. It is
// idempotent: a missing row is created, an existing row is left untouched so
// callers may invoke it per batch without tripping runs.id uniqueness.
func seedRunRow(t *testing.T, store state.Store, runID, status string) {
	t.Helper()
	ctx := context.Background()
	// GetRun returns (nil, nil) for "not found" (ErrNoRows is not wrapped),
	// so we must check the returned *Run for nil, not the error.
	if existing, err := store.GetRun(ctx, runID); err == nil && existing != nil {
		return // already present
	}
	now := utcNow()
	require.NoError(t, store.CreateRun(ctx, &state.Run{
		ID: runID, WorkflowName: "wf", TemplateName: "tpl", Params: "{}",
		PlanHash: "ph", Status: status, ApprovalStatus: "approved",
		CreatedAt: now, UpdatedAt: now, Creator: "test",
	}))
}

// seedBatchAndSteps writes one batch row and one step row per (step, host)
// triple, all with status "success", mirroring what a crashed execution would
// have left behind for the batches that completed before the crash. The run
// row is seeded idempotently (batch.run_id FK).
func seedBatchAndSteps(t *testing.T, store state.Store, runID string, b plan.Batch, batchNo int) {
	t.Helper()
	seedRunRow(t, store, runID, "interrupted")
	now := utcNow()
	require.NoError(t, store.CreateBatch(context.Background(), &state.Batch{
		ID: batchNoID(runID, batchNo), RunID: runID, BatchNo: batchNo,
		Status: "completed", TotalHosts: len(b.Targets), Succeeded: len(b.Targets),
		StartedAt: &now, CompletedAt: &now,
	}))
	for _, ps := range b.Steps {
		for _, host := range b.Targets {
			require.NoError(t, store.CreateStep(context.Background(), &state.Step{
				ID: newID("stp-"), RunID: runID, BatchID: batchNoID(runID, batchNo),
				Host: host, StepName: ps.Name, Action: ps.Module + "." + ps.Action,
				Status: "success", DurationMs: 10, StartedAt: &now, CompletedAt: &now,
			}))
		}
	}
}

// batchNoID builds a deterministic batch row id from run + batch number so
// that ListBatches -> BatchID lookups in the resume logic resolve.
func batchNoID(runID string, batchNo int) string {
	return runID + "-batch-" + string(rune('0'+batchNo))
}

func TestCompletedIdempotentBatches_AllIdempotentAllSucceeded(t *testing.T) {
	eng, store := newResumeEngineAndStore(t)
	ctx := context.Background()
	const runID = "run-resume-1"

	// Two batches, both idempotent modules, both fully succeeded.
	p := &plan.Plan{
		ID: "plan-1", CreatedAt: utcNow(),
		Batches: []plan.Batch{
			{Index: 0, Targets: []string{"web-1"}, Steps: []plan.PlanStep{
				{Name: "install", Module: "pkg", Action: "install"},
			}},
			{Index: 1, Targets: []string{"web-1"}, Steps: []plan.PlanStep{
				{Name: "deploy", Module: "file", Action: "copy"},
			}},
		},
	}
	seedBatchAndSteps(t, store, runID, p.Batches[0], 1)
	seedBatchAndSteps(t, store, runID, p.Batches[1], 2)

	skip, err := eng.completedIdempotentBatches(ctx, runID, p)
	require.NoError(t, err)
	assert.True(t, skip[1], "batch 1 (pkg.install, all succeeded+idempotent) should be skippable")
	assert.True(t, skip[2], "batch 2 (file.copy, all succeeded+idempotent) should be skippable")
}

func TestCompletedIdempotentBatches_NonIdempotentBatchNotSkipped(t *testing.T) {
	eng, store := newResumeEngineAndStore(t)
	ctx := context.Background()
	const runID = "run-resume-2"

	p := &plan.Plan{
		ID: "plan-2", CreatedAt: utcNow(),
		Batches: []plan.Batch{
			{Index: 0, Targets: []string{"web-1"}, Steps: []plan.PlanStep{
				{Name: "install", Module: "pkg", Action: "install"},
			}},
			// shell.exec is non-idempotent: even with a success row, never skip.
			{Index: 1, Targets: []string{"web-1"}, Steps: []plan.PlanStep{
				{Name: "boot", Module: "shell", Action: "exec"},
			}},
		},
	}
	seedBatchAndSteps(t, store, runID, p.Batches[0], 1)
	seedBatchAndSteps(t, store, runID, p.Batches[1], 2)

	skip, err := eng.completedIdempotentBatches(ctx, runID, p)
	require.NoError(t, err)
	assert.True(t, skip[1], "idempotent batch should be skippable")
	assert.False(t, skip[2], "non-idempotent (shell) batch must NOT be skipped")
}

func TestCompletedIdempotentBatches_MissingStepRowNotSkipped(t *testing.T) {
	eng, store := newResumeEngineAndStore(t)
	ctx := context.Background()
	const runID = "run-resume-3"

	p := &plan.Plan{
		ID: "plan-3", CreatedAt: utcNow(),
		Batches: []plan.Batch{
			{Index: 0, Targets: []string{"web-1", "web-2"}, Steps: []plan.PlanStep{
				{Name: "install", Module: "pkg", Action: "install"},
			}},
		},
	}
	// Seed batch row + only ONE of the two host step rows: web-2 is missing.
	seedRunRow(t, store, runID, "interrupted")
	now := utcNow()
	require.NoError(t, store.CreateBatch(ctx, &state.Batch{
		ID: batchNoID(runID, 1), RunID: runID, BatchNo: 1, Status: "completed",
		TotalHosts: 2, Succeeded: 1, StartedAt: &now, CompletedAt: &now,
	}))
	require.NoError(t, store.CreateStep(ctx, &state.Step{
		ID: newID("stp-"), RunID: runID, BatchID: batchNoID(runID, 1),
		Host: "web-1", StepName: "install", Action: "pkg.install",
		Status: "success", DurationMs: 10, StartedAt: &now, CompletedAt: &now,
	}))

	skip, err := eng.completedIdempotentBatches(ctx, runID, p)
	require.NoError(t, err)
	assert.False(t, skip[1], "batch with a missing step row must NOT be skipped")
}

func TestCompletedIdempotentBatches_FailedStepRowNotSkipped(t *testing.T) {
	eng, store := newResumeEngineAndStore(t)
	ctx := context.Background()
	const runID = "run-resume-4"

	p := &plan.Plan{
		ID: "plan-4", CreatedAt: utcNow(),
		Batches: []plan.Batch{
			{Index: 0, Targets: []string{"web-1"}, Steps: []plan.PlanStep{
				{Name: "install", Module: "pkg", Action: "install"},
			}},
		},
	}
	// Seed the run row first (batch.run_id FK), then a failed step row.
	seedRunRow(t, store, runID, "interrupted")
	now := utcNow()
	require.NoError(t, store.CreateBatch(ctx, &state.Batch{
		ID: batchNoID(runID, 1), RunID: runID, BatchNo: 1, Status: "completed",
		TotalHosts: 1, Succeeded: 0, Failed: 1, StartedAt: &now, CompletedAt: &now,
	}))
	require.NoError(t, store.CreateStep(ctx, &state.Step{
		ID: newID("stp-"), RunID: runID, BatchID: batchNoID(runID, 1),
		Host: "web-1", StepName: "install", Action: "pkg.install",
		Status: "failed", DurationMs: 10, StartedAt: &now, CompletedAt: &now,
	}))

	skip, err := eng.completedIdempotentBatches(ctx, runID, p)
	require.NoError(t, err)
	assert.False(t, skip[1], "batch with a failed step row must NOT be skipped")
}

func TestCompletedIdempotentBatches_NoEvidenceReturnsNothing(t *testing.T) {
	eng, _ := newResumeEngineAndStore(t)
	ctx := context.Background()

	p := &plan.Plan{
		ID: "plan-5", CreatedAt: utcNow(),
		Batches: []plan.Batch{
			{Index: 0, Targets: []string{"web-1"}, Steps: []plan.PlanStep{
				{Name: "install", Module: "pkg", Action: "install"},
			}},
		},
	}
	// No batches, no steps persisted at all (fresh apply scenario).
	skip, err := eng.completedIdempotentBatches(ctx, "run-resume-5", p)
	require.NoError(t, err)
	assert.Empty(t, skip, "no evidence => nothing skippable")
}

func TestBuildResumePlan_RemovesSkippedBatchesAndRenumbers(t *testing.T) {
	p := &plan.Plan{
		ID: "plan-6", CreatedAt: utcNow(),
		Batches: []plan.Batch{
			{Index: 0, Targets: []string{"web-1"}, Steps: []plan.PlanStep{
				{Name: "install", Module: "pkg", Action: "install"},
			}},
			{Index: 1, Targets: []string{"web-1"}, Steps: []plan.PlanStep{
				{Name: "boot", Module: "shell", Action: "exec"},
			}},
			{Index: 2, Targets: []string{"web-1"}, Steps: []plan.PlanStep{
				{Name: "enable", Module: "svc", Action: "enable"},
			}},
		},
	}
	skip := map[int]bool{1: true} // skip the first batch

	resume, skipped := buildResumePlan(p, skip)

	// Two batches remain, renumbered 0 and 1.
	require.Len(t, resume.Batches, 2)
	assert.Equal(t, 0, resume.Batches[0].Index)
	assert.Equal(t, "shell", resume.Batches[0].Steps[0].Module) // was batch 1
	assert.Equal(t, 1, resume.Batches[1].Index)
	assert.Equal(t, "svc", resume.Batches[1].Steps[0].Module) // was batch 2

	// Skipped evidence covers the one removed batch.
	require.Len(t, skipped, 1)
	assert.Equal(t, 1, skipped[0].BatchNo) // ORIGINAL batch number
	assert.Equal(t, "install", skipped[0].StepName)
	assert.Equal(t, "pkg.install", skipped[0].Action)

	// Input plan is not mutated.
	assert.Len(t, p.Batches, 3)
	assert.Equal(t, 0, p.Batches[0].Index)
}

func TestBuildResumePlan_AllBatchesSkippedReturnsEmptyPlan(t *testing.T) {
	p := &plan.Plan{
		ID: "plan-7", CreatedAt: utcNow(),
		Batches: []plan.Batch{
			{Index: 0, Targets: []string{"web-1"}, Steps: []plan.PlanStep{
				{Name: "install", Module: "pkg", Action: "install"},
			}},
		},
	}
	resume, skipped := buildResumePlan(p, map[int]bool{1: true})
	assert.Empty(t, resume.Batches)
	assert.Len(t, skipped, 1)
}

func TestBuildResumePlan_EmptyBatchNeverSkipped(t *testing.T) {
	// A batch with no steps (or no targets) is never reported as completable
	// even if it somehow had a batch row — avoids a surprising "skip
	// everything" when evidence is absent.
	p := &plan.Plan{
		ID: "plan-8", CreatedAt: utcNow(),
		Batches: []plan.Batch{
			{Index: 0, Targets: []string{"web-1"}, Steps: []plan.PlanStep{}},
		},
	}
	resume, skipped := buildResumePlan(p, map[int]bool{1: true})
	require.Len(t, resume.Batches, 1, "empty batch is kept, not skipped")
	assert.Empty(t, skipped)
}

// newResumeEngineAndStore returns an Engine backed by a fresh in-memory
// SQLite store (via the package's shared newTestEngine helper). The default
// executor module registry is populated by the modules' init() funcs.
func newResumeEngineAndStore(t *testing.T) (*Engine, state.Store) {
	t.Helper()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "resume-test.db")
	store, err := state.NewSQLiteStore(ctx, path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	eng := NewEngine(store)
	_ = executor.DefaultExecutor()
	return eng, store
}
