// Package pause implements run-level and global pause/resume operations for
// LEVEE's change pipeline. A run represents a top-level change execution
// unit; pausing a run freezes its progress (no new batches are dispatched)
// while preserving all state for later resumption.
//
// Two scopes are supported:
//   - Single run: PauseRun / ResumeRun operate on one run by ID. These
//     require the run to be in a pausable ("running" or "pending") or
//     resumable ("paused") state respectively.
//   - Global: PauseAll / ResumeAll operate on every matching run in the
//     store. These require elevated permissions, checked via the
//     PermissionChecker abstraction.
//
// Every successful or attempted operation is recorded as a state.Audit
// entry so that the operator action trail is preserved. The package is
// safe for concurrent use provided the underlying state.Store is.
package pause

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
// These mirror the values used by the engine and stored in
// state.Run.Status. They are repeated here to keep the pause package
// self-documenting and avoid importing the engine package (which would
// create an import cycle).

const (
	StatusPending   = "pending"
	StatusRunning   = "running"
	StatusPaused    = "paused"
	StatusCompleted = "completed"
	StatusFailed    = "failed"
	StatusCancelled = "cancelled"
)

// Audit action constants recorded in state.Audit.Action.
const (
	ActionPause     = "pause"
	ActionResume    = "resume"
	ActionPauseAll  = "pause_all"
	ActionResumeAll = "resume_all"
)

// TargetAll is the audit Target wildcard used for global summary entries.
const TargetAll = "*"

// Audit result constants recorded in state.Audit.Result.
const (
	ResultSuccess = "success"
	ResultFailed  = "failed"
)

// Permission strings consumed by PauseAll / ResumeAll and checked via
// PermissionChecker.HasPermission.
const (
	PermissionPauseAll  = "pause:all"
	PermissionResumeAll = "resume:all"
)

// --- Sentinel errors --------------------------------------------------------

var (
	// ErrRunNotFound is returned when the target run does not exist.
	ErrRunNotFound = errors.New("pause: run not found")

	// ErrNotPausable is returned when the run is not in a pausable
	// state ("running" or "pending").
	ErrNotPausable = errors.New("pause: run is not in a pausable state")

	// ErrNotResumable is returned when the run is not paused.
	ErrNotResumable = errors.New("pause: run is not paused")

	// ErrPermissionDenied is returned when the actor lacks the
	// required permission for a global operation.
	ErrPermissionDenied = errors.New("pause: permission denied")

	// ErrEmptyRunID is returned when the run identifier is empty.
	ErrEmptyRunID = errors.New("pause: empty run id")

	// ErrEmptyActor is returned when the actor identifier is empty.
	ErrEmptyActor = errors.New("pause: empty actor")
)

// pausableStates is the set of run statuses from which a pause is allowed.
var pausableStates = map[string]struct{}{
	StatusRunning: {},
	StatusPending: {},
}

// resumableStates is the set of run statuses from which a resume is allowed.
var resumableStates = map[string]struct{}{
	StatusPaused: {},
}

// --- PermissionChecker ------------------------------------------------------

// PermissionChecker is the abstraction used to authorise global
// pause/resume operations. The MVP uses SimplePermissionChecker; a
// production deployment can swap in an RBAC-backed implementation
// without changing PauseManager.
type PermissionChecker interface {
	HasPermission(actor string, permission string) bool
}

// SimplePermissionChecker is a map-based PermissionChecker. The map keys
// are actor identifiers and the values are the set of permissions granted
// to that actor. A nil or empty checker denies every permission.
type SimplePermissionChecker struct {
	perms map[string][]string
}

// NewSimplePermissionChecker builds a SimplePermissionChecker from a map
// of actor → permissions. The map and its slices are copied so later
// mutations to the caller's map do not affect the checker.
func NewSimplePermissionChecker(perms map[string][]string) *SimplePermissionChecker {
	copied := make(map[string][]string, len(perms))
	for k, v := range perms {
		cp := make([]string, len(v))
		copy(cp, v)
		copied[k] = cp
	}
	return &SimplePermissionChecker{perms: copied}
}

// HasPermission reports whether the actor holds the named permission.
// A nil receiver always returns false.
func (c *SimplePermissionChecker) HasPermission(actor, permission string) bool {
	if c == nil {
		return false
	}
	for _, p := range c.perms[actor] {
		if p == permission {
			return true
		}
	}
	return false
}

// --- PauseResult ------------------------------------------------------------

// PauseResult describes the outcome of a global PauseAll / ResumeAll call.
// Affected lists the run IDs that were transitioned successfully; Failed
// maps a run ID to the error that prevented its transition. The Action
// field records whether this was a pause_all or resume_all.
type PauseResult struct {
	Action   string
	Affected []string
	Failed   map[string]error
}

// --- PauseManager -----------------------------------------------------------

// PauseManager manages single-run and global pause/resume operations. It
// reads and writes run state via state.Store and records every operation
// as a state.Audit entry for traceability.
//
// A PauseManager is safe for concurrent use provided the underlying
// state.Store is.
type PauseManager struct {
	store state.Store
}

// NewPauseManager returns a PauseManager backed by the given state.Store.
// The store must be non-nil; a nil store will cause subsequent operations
// to panic on nil-dereference and is therefore a programmer error.
func NewPauseManager(store state.Store) *PauseManager {
	return &PauseManager{store: store}
}

// PauseRun transitions a single run from "running" or "pending" to
// "paused" and records an audit entry. Returns:
//   - ErrEmptyRunID / ErrEmptyActor for empty inputs
//   - ErrRunNotFound when the run does not exist
//   - ErrNotPausable when the run state is not "running" or "pending"
//
// Audit entry: Action="pause", Actor=actor, Target=runID, Result="success".
func (m *PauseManager) PauseRun(ctx context.Context, runID, actor string) error {
	if runID == "" {
		return ErrEmptyRunID
	}
	if actor == "" {
		return ErrEmptyActor
	}
	return m.transitionOne(ctx, ActionPause, runID, actor, pausableStates, StatusPaused, ErrNotPausable)
}

// ResumeRun transitions a single run from "paused" to "running" and
// records an audit entry. Returns:
//   - ErrEmptyRunID / ErrEmptyActor for empty inputs
//   - ErrRunNotFound when the run does not exist
//   - ErrNotResumable when the run state is not "paused"
//
// Audit entry: Action="resume", Actor=actor, Target=runID, Result="success".
func (m *PauseManager) ResumeRun(ctx context.Context, runID, actor string) error {
	if runID == "" {
		return ErrEmptyRunID
	}
	if actor == "" {
		return ErrEmptyActor
	}
	return m.transitionOne(ctx, ActionResume, runID, actor, resumableStates, StatusRunning, ErrNotResumable)
}

// transitionOne is the shared implementation for PauseRun / ResumeRun.
// allowed is the set of source states; target is the new status; stateErr
// is the sentinel returned when the current status is not in allowed.
func (m *PauseManager) transitionOne(ctx context.Context, action, runID, actor string,
	allowed map[string]struct{}, target string, stateErr error) error {

	run, err := m.store.GetRun(ctx, runID)
	if err != nil {
		return fmt.Errorf("pause: get run: %w", err)
	}
	if run == nil {
		return ErrRunNotFound
	}
	if _, ok := allowed[run.Status]; !ok {
		return fmt.Errorf("%w: status %q", stateErr, run.Status)
	}

	run.Status = target
	run.UpdatedAt = time.Now().UTC()
	if err := m.store.UpdateRun(ctx, run); err != nil {
		return fmt.Errorf("pause: update run: %w", err)
	}

	if err := m.writeAudit(ctx, action, actor, runID, ResultSuccess, runID); err != nil {
		// Audit write failure is observability-only: the state transition
		// has already been persisted, so we log and continue rather than
		// undoing the pause/resume.
		log.WarnCtx(ctx, "pause audit write failed",
			"action", action, "run_id", runID, "actor", actor, "err", err)
	}
	log.InfoCtx(ctx, "run transitioned",
		"action", action, "run_id", runID, "actor", actor)
	return nil
}

// PauseAll transitions every "running" or "pending" run to "paused". It
// requires the actor to hold the "pause:all" permission; otherwise
// ErrPermissionDenied is returned and no runs are touched.
//
// The returned PauseResult lists affected run IDs and any per-run errors.
// A per-run error does not abort the whole operation: the manager
// continues with the remaining runs so a single bad run does not block
// pausing the rest. A global summary audit entry (Target="*") is also
// written.
func (m *PauseManager) PauseAll(ctx context.Context, actor string,
	perm PermissionChecker) (*PauseResult, error) {

	if actor == "" {
		return nil, ErrEmptyActor
	}
	if perm == nil || !perm.HasPermission(actor, PermissionPauseAll) {
		return nil, ErrPermissionDenied
	}
	return m.transitionAll(ctx, ActionPauseAll, actor, StatusPaused,
		[]string{StatusRunning, StatusPending})
}

// ResumeAll transitions every "paused" run to "running". It requires the
// actor to hold the "resume:all" permission; otherwise ErrPermissionDenied
// is returned and no runs are touched. See PauseAll for result semantics.
func (m *PauseManager) ResumeAll(ctx context.Context, actor string,
	perm PermissionChecker) (*PauseResult, error) {

	if actor == "" {
		return nil, ErrEmptyActor
	}
	if perm == nil || !perm.HasPermission(actor, PermissionResumeAll) {
		return nil, ErrPermissionDenied
	}
	return m.transitionAll(ctx, ActionResumeAll, actor, StatusRunning,
		[]string{StatusPaused})
}

// transitionAll is the shared implementation for PauseAll / ResumeAll.
// It queries runs by each source status, transitions them, and records
// per-run audit entries plus a global summary audit entry.
func (m *PauseManager) transitionAll(ctx context.Context, action, actor, target string,
	sourceStatuses []string) (*PauseResult, error) {

	result := &PauseResult{
		Action: action,
		Failed: make(map[string]error),
	}

	// Collect candidate runs across all source statuses. A run has exactly
	// one status, so no run appears twice even though we issue one query
	// per source status.
	seen := make(map[string]struct{})
	var runs []*state.Run
	for _, st := range sourceStatuses {
		list, err := m.store.ListRuns(ctx, state.RunFilter{Status: st})
		if err != nil {
			return nil, fmt.Errorf("pause: list runs with status %q: %w", st, err)
		}
		for _, r := range list {
			if _, dup := seen[r.ID]; dup {
				continue
			}
			seen[r.ID] = struct{}{}
			runs = append(runs, r)
		}
	}

	for _, run := range runs {
		run.Status = target
		run.UpdatedAt = time.Now().UTC()
		if err := m.store.UpdateRun(ctx, run); err != nil {
			result.Failed[run.ID] = fmt.Errorf("update run: %w", err)
			if auditErr := m.writeAudit(ctx, action, actor, run.ID, ResultFailed, run.ID); auditErr != nil {
				log.WarnCtx(ctx, "pause audit write failed",
					"action", action, "run_id", run.ID, "actor", actor, "err", auditErr)
			}
			continue
		}
		result.Affected = append(result.Affected, run.ID)
		if err := m.writeAudit(ctx, action, actor, run.ID, ResultSuccess, run.ID); err != nil {
			log.WarnCtx(ctx, "pause audit write failed",
				"action", action, "run_id", run.ID, "actor", actor, "err", err)
		}
	}

	// Global summary audit entry with Target="*".
	summary := ResultSuccess
	if len(result.Failed) > 0 {
		summary = ResultFailed
	}
	if err := m.writeAudit(ctx, action, actor, TargetAll, summary, ""); err != nil {
		log.WarnCtx(ctx, "pause global audit write failed",
			"action", action, "actor", actor, "err", err)
	}

	log.InfoCtx(ctx, "bulk run transition",
		"action", action, "actor", actor,
		"affected", len(result.Affected), "failed", len(result.Failed))
	return result, nil
}

// writeAudit writes a single audit entry. runIDForAudit is the RunID
// field of the audit record (the associated run ID for per-run entries,
// empty for the global summary entry whose Target is TargetAll).
func (m *PauseManager) writeAudit(ctx context.Context, action, actor, target, result,
	runIDForAudit string) error {

	audit := &state.Audit{
		ID:        newID(),
		RunID:     runIDForAudit,
		Action:    action,
		Actor:     actor,
		Target:    target,
		Result:    result,
		Timestamp: time.Now().UTC(),
	}
	return m.store.CreateAudit(ctx, audit)
}

// newID generates a unique audit identifier using crypto/rand. The ID has
// the form "pause-<16-hex-chars>". On the extremely unlikely event that
// rand.Read fails, it falls back to a timestamp-based ID so the caller
// always gets a usable, unique-enough identifier.
func newID() string {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("pause-%d", time.Now().UnixNano())
	}
	return "pause-" + hex.EncodeToString(b)
}
