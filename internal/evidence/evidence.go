// Package evidence answers one question about an executed run: how much of what
// the platform claims actually happened is written down.
//
// The gap it closes: execution evidence (batches, steps) is persisted in ONE
// write after the batch closure returns (internal/wiring/persist.go). Three
// routes leave a run whose status says something happened while the evidence
// table says nothing did:
//
//   - the executing node dies mid-run: the closure never returns, nothing is
//     written, and the cluster takeover settles the run to `interrupted`;
//   - the executing node loses its lease just before persisting: the evidence
//     it holds is discarded ON PURPOSE (a superseded executor must not write
//     into a run the cluster has written off — see wiring.executePlan's fence);
//   - persistence itself fails after the steps ran: the run settles `failed`
//     while only some (or none) of the step rows landed.
//
// In all three the honest answer to "did this change touch my hosts" is "we do
// not know", and the platform used to render that as an empty step list — which
// reads exactly like "nothing ran". That is the failure mode this package
// exists to name, because the two readings lead to opposite operator actions:
// one is a safe retry, the other is a retry over an unknown half-applied state.
//
// The package is a pure function over facts the caller already has. It
// deliberately does not read the store: the same verdict is needed by the CLI
// (`levee show`), by the retry path, and by any future reader (the web UI, an
// RPC field), and a second implementation on the second surface is how the two
// start disagreeing about whether a run is safe to retry.
package evidence

import (
	"context"
	"fmt"

	"github.com/nexus/levee/internal/runstatus"
	"github.com/nexus/levee/internal/state"
)

// Kind is the evidence verdict. The set is closed: every fact combination maps
// onto exactly one of these, and each one names a DIFFERENT operator action.
type Kind string

const (
	// NotStarted — nothing was dispatched, so there is nothing to have evidence
	// about. Safe to start.
	NotStarted Kind = "not_started"
	// InFlight — an executor is holding this run right now. Step evidence is
	// written when the batch closure returns, so an empty table is the design
	// rather than a gap.
	InFlight Kind = "in_flight"
	// Complete — every batch the run executed has an evidence row, and no gap
	// was recorded.
	Complete Kind = "complete"
	// Partial — some evidence exists and some is known to be missing (the
	// executing attempt's rows were never written). A retry resumes on top of a
	// state that is only partly documented.
	Partial Kind = "partial"
	// Missing — the run was dispatched and left NO evidence: it may have
	// changed hosts. This is the verdict that must never render as "no steps".
	Missing Kind = "missing"
	// Unrecorded — a terminal run with no evidence rows and no dispatch record.
	// Either it predates per-batch persistence or it never executed; this
	// platform cannot tell which, and says so instead of guessing.
	Unrecorded Kind = "unrecorded"
)

// Facts is everything the verdict depends on. The caller collects them from the
// run row, its traces, its batch/step rows and its dispatch assignment.
type Facts struct {
	// Status is the run's status (internal/runstatus vocabulary).
	Status string
	// Events are the trace events recorded for the run, in any order.
	Events []string
	// Batches and Steps are the counts of evidence rows for the run.
	Batches int
	Steps   int
	// Assignment reports whether a dispatch assignment row exists for the run.
	// It is the only durable record that the cluster ever handed this run to a
	// worker, which is what separates "cancelled before dispatch" from
	// "cancelled while executing".
	Assignment bool
}

// Verdict is the classification plus the sentence an operator needs.
type Verdict struct {
	Kind   Kind
	Reason string
}

// hasEvent reports whether the run's traces name event.
func (f Facts) hasEvent(event string) bool {
	for _, e := range f.Events {
		if e == event {
			return true
		}
	}
	return false
}

// Classify turns the facts into one verdict.
//
// The order is deliberate: recorded gaps win over status, because a gap event is
// a fact written at the moment the loss was known, while a status is an
// inference from it. An interrupted run is never Complete even when rows exist —
// those rows belong to an earlier attempt, and the attempt that was killed left
// nothing.
//
// "Was it dispatched" is answered by the assignment row and nothing else. It is
// the only durable record that the cluster handed this run to a worker; a
// cancelled run in single-node mode has none, and calling that "dispatched and
// empty" would be a false alarm on every cancelled draft.
func Classify(f Facts) Verdict {
	hasRows := f.Batches > 0 || f.Steps > 0
	switch {
	case f.hasEvent(state.EventEvidenceDiscarded):
		return Verdict{Missing,
			"the executing node lost its lease and discarded the evidence it had already recorded; " +
				"the run may have changed hosts"}
	case f.hasEvent(state.EventEvidenceIncomplete):
		return Verdict{Partial,
			"execution finished but part of its step evidence could not be written; " +
				"the run may have changed hosts it cannot show"}
	case f.Status == runstatus.StatusInterrupted:
		if hasRows {
			return Verdict{Partial,
				"the node executing the latest attempt died; earlier attempts' evidence exists, " +
					"the interrupted attempt's does not"}
		}
		return Verdict{Missing,
			"the executing node died mid-run and wrote no step evidence; " +
				"the run may have changed hosts"}
	case hasRows:
		return Verdict{Complete, "every step this run executed has an evidence row"}
	case f.Status == runstatus.StatusDraft || f.Status == runstatus.StatusPlanned ||
		f.Status == runstatus.StatusPending:
		return Verdict{NotStarted, "nothing was dispatched"}
	case f.Status == runstatus.StatusApproved && !f.Assignment:
		return Verdict{NotStarted, "nothing was dispatched: the run is waiting for a worker to claim it"}
	case f.Status == runstatus.StatusRunning || f.Status == runstatus.StatusPaused ||
		(f.Status == runstatus.StatusApproved && f.Assignment):
		return Verdict{InFlight,
			"the run is still held by an executor (running or paused); " +
				"step evidence is written when the batch closure returns"}
	case f.Assignment:
		return Verdict{Missing,
			"the run was dispatched and left no step evidence; the run may have changed hosts"}
	}
	return Verdict{Unrecorded,
		"no step evidence exists for this run and it carries no dispatch record; " +
			"it either predates per-batch persistence or never executed"}
}

// UnsafeToBlindRetry reports whether retrying this run would re-apply steps over
// a state the platform cannot describe. Every verdict except NotStarted and
// Complete is such a state; InFlight is included because the owner may still be
// writing.
func (k Kind) UnsafeToBlindRetry() bool {
	switch k {
	case NotStarted, Complete:
		return false
	}
	return true
}

// ForRun loads one run's facts from the store and classifies them.
//
// This exists so the CLI and the executor cannot answer "is this run documented"
// differently: `levee show` renders this verdict, and the retry path records it
// before re-running. Two collectors would eventually disagree about which rows
// count — and the disagreement would be discovered by re-running a change over
// hosts nobody could describe.
//
// Step rows are fetched with Limit 1 because Classify only asks whether any
// exist; a caller that already holds the full list should build Facts directly.
func ForRun(ctx context.Context, store state.Store, runID, status string) (Verdict, error) {
	traces, err := store.ListTraces(ctx, state.TraceFilter{RunID: runID})
	if err != nil {
		return Verdict{}, fmt.Errorf("evidence: list traces for %q: %w", runID, err)
	}
	events := make([]string, 0, len(traces))
	for _, tr := range traces {
		events = append(events, tr.Event)
	}
	batches, err := store.ListBatches(ctx, state.BatchFilter{RunID: runID})
	if err != nil {
		return Verdict{}, fmt.Errorf("evidence: list batches for %q: %w", runID, err)
	}
	steps, err := store.ListSteps(ctx, state.StepFilter{RunID: runID, Limit: 1})
	if err != nil {
		return Verdict{}, fmt.Errorf("evidence: list steps for %q: %w", runID, err)
	}
	assignment, err := store.GetAssignment(ctx, runID)
	if err != nil {
		return Verdict{}, fmt.Errorf("evidence: get assignment for %q: %w", runID, err)
	}
	return Classify(Facts{
		Status:     status,
		Events:     events,
		Batches:    len(batches),
		Steps:      len(steps),
		Assignment: assignment != nil,
	}), nil
}
