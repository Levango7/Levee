// Package template implements change-run cloning for LEVEE. Cloning takes an
// existing historical run and produces an editable draft copy that preserves
// the original's parameters, batch structure and step definitions but starts
// a fresh execution history (no trace, no audit, no approval records).
//
// The cloned run gets a brand-new ID, its status is reset to "draft" and its
// creator is set to the actor performing the clone. Operators can then edit
// the draft (adjust parameters, add or remove hosts, etc.) before submitting
// it for approval and execution.
//
// The package is safe for concurrent use provided the underlying state.Store
// is.
package template

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/nexus/levee/internal/log"
	"github.com/nexus/levee/internal/state"
)

// --- Status / action / result constants -------------------------------------
//
// These mirror the values used by the engine and stored in state.Run.Status.
// They are repeated here to keep the template package self-documenting and
// avoid importing the engine package (which would create an import cycle).

const (
	// StatusDraft is the status assigned to a cloned run. A draft is editable
	// and has not yet been submitted for approval or execution.
	StatusDraft = "draft"

	// StatusPending is the default batch/step status assigned to cloned
	// batches and steps so they appear as "not yet started".
	StatusPending = "pending"
)

// Audit action constants recorded in state.Audit.Action.
const (
	ActionClone = "clone"
)

// Audit result constants recorded in state.Audit.Result.
const (
	ResultSuccess = "success"
	ResultFailed  = "failed"
)

// --- Sentinel errors --------------------------------------------------------

var (
	// ErrRunNotFound is returned when the source run does not exist.
	ErrRunNotFound = errors.New("template: run not found")

	// ErrEmptyRunID is returned when the source run identifier is empty.
	ErrEmptyRunID = errors.New("template: empty run id")

	// ErrEmptyActor is returned when the actor identifier is empty.
	ErrEmptyActor = errors.New("template: empty actor")

	// ErrCloneFailed is returned when the clone operation fails partway
	// through; the underlying error is wrapped via fmt.Errorf.
	ErrCloneFailed = errors.New("template: clone failed")
)

// --- CloneResult ------------------------------------------------------------

// CloneResult is the outcome of a successful Clone call. It records the
// original run ID, the newly generated cloned run ID, the clone timestamp
// and the actor who performed the clone.
type CloneResult struct {
	OriginalRunID string    // OriginalRunID is the ID of the source run.
	ClonedRunID   string    // ClonedRunID is the ID of the newly created draft run.
	ClonedAt      time.Time // ClonedAt is the UTC timestamp of the clone.
	ClonedBy      string    // ClonedBy is the actor who performed the clone.
}

// --- RunCloner --------------------------------------------------------------

// RunCloner clones a historical change run into an editable draft copy.
// The copy preserves the original run's parameters, batch structure, step
// definitions and target host list, but gets a new run ID, status "draft"
// and a fresh creator. The original run's trace, audit and approval records
// are not copied: the cloned run starts with a clean execution history.
//
// A RunCloner is safe for concurrent use provided the underlying state.Store
// is.
type RunCloner struct {
	store state.Store
}

// NewRunCloner returns a RunCloner backed by the given state.Store. The
// store must be non-nil; a nil store will cause subsequent operations to
// panic on nil-dereference and is therefore a programmer error.
func NewRunCloner(store state.Store) *RunCloner {
	return &RunCloner{store: store}
}

// Clone creates an editable draft copy of the run identified by runID.
//
// The clone:
//  1. Reads the source run, its batches and its steps.
//  2. Generates a fresh run ID (crypto/rand + hex).
//  3. Creates a new run with status "draft", creator=actor, preserving the
//     original workflow name, template name, parameters, plan hash, approval
//     level and incident ID.
//  4. Creates new batches with the same batch_no, status, total_hosts and
//     host counters as the originals (but reset to "pending" status and zero
//     succeeded/failed counts).
//  5. Creates new steps with the same host, step_name and action as the
//     originals (but reset to "pending" status and cleared execution output).
//  6. Writes an audit entry recording the clone action.
//
// Returns:
//   - ErrEmptyRunID / ErrEmptyActor for empty inputs
//   - ErrRunNotFound when the source run does not exist
//   - ErrCloneFailed (wrapping the underlying error) on persistence failure
func (c *RunCloner) Clone(ctx context.Context, runID, actor string) (*CloneResult, error) {
	if runID == "" {
		return nil, ErrEmptyRunID
	}
	if actor == "" {
		return nil, ErrEmptyActor
	}

	// 1. Read the source run.
	srcRun, err := c.store.GetRun(ctx, runID)
	if err != nil {
		return nil, fmt.Errorf("template: get source run: %w", err)
	}
	if srcRun == nil {
		return nil, ErrRunNotFound
	}

	// 2. Read the source run's batches and steps.
	srcBatches, err := c.store.ListBatches(ctx, state.BatchFilter{RunID: runID})
	if err != nil {
		return nil, fmt.Errorf("template: list source batches: %w", err)
	}
	srcSteps, err := c.store.ListSteps(ctx, state.StepFilter{RunID: runID})
	if err != nil {
		return nil, fmt.Errorf("template: list source steps: %w", err)
	}

	// 3. Generate a fresh run ID and clone timestamp.
	clonedRunID, err := newRunID()
	if err != nil {
		return nil, fmt.Errorf("%w: generate run id: %v", ErrCloneFailed, err)
	}
	now := time.Now().UTC()

	// 4. Create the cloned run with status "draft".
	clonedRun := &state.Run{
		ID:             clonedRunID,
		WorkflowName:   srcRun.WorkflowName,
		TemplateName:   srcRun.TemplateName,
		Params:         srcRun.Params,
		PlanHash:       srcRun.PlanHash,
		Status:         StatusDraft,
		ApprovalStatus: "pending",
		ApprovalLevel:  srcRun.ApprovalLevel,
		CreatedAt:      now,
		UpdatedAt:      now,
		Creator:        actor,
		IncidentID:     srcRun.IncidentID,
	}
	if err := c.store.CreateRun(ctx, clonedRun); err != nil {
		return nil, fmt.Errorf("%w: create cloned run: %v", ErrCloneFailed, err)
	}

	// 5. Create the cloned batches. We preserve batch_no, total_hosts and
	//    the batch ID mapping (old ID -> new ID) so steps can be re-linked.
	//    Status is reset to "pending" and succeeded/failed counters zeroed
	//    because the cloned run has not executed yet.
	batchIDMap := make(map[string]string, len(srcBatches))
	for _, b := range srcBatches {
		newBatchID, err := newBatchID()
		if err != nil {
			return nil, fmt.Errorf("%w: generate batch id: %v", ErrCloneFailed, err)
		}
		batchIDMap[b.ID] = newBatchID

		clonedBatch := &state.Batch{
			ID:          newBatchID,
			RunID:       clonedRunID,
			BatchNo:     b.BatchNo,
			Status:      StatusPending,
			TotalHosts:  b.TotalHosts,
			Succeeded:   0,
			Failed:      0,
			StartedAt:   nil,
			CompletedAt: nil,
		}
		if err := c.store.CreateBatch(ctx, clonedBatch); err != nil {
			return nil, fmt.Errorf("%w: create cloned batch %d: %v", ErrCloneFailed, b.BatchNo, err)
		}
	}

	// 6. Create the cloned steps. We preserve host, step_name and action
	//    but reset status to "pending" and clear execution output because
	//    the cloned run has not executed yet. Steps are re-linked to the
	//    new batch IDs via batchIDMap; any step whose original batch is
	//    missing from the map (defensive: should not happen) is skipped.
	for _, s := range srcSteps {
		newStepID, err := newStepID()
		if err != nil {
			return nil, fmt.Errorf("%w: generate step id: %v", ErrCloneFailed, err)
		}
		newBatchID, ok := batchIDMap[s.BatchID]
		if !ok {
			// Defensive: the step references a batch that was not cloned.
			// Skip it rather than failing the whole clone.
			log.WarnCtx(ctx, "clone: step references missing batch, skipping",
				"step_id", s.ID, "batch_id", s.BatchID, "run_id", runID)
			continue
		}

		clonedStep := &state.Step{
			ID:          newStepID,
			RunID:       clonedRunID,
			BatchID:     newBatchID,
			Host:        s.Host,
			StepName:    s.StepName,
			Action:      s.Action,
			Status:      StatusPending,
			ExitCode:    nil,
			Stdout:      "",
			Stderr:      "",
			DurationMs:  0,
			StartedAt:   nil,
			CompletedAt: nil,
		}
		if err := c.store.CreateStep(ctx, clonedStep); err != nil {
			return nil, fmt.Errorf("%w: create cloned step: %v", ErrCloneFailed, err)
		}
	}

	// 7. Write an audit entry recording the clone action.
	audit := &state.Audit{
		ID:        newAuditID(),
		RunID:     clonedRunID,
		Action:    ActionClone,
		Actor:     actor,
		Target:    runID,
		Result:    ResultSuccess,
		Timestamp: now,
	}
	if err := c.store.CreateAudit(ctx, audit); err != nil {
		// Audit write failure is observability-only: the clone has already
		// been persisted, so we log and continue rather than undoing it.
		log.WarnCtx(ctx, "clone audit write failed",
			"original_run_id", runID, "cloned_run_id", clonedRunID, "actor", actor, "err", err)
	}

	log.InfoCtx(ctx, "run cloned",
		"original_run_id", runID, "cloned_run_id", clonedRunID, "actor", actor,
		"batches", len(srcBatches), "steps", len(srcSteps))

	return &CloneResult{
		OriginalRunID: runID,
		ClonedRunID:   clonedRunID,
		ClonedAt:      now,
		ClonedBy:      actor,
	}, nil
}

// --- ID generators ----------------------------------------------------------
//
// Each generator uses crypto/rand to produce 16 hex characters (8 random
// bytes). The prefix makes the ID kind identifiable in logs and audit
// trails. On the extremely unlikely event that rand.Read fails, the
// generator returns an error so the caller can wrap it with ErrCloneFailed.

func newRunID() (string, error) {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return "run-" + hex.EncodeToString(b), nil
}

func newBatchID() (string, error) {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return "batch-" + hex.EncodeToString(b), nil
}

func newStepID() (string, error) {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return "step-" + hex.EncodeToString(b), nil
}

func newAuditID() string {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		// Fallback: timestamp-based ID so the audit entry is still usable.
		return fmt.Sprintf("audit-%d", time.Now().UnixNano())
	}
	return "audit-" + hex.EncodeToString(b)
}
