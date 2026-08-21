package dsl

import (
	"fmt"
	"io"
	"os"
	"strings"

	"gopkg.in/yaml.v3"
)

// ParseError is a structured parse error carrying a stable error code, the
// 1-based line number where the error was detected (0 when unknown), the
// offending field path and a human-readable message. It implements the error
// interface and wraps an optional cause.
type ParseError struct {
	Code    string
	Line    int
	Field   string
	Message string
	Cause   error
}

// Error returns a single-line representation suitable for logging and CLI
// output. The format is: "LE###: <message> (field=<field>, line=<line>)".
func (e *ParseError) Error() string {
	if e == nil {
		return "<nil>"
	}
	msg := e.Message
	if e.Code != "" {
		msg = e.Code + ": " + msg
	}
	parts := make([]string, 0, 2)
	if e.Field != "" {
		parts = append(parts, "field="+e.Field)
	}
	if e.Line > 0 {
		parts = append(parts, fmt.Sprintf("line=%d", e.Line))
	}
	if len(parts) > 0 {
		msg = msg + " (" + strings.Join(parts, ", ") + ")"
	}
	if e.Cause != nil {
		msg = msg + ": " + e.Cause.Error()
	}
	return msg
}

// Unwrap returns the wrapped cause, enabling errors.Is and errors.As.
func (e *ParseError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Cause
}

// newError constructs a ParseError with the given code, field and message.
func newError(code, field, message string) *ParseError {
	return &ParseError{Code: code, Field: field, Message: message}
}

// newErrorLine constructs a ParseError with a line number.
func newErrorLine(code, field string, line int, message string) *ParseError {
	return &ParseError{Code: code, Field: field, Line: line, Message: message}
}

// Parser parses LEVEELang YAML subset documents into Workflow AST nodes.
// It is safe for concurrent use after construction; the zero value is not
// ready — use NewParser.
type Parser struct{}

// NewParser returns a ready-to-use Parser.
func NewParser() *Parser {
	return &Parser{}
}

// ParseFile reads and parses the YAML file at path.
func (p *Parser) ParseFile(path string) (*Workflow, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, &ParseError{
			Code:    "LE002",
			Field:   "file",
			Message: fmt.Sprintf("cannot read file %q", path),
			Cause:   err,
		}
	}
	return p.ParseBytes(data)
}

// ParseReader parses YAML content read from r.
func (p *Parser) ParseReader(r io.Reader) (*Workflow, error) {
	data, err := io.ReadAll(r)
	if err != nil {
		return nil, &ParseError{
			Code:    "LE002",
			Field:   "reader",
			Message: "cannot read from reader",
			Cause:   err,
		}
	}
	return p.ParseBytes(data)
}

// ParseBytes parses YAML content from data and returns the resulting Workflow
// AST. On failure it returns a *ParseError carrying a code, field path and
// line number when available.
func (p *Parser) ParseBytes(data []byte) (*Workflow, error) {
	// Decode into a yaml.Node first so we can capture line numbers on errors
	// and detect empty documents.
	var root yaml.Node
	if err := yaml.Unmarshal(data, &root); err != nil {
		return nil, &ParseError{
			Code:    "LE002",
			Message: "YAML syntax error",
			Cause:   err,
		}
	}
	if root.Kind == 0 {
		return nil, newError("LE002", "", "empty YAML document")
	}

	// Decode into the intermediate raw structure.
	raw := &yamlWorkflowRaw{}
	if err := root.Decode(raw); err != nil {
		line := extractLine(err)
		return nil, &ParseError{
			Code:    "LE001",
			Line:    line,
			Message: "YAML decode error",
			Cause:   err,
		}
	}

	wf, err := convertWorkflow(raw, &root)
	if err != nil {
		return nil, err
	}

	if err := validate(wf); err != nil {
		return nil, err
	}
	return wf, nil
}

// extractLine attempts to pull a line number out of a yaml.v3 error message.
// yaml.v3 TypeError strings look like "line 5: ...". Returns 0 when no line
// number can be found.
func extractLine(err error) int {
	if err == nil {
		return 0
	}
	msg := err.Error()
	// Look for "line N:" pattern.
	idx := strings.Index(msg, "line ")
	if idx < 0 {
		return 0
	}
	rest := msg[idx+5:]
	end := 0
	for end < len(rest) && rest[end] >= '0' && rest[end] <= '9' {
		end++
	}
	if end == 0 {
		return 0
	}
	var n int
	if _, e := fmt.Sscanf(rest[:end], "%d", &n); e == nil {
		return n
	}
	return 0
}

// ---------------------------------------------------------------------------
// Intermediate YAML structures (with yaml tags)
// ---------------------------------------------------------------------------

type yamlWorkflowRaw struct {
	Name        string           `yaml:"name"`
	Version     string           `yaml:"version"`
	Description string           `yaml:"description"`
	Input       any              `yaml:"input"` // list or map; handled manually
	Target      *yamlTargetRaw   `yaml:"target"`
	Targets     []yamlTargetRaw  `yaml:"targets"`
	Window      *yamlWindowRaw   `yaml:"window"`
	Batches     *yamlBatchesRaw  `yaml:"batches"`
	Approval    *yamlApprovalRaw `yaml:"approval"`
	Steps       []yamlStepRaw    `yaml:"steps"`
	Gates       []yamlGateRaw    `yaml:"gates"`
	Rollback    *yamlRollbackRaw `yaml:"rollback"`
}

type yamlTargetRaw struct {
	Name     string   `yaml:"name"`
	Type     string   `yaml:"type"`
	Hosts    []string `yaml:"hosts"`
	Query    string   `yaml:"query"`
	MinCount int      `yaml:"min_count"`
	MaxCount int      `yaml:"max_count"`
}

type yamlWindowRaw struct {
	Start          string   `yaml:"start"`
	End            string   `yaml:"end"`
	Timezone       string   `yaml:"timezone"`
	MaxConcurrency int      `yaml:"max_concurrency"`
	Days           []string `yaml:"days"`
}

type yamlBatchesRaw struct {
	Strategy       string       `yaml:"strategy"`
	MaxConcurrency int          `yaml:"max_concurrency"`
	Serial         bool         `yaml:"serial"`
	Steps          []int        `yaml:"steps"`
	Gate           *yamlGateRaw `yaml:"gate"`
}

type yamlApprovalRaw struct {
	Level            string   `yaml:"level"`
	Approvers        []string `yaml:"approvers"`
	MinApprovers     int      `yaml:"min_approvers"`
	ExcludeInitiator bool     `yaml:"exclude_initiator"`
	Timeout          string   `yaml:"timeout"`
}

type yamlStepRaw struct {
	Name           string           `yaml:"name"`
	Action         string           `yaml:"action"`
	Args           map[string]any   `yaml:"args"`
	Idempotent     bool             `yaml:"idempotent"`
	Irreversible   bool             `yaml:"irreversible"`
	RequiresReboot bool             `yaml:"requires_reboot"`
	DependsOn      []string         `yaml:"depends_on"`
	Verify         *yamlGateRaw     `yaml:"verify"`
	Rollback       *yamlRollbackRaw `yaml:"rollback"`
	Approval       *yamlApprovalRaw `yaml:"approval"`
}

type yamlGateRaw struct {
	Position string        `yaml:"position"`
	Cmd      *yamlCmdRaw   `yaml:"cmd"`
	Slo      *yamlSloRaw   `yaml:"slo"`
	Probe    *yamlProbeRaw `yaml:"probe"`
	Human    *yamlHumanRaw `yaml:"human"`
}

type yamlCmdRaw struct {
	Run          string `yaml:"run"`
	ExpectExit   int    `yaml:"expect_exit"`
	ExpectStdout string `yaml:"expect_stdout"`
	Timeout      string `yaml:"timeout"`
}

type yamlSloRaw struct {
	Query   string `yaml:"query"`
	Source  string `yaml:"source"`
	Timeout string `yaml:"timeout"`
	Wait    string `yaml:"wait"`
}

type yamlProbeRaw struct {
	URL          string `yaml:"url"`
	ExpectStatus int    `yaml:"expect_status"`
	ExpectBody   string `yaml:"expect_body"`
	Timeout      string `yaml:"timeout"`
}

type yamlHumanRaw struct {
	Message string   `yaml:"message"`
	Timeout string   `yaml:"timeout"`
	Notify  []string `yaml:"notify"`
}

type yamlRollbackRaw struct {
	Strategy    string        `yaml:"strategy"`
	OnFailure   string        `yaml:"on_failure"`
	VerifyAfter bool          `yaml:"verify_after"`
	Step        *yamlStepRaw  `yaml:"step"`
	Steps       []yamlStepRaw `yaml:"steps"`
}

// ---------------------------------------------------------------------------
// Conversion from raw YAML to AST
// ---------------------------------------------------------------------------

func convertWorkflow(raw *yamlWorkflowRaw, root *yaml.Node) (*Workflow, error) {
	wf := &Workflow{
		Meta: WorkflowMeta{
			Name:        raw.Name,
			Version:     raw.Version,
			Description: raw.Description,
		},
	}

	// Inputs: support list form, detailed map form and shorthand map form.
	inputs, err := convertInput(raw.Input)
	if err != nil {
		return nil, err
	}
	wf.Inputs = inputs

	// Targets: accept both singular "target" and plural "targets".
	if raw.Target != nil {
		wf.Targets = append(wf.Targets, convertTarget(raw.Target))
	}
	for i := range raw.Targets {
		wf.Targets = append(wf.Targets, convertTarget(&raw.Targets[i]))
	}

	// Window.
	if raw.Window != nil {
		wf.Window = ChangeWindow{
			Start:          raw.Window.Start,
			End:            raw.Window.End,
			Timezone:       raw.Window.Timezone,
			MaxConcurrency: raw.Window.MaxConcurrency,
			Days:           raw.Window.Days,
		}
	}

	// Batches.
	if raw.Batches != nil {
		wf.Batches = BatchConfig{
			Strategy:       raw.Batches.Strategy,
			MaxConcurrency: raw.Batches.MaxConcurrency,
			Serial:         raw.Batches.Serial,
			Steps:          raw.Batches.Steps,
		}
		if raw.Batches.Gate != nil {
			wf.Batches.Gate = convertGate(raw.Batches.Gate)
		}
	}

	// Approval.
	if raw.Approval != nil {
		wf.Approval = convertApproval(raw.Approval)
	}

	// Steps.
	for i := range raw.Steps {
		s, err := convertStep(&raw.Steps[i])
		if err != nil {
			return nil, err
		}
		wf.Steps = append(wf.Steps, s)
	}

	// Rollback.
	if raw.Rollback != nil {
		rb, err := convertRollback(raw.Rollback)
		if err != nil {
			return nil, err
		}
		wf.Rollback = rb
	}

	// Workflow-level gates: distribute by position.
	for i := range raw.Gates {
		gc, err := convertGateCheck(&raw.Gates[i])
		if err != nil {
			return nil, err
		}
		pos := raw.Gates[i].Position
		if wf.Gate == nil {
			wf.Gate = &GateSpec{}
		}
		switch pos {
		case "pre_apply", "":
			wf.Gate.Pre = append(wf.Gate.Pre, gc)
		case "post_apply", "post_batch":
			wf.Gate.Post = append(wf.Gate.Post, gc)
		default:
			return nil, newError("LE051", "gates[].position",
				fmt.Sprintf("unknown gate position %q", pos))
		}
	}

	return wf, nil
}

// convertInput converts the raw input declaration (which may be a list, a
// detailed map or a shorthand map) into a slice of InputParam.
func convertInput(raw any) ([]InputParam, error) {
	if raw == nil {
		return nil, nil
	}
	switch v := raw.(type) {
	case []any:
		out := make([]InputParam, 0, len(v))
		for i, item := range v {
			m, ok := item.(map[string]any)
			if !ok {
				return nil, newError("LE001", fmt.Sprintf("input[%d]", i),
					"input list entry must be a mapping")
			}
			p, err := inputFromMap(m, fmt.Sprintf("input[%d]", i))
			if err != nil {
				return nil, err
			}
			out = append(out, p)
		}
		return out, nil
	case map[string]any:
		out := make([]InputParam, 0, len(v))
		for name, val := range v {
			switch vv := val.(type) {
			case string:
				// Shorthand: "name: type".
				out = append(out, InputParam{Name: name, Type: vv})
			case map[string]any:
				// Detailed: "name: {type: ..., default: ...}".
				p, err := inputFromMap(vv, "input."+name)
				if err != nil {
					return nil, err
				}
				p.Name = name
				out = append(out, p)
			default:
				return nil, newError("LE001", "input."+name,
					"input value must be a type string or a mapping")
			}
		}
		return out, nil
	default:
		return nil, newError("LE001", "input",
			"input must be a list or a mapping")
	}
}

// inputFromMap builds an InputParam from a mapping with keys type, default,
// required and validate. The name key is optional here (set by caller).
func inputFromMap(m map[string]any, field string) (InputParam, error) {
	p := InputParam{}
	if t, ok := m["type"]; ok {
		s, ok := t.(string)
		if !ok {
			return p, newError("LE001", field+".type",
				"input type must be a string")
		}
		p.Type = s
	}
	if d, ok := m["default"]; ok {
		p.Default = d
	}
	if r, ok := m["required"]; ok {
		switch b := r.(type) {
		case bool:
			p.Required = b
		default:
			return p, newError("LE001", field+".required",
				"required must be a boolean")
		}
	}
	if v, ok := m["validate"]; ok {
		s, ok := v.(string)
		if !ok {
			return p, newError("LE001", field+".validate",
				"validate must be a string")
		}
		p.Validate = s
	}
	if n, ok := m["name"]; ok {
		s, ok := n.(string)
		if !ok {
			return p, newError("LE001", field+".name",
				"name must be a string")
		}
		p.Name = s
	}
	return p, nil
}

// convertTarget maps a raw target to a TargetGroup.
func convertTarget(t *yamlTargetRaw) TargetGroup {
	return TargetGroup{
		Name:     t.Name,
		Hosts:    t.Hosts,
		Query:    t.Query,
		Type:     t.Type,
		MinCount: t.MinCount,
		MaxCount: t.MaxCount,
	}
}

// convertApproval maps a raw approval to an ApprovalSpec.
func convertApproval(a *yamlApprovalRaw) *ApprovalSpec {
	return &ApprovalSpec{
		Level:            a.Level,
		Approvers:        a.Approvers,
		MinApprovers:     a.MinApprovers,
		ExcludeInitiator: a.ExcludeInitiator,
		Timeout:          a.Timeout,
	}
}

// convertStep maps a raw step to a Step, splitting the dotted action ref into
// Module and Action.
func convertStep(s *yamlStepRaw) (Step, error) {
	module, action := splitAction(s.Action)
	out := Step{
		Name:           s.Name,
		Module:         module,
		Action:         action,
		Args:           s.Args,
		Idempotent:     s.Idempotent,
		Irreversible:   s.Irreversible,
		RequiresReboot: s.RequiresReboot,
		DependsOn:      s.DependsOn,
	}
	if s.Verify != nil {
		out.Gate = convertGate(s.Verify)
	}
	if s.Approval != nil {
		out.Approval = convertApproval(s.Approval)
	}
	if s.Rollback != nil {
		rb, err := convertRollback(s.Rollback)
		if err != nil {
			return out, err
		}
		out.Rollback = rb
	}
	return out, nil
}

// splitAction splits a dotted action reference "module.name" into its parts.
// When there is no dot, Module is empty and Action is the whole string.
func splitAction(action string) (module, name string) {
	if action == "" {
		return "", ""
	}
	if idx := strings.IndexByte(action, '.'); idx >= 0 {
		return action[:idx], action[idx+1:]
	}
	return "", action
}

// convertRollback maps a raw rollback to a RollbackSpec, collecting both the
// singular "step" and plural "steps" forms.
func convertRollback(r *yamlRollbackRaw) (*RollbackSpec, error) {
	spec := &RollbackSpec{
		Strategy:    r.Strategy,
		OnFailure:   r.OnFailure,
		VerifyAfter: r.VerifyAfter,
	}
	if r.Step != nil {
		s, err := convertStep(r.Step)
		if err != nil {
			return nil, err
		}
		spec.Steps = append(spec.Steps, s)
	}
	for i := range r.Steps {
		s, err := convertStep(&r.Steps[i])
		if err != nil {
			return nil, err
		}
		spec.Steps = append(spec.Steps, s)
	}
	return spec, nil
}

// convertGate converts a raw gate (used for step verify and batches gate)
// into a GateSpec. For verify/batches gates the position is ignored and all
// checks go into Post.
func convertGate(g *yamlGateRaw) *GateSpec {
	gc, _ := convertGateCheck(g)
	return &GateSpec{Post: []GateCheck{gc}}
}

// convertGateCheck converts a raw gate into a GateCheck.
func convertGateCheck(g *yamlGateRaw) (GateCheck, error) {
	switch {
	case g.Cmd != nil:
		return GateCheck{
			Type:         "cmd",
			Command:      g.Cmd.Run,
			ExpectExit:   g.Cmd.ExpectExit,
			ExpectStdout: g.Cmd.ExpectStdout,
			Timeout:      g.Cmd.Timeout,
		}, nil
	case g.Slo != nil:
		return GateCheck{
			Type:    "slo",
			Command: g.Slo.Query,
			Source:  g.Slo.Source,
			Timeout: g.Slo.Timeout,
		}, nil
	case g.Probe != nil:
		return GateCheck{
			Type:    "probe",
			Command: g.Probe.URL,
			Timeout: g.Probe.Timeout,
		}, nil
	case g.Human != nil:
		return GateCheck{
			Type:    "human",
			Command: g.Human.Message,
			Timeout: g.Human.Timeout,
		}, nil
	default:
		return GateCheck{Type: "empty"}, newError("LE051", "gate",
			"gate has no check (cmd/slo/probe/human)")
	}
}

// ---------------------------------------------------------------------------
// Basic validation (MVP subset)
// ---------------------------------------------------------------------------

// validate performs basic structural validation on the converted AST: required
// fields, enum values and batch legality. It does not do full type checking
// (deferred to V1).
func validate(wf *Workflow) error {
	if wf.Meta.Name == "" {
		return newError("LE002", "name", "workflow name is required")
	}
	if len(wf.Targets) == 0 {
		return newError("LE092", "target", "at least one target is required")
	}
	for i, t := range wf.Targets {
		f := fmt.Sprintf("target[%d]", i)
		if t.Type == "" && len(t.Hosts) == 0 {
			return newError("LE002", f,
				"target must have a type or a static hosts list")
		}
	}
	if len(wf.Steps) == 0 {
		return newError("LE093", "steps", "at least one step is required")
	}
	for i, s := range wf.Steps {
		f := fmt.Sprintf("steps[%d]", i)
		if s.Name == "" {
			return newError("LE002", f+".name", "step name is required")
		}
		if s.Action == "" && s.Module == "" {
			return newError("LE002", f+".action", "step action is required")
		}
	}
	if wf.Approval != nil {
		switch wf.Approval.Level {
		case "", "standard", "high", "emergency":
			// valid
		default:
			return newError("LE044", "approval.level",
				fmt.Sprintf("invalid approval level %q", wf.Approval.Level))
		}
	}
	if wf.Batches.Strategy != "" {
		switch wf.Batches.Strategy {
		case "percent", "one-per-target", "count", "by-tag", "by-group":
			// valid
		default:
			return newError("LE034", "batches.strategy",
				fmt.Sprintf("unknown batch strategy %q", wf.Batches.Strategy))
		}
		if wf.Batches.Strategy == "percent" && len(wf.Batches.Steps) > 0 {
			if err := validatePercentSteps(wf.Batches.Steps); err != nil {
				return err
			}
		}
	}
	return nil
}

// validatePercentSteps checks that a percentage array is strictly increasing
// and ends at 100.
func validatePercentSteps(steps []int) error {
	if len(steps) == 0 {
		return nil
	}
	for i := 1; i < len(steps); i++ {
		if steps[i] <= steps[i-1] {
			return newError("LE031", "batches.steps",
				fmt.Sprintf("percentage array must be strictly increasing (got %d after %d at index %d)",
					steps[i], steps[i-1], i))
		}
	}
	if steps[len(steps)-1] != 100 {
		return newError("LE032", "batches.steps",
			fmt.Sprintf("percentage array must end at 100 (got %d)", steps[len(steps)-1]))
	}
	return nil
}
