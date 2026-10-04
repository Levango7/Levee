// calendar_gate.go wires the organisation change calendar into the two places a
// change can still be refused cheaply: before a plan artifact exists, and again
// immediately before the first mutation.
//
// The gate is the reason `internal/calendar` exists at all. Its verdict
// functions (IsFrozen / AssertNotFrozen / CheckWindowForPlan) were complete,
// tested and unreachable: `CalendarService` was constructed only by
// `cmd/levee/cmd_calendar.go`, so `levee calendar freeze` wrote rows that no
// execution path ever read. That gap is what these functions close — and it is
// why the checks sit in wiring rather than in the RPC layer: gRPC PlanChange,
// `levee plan --local` and the re-plan inside apply all funnel through here, so
// an entry point cannot plan around them.
//
// Deliberate shapes:
//
//   - Rollback is exempt, as it is for the change window. A change that has
//     already modified targets must always be recoverable, and a freeze raised
//     during an incident is precisely the moment a rollback is needed. Both
//     gates therefore live in the forward paths (GeneratePlan and
//     engine.ClosureRunner.Run), and TestCalendarGateIsForwardOnly pins that no
//     rollback path reaches them.
//   - The emergency override comes from the workflow's own declared
//     `approval.level: emergency`, never from the tier the risk scorer derived.
//     A derived tier would let a high risk score unlock the override by itself,
//     which inverts the point of a freeze.
//   - A deployment with no calendar installed is reported on every plan rather
//     than assumed: enforcement silently absent is how a governance promise
//     becomes decoration.
package wiring

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/nexus/levee/internal/dsl"
	"github.com/nexus/levee/internal/log"
	"github.com/nexus/levee/internal/state"
)

// targetVocabulary renders everything a calendar window's `--targets` entry may
// name for this set of hosts: the hostname, its inventory group, each label as
// `key=value` and as the bare `value`.
//
// Four forms because the documented vocabulary is not one form: `levee calendar
// create --targets prod` and `--targets env=prod` both appear in docs and both
// have to bite, and a change aimed at `web-01` should be stoppable by name.
// Bare label *keys* are deliberately excluded — every host has an `env`, so a
// window entry of `env` would freeze the fleet, which is the opposite of what
// an entry that specific-looking looks like.
//
// De-duplicated and sorted, so a refusal message is stable across runs over the
// same host set.
func targetVocabulary(targets []*state.Target) []string {
	seen := make(map[string]bool)
	var out []string
	add := func(s string) {
		if s == "" || seen[s] {
			return
		}
		seen[s] = true
		out = append(out, s)
	}
	for _, t := range targets {
		if t == nil {
			continue
		}
		add(t.Hostname)
		add(t.GroupID)
		for k, v := range t.Labels {
			add(k + "=" + v)
			add(v)
		}
	}
	sort.Strings(out)
	return out
}

// declaresEmergency reports whether the workflow itself claims emergency
// authority. Only the author's declaration counts — see the package comment.
func declaresEmergency(wf *dsl.Workflow) bool {
	return wf != nil && wf.Approval != nil && wf.Approval.Level == dsl.ApprovalLevelEmergency
}

// assertCalendarAllows is the plan-time half of the gate.
func (e *Engine) assertCalendarAllows(ctx context.Context, changeID string, targets []*state.Target, wf *dsl.Workflow) error {
	if e.calendar == nil {
		log.Warn("change calendar is not wired: freeze periods and window conflicts are NOT enforced for this plan",
			"change_id", changeID)
		return nil
	}
	labels := targetVocabulary(targets)
	if err := e.calendar.AssertNotFrozen(ctx, labels, declaresEmergency(wf)); err != nil {
		return err
	}
	// Conflicts stay advisory, which is what calendar.CheckWindowForPlan
	// documents: two change windows covering the same targets is information an
	// operator wants before approving, not a refusal — only a freeze refuses.
	// Duration zero means "a change starting now", the API's instantaneous case.
	//
	// Its error is logged, not returned: the only way this call fails is a store
	// that cannot answer, and the freeze check above reads the same store and has
	// already refused the plan if so. Returning here would be a guard no reachable
	// state can trigger, which reads as protection while testing nothing.
	conflicts, err := e.calendar.CheckWindowForPlan(ctx, labels, time.Now().UTC(), 0)
	if err != nil {
		log.Warn("calendar conflict advisory unavailable", "change_id", changeID, "error", err)
		return nil
	}
	if len(conflicts) > 0 {
		parts := make([]string, 0, len(conflicts))
		for _, c := range conflicts {
			parts = append(parts, fmt.Sprintf("%s overlaps %s on [%s, %s) for %s",
				c.WindowAID, c.WindowBID,
				c.OverlapStart.UTC().Format(time.RFC3339), c.OverlapEnd.UTC().Format(time.RFC3339),
				strings.Join(c.SharedTargets, "+")))
		}
		log.Warn("planned change overlaps active calendar windows",
			"workflow", workflowNameForLog(wf), "targets", strings.Join(labels, ","),
			"conflicts", strings.Join(parts, "; "))
	}
	return nil
}

// assertCalendarAllowsExecution is the apply-time re-check, installed as the
// engine host guard so a freeze created between approval and execution still
// refuses before anything is touched. It shares the plan-time verdict function
// and the emergency decision, so a change that was approved under a freeze it
// could see is refused under one it could not.
func (e *Engine) assertCalendarAllowsExecution(ctx context.Context, rx *runExec, hosts []string, emergency bool) error {
	if e.calendar == nil {
		return nil
	}
	targets := make([]*state.Target, 0, len(hosts))
	if rx != nil {
		for _, h := range hosts {
			if t, ok := rx.targets[h]; ok {
				targets = append(targets, t)
			}
		}
	} else {
		// No inventory snapshot for this runner: read the inventory the same way
		// validateTargets does rather than skipping the labels, because an
		// unlabelled lookup would match every freeze window's "no labels"
		// branch and refuse unrelated changes.
		all, err := e.store.ListTargets(ctx, state.TargetFilter{})
		if err != nil {
			return fmt.Errorf("wiring: calendar re-check needs the inventory: %w", err)
		}
		want := make(map[string]bool, len(hosts))
		for _, h := range hosts {
			want[h] = true
		}
		for _, t := range all {
			if want[t.Hostname] {
				targets = append(targets, t)
			}
		}
	}
	return e.calendar.AssertNotFrozen(ctx, targetVocabulary(targets), emergency)
}

// declaresEmergencyFor resolves the stored workflow behind a change and reports
// whether its author declared emergency approval. Any failure on the way
// (change gone, source unparsable) answers false: an override nobody can show a
// declaration for is no override.
func (e *Engine) declaresEmergencyFor(ctx context.Context, changeID string) bool {
	run, err := e.store.GetRun(ctx, changeID)
	if err != nil || run == nil {
		return false
	}
	wf, err := resolveWorkflow(run)
	if err != nil {
		return false
	}
	return declaresEmergency(wf)
}

// workflowNameForLog renders the workflow name for a log line. The run id is
// what an operator greps for, but the conflict warning is about the document,
// and an unnamed document must not log an empty field.
func workflowNameForLog(wf *dsl.Workflow) string {
	if wf == nil || wf.Meta.Name == "" {
		return "<unnamed>"
	}
	return wf.Meta.Name
}
