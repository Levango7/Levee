package rollback

import "errors"

// ErrIncomplete marks a rollback that RAN and did not fully restore the targets:
// compensations were dispatched and at least one could not be completed, or the
// run baseline could not be restored.
//
// This exists as a distinct error class because the outcome is something an
// operator acts on (retry the rollback, or fix the target and retry), whereas a
// server fault is something they report. Callers — the gRPC layer in particular
// — must not flatten it into codes.Internal: doing so hides the fact that the
// compensations already happened and leaves the run record claiming a state that
// is no longer true.
//
// Producers wrap it together with the concrete cause (fmt.Errorf("%w: %w", …)) so
// errors.Is keeps working while the message still names the failed undo step.
var ErrIncomplete = errors.New("rollback incomplete")
