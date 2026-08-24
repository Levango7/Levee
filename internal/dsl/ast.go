// Package dsl parses the LEVEELang YAML subset (MVP stage) into an abstract
// syntax tree (AST). The AST is consumed by downstream phases (plan, apply,
// rollback) and carries enough structure to support basic compile-time
// validation such as required-field checks, enum validation and batch legality.
//
// The structures defined here follow the LEVEELang specification
// (docs/leveelang-spec.md). The MVP YAML subset is described in section 10.3
// of the spec. Fields not present in the MVP subset are accepted when provided
// so that the parser is forward-compatible with V1.
package dsl

// Workflow is the root AST node representing a complete LEVEE change workflow.
// It aggregates all top-level declarations: metadata, inputs, targets, change
// window, batch strategy, steps, rollback plan, approval requirements and
// workflow-level verification gates.
type Workflow struct {
	Meta     WorkflowMeta
	Inputs   []InputParam
	Targets  []TargetGroup
	Window   ChangeWindow
	Batches  BatchConfig
	Steps    []Step
	Rollback *RollbackSpec
	Approval *ApprovalSpec
	// Gate holds workflow-level verification gates keyed by timing
	// (pre_apply / post_apply). post_batch gates live on Batches.Gate.
	Gate *GateSpec
}

// WorkflowMeta carries workflow identification metadata.
type WorkflowMeta struct {
	Name        string
	Version     string
	Description string
}

// InputParam declares a single workflow input parameter with its type, optional
// default value, required flag and validation rule expression.
type InputParam struct {
	Name     string
	Type     string
	Default  any
	Required bool
	Validate string
}

// TargetGroup declares a set of change targets. A target group is either
// static (Hosts populated) or dynamic (Query populated, resolved at plan time).
type TargetGroup struct {
	Name     string
	Hosts    []string
	Query    string
	Type     string
	MinCount int
	MaxCount int
}

// ChangeWindow declares the time window during which the change may be applied.
// MaxConcurrency caps the number of targets that may be changed in parallel
// within the window.
type ChangeWindow struct {
	Start          string
	End            string
	Timezone       string
	MaxConcurrency int
	Days           []string
}

// BatchConfig declares the batching strategy. MaxConcurrency caps in-batch
// parallelism; Serial forces one-target-at-a-time execution.
type BatchConfig struct {
	Strategy       string
	MaxConcurrency int
	Serial         bool
	Steps          []int
	// Gate holds the post_batch gate applied between batches.
	Gate *GateSpec
}

// Step declares a single change step. Module and Action are derived from the
// dotted action reference (e.g. "pkg.upgrade" -> Module=pkg, Action=upgrade).
// Args carries the action parameters. Rollback, Approval and Gate are optional
// step-level overrides. Idempotent and Irreversible annotate the step's
// safety properties.
type Step struct {
	Name           string
	Module         string
	Action         string
	Args           map[string]any
	Rollback       *RollbackSpec
	Approval       *ApprovalSpec
	Gate           *GateSpec
	Idempotent     bool
	Irreversible   bool
	RequiresReboot bool
	DependsOn      []string
}

// RollbackSpec declares the rollback plan. Strategy selects the rollback
// mechanism (snapshot / undo-action / config-revert). Steps holds the
// undo steps for the undo-action strategy.
type RollbackSpec struct {
	Steps       []Step
	Strategy    string
	OnFailure   string
	VerifyAfter bool
}

// ApprovalSpec declares the approval requirement. Level is one of
// standard / high / emergency. Approvers is an explicit list of approver
// identifiers; MinApprovers is the minimum count.
type ApprovalSpec struct {
	Level            string
	Approvers        []string
	Timeout          string
	MinApprovers     int
	ExcludeInitiator bool
}

// GateSpec groups verification checks by timing. Pre runs before the change
// (pre_apply); Post runs after the change or after each batch (post_apply /
// post_batch); Batch holds batch-timing checks that run after every batch
// completes (post_batch) and is populated by batch-level gate declarations.
type GateSpec struct {
	Pre   []GateCheck
	Post  []GateCheck
	Batch []GateCheck
}

// GateCheck declares a single verification check. Type is one of
// cmd / slo / probe / human. Command holds the command to run (for cmd) or
// the query expression (for slo). ExpectExit and ExpectStdout apply to cmd
// checks. Source identifies the metric backend for slo checks. Params is the
// free-form parameter mapping for parameterised checks (probe / slo / human):
// it is passed through verbatim by the parser and validated by the gate
// implementation at materialisation / check time (fail-closed).
type GateCheck struct {
	Type         string
	Command      string
	ExpectExit   int
	ExpectStdout string
	Source       string
	Timeout      string
	Params       map[string]any
}
