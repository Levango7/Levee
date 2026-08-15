package executor

// Package executor implements LEVEE's module execution framework (design doc
// section 4.3, MVP task T016). It defines two abstractions:
//
//   - Module: a self-registering unit of work (shell, file, pkg, svc, user,
//     ...). Each Module declares the actions it supports (e.g. "exec",
//     "copy", "install") and whether it is idempotent.
//   - Executor: a registry + dispatcher that looks up a Module by name and
//     invokes one of its actions with a structured ModuleInput.
//
// The framework deliberately stays transport-agnostic: a Module receives a
// channel.Channel through ModuleInput and drives the target through it. This
// keeps the executor free of any SSH / WinRM / API specifics and lets the same
// Module run unchanged across transports.
//
// All output is structured (ModuleOutput) so that the batch controller, audit
// log and trace chain can record exit_code / stdout / stderr / duration /
// changed uniformly regardless of which Module produced them.

import (
	"fmt"
	"sort"
	"sync"
)

// --- Irreversible operation marking (T034) ----------------------------------
//
// IrreversibleChecker decides whether a workflow step performs an action that
// cannot be undone. The decision drives two safety mechanisms downstream:
//
//   - Approval escalation: irreversible steps must be approved at the "high"
//     level (design doc 4.4.6, LEVEELang spec LE083). The checker surfaces a
//     SuggestLevel so the planner can raise the approval tier without parsing
//     the reason itself.
//   - Rollback gating: steps flagged irreversible are excluded from automatic
//     rollback planning (design doc 4.5); a human must author an explicit
//     rollback block for them.
//
// The verdict is derived from two signals, in priority order:
//
//  1. Explicit declaration: step.Irreversible == true. This is the strongest
//     signal — the workflow author asserts that the action is destructive.
//  2. Whitelist match: the module.action pair (e.g. "pkg.remove",
//     "file.delete", "user.remove") is registered in the checker's
//     irreversible whitelist. This catches actions that are irreversible by
//     nature regardless of how the author marked them.
//
// Everything else is treated as reversible. The checker never returns an
// error: it always produces a definitive IrreversibleResult so that callers
// can branch on a single field without a second error-handling path.

// Step is the subset of a workflow step that the IrreversibleChecker needs to
// decide reversibility. It is a local struct rather than an alias for
// dsl.Step so that the executor package stays free of dsl/plan dependencies
// and can be unit-tested in isolation. Callers convert from dsl.Step or
// plan.PlanStep at the boundary.
type Step struct {
	// Module is the module name, e.g. "pkg", "file", "user".
	Module string

	// Action is the action verb, e.g. "remove", "delete".
	Action string

	// Irreversible is the explicit author declaration. When true the step
	// is treated as irreversible regardless of the whitelist.
	Irreversible bool
}

// IrreversibleResult is the outcome of IrreversibleChecker.Check. It is always
// populated; callers never need to inspect a separate error.
type IrreversibleResult struct {
	// Irreversible reports whether the step performs an action that cannot
	// be undone.
	Irreversible bool

	// Reason is a human-readable explanation of the verdict, suitable for
	// inclusion in the audit log and CLI output. It identifies which
	// signal triggered the verdict (explicit / whitelist / reversible).
	Reason string

	// SuggestLevel is the approval level the planner should raise to when
	// Irreversible is true. It is the empty string for reversible steps
	// (no escalation needed) and "high" for irreversible ones, matching
	// the three-tier approval vocabulary (standard / high / emergency).
	SuggestLevel string
}

// IrreversibleChecker decides whether a step performs an irreversible action.
// It is safe for concurrent use: the whitelist is guarded by an RWMutex so
// that RegisterWhitelist and Check can run in parallel from multiple
// goroutines (the planner validates steps in parallel).
type IrreversibleChecker struct {
	mu        sync.RWMutex
	whitelist map[string]bool // "module.action" -> true
}

// NewIrreversibleChecker returns a checker with an empty whitelist. Callers
// populate it via RegisterWhitelist; the common destructive actions
// (pkg.remove, file.delete, user.remove) are registered by the engine
// bootstrap, but tests and embedded users may start empty.
func NewIrreversibleChecker() *IrreversibleChecker {
	return &IrreversibleChecker{whitelist: make(map[string]bool)}
}

// RegisterWhitelist adds the module.action pair to the irreversible
// whitelist. Re-registering the same pair is a no-op. RegisterWhitelist is
// safe for concurrent use with Check and itself.
func (c *IrreversibleChecker) RegisterWhitelist(module, action string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.whitelist[module+"."+action] = true
}

// UnregisterWhitelist removes a pair from the whitelist. It is primarily
// useful in tests; production code treats the whitelist as append-only.
func (c *IrreversibleChecker) UnregisterWhitelist(module, action string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.whitelist, module+"."+action)
}

// Whitelist returns the registered irreversible actions in sorted order. The
// returned slice is a copy and may be safely modified by the caller. It is
// intended for diagnostics (e.g. `levee plan --show-irreversible`).
func (c *IrreversibleChecker) Whitelist() []string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	out := make([]string, 0, len(c.whitelist))
	for k := range c.whitelist {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// Check decides whether step performs an irreversible action. The decision
// follows the priority order documented on IrreversibleChecker:
//
//  1. step.Irreversible == true  -> irreversible (explicit declaration)
//  2. module.action in whitelist -> irreversible (whitelist match)
//  3. otherwise                  -> reversible
//
// When the verdict is irreversible, SuggestLevel is set to "high" so the
// planner can raise the approval tier without re-deriving the reason.
func (c *IrreversibleChecker) Check(step Step) IrreversibleResult {
	// Signal 1: explicit author declaration. This takes priority so that a
	// workflow author can mark a custom module action as irreversible even
	// when the engine whitelist does not know about it.
	if step.Irreversible {
		return IrreversibleResult{
			Irreversible: true,
			Reason:       fmt.Sprintf("step %s.%s explicitly marked as irreversible", step.Module, step.Action),
			SuggestLevel: ApprovalLevelHigh,
		}
	}

	// Signal 2: whitelist match. The whitelist captures actions that are
	// irreversible by nature (pkg.remove, file.delete, user.remove, ...)
	// so that authors do not have to repeat irreversible: true on every
	// such step.
	key := step.Module + "." + step.Action
	c.mu.RLock()
	matched := c.whitelist[key]
	c.mu.RUnlock()
	if matched {
		return IrreversibleResult{
			Irreversible: true,
			Reason:       fmt.Sprintf("%s is in the irreversible whitelist", key),
			SuggestLevel: ApprovalLevelHigh,
		}
	}

	// Default: reversible. SuggestLevel is empty because no escalation is
	// needed.
	return IrreversibleResult{
		Irreversible: false,
		Reason:       fmt.Sprintf("step %s.%s is reversible (not marked, not in whitelist)", step.Module, step.Action),
		SuggestLevel: "",
	}
}

// ApprovalLevelHigh is the approval tier the checker suggests for
// irreversible operations. It is exported as a constant rather than a magic
// string so that callers can compare against a stable identifier. The value
// matches the three-tier approval vocabulary (standard / high / emergency)
// defined in dsl.ApprovalSpec and the LEVEELang spec.
const ApprovalLevelHigh = "high"
