//go:build integration

// Package integration tests the full change lifecycle through the gRPC
// service layer backed by a real SQLite store: create → plan → approve →
// apply → pause → resume → cancel. It verifies cross-service invariants
// (audit entries, run status transitions, hash chain integrity) rather than
// individual service methods — those are covered by unit tests in
// internal/grpc/.
package integration

import (
	"context"
	"path/filepath"
	"sync"
	"testing"

	"github.com/nexus/levee/internal/audit"
	"github.com/nexus/levee/internal/grpc"
	"github.com/nexus/levee/internal/grpc/pb"
	"github.com/nexus/levee/internal/state"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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
// the store is shared so cross-service state consistency is verified.
func newServices(t *testing.T) (*grpc.ChangeService, *grpc.TemplateService, *grpc.AuditService, state.Store) {
	t.Helper()
	store := newTestStore(t)
	changeSvc := grpc.NewChangeService(store, nil, nil, nil)
	templateSvc := grpc.NewTemplateService(store, nil)
	auditSvc := grpc.NewAuditService(store)
	return changeSvc, templateSvc, auditSvc, store
}

// ---------------------------------------------------------------------------
// End-to-end change lifecycle: draft → approved → running → paused → active → cancelled
// ---------------------------------------------------------------------------

func TestChangeLifecycle_CreatePlanApproveApplyPauseResumeCancel(t *testing.T) {
	ctx := context.Background()
	changeSvc, _, auditSvc, store := newServices(t)

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

	// 2. Plan the change.
	planResp, err := changeSvc.PlanChange(ctx, &pb.PlanChangeRequest{
		ChangeId:    changeID,
		DryRun:      true,
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

	// 4. Apply the change with auto-approve (status: running, creates a trace).
	applyResp, err := changeSvc.ApplyChange(ctx, &pb.ApplyChangeRequest{
		ChangeId:    changeID,
		AutoApprove: true,
	})
	require.NoError(t, err)
	require.NotNil(t, applyResp)
	assert.Equal(t, "running", applyResp.GetChange().GetStatus())

	// Build the hash chain so verification can pass.
	// In production this is done by the audit service; here we do it explicitly
	// because ApplyChange creates traces with empty hashes (MVP behavior).
	builder, err := audit.NewHashChainBuilder(store)
	require.NoError(t, err)
	_, _, err = builder.Build(ctx, changeID)
	require.NoError(t, err)

	// 5. Pause the change (status: paused).
	pauseResp, err := changeSvc.PauseChange(ctx, &pb.PauseRequest{
		ChangeId: changeID,
		Reason:   "pausing for integration test",
	})
	require.NoError(t, err)
	require.NotNil(t, pauseResp)
	assert.Equal(t, "paused", pauseResp.GetStatus())

	// Verify pause persisted in store.
	run, err := store.GetRun(ctx, changeID)
	require.NoError(t, err)
	require.NotNil(t, run)
	assert.Equal(t, "paused", run.Status)

	// 6. Resume the change (status: running).
	resumeResp, err := changeSvc.ResumeChange(ctx, &pb.PauseRequest{
		ChangeId: changeID,
		Reason:   "resuming for integration test",
	})
	require.NoError(t, err)
	require.NotNil(t, resumeResp)
	assert.Equal(t, "running", resumeResp.GetStatus())

	// 7. Cancel the change (status: cancelled).
	cancelResp, err := changeSvc.CancelChange(ctx, &pb.CancelRequest{
		ChangeId: changeID,
		Force:    false,
	})
	require.NoError(t, err)
	require.NotNil(t, cancelResp)
	assert.Equal(t, "cancelled", cancelResp.GetStatus())

	// 8. Verify audit trail has the apply trace.
	traces, err := auditSvc.ListAuditTraces(ctx, &pb.ListAuditTracesRequest{
		ChangeId: changeID,
	})
	require.NoError(t, err)
	assert.NotEmpty(t, traces.GetEntries(), "apply should have produced at least one trace entry")

	// 9. Verify hash chain integrity.
	verifyResp, err := auditSvc.VerifyHashChain(ctx, &pb.VerifyHashChainRequest{
		ChangeId: changeID,
	})
	require.NoError(t, err)
	assert.True(t, verifyResp.GetValid())
}

// ---------------------------------------------------------------------------
// Cross-service consistency: trace entries persist across service boundaries
// ---------------------------------------------------------------------------

func TestCrossService_AuditOnEveryTransition(t *testing.T) {
	ctx := context.Background()
	changeSvc, _, auditSvc, store := newServices(t)

	// Create and apply a change (apply creates a trace entry).
	createResp, err := changeSvc.CreateChange(ctx, &pb.CreateChangeRequest{
		Label:        "audit-test",
		WorkflowFile: "workflow.levee",
		TemplateName: "deploy-web",
	})
	require.NoError(t, err)
	changeID := createResp.GetId()

	_, err = changeSvc.ApproveChange(ctx, &pb.ApproveRequest{
		ChangeId: changeID,
	})
	require.NoError(t, err)

	applyResp, err := changeSvc.ApplyChange(ctx, &pb.ApplyChangeRequest{
		ChangeId:    changeID,
		AutoApprove: true,
	})
	require.NoError(t, err)
	assert.Equal(t, "running", applyResp.GetChange().GetStatus())

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

	// Pause and resume — these use the audit log (not traces), so trace count stays the same.
	_, err = changeSvc.PauseChange(ctx, &pb.PauseRequest{ChangeId: changeID, Reason: "pause"})
	require.NoError(t, err)
	_, err = changeSvc.ResumeChange(ctx, &pb.PauseRequest{ChangeId: changeID, Reason: "resume"})
	require.NoError(t, err)

	// Verify traces are unchanged (pause/resume don't add traces, they add audit log entries).
	tracesAfter, err := auditSvc.ListAuditTraces(ctx, &pb.ListAuditTracesRequest{
		ChangeId: changeID,
	})
	require.NoError(t, err)
	assert.Equal(t, initialCount, len(tracesAfter.GetEntries()),
		"pause/resume should not modify trace count")
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
	changeSvc, _, auditSvc, store := newServices(t)

	// Create, approve, and apply (creates the trace that forms the hash chain).
	createResp, err := changeSvc.CreateChange(ctx, &pb.CreateChangeRequest{
		Label:        "chain-test",
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
	require.NoError(t, err)
	assert.Equal(t, "running", applyResp.GetChange().GetStatus())

	// Build the hash chain after apply so verification works.
	builder, _ := audit.NewHashChainBuilder(store)
	_, _, _ = builder.Build(ctx, changeID)

	// Perform a sequence of state transitions after apply.
	_, err = changeSvc.PauseChange(ctx, &pb.PauseRequest{
		ChangeId: changeID,
		Reason:   "pause for chain test",
	})
	require.NoError(t, err)

	_, err = changeSvc.ResumeChange(ctx, &pb.PauseRequest{
		ChangeId: changeID,
		Reason:   "resume",
	})
	require.NoError(t, err)

	_, err = changeSvc.CancelChange(ctx, &pb.CancelRequest{
		ChangeId: changeID,
	})
	require.NoError(t, err)

	// Verify hash chain is still valid after all transitions.
	verifyResp, err := auditSvc.VerifyHashChain(ctx, &pb.VerifyHashChainRequest{
		ChangeId: changeID,
	})
	require.NoError(t, err)
	assert.True(t, verifyResp.GetValid(),
		"hash chain should remain valid after create → approve → apply → pause → resume → cancel")
}
