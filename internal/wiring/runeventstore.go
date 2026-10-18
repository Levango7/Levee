// runeventstore.go is the single funnel for run lifecycle events. Run status
// transitions were scattered across six write sites in four packages (apply
// funnel, retry, takeover interrupt, pause/resume, CLI paths) — the reason
// four chatops event types sat with zero producers: every new flow had to
// remember to broadcast, and none did. Rather than auditing call sites
// forever, the funnel sits in the store seam: every successful run-status
// write, whatever flow wrote it, crosses exactly one decorator.
//
// A store decorator (not an engine option) because the writes happen below
// the engine — the same reason TenantStore wraps at this seam. The decorator
// is transparent everywhere else: methods it does not intercept forward
// untouched, so wrapping it around the serve store changes no behaviour
// except the events.

package wiring

import (
	"context"
	"sync"
	"time"

	"github.com/nexus/levee/internal/log"
	"github.com/nexus/levee/internal/state"
)

// RunEventSink receives one event per successful run-status transition.
// from/to are the run statuses as the store recorded them (runstatus
// vocabulary); at is the timestamp the write stamped. The sink is
// synchronous and trusted in-process: it runs before the write's caller
// proceeds, on the same goroutine, and must not block on external I/O —
// slow sinks belong behind their own queue.
type RunEventSink func(changeID, from, to string, at time.Time)

// RunEventStore wraps a state.Store and reports every successful run-status
// transition to the sink. The sink is installed via SetSink (serve sets it
// once the ChatOps bridge exists — the store is built first, and a nil sink
// simply drops events, which keeps the decorator inert in tests and in
// deployments without any event consumer).
type RunEventStore struct {
	state.Store

	mu   sync.RWMutex
	sink RunEventSink
}

// NewRunEventStore wraps base. The initial sink is nil: nothing is emitted
// until SetSink installs one.
func NewRunEventStore(base state.Store) *RunEventStore {
	return &RunEventStore{Store: base}
}

// Underlying exposes the wrapped store to the state.Underlying walk (the
// Unwrapper seam, same as TenantStore). Everything that needs the raw
// backend — driver dialect detection, the agent registry's deliberately
// un-tenanted path — must see through this decorator: before this method
// existed the walk stopped here, the agent registry judged the wrapped
// store "cannot persist agents" and served an Unimplemented stub.
func (s *RunEventStore) Underlying() state.Store { return s.Store }

// SetSink installs (or replaces) the transition sink. Safe to call at any
// time; serve sets it during startup, before the listeners open, so no
// transition can race the installation.
func (s *RunEventStore) SetSink(fn RunEventSink) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sink = fn
}

// emit delivers one transition. The sink must not be able to fail the write
// that produced the event — the same posture as the engine's GateObserver,
// whose notification path can never become a second failure reason for a
// run. A panicking sink is contained here and logged; the store's own
// result is returned untouched by the caller.
func (s *RunEventStore) emit(changeID, from, to string, at time.Time) {
	s.mu.RLock()
	sink := s.sink
	s.mu.RUnlock()
	if sink == nil || changeID == "" || from == to {
		return
	}
	defer func() {
		if r := recover(); r != nil {
			log.Warn("run event sink panicked (recovered)",
				"change_id", changeID, "to", to, "panic", r)
		}
	}()
	sink(changeID, from, to, at)
}

// UpdateRunStatusIf intercepts the CAS transition writes. Only an APPLIED
// transition emits: a (false, nil) result means the world already moved on
// and there is no transition to report.
func (s *RunEventStore) UpdateRunStatusIf(ctx context.Context, id string, from string, to string, updatedAt time.Time) (bool, error) {
	ok, err := s.Store.UpdateRunStatusIf(ctx, id, from, to, updatedAt)
	if ok {
		s.emit(id, from, to, updatedAt)
	}
	return ok, err
}

// UpdateRunApprovalStatusIf intercepts the approval-settlement CAS (the
// atomic status + approval_status write behind every approve/reject). The
// run-status half of the pair is the transition the sink cares about.
func (s *RunEventStore) UpdateRunApprovalStatusIf(ctx context.Context, id string, from string, to string, approvalStatus string, updatedAt time.Time) (bool, error) {
	ok, err := s.Store.UpdateRunApprovalStatusIf(ctx, id, from, to, approvalStatus, updatedAt)
	if ok {
		s.emit(id, from, to, updatedAt)
	}
	return ok, err
}

// UpdateRun intercepts full-row writes, which carry a status change on some
// paths (pause/resume, archive) and only metadata on others (replan persists
// the plan hash; kickoff stamps the approval level). The previous row is read
// so only real status changes emit — one extra read per full-row write, a
// path that runs at operator frequency, never per-step.
func (s *RunEventStore) UpdateRun(ctx context.Context, run *state.Run) error {
	prev, _ := s.GetRun(ctx, run.ID)   // not overridden: the promoted method is the base's
	err := s.Store.UpdateRun(ctx, run) // the qualifier MUST stay: UpdateRun is shadowed by this method
	if err == nil && prev != nil && prev.Status != run.Status && run.Status != "" {
		at := run.UpdatedAt
		if at.IsZero() {
			at = time.Now().UTC()
		}
		s.emit(run.ID, prev.Status, run.Status, at)
	}
	return err
}
