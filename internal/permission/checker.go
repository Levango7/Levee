// Package permission implements the team × environment permission matrix
// for LEVEE. This file defines PermissionChecker, a higher-level wrapper
// around PermissionMatrix that adds operation context, structured denial
// errors, and batch validation for plan/apply/approve/rollback and other
// actions.
//
// PermissionChecker is intended to be called by the orchestrator before
// any state-changing operation. A denied check returns a
// PermissionDeniedError that carries enough information for audit
// logging and user-facing messages.
package permission

import (
	"context"
	"errors"
	"fmt"

	"github.com/nexus/levee/internal/audit"
)

// EventPermissionDenied is the audit event recorded when a permission
// check is denied. It is recorded by PermissionChecker.Check when a
// TraceRecorder has been injected via WithRecorder.
const EventPermissionDenied = "permission.denied"

// permissionCheckRunID is the placeholder run id used when recording an
// audit trace for a denial that is not associated with a specific run
// (i.e. OperationContext.RunID is empty). The trace is best-effort: if
// the underlying store enforces a foreign-key constraint on trace.run_id
// and no matching run row exists, the recording is silently skipped so
// that the denial error is still returned to the caller.
const permissionCheckRunID = "permission-check"

// Sentinel errors returned by the permission checker. ErrEmptyTeam and
// ErrEmptyEnv are reused from matrix.go; the ones below are specific to
// the checker layer.
var (
	// ErrPermissionDenied is the base sentinel wrapped by every
	// PermissionDeniedError. Callers can use errors.Is(err,
	// ErrPermissionDenied) to detect any denial without inspecting the
	// concrete type.
	ErrPermissionDenied = errors.New("permission: denied")

	// ErrEmptyActor is returned when an operation context has an empty
	// actor field.
	ErrEmptyActor = errors.New("permission: empty actor")

	// ErrNilMatrix is returned when a PermissionChecker is constructed
	// with a nil PermissionMatrix.
	ErrNilMatrix = errors.New("permission: nil matrix")
)

// PermissionChecker wraps a PermissionMatrix and adds:
//   - Operation context (actor, team, environment, action, run id, target)
//   - Structured denial errors carrying audit-friendly details
//   - Batch validation (check many operations in one call)
//   - Convenience methods for the common LEVEE actions
//   - Optional audit trace recording on denial (via WithRecorder)
//
// A PermissionChecker is safe for concurrent use as long as the
// underlying PermissionMatrix is not mutated after construction.
type PermissionChecker struct {
	matrix   *PermissionMatrix
	recorder *audit.TraceRecorder
}

// OperationContext describes a single operation to be authorised. It is
// the input to Check and CheckBatch.
//
// Fields:
//   - Actor:  the user performing the operation (required, non-empty)
//   - Team:   the team the actor belongs to (required, non-empty)
//   - Env:    the target environment (required, non-empty)
//   - Action: the operation type, one of the ActionXxx constants
//     (required, non-empty)
//   - RunID:  the associated run id, used for audit logging only;
//     may be empty
//   - Target: the target machine, used for fine-grained audit logging
//     only; may be empty
type OperationContext struct {
	Actor  string
	Team   string
	Env    string
	Action string
	RunID  string
	Target string
}

// PermissionDeniedError is the structured error returned when a
// permission check fails. It carries enough information to produce a
// user-facing message and an audit-log entry.
//
// Use errors.As to extract it from a returned error, and errors.Is with
// ErrPermissionDenied to detect any denial.
type PermissionDeniedError struct {
	Team   string
	Env    string
	Action string
	Actor  string
	Reason string
}

// Error implements the error interface. The format is stable and
// suitable for audit logging.
func (e *PermissionDeniedError) Error() string {
	return fmt.Sprintf("permission denied: actor %q (team %q) cannot %q on env %q: %s",
		e.Actor, e.Team, e.Action, e.Env, e.Reason)
}

// Unwrap allows errors.Is(err, ErrPermissionDenied) to return true for
// any PermissionDeniedError.
func (e *PermissionDeniedError) Unwrap() error {
	return ErrPermissionDenied
}

// NewPermissionChecker creates a PermissionChecker backed by the given
// matrix. The matrix must be non-nil; otherwise ErrNilMatrix is
// returned.
//
// The returned checker shares the matrix with the caller. Mutating the
// matrix after construction is not safe for concurrent checks.
func NewPermissionChecker(matrix *PermissionMatrix) (*PermissionChecker, error) {
	if matrix == nil {
		return nil, ErrNilMatrix
	}
	return &PermissionChecker{matrix: matrix}, nil
}

// NewPermissionCheckerOrPanic is a convenience constructor for use in
// tests and program initialisation where a nil matrix is a programmer
// error. It panics if matrix is nil.
func NewPermissionCheckerOrPanic(matrix *PermissionMatrix) *PermissionChecker {
	c, err := NewPermissionChecker(matrix)
	if err != nil {
		panic(err)
	}
	return c
}

// WithRecorder injects an audit.TraceRecorder into the checker. When a
// recorder is present, Check records an audit trace (event
// "permission.denied") every time a permission check is denied. The
// recording is best-effort: failures are silently ignored so that the
// denial error is always returned to the caller.
//
// Passing a nil recorder clears any previously injected recorder and
// restores the default no-audit behaviour.
//
// The receiver is returned to support builder-style chaining:
//
//	c, _ := permission.NewPermissionChecker(m)
//	c = c.WithRecorder(rec)
func (c *PermissionChecker) WithRecorder(recorder *audit.TraceRecorder) *PermissionChecker {
	if c == nil {
		return nil
	}
	c.recorder = recorder
	return c
}

// Check authorises a single operation. It returns nil if the operation
// is allowed, or a PermissionDeniedError (wrapping ErrPermissionDenied)
// if denied. Empty actor, team, env, or action yield the corresponding
// sentinel error.
//
// When a TraceRecorder has been injected via WithRecorder, Check also
// records an audit trace (event "permission.denied") on denial. The
// recording is best-effort: if it fails (e.g. because the store rejects
// the row), the failure is silently ignored and the denial error is
// still returned. When no recorder is injected the behaviour is
// unchanged (backward compatible).
//
// The context.Context is accepted for future tracing/cancellation hooks
// but is not currently used.
func (c *PermissionChecker) Check(ctx context.Context, op OperationContext) error {
	_ = ctx

	if c == nil || c.matrix == nil {
		return ErrNilMatrix
	}

	if op.Actor == "" {
		return fmt.Errorf("%w: action %q team %q env %q", ErrEmptyActor, op.Action, op.Team, op.Env)
	}
	if op.Team == "" {
		return fmt.Errorf("%w: actor %q action %q env %q", ErrEmptyTeam, op.Actor, op.Action, op.Env)
	}
	if op.Env == "" {
		return fmt.Errorf("%w: actor %q team %q action %q", ErrEmptyEnv, op.Actor, op.Team, op.Action)
	}
	if op.Action == "" {
		return fmt.Errorf("%w: actor %q team %q env %q", ErrEmptyAction, op.Actor, op.Team, op.Env)
	}

	if c.matrix.Allow(op.Team, op.Env, op.Action) {
		return nil
	}

	denied := &PermissionDeniedError{
		Team:   op.Team,
		Env:    op.Env,
		Action: op.Action,
		Actor:  op.Actor,
		Reason: "no matching grant in permission matrix",
	}

	c.recordDenial(ctx, op, denied)

	return denied
}

// recordDenial records an audit trace for a denied permission check. It
// is best-effort: any error from the recorder is silently ignored so
// that the denial error is always returned to the caller. When no
// recorder is injected this is a no-op.
//
// The trace carries the actor, team, env, action and target of the
// denied operation in both Output (structured, for querying) and
// Metadata (string map, for indexing). When OperationContext.RunID is
// empty the placeholder "permission-check" is used so that the record
// can satisfy the audit.TraceRecorder non-empty-run-id requirement;
// stores with a foreign-key constraint on trace.run_id will reject the
// row and the recording is silently skipped.
func (c *PermissionChecker) recordDenial(ctx context.Context, op OperationContext, denied *PermissionDeniedError) {
	if c == nil || c.recorder == nil {
		return
	}

	runID := op.RunID
	if runID == "" {
		runID = permissionCheckRunID
	}

	target := op.Target
	if target == "" {
		target = "*"
	}

	// Best-effort: ignore the returned trace and error. The denial
	// error has already been built and will be returned to the caller
	// regardless of whether the audit recording succeeds.
	_, _ = c.recorder.Record(ctx, audit.TraceRecord{
		RunID:  runID,
		Event:  EventPermissionDenied,
		Actor:  op.Actor,
		Target: target,
		Output: map[string]any{
			"team":     op.Team,
			"env":      op.Env,
			"action":   op.Action,
			"resource": target,
			"reason":   denied.Reason,
		},
		Error: denied,
		Metadata: map[string]string{
			"team":   op.Team,
			"env":    op.Env,
			"action": op.Action,
		},
	})
}

// CheckBatch authorises multiple operations in order. It returns nil if
// every operation is allowed. Otherwise it returns the error from the
// first failing operation; subsequent operations are not checked.
//
// A nil or empty batch returns nil.
func (c *PermissionChecker) CheckBatch(ctx context.Context, ops ...OperationContext) error {
	for _, op := range ops {
		if err := c.Check(ctx, op); err != nil {
			return err
		}
	}
	return nil
}

// CheckPlan is a convenience wrapper for authorising a plan action.
func (c *PermissionChecker) CheckPlan(ctx context.Context, actor, team, env, runID string) error {
	return c.Check(ctx, OperationContext{
		Actor:  actor,
		Team:   team,
		Env:    env,
		Action: ActionPlan,
		RunID:  runID,
	})
}

// CheckApply is a convenience wrapper for authorising an apply action.
func (c *PermissionChecker) CheckApply(ctx context.Context, actor, team, env, runID string) error {
	return c.Check(ctx, OperationContext{
		Actor:  actor,
		Team:   team,
		Env:    env,
		Action: ActionApply,
		RunID:  runID,
	})
}

// CheckApprove is a convenience wrapper for authorising an approve
// action.
func (c *PermissionChecker) CheckApprove(ctx context.Context, actor, team, env, runID string) error {
	return c.Check(ctx, OperationContext{
		Actor:  actor,
		Team:   team,
		Env:    env,
		Action: ActionApprove,
		RunID:  runID,
	})
}

// CheckRollback is a convenience wrapper for authorising a rollback
// action.
func (c *PermissionChecker) CheckRollback(ctx context.Context, actor, team, env, runID string) error {
	return c.Check(ctx, OperationContext{
		Actor:  actor,
		Team:   team,
		Env:    env,
		Action: ActionRollback,
		RunID:  runID,
	})
}

// CheckPause is a convenience wrapper for authorising a pause action on
// a single run.
func (c *PermissionChecker) CheckPause(ctx context.Context, actor, team, env, runID string) error {
	return c.Check(ctx, OperationContext{
		Actor:  actor,
		Team:   team,
		Env:    env,
		Action: ActionPause,
		RunID:  runID,
	})
}

// CheckResume is a convenience wrapper for authorising a resume action
// on a single run.
func (c *PermissionChecker) CheckResume(ctx context.Context, actor, team, env, runID string) error {
	return c.Check(ctx, OperationContext{
		Actor:  actor,
		Team:   team,
		Env:    env,
		Action: ActionResume,
		RunID:  runID,
	})
}

// CheckPauseAll is a convenience wrapper for authorising a pause_all
// action. There is no run id because the operation targets every run in
// the environment.
func (c *PermissionChecker) CheckPauseAll(ctx context.Context, actor, team, env string) error {
	return c.Check(ctx, OperationContext{
		Actor:  actor,
		Team:   team,
		Env:    env,
		Action: ActionPauseAll,
	})
}

// CheckResumeAll is a convenience wrapper for authorising a resume_all
// action. There is no run id because the operation targets every run in
// the environment.
func (c *PermissionChecker) CheckResumeAll(ctx context.Context, actor, team, env string) error {
	return c.Check(ctx, OperationContext{
		Actor:  actor,
		Team:   team,
		Env:    env,
		Action: ActionResumeAll,
	})
}
