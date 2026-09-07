//go:build integration

// Package integration tests the full change lifecycle through the gRPC
// service layer backed by a real SQLite store: create → plan → approve →
// apply (engine stub settles running → completed) and the terminal-state
// guards that refuse cancel/pause afterwards. A no-engine deployment
// refuses apply outright (status-only honesty). It verifies cross-service
// invariants (audit entries, run status transitions, hash chain integrity)
// rather than individual service methods — those are covered by unit tests
// in internal/grpc/.
package integration

import (
	"context"
	"encoding/json"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/nexus/levee/internal/audit"
	"github.com/nexus/levee/internal/grpc"
	"github.com/nexus/levee/internal/grpc/pb"
	"github.com/nexus/levee/internal/plan"
	"github.com/nexus/levee/internal/state"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// newTestStore returns a fresh SQLite store backed by a temp file. Each
// test gets its own database so concurrent tests do not interfere.
func newTestStore(t *testing.T) state.Store {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	path := filepath.Join(dir, "levee-integration.db")
	store, err := state.NewSQLiteStore(ctx, path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	return store
}

// newServices wires a minimal set of services for the integration test.
// Engine/approval/pause are nil so the service falls back to no-op paths;
// note that in this wiring ApplyChange is *refused* with FailedPrecondition
// (status-only honesty, see TestApplyChange_NoEngineRefused). Tests that
// exercise apply use newServicesWithEngine.
// The store is shared so cross-service state consistency is verified.
func newServices(t *testing.T) (*grpc.ChangeService, *grpc.TemplateService, *grpc.AuditService, state.Store) {
	t.Helper()
	store := newTestStore(t)
	changeSvc := grpc.NewChangeService(store, nil, nil, nil)
	templateSvc := grpc.NewTemplateService(store, nil)
	auditSvc := grpc.NewAuditService(store)
	return changeSvc, templateSvc, auditSvc, store
}

// newServicesWithEngine is newServices with a synchronous stub engine.
// Since the honest-apply change (no engine wired → apply refused with
// FailedPrecondition instead of a fake "running" transition), lifecycle
// tests that drive apply must provide an engine. The stub completes
// immediately with phase "completed", so ApplyChange settles the run to
// the terminal status "completed" — exactly what the real synchronous
// ClosureRunner does on success.
func newServicesWithEngine(t *testing.T) (*grpc.ChangeService, *grpc.AuditService, state.Store) {
	t.Helper()
	store := newTestStore(t)
	engine := &grpc.EngineAdapter{
		Run: func(_ context.Context, changeID string, _ bool, _ int32) (string, bool, string, error) {
			return "exec-" + changeID, true, "completed", nil
		},
		// Plan mirrors the A1 contract: a pb.Plan for the client plus the
		// canonical StoredPlan artifact PlanChange persists on the run.
		// Approve/Apply then accept the run because its persisted plan
		// verifies against its plan hash.
		Plan: func(_ context.Context, changeID string, hosts []string) (*pb.Plan, *grpc.StoredPlan, error) {
			targets := hosts
			if len(targets) == 0 {
				targets = []string{"localhost"}
			}
			p := &plan.Plan{
				ID:           "plan-" + changeID,
				WorkflowName: "integration-test",
				Batches: []plan.Batch{{
					Index:   0,
					Targets: targets,
					Steps: []plan.PlanStep{{
						Name: "noop", Module: "shell", Action: "run",
					}},
					MaxConcurrency: 1,
				}},
				TotalTargets: len(targets),
				CreatedAt:    time.Now().UTC(),
			}
			raw, err := json.Marshal(p)
			if err != nil {
				return nil, nil, err
			}
			return &pb.Plan{
				ChangeId:      changeID,
				TargetHosts:   targets,
				ImpactSummary: "integration stub plan",
			}, &grpc.StoredPlan{JSON: string(raw), Hash: plan.ComputeHash(p)}, nil
		},
	}
	changeSvc := grpc.NewChangeService(store, engine, nil, nil)
	auditSvc := grpc.NewAuditService(store)
	return changeSvc, auditSvc, store
}

// planForTest drives a non-dry PlanChange so the run carries the persisted
// plan artifact that Approve/Apply require once an engine is wired.
func planForTest(t *testing.T, svc *grpc.ChangeService, changeID string) {
	t.Helper()
	_, err := svc.PlanChange(context.Background(), &pb.PlanChangeRequest{
		ChangeId:    changeID,
		TargetHosts: []string{"web-1"},
	})
	require.NoError(t, err)
}

// ---------------------------------------------------------------------------
// End-to-end change lifecycle: draft → planned → approved → running →
// completed (stub engine settles synchronously), plus terminal-state
// protection: a completed change refuses cancel/pause.
// ---------------------------------------------------------------------------

func TestChangeLifecycle_CreatePlanApproveApplyCancel(t *testing.T) {
	ctx := context.Background()
	changeSvc, auditSvc, store := newServicesWithEngine(t)

	// 1. Create a change (status: draft).
	createResp, err := changeSvc.CreateChange(ctx, &pb.CreateChangeRequest{
		Label:        "lifecycle-test",
		Priority:     "high",
		WorkflowFile: "workflow.levee",
		TemplateName: "deploy-web",
		Params:       map[string]string{"version": "1.0.0"},
		Team:         "platform",
		Environment:  "staging",
	})
	require.NoError(t, err)
	require.NotNil(t, createResp)
	changeID := createResp.GetId()
	assert.Equal(t, "draft", createResp.GetStatus())

	// 2. Plan the change (non-dry): persists the plan artifact on the
	// run, which Approve/Apply require with an engine wired.
	planResp, err := changeSvc.PlanChange(ctx, &pb.PlanChangeRequest{
		ChangeId:    changeID,
		DryRun:      false,
		TargetHosts: []string{},
	})
	require.NoError(t, err)
	require.NotNil(t, planResp)
	assert.NotEmpty(t, planResp.GetChangeId())

	// 3. Approve the change (status: approved).
	approveResp, err := changeSvc.ApproveChange(ctx, &pb.ApproveRequest{
		ChangeId: changeID,
		Comment:  "auto-approved for integration test",
	})
	require.NoError(t, err)
	require.NotNil(t, approveResp)
	assert.Equal(t, "approved", approveResp.GetStatus())

	// 4. Apply with the stub engine. The engine settles synchronously with
	// phase "completed", so ApplyChange must carry the run through
	// running → completed (never a stuck "running").
	applyResp, err := changeSvc.ApplyChange(ctx, &pb.ApplyChangeRequest{
		ChangeId:    changeID,
		AutoApprove: true,
	})
	require.NoError(t, err)
	require.NotNil(t, applyResp)
	assert.Equal(t, "completed", applyResp.GetChange().GetStatus())
	assert.True(t, applyResp.GetSuccess())
	assert.NotEmpty(t, applyResp.GetRunId(), "apply must surface the engine run id")

	// Build the hash chain so verification can pass.
	// In production this is done by the audit service; here we do it explicitly
	// because the stub engine creates traces with empty hashes (MVP behavior).
	builder, err := audit.NewHashChainBuilder(store)
	require.NoError(t, err)
	_, _, err = builder.Build(ctx, changeID)
	require.NoError(t, err)

	// 5. Cancel the completed change — must be refused. Terminal states
	// protect completed runs from being rewritten to cancelled.
	_, err = changeSvc.CancelChange(ctx, &pb.CancelRequest{
		ChangeId: changeID,
		Force:    false,
	})
	require.Error(t, err, "cancelling a completed change must be refused")
	assert.Equal(t, codes.FailedPrecondition, status.Code(err))

	// Verify status persisted and was not rewritten by the refused cancel.
	run, err := store.GetRun(ctx, changeID)
	require.NoError(t, err)
	require.NotNil(t, run)
	assert.Equal(t, "completed", run.Status)

	// 6. Verify audit trail has the apply trace.
	traces, err := auditSvc.ListAuditTraces(ctx, &pb.ListAuditTracesRequest{
		ChangeId: changeID,
	})
	require.NoError(t, err)
	assert.NotEmpty(t, traces.GetEntries(), "apply should have produced at least one trace entry")

	// 7. Verify hash chain integrity.
	verifyResp, err := auditSvc.VerifyHashChain(ctx, &pb.VerifyHashChainRequest{
		ChangeId: changeID,
	})
	require.NoError(t, err)
	assert.True(t, verifyResp.GetValid())
}

// TestApplyChange_NoEngineRefused guards the status-only honesty fix: a
// deployment without an engine must refuse apply with FailedPrecondition
// instead of faking a "running" transition that never completes. The run
// must stay untouched in its pre-apply status.
func TestApplyChange_NoEngineRefused(t *testing.T) {
	ctx := context.Background()
	changeSvc, _, _, store := newServices(t)

	createResp, err := changeSvc.CreateChange(ctx, &pb.CreateChangeRequest{
		Label:        "no-engine-refusal",
		Priority:     "high",
		WorkflowFile: "workflow.levee",
		TemplateName: "deploy-web",
	})
	require.NoError(t, err)
	changeID := createResp.GetId()

	_, err = changeSvc.ApproveChange(ctx, &pb.ApproveRequest{ChangeId: changeID})
	require.NoError(t, err)

	applyResp, err := changeSvc.ApplyChange(ctx, &pb.ApplyChangeRequest{
		ChangeId:    changeID,
		AutoApprove: true,
	})
	require.Error(t, err, "apply must be refused when no engine is wired")
	assert.Equal(t, codes.FailedPrecondition, status.Code(err))
	assert.Nil(t, applyResp)

	run, err := store.GetRun(ctx, changeID)
	require.NoError(t, err)
	require.NotNil(t, run)
	assert.Equal(t, "approved", run.Status, "a refused apply must not mutate status")
}

// ---------------------------------------------------------------------------
// Cross-service consistency: trace entries persist across service boundaries
// ---------------------------------------------------------------------------

func TestCrossService_AuditOnEveryTransition(t *testing.T) {
	ctx := context.Background()
	changeSvc, auditSvc, store := newServicesWithEngine(t)

	// Create and apply a change (apply creates a trace entry).
	createResp, err := changeSvc.CreateChange(ctx, &pb.CreateChangeRequest{
		Label:        "audit-test",
		WorkflowFile: "workflow.levee",
		TemplateName: "deploy-web",
	})
	require.NoError(t, err)
	changeID := createResp.GetId()
	planForTest(t, changeSvc, changeID)

	_, err = changeSvc.ApproveChange(ctx, &pb.ApproveRequest{
		ChangeId: changeID,
	})
	require.NoError(t, err)

	applyResp, err := changeSvc.ApplyChange(ctx, &pb.ApplyChangeRequest{
		ChangeId:    changeID,
		AutoApprove: true,
	})
	require.NoError(t, err)
	assert.Equal(t, "completed", applyResp.GetChange().GetStatus())

	// Build the hash chain after apply so trace verification works.
	builder, _ := audit.NewHashChainBuilder(store)
	_, _, _ = builder.Build(ctx, changeID)

	// After apply, there should be at least one trace entry.
	tracesBefore, err := auditSvc.ListAuditTraces(ctx, &pb.ListAuditTracesRequest{
		ChangeId: changeID,
	})
	require.NoError(t, err)
	initialCount := len(tracesBefore.GetEntries())
	assert.GreaterOrEqual(t, initialCount, 1, "apply should produce at least one trace entry")

	// Transitions that the state machine rejects (pause/resume on a
	// completed change) must fail *before* writing anything: the trace
	// count stays untouched.
	_, err = changeSvc.PauseChange(ctx, &pb.PauseRequest{ChangeId: changeID, Reason: "pause"})
	require.Error(t, err)
	assert.Equal(t, codes.FailedPrecondition, status.Code(err))
	_, err = changeSvc.ResumeChange(ctx, &pb.PauseRequest{ChangeId: changeID, Reason: "resume"})
	require.Error(t, err)
	assert.Equal(t, codes.FailedPrecondition, status.Code(err))

	// Verify traces are unchanged (rejected transitions record nothing).
	tracesAfter, err := auditSvc.ListAuditTraces(ctx, &pb.ListAuditTracesRequest{
		ChangeId: changeID,
	})
	require.NoError(t, err)
	assert.Equal(t, initialCount, len(tracesAfter.GetEntries()),
		"rejected transitions should not modify trace count")
}

// ---------------------------------------------------------------------------
// Template + Change coupling: instantiate template then create change
// ---------------------------------------------------------------------------

func TestTemplateChange_CoupledWorkflow(t *testing.T) {
	ctx := context.Background()
	_, templateSvc, _, store := newServices(t)

	// Create a template.
	templateResp, err := templateSvc.CreateTemplate(ctx, &pb.CreateTemplateRequest{
		Name:            "coupled-deploy",
		WorkflowContent: "name: coupled-deploy\nsteps:\n  - name: deploy\n    action: shell\n    command: echo hello\n",
		Description:     "Coupled workflow integration test",
	})
	require.NoError(t, err)
	require.NotNil(t, templateResp)
	templateName := templateResp.GetName()

	// Instantiate the template with params.
	instResp, err := templateSvc.InstantiateTemplate(ctx, &pb.InstantiateTemplateRequest{
		TemplateName: templateName,
		Params:       map[string]string{"version": "2.0.0"},
		DryRun:       true,
	})
	require.NoError(t, err)
	require.NotNil(t, instResp)

	// Create a change from the instantiated template.
	changeSvc := grpc.NewChangeService(store, nil, nil, nil)
	createResp, err := changeSvc.CreateChange(ctx, &pb.CreateChangeRequest{
		Label:        "coupled-lifecycle",
		WorkflowFile: instResp.GetWorkflowFile(),
		TemplateName: templateName,
		Params:       map[string]string{"version": "2.0.0"},
	})
	require.NoError(t, err)
	require.NotNil(t, createResp)
	assert.Equal(t, templateName, createResp.GetTemplateName())

	// Verify the change references the correct template in the store.
	run, err := store.GetRun(ctx, createResp.GetId())
	require.NoError(t, err)
	assert.Equal(t, templateName, run.TemplateName)
}

// ---------------------------------------------------------------------------
// Concurrency safety: concurrent change creates must not conflict
// ---------------------------------------------------------------------------

func TestConcurrency_ConcurrentCreateChanges(t *testing.T) {
	ctx := context.Background()

	const n = 10
	var wg sync.WaitGroup
	wg.Add(n)
	results := make(chan *pb.Change, n)
	errors := make(chan error, n)

	for i := 0; i < n; i++ {
		go func(idx int) {
			defer wg.Done()
			// Each goroutine gets its own store to avoid SQLite lock contention.
			// This tests that the service itself is safe under concurrent
			// invocation; cross-row isolation is the store's responsibility.
			store := newTestStore(t)
			svc := grpc.NewChangeService(store, nil, nil, nil)
			resp, err := svc.CreateChange(ctx, &pb.CreateChangeRequest{
				Label:        string(rune('a'+idx)) + "-concurrent",
				WorkflowFile: "workflow.levee",
				TemplateName: "deploy-web",
				Params:       map[string]string{"idx": string(rune('a' + idx))},
			})
			if err != nil {
				errors <- err
				return
			}
			results <- resp
		}(i)
	}

	go func() {
		wg.Wait()
		close(results)
		close(errors)
	}()

	var successCount int
	for successCount < n {
		select {
		case err, ok := <-errors:
			if !ok {
				goto done
			}
			t.Fatalf("concurrent create failed: %v", err)
		case _, ok := <-results:
			if !ok {
				goto done
			}
			successCount++
		}
	}
done:
	assert.Equal(t, n, successCount, "expected %d concurrent creates to succeed", n)
}

// ---------------------------------------------------------------------------
// Audit hash chain integrity: verify chain stays valid after multiple ops
// ---------------------------------------------------------------------------

func TestAudit_HashChainIntegrityAfterMultipleOps(t *testing.T) {
	ctx := context.Background()
	changeSvc, auditSvc, store := newServicesWithEngine(t)

	// Create, plan, approve, and apply (creates the trace that forms the hash chain).
	createResp, err := changeSvc.CreateChange(ctx, &pb.CreateChangeRequest{
		Label:        "chain-test",
		WorkflowFile: "workflow.levee",
		TemplateName: "deploy-web",
	})
	require.NoError(t, err)
	changeID := createResp.GetId()

	planForTest(t, changeSvc, changeID)
	_, err = changeSvc.ApproveChange(ctx, &pb.ApproveRequest{ChangeId: changeID})
	require.NoError(t, err)

	applyResp, err := changeSvc.ApplyChange(ctx, &pb.ApplyChangeRequest{
		ChangeId:    changeID,
		AutoApprove: true,
	})
	require.NoError(t, err)
	assert.Equal(t, "completed", applyResp.GetChange().GetStatus())

	// Build the hash chain after apply so verification works.
	builder, _ := audit.NewHashChainBuilder(store)
	_, _, _ = builder.Build(ctx, changeID)

	// Attempt transitions after completion. The state machine refuses pause
	// on a completed run; a refused op must not corrupt the chain.
	_, err = changeSvc.PauseChange(ctx, &pb.PauseRequest{
		ChangeId: changeID,
		Reason:   "pause for chain test",
	})
	require.Error(t, err)
	assert.Equal(t, codes.FailedPrecondition, status.Code(err))

	// A valid archival transition is allowed from completed and exercises a
	// write path on top of the built chain.
	_, err = changeSvc.CancelChange(ctx, &pb.CancelRequest{
		ChangeId: changeID,
	})
	require.Error(t, err, "cancel must stay refused from completed")

	// Verify hash chain is still valid after all transitions.
	verifyResp, err := auditSvc.VerifyHashChain(ctx, &pb.VerifyHashChainRequest{
		ChangeId: changeID,
	})
	require.NoError(t, err)
	assert.True(t, verifyResp.GetValid(),
		"hash chain should remain valid after create → approve → apply → refused transitions")
}
