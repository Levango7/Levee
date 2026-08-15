// Package plan transforms a parsed Workflow AST into an executable Plan
// structure. The Plan divides the resolved target hosts into batches
// according to the workflow's BatchConfig strategy (percent / fixed /
// serial) and orchestrates the workflow steps into each batch.
//
// The Generator is the entry point: given a parsed *dsl.Workflow and a
// prechecked list of reachable targets, it produces a *Plan ready for
// the apply phase. Empty target lists are rejected. Batch division is
// total: every resolved target appears in exactly one batch, no target
// is dropped and no target is duplicated.
package plan

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"time"

	"github.com/nexus/levee/internal/dsl"
	"github.com/nexus/levee/internal/errors"
)

// Plan is the executable plan structure produced by the Generator. It
// divides the resolved targets into ordered batches and assigns the
// workflow steps to each batch. The apply phase consumes a Plan to
// drive execution batch by batch.
type Plan struct {
	// ID is the plan unique identifier, generated at creation time.
	ID string

	// WorkflowName is the name of the source workflow.
	WorkflowName string

	// Batches is the ordered list of execution batches. Batches run
	// sequentially; targets within a batch run concurrently up to
	// MaxConcurrency.
	Batches []Batch

	// TotalTargets is the total number of resolved targets across all
	// batches.
	TotalTargets int

	// CreatedAt is the plan creation timestamp (UTC).
	CreatedAt time.Time
}

// Batch is a single execution batch. It contains a subset of the
// resolved targets and the steps to execute on each target in the
// batch. Targets within a batch run concurrently up to
// MaxConcurrency; batches themselves run sequentially.
type Batch struct {
	// Index is the 0-based batch ordinal.
	Index int

	// Targets is the list of target hosts in this batch.
	Targets []string

	// Steps is the ordered list of steps to execute on each target in
	// the batch. Every batch receives the same step sequence.
	Steps []PlanStep

	// MaxConcurrency caps the in-batch parallelism. Zero means
	// unlimited (the apply phase decides its own default).
	MaxConcurrency int
}

// PlanStep is a single step in a plan, derived from a dsl.Step. It
// carries the module/action to invoke, the action arguments and the
// optional rollback / approval / gate overrides. Step-level overrides
// take precedence over workflow-level defaults during execution.
type PlanStep struct {
	Name     string
	Module   string
	Action   string
	Args     map[string]any
	Rollback *dsl.RollbackSpec
	Approval *dsl.ApprovalSpec
	Gate     *dsl.GateSpec
}

// Generator transforms a parsed Workflow AST into an executable Plan.
// The zero value is not ready — use NewGenerator. A Generator is
// stateless and safe for concurrent use.
type Generator struct{}

// NewGenerator returns a ready-to-use Generator.
func NewGenerator() *Generator {
	return &Generator{}
}

// Generate builds a Plan from the given Workflow and resolved target
// list. The resolved targets are the hosts that passed precheck and are
// confirmed reachable. Generate divides them into batches according to
// wf.Batches.Strategy and orchestrates wf.Steps into each batch.
//
// Returns an error when:
//   - wf is nil;
//   - resolvedTargets is empty;
//   - the batch strategy is unknown.
//
// The division is total: every resolved target appears in exactly one
// batch.
func (g *Generator) Generate(wf *dsl.Workflow, resolvedTargets []string) (*Plan, error) {
	if wf == nil {
		return nil, errors.New(errors.LE002, "workflow is nil", errors.Fatal)
	}
	if len(resolvedTargets) == 0 {
		return nil, errors.New(errors.LE092, "resolved targets is empty", errors.Fatal)
	}

	// Divide targets into batches according to the strategy.
	batchTargets, err := splitBatches(resolvedTargets, wf.Batches)
	if err != nil {
		return nil, err
	}

	// Convert workflow steps to plan steps once; every batch shares
	// the same step sequence.
	planSteps := convertSteps(wf.Steps)

	// Build batches with consecutive 0-based indices.
	batches := make([]Batch, 0, len(batchTargets))
	for i, targets := range batchTargets {
		batches = append(batches, Batch{
			Index:          i,
			Targets:        targets,
			Steps:          planSteps,
			MaxConcurrency: wf.Batches.MaxConcurrency,
		})
	}

	plan := &Plan{
		ID:           newPlanID(),
		WorkflowName: wf.Meta.Name,
		Batches:      batches,
		TotalTargets: len(resolvedTargets),
		CreatedAt:    time.Now().UTC(),
	}
	return plan, nil
}

// splitBatches divides targets into batches according to the batch
// config strategy. An empty strategy defaults to "serial" (all targets
// in a single batch), matching the LEVEELang spec default.
func splitBatches(targets []string, cfg dsl.BatchConfig) ([][]string, error) {
	strategy := cfg.Strategy
	if strategy == "" {
		strategy = "serial"
	}
	switch strategy {
	case "percent":
		return splitPercent(targets, cfg.Steps), nil
	case "fixed":
		return splitFixed(targets, cfg.Steps), nil
	case "serial":
		return [][]string{targets}, nil
	default:
		return nil, errors.New(errors.LE034,
			fmt.Sprintf("unknown batch strategy %q (allowed: percent, fixed, serial)", strategy),
			errors.Fatal)
	}
}

// splitPercent divides targets into batches by percentage milestones.
// steps is a strictly increasing array ending at 100, e.g.
// [1, 10, 50, 100]. The first batch is total*steps[0]/100 targets,
// subsequent batches are total*(steps[i]-steps[i-1])/100 targets.
//
// To avoid rounding loss (integer division can drop targets when
// total*percent < 100), the last batch takes all remaining targets.
// Empty batches (when a percentage milestone rounds to the same
// cumulative count as the previous one) are skipped so that batch
// indices stay consecutive and every batch has at least one target.
func splitPercent(targets []string, steps []int) [][]string {
	total := len(targets)
	if total == 0 || len(steps) == 0 {
		return [][]string{targets}
	}

	var batches [][]string
	prev := 0
	for i, step := range steps {
		var end int
		if i == len(steps)-1 {
			// Last milestone: take all remaining targets to absorb
			// rounding error and guarantee total coverage.
			end = total
		} else {
			end = total * step / 100
		}
		if end > total {
			end = total
		}
		if end < prev {
			// Defensive: steps should be non-decreasing (validated
			// upstream), but clamp to avoid negative slices.
			end = prev
		}
		if end > prev {
			batches = append(batches, targets[prev:end])
		}
		prev = end
	}

	// Fallback: if no non-empty batches were produced (e.g. all
	// milestones rounded to 0 and the last milestone was also 0,
	// which cannot happen when steps ends at 100, but guard anyway),
	// return a single batch with all targets.
	if len(batches) == 0 {
		return [][]string{targets}
	}
	return batches
}

// splitFixed divides targets into batches of fixed sizes. steps is an
// array of batch sizes, e.g. [2, 3, 5] produces batches of 2, 3 and 5
// targets. The last declared batch may be smaller than its size when
// targets run out. Any leftover targets beyond the sum of steps are
// placed in a final trailing batch so that no target is dropped.
//
// Non-positive step values are skipped. Empty batches are never
// produced.
func splitFixed(targets []string, steps []int) [][]string {
	total := len(targets)
	if total == 0 {
		return nil
	}
	if len(steps) == 0 {
		return [][]string{targets}
	}

	var batches [][]string
	pos := 0
	for _, step := range steps {
		if step <= 0 {
			continue
		}
		end := pos + step
		if end > total {
			end = total
		}
		if end > pos {
			batches = append(batches, targets[pos:end])
		}
		pos = end
		if pos >= total {
			break
		}
	}

	// Leftover targets beyond the declared steps go into a final batch.
	if pos < total {
		batches = append(batches, targets[pos:total])
	}

	if len(batches) == 0 {
		return [][]string{targets}
	}
	return batches
}

// convertSteps converts workflow steps to plan steps, preserving the
// module/action reference, arguments and the optional rollback /
// approval / gate overrides. The output slice is always non-nil when
// the input is non-nil, so that batches carry an explicit (possibly
// empty) step sequence.
func convertSteps(steps []dsl.Step) []PlanStep {
	out := make([]PlanStep, len(steps))
	for i, s := range steps {
		out[i] = PlanStep{
			Name:     s.Name,
			Module:   s.Module,
			Action:   s.Action,
			Args:     s.Args,
			Rollback: s.Rollback,
			Approval: s.Approval,
			Gate:     s.Gate,
		}
	}
	return out
}

// newPlanID generates a unique plan identifier using crypto/rand. The
// ID has the form "plan-<16-hex-chars>". On the extremely unlikely
// event that rand.Read fails, it falls back to a timestamp-based ID.
func newPlanID() string {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("plan-%d", time.Now().UnixNano())
	}
	return "plan-" + hex.EncodeToString(b)
}
