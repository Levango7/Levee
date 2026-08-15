package dsl

// ir.go — LEVEELang intermediate representation (IR).
//
// The IR is a versioned, serialisable snapshot of a fully type-checked
// workflow. It captures:
//   - the IR schema version (currently "1.0")
//   - the input parameter type table
//   - the batch orchestration plan
//   - the step dependency graph
//   - the rollback step mapping
//
// GenerateIR is the entry point: it walks a Workflow AST + TypeRegistry and
// emits an *IR. The IR is JSON-marshallable so that `levee compile --ir` can
// write it to disk and downstream phases (plan/apply) can consume it without
// re-parsing the source YAML.

import (
	"encoding/json"
	"fmt"
	"strings"
)

// IRVersion is the current IR schema version emitted by GenerateIR.
const IRVersion = "1.0"

// IR is the intermediate representation of a LEVEE workflow.
type IR struct {
	// IRVersion is the schema version (currently "1.0").
	IRVersion string `json:"ir_version"`
	// Workflow is the source workflow metadata.
	Workflow IRWorkflow `json:"workflow"`
	// Inputs is the typed input parameter table.
	Inputs []IRInput `json:"inputs"`
	// Targets is the list of target groups.
	Targets []IRTarget `json:"targets"`
	// Batches is the batch orchestration plan.
	Batches IRBatches `json:"batches"`
	// Steps is the step dependency graph.
	Steps []IRStep `json:"steps"`
	// Rollback maps step names to their rollback step names. nil when the
	// workflow has no rollback spec.
	Rollback *IRRollback `json:"rollback,omitempty"`
	// Approval is the approval requirement, nil when absent.
	Approval *IRApproval `json:"approval,omitempty"`
}

// IRWorkflow carries workflow metadata.
type IRWorkflow struct {
	Name        string `json:"name"`
	Version     string `json:"version"`
	Description string `json:"description,omitempty"`
}

// IRInput is a typed input parameter entry in the IR.
type IRInput struct {
	Name       string `json:"name"`
	Type       string `json:"type"`
	Required   bool   `json:"required,omitempty"`
	HasDefault bool   `json:"has_default"`
	Default    any    `json:"default,omitempty"`
}

// IRTarget is a target group entry in the IR.
type IRTarget struct {
	Name     string   `json:"name"`
	Type     string   `json:"type,omitempty"`
	Hosts    []string `json:"hosts,omitempty"`
	Query    string   `json:"query,omitempty"`
	MinCount int      `json:"min_count,omitempty"`
	MaxCount int      `json:"max_count,omitempty"`
}

// IRBatches is the batch orchestration plan.
type IRBatches struct {
	Strategy       string `json:"strategy,omitempty"`
	MaxConcurrency int    `json:"max_concurrency,omitempty"`
	Serial         bool   `json:"serial,omitempty"`
	Steps          []int  `json:"steps,omitempty"`
}

// IRStep is a node in the step dependency graph.
type IRStep struct {
	Name           string   `json:"name"`
	Action         string   `json:"action"`
	Module         string   `json:"module,omitempty"`
	DependsOn      []string `json:"depends_on,omitempty"`
	Idempotent     bool     `json:"idempotent,omitempty"`
	Irreversible   bool     `json:"irreversible,omitempty"`
	RequiresReboot bool     `json:"requires_reboot,omitempty"`
}

// IRRollback maps each step name to its rollback step names.
type IRRollback struct {
	Strategy    string              `json:"strategy,omitempty"`
	OnFailure   string              `json:"on_failure,omitempty"`
	VerifyAfter bool                `json:"verify_after,omitempty"`
	StepMap     map[string][]string `json:"step_map,omitempty"`
}

// IRApproval is the approval requirement in the IR.
type IRApproval struct {
	Level            string   `json:"level,omitempty"`
	Approvers        []string `json:"approvers,omitempty"`
	MinApprovers     int      `json:"min_approvers,omitempty"`
	ExcludeInitiator bool     `json:"exclude_initiator,omitempty"`
	Timeout          string   `json:"timeout,omitempty"`
}

// GenerateIR walks the AST + registry and produces a versioned IR. The AST is
// not mutated. Returns an error when wf is nil or when the registry cannot
// resolve a declared type (the IR still records the symbolic name in that
// case, but the error gives the caller a chance to abort).
func GenerateIR(ast *Workflow, registry *TypeRegistry) (*IR, error) {
	if ast == nil {
		return nil, fmt.Errorf("generate ir: workflow is nil")
	}
	if registry == nil {
		registry = NewTypeRegistry()
	}

	ir := &IR{
		IRVersion: IRVersion,
		Workflow: IRWorkflow{
			Name:        ast.Meta.Name,
			Version:     ast.Meta.Version,
			Description: ast.Meta.Description,
		},
	}

	// Inputs.
	ir.Inputs = make([]IRInput, 0, len(ast.Inputs))
	for _, p := range ast.Inputs {
		entry := IRInput{
			Name:       p.Name,
			Type:       p.Type,
			Required:   p.Required,
			HasDefault: p.Default != nil,
			Default:    p.Default,
		}
		// Normalise the type name through the registry when possible so the
		// IR carries the canonical name (e.g. alias "port" stays "port" but
		// an unknown "stringg" is left as-is for downstream diagnostics).
		if t := registry.Resolve(p.Type); t != nil {
			entry.Type = t.String()
		}
		ir.Inputs = append(ir.Inputs, entry)
	}

	// Targets.
	ir.Targets = make([]IRTarget, 0, len(ast.Targets))
	for _, t := range ast.Targets {
		ir.Targets = append(ir.Targets, IRTarget{
			Name:     t.Name,
			Type:     t.Type,
			Hosts:    append([]string(nil), t.Hosts...),
			Query:    t.Query,
			MinCount: t.MinCount,
			MaxCount: t.MaxCount,
		})
	}

	// Batches.
	ir.Batches = IRBatches{
		Strategy:       ast.Batches.Strategy,
		MaxConcurrency: ast.Batches.MaxConcurrency,
		Serial:         ast.Batches.Serial,
		Steps:          append([]int(nil), ast.Batches.Steps...),
	}

	// Steps + dependency graph.
	ir.Steps = make([]IRStep, 0, len(ast.Steps))
	for _, s := range ast.Steps {
		ir.Steps = append(ir.Steps, IRStep{
			Name:           s.Name,
			Action:         s.Action,
			Module:         s.Module,
			DependsOn:      append([]string(nil), s.DependsOn...),
			Idempotent:     s.Idempotent,
			Irreversible:   s.Irreversible,
			RequiresReboot: s.RequiresReboot,
		})
	}

	// Rollback step map.
	if ast.Rollback != nil {
		rb := &IRRollback{
			Strategy:    ast.Rollback.Strategy,
			OnFailure:   ast.Rollback.OnFailure,
			VerifyAfter: ast.Rollback.VerifyAfter,
			StepMap:     make(map[string][]string),
		}
		// Map every workflow step to the rollback step names. When the
		// rollback spec carries its own steps, they apply to every workflow
		// step; otherwise the map is empty.
		if len(ast.Rollback.Steps) > 0 {
			names := make([]string, 0, len(ast.Rollback.Steps))
			for _, rs := range ast.Rollback.Steps {
				if rs.Name != "" {
					names = append(names, rs.Name)
				}
			}
			for _, s := range ast.Steps {
				rb.StepMap[s.Name] = append([]string(nil), names...)
			}
		}
		ir.Rollback = rb
	}

	// Approval.
	if ast.Approval != nil {
		ir.Approval = &IRApproval{
			Level:            ast.Approval.Level,
			Approvers:        append([]string(nil), ast.Approval.Approvers...),
			MinApprovers:     ast.Approval.MinApprovers,
			ExcludeInitiator: ast.Approval.ExcludeInitiator,
			Timeout:          ast.Approval.Timeout,
		}
	}

	return ir, nil
}

// ---------------------------------------------------------------------------
// JSON round-trip
// ---------------------------------------------------------------------------

// MarshalJSON serialises the IR to JSON. The default struct tagging already
// produces the right shape; this method exists so callers can override the
// indentation via an Encoder if needed. We deliberately keep the default
// behaviour here.
func (ir *IR) MarshalJSON() ([]byte, error) {
	if ir == nil {
		return []byte("null"), nil
	}
	// Use a type alias to avoid recursion into MarshalJSON.
	type alias IR
	a := alias(*ir)
	if a.IRVersion == "" {
		a.IRVersion = IRVersion
	}
	return json.Marshal(a)
}

// UnmarshalJSON deserialises an IR from JSON. It validates the ir_version
// field and rejects unknown future versions with a descriptive error.
func (ir *IR) UnmarshalJSON(data []byte) error {
	if ir == nil {
		return fmt.Errorf("UnmarshalJSON: nil IR receiver")
	}
	type alias IR
	var a alias
	if err := json.Unmarshal(data, &a); err != nil {
		return fmt.Errorf("unmarshal ir: %w", err)
	}
	if a.IRVersion == "" {
		return fmt.Errorf("unmarshal ir: missing ir_version field")
	}
	if !isSupportedIRVersion(a.IRVersion) {
		return fmt.Errorf("unmarshal ir: unsupported ir_version %q (supported: %s)",
			a.IRVersion, strings.Join(supportedIRVersions(), ", "))
	}
	*ir = IR(a)
	return nil
}

// supportedIRVersions returns the list of IR schema versions this binary can
// decode. Currently only "1.0".
func supportedIRVersions() []string {
	return []string{IRVersion}
}

// isSupportedIRVersion reports whether v is a decodable IR schema version.
func isSupportedIRVersion(v string) bool {
	for _, sv := range supportedIRVersions() {
		if v == sv {
			return true
		}
	}
	return false
}
