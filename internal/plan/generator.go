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
	"strings"
	"time"

	"github.com/nexus/levee/internal/dsl"
	"github.com/nexus/levee/internal/errors"
	"github.com/nexus/levee/internal/executor"
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

	// RiskScore is the plan's danger score (0-100) computed at plan
	// time. It ships inside the plan artifact so the approved change's
	// score is exactly the executed one (the plan_hash binds them).
	RiskScore int `json:"risk_score,omitempty"`

	// RiskFactors is the explainable breakdown behind RiskScore (one
	// entry per contributing rule). Serialised into the artifact for
	// the CLI / audit report. The assessor (internal/risk) produces the
	// values; the plan package mirrors the entry shape so it can live
	// in the artifact without an import cycle.
	RiskFactors []RiskFactor `json:"risk_factors,omitempty"`

	// ApprovalFloor is the minimum approval tier the plan demands
	// (R4: any irreversible step ⇒ at least high). The approval
	// routing takes max(workflow declaration, ApprovalFloor) — the
	// floor can raise but never lower the tier.
	ApprovalFloor string `json:"approval_floor,omitempty"`

	// Approval, Rollback and Gate preserve workflow-level governance
	// declarations in the approved artifact. ChangeService consumes
	// Approval directly for kickoff routing.
	//
	// Rollback is run-level policy only (spec §7.1, LE097): on_failure and
	// verify_after describe what happens when the whole run fails. The
	// compensation contract — strategy, undo steps, snapshot_paths — lives
	// on the step (PlanStep.Rollback), because the compensation ledger
	// attributes every compensation to a (host, forward step) pair and the
	// generator refuses plan-level compensation content outright.
	Approval *dsl.ApprovalSpec `json:"approval,omitempty"`
	Rollback *dsl.RollbackSpec `json:"rollback,omitempty"`
	Gate     *dsl.GateSpec     `json:"gate,omitempty"`
}

// RiskFactor mirrors risk.Factor (rule / points / detail) as a plan
// artifact type. Kept structurally identical so the wiring layer can
// copy entries over field by field without reflection.
type RiskFactor struct {
	Rule   string `json:"rule"`
	Points int    `json:"points"`
	Detail string `json:"detail"`
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

	// Gate preserves the workflow's post_batch gate declaration. Current
	// execution still consumes gates from PlanStep; persisting this keeps
	// declared batch-boundary governance inside the approved hash.
	Gate *dsl.GateSpec `json:"gate,omitempty"`
}

// PlanStep is a single step in a plan, derived from a dsl.Step. It
// carries the module/action to invoke, the action arguments and optional
// step-level rollback / approval / gate declarations. Execution consumes
// these step declarations directly; workflow-level declarations are retained
// at the Plan level for approval routing and hash identity.
//
// Irreversible / IrreversibleReason record the verdict of the executor's
// IrreversibleChecker at plan time (explicit author declaration or
// whitelist match). Downstream consumers (approval tier routing per R4,
// automatic-rollback gating per R2) read these fields instead of
// re-deriving the verdict, so the plan artifact is the single source of
// truth for what was judged irreversible when the plan was approved.
type PlanStep struct {
	Name               string
	Module             string
	Action             string
	Args               map[string]any
	Rollback           *dsl.RollbackSpec
	Approval           *dsl.ApprovalSpec
	Gate               *dsl.GateSpec
	Irreversible       bool
	IrreversibleReason string
}

// Generator transforms a parsed Workflow AST into an executable Plan.
// The zero value is not ready — use NewGenerator. A Generator is
// stateless and safe for concurrent use.
type Generator struct {
	// irreversible judges each step's reversibility at plan time. The
	// verdict is persisted onto PlanStep so approval tier routing (R4)
	// and rollback gating (R2) consume the plan artifact rather than
	// re-deriving the verdict.
	irreversible *executor.IrreversibleChecker
}

// NewGenerator returns a ready-to-use Generator with an IrreversibleChecker
// populated with the engine's default destructive-action whitelist. The
// whitelist registers the actions that are irreversible by nature so that
// workflow authors do not have to repeat irreversible: true on every such
// step; an explicit author declaration still takes priority (checked
// first).
func NewGenerator() *Generator {
	c := executor.NewIrreversibleChecker()
	for _, pair := range [][2]string{
		{"pkg", "remove"},
		{"file", "delete"},
		{"user", "remove"},
		{"mysql", "replica_switch"},
		{"mysql", "pt_osc"},
	} {
		c.RegisterWhitelist(pair[0], pair[1])
	}
	return &Generator{irreversible: c}
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

	// Workflow-level rollback is run-level policy only (spec §7.1): strategy,
	// undo steps and snapshot_paths there are unattributable — the
	// compensation ledger keys on (host, forward step) — so this boundary
	// refuses them (LE097) instead of persisting governance that no
	// execution path can honour. dsl.Validator reports the same violation
	// for the `levee compile` path; this gate is what protects the server
	// path, because wiring.GeneratePlan parses the workflow document and
	// calls the generator directly, without the validator in between.
	if verrs := dsl.ValidateRunLevelRollback(wf.Rollback, "rollback"); len(verrs) > 0 {
		return nil, errors.New(verrs[0].Code,
			fmt.Sprintf("workflow %q: %s", wf.Meta.Name, verrs[0].Message), errors.Fatal)
	}

	// Divide targets into batches according to the strategy.
	batchTargets, err := splitBatches(resolvedTargets, wf.Batches)
	if err != nil {
		return nil, err
	}

	// Convert workflow steps to plan steps once; every batch shares
	// the same step sequence. The conversion also stamps the
	// irreversible verdict onto each step.
	planSteps := g.convertSteps(wf.Steps)

	// Build batches with consecutive 0-based indices.
	batches := make([]Batch, 0, len(batchTargets))
	for i, targets := range batchTargets {
		batches = append(batches, Batch{
			Index:          i,
			Targets:        targets,
			Steps:          planSteps,
			MaxConcurrency: wf.Batches.MaxConcurrency,
			Gate:           wf.Batches.Gate,
		})
	}

	planID, err := newPlanID()
	if err != nil {
		return nil, err
	}
	plan := &Plan{
		ID:           planID,
		WorkflowName: wf.Meta.Name,
		Batches:      batches,
		TotalTargets: len(resolvedTargets),
		CreatedAt:    time.Now().UTC(),
		Approval:     wf.Approval,
		Rollback:     wf.Rollback,
		Gate:         wf.Gate,
	}
	return plan, nil
}

// splitBatches divides targets into batches according to the batch
// config strategy. An empty strategy defaults to dsl.BatchStrategySerial (all
// targets in a single batch), matching the LEVEELang spec default.
//
// The accepted vocabulary is dsl.BatchStrategies — the parser validates
// against the same list, so the two layers cannot disagree. The error message
// is built from that list rather than a hand-copied literal so it cannot fall
// out of date either.
func splitBatches(targets []string, cfg dsl.BatchConfig) ([][]string, error) {
	strategy := cfg.Strategy
	if strategy == "" {
		strategy = dsl.BatchStrategySerial
	}
	switch strategy {
	case dsl.BatchStrategyPercent:
		return splitPercent(targets, cfg.Steps), nil
	case dsl.BatchStrategyFixed:
		return splitFixed(targets, cfg.Steps), nil
	case dsl.BatchStrategySerial:
		return [][]string{targets}, nil
	case dsl.BatchStrategyOnePerTarget:
		return splitOnePerTarget(targets), nil
	default:
		return nil, errors.New(errors.LE034,
			fmt.Sprintf("unknown batch strategy %q (allowed: %s)",
				strategy, strings.Join(dsl.BatchStrategies, ", ")),
			errors.Fatal)
	}
}

// splitOnePerTarget puts exactly one target in each batch, so the executor
// walks the target list strictly serially. It is the documented strategy for
// rolling database primaries over one at a time, where concurrent batches
// would mean two simultaneous DDL windows on the same cluster.
//
// The parser has always accepted this strategy (and examples/gate-templates/
// mysql.yaml uses it) but the generator had no case for it, so planning such a
// workflow died with a Fatal LE034. batches.steps is unused here by design —
// the spec says one-per-target needs no steps.
func splitOnePerTarget(targets []string) [][]string {
	if len(targets) == 0 {
		return [][]string{targets}
	}
	batches := make([][]string, len(targets))
	for i, t := range targets {
		batches[i] = []string{t}
	}
	return batches
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
// convertSteps converts dsl steps to plan steps, stamping each with the
// IrreversibleChecker verdict. An explicit author declaration
// (irreversible: true) wins over the whitelist; both routes record a
// human-readable reason on the step for the audit trail.
func (g *Generator) convertSteps(steps []dsl.Step) []PlanStep {
	out := make([]PlanStep, len(steps))
	for i, s := range steps {
		verdict := g.irreversible.Check(executor.Step{
			Module:       s.Module,
			Action:       s.Action,
			Irreversible: s.Irreversible,
		})
		out[i] = PlanStep{
			Name:               s.Name,
			Module:             s.Module,
			Action:             s.Action,
			Args:               s.Args,
			Rollback:           s.Rollback,
			Approval:           s.Approval,
			Gate:               s.Gate,
			Irreversible:       verdict.Irreversible,
			IrreversibleReason: verdict.Reason,
		}
	}
	return out
}

// newPlanID generates a unique plan identifier using crypto/rand. The
// ID has the form "plan-<16-hex-chars>". Plan IDs are uniqueness-critical,
// so a rand.Read failure is returned as an error — no timestamp fallback
// (SA-012).
func newPlanID() (string, error) {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("plan: generate plan id: %w", err)
	}
	return "plan-" + hex.EncodeToString(b), nil
}
