// step_gates.go — materialisation of inline workflow gate declarations into
// runnable verify.Gate implementations.
//
// A compiled plan carries per-step gate DECLARATIONS (plan.PlanStep.Gate,
// a *dsl.GateSpec). The ClosureRunner's verifier only executes gates that
// are REGISTERED with it, so before a run starts every declaration is
// compiled into a named verify.Gate and registered. From that point the
// existing RunPhase machinery executes them like any other gate.
//
// Supported check types and their runtime dependencies (GateRuntime):
//
//   - cmd    — executes against GateInput.Channel; no runtime dependency.
//     When the caller supplies no channel the command gate reports
//     Passed=false with a "missing channel" reason, which fails the phase
//     honestly.
//   - probe  — parameterised http/tcp/script reachability check; needs
//     nothing from GateRuntime. All configuration lives in the declaration's
//     Params mapping and is validated by the gate itself (fail-closed).
//   - slo    — Prometheus threshold query; REQUIRES GateRuntime.PrometheusURL
//     (verify.prometheus_url configuration). Materialisation aborts without
//     it rather than silently skipping the gate.
//   - human  — blocking approval checkpoint; REQUIRES GateRuntime.Approver.
//     Materialisation aborts without one rather than auto-passing.
//
// Fail-closed policy: a declaration the engine cannot execute — unknown
// check type, invalid params, or a missing runtime dependency above — aborts
// materialisation with an error instead of being silently skipped. A
// declared-but-unexecutable gate must never masquerade as a passing one.

package engine

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/nexus/levee/internal/dsl"
	"github.com/nexus/levee/internal/plan"
	"github.com/nexus/levee/internal/verify"
)

// GateRuntime carries the process-level dependencies that parameterised
// verification gates need at materialisation time. It is supplied to the
// ClosureRunner via WithGateRuntime and threaded into materializeStepGates.
//
//   - PrometheusURL is the Prometheus HTTP API base URL
//     (verify.prometheus_url configuration, e.g. "http://prom:9090").
//     Required only when the plan declares slo checks.
//   - Approver is the human approval transport behind human gates. Required
//     only when the plan declares human checks.
//
// The zero value is valid: a plan without slo/human declarations materialises
// fine against it, and any such declaration fails closed with an explicit
// error naming the missing configuration.
type GateRuntime struct {
	PrometheusURL string
	Approver      verify.HumanApprover

	// Channels supplies the live channel to one target, so gates that execute
	// ON targets (cmd, probe in remote mode) can run their declared check per
	// target. Nil keeps the previous honest failure: such gates report
	// "missing channel" and fail the phase rather than pretending to pass.
	Channels verify.GateChannelProvider
}

// WithChannels returns a copy of the runtime with the target-channel provider
// attached. It exists so the per-run assembly (which owns the channel cache and
// the execution lease) can hand the same sessions to gates without mutating the
// Engine-level view that plan-time refusal reads.
func (rt GateRuntime) WithChannels(p verify.GateChannelProvider) GateRuntime {
	rt.Channels = p
	return rt
}

// walkPlanGates calls fn once per inline gate declaration in the plan, in
// registration order, with the deterministic name materialisation uses
// ("step:<name>:pre:<i>", "workflow:batch:0", "batches:post:1" and so on).
//
// It exists as the single traversal: materializeStepGates and the plan-time
// executability refusal (internal/wiring) must agree about which declarations
// exist. A second hand-written walk is how a refusal ends up covering pre-apply
// checks only while the phase runs post-apply ones.
//
// All three declaration sites are walked. Coverage used to stop at the step
// level, which silently voided the other two: a workflow-level `gates:` entry and
// `batches.gate` were parsed, copied into the plan and hashed into plan_hash — so
// the approved artifact promised them — while nothing registered a gate for them
// and the executability refusal never looked. A declared gate that runs nowhere is
// a false compliance record, and a human gate there was not even refused at plan
// time.
func walkPlanGates(p *plan.Plan, fn func(name string, phase verify.GatePhase, c *dsl.GateCheck) error) error {
	if p == nil {
		return nil
	}
	if err := walkGateSpec("workflow", p.Gate, fn); err != nil {
		return err
	}
	// The generator hands every Batch the same *dsl.GateSpec, so the between-batches
	// declaration is walked once per distinct block rather than once per batch. The
	// registered gate is visited after every batch anyway: RunPhase(PhasePostBatch)
	// runs once per batch.
	seen := make(map[*dsl.GateSpec]struct{}, len(p.Batches))
	for _, b := range p.Batches {
		if b.Gate == nil {
			continue
		}
		if _, dup := seen[b.Gate]; dup {
			continue
		}
		seen[b.Gate] = struct{}{}
		if err := walkGateSpec("batches", b.Gate, fn); err != nil {
			return err
		}
	}
	for _, b := range p.Batches {
		for _, s := range b.Steps {
			if err := walkGateSpec("step:"+s.Name, s.Gate, fn); err != nil {
				return err
			}
		}
	}
	return nil
}

// walkGateSpec feeds one declaration block's checks to fn, mapping slot to phase.
// Batch-timing checks run after EVERY batch completes, so they register under the
// post-batch phase.
func walkGateSpec(prefix string, spec *dsl.GateSpec, fn func(name string, phase verify.GatePhase, c *dsl.GateCheck) error) error {
	if spec == nil {
		return nil
	}
	for _, slot := range []struct {
		kind  string
		phase verify.GatePhase
		items []dsl.GateCheck
	}{
		{"pre", verify.PhasePreApply, spec.Pre},
		{"batch", verify.PhasePostBatch, spec.Batch},
		{"post", verify.PhasePostApply, spec.Post},
	} {
		for i := range slot.items {
			name := fmt.Sprintf("%s:%s:%d", prefix, slot.kind, i)
			if err := fn(name, slot.phase, &slot.items[i]); err != nil {
				return err
			}
		}
	}
	return nil
}

// materializeStepGates registers a runnable verify.Gate for every inline
// gate declaration in the plan. Names are deterministic per step, timing
// and index ("step:<name>:pre:<i>", "step:<name>:batch:<i>"), so re-running
// a runner over the same plan overwrites rather than duplicates.
func materializeStepGates(verifier *verify.GateManager, p *plan.Plan, rt GateRuntime) error {
	return walkPlanGates(p, func(name string, phase verify.GatePhase, c *dsl.GateCheck) error {
		g, err := rt.gateFromCheck(name, phase, c)
		if err != nil {
			return err
		}
		verifier.Register(g)
		return nil
	})
}

// executabilityProblem reports why this check cannot run as declared here, or
// nil when it can. Two causes share one judge deliberately: a missing deployment
// capability (slo needs a PromQL endpoint, human needs an approval transport),
// and a declaration the engine can never honour (slo outside post_batch).
//
// The judge is shared with the plan-time refusal so that "executable here" has
// exactly one definition. Without it, a workflow declaring a human or slo check
// compiles and plans, then dies at the first phase that materialises the gate —
// after earlier batches already modified the targets. A declared gate must never
// silently pass, and it must not be discovered unexecutable mid-flight either.
func (rt GateRuntime) executabilityProblem(name string, phase verify.GatePhase, c *dsl.GateCheck) error {
	switch c.Type {
	case "slo":
		// Without a Prometheus endpoint there is no honest way to evaluate an
		// SLO threshold, so refuse instead of registering a gate that would guess.
		if rt.PrometheusURL == "" {
			return fmt.Errorf("slo gate %q requires verify.prometheus_url configuration", name)
		}
		// The phase rule belongs to the same judge because it is the other way a
		// declaration cannot be honoured: an slo check anywhere but post_batch
		// registers a gate RunPhase never visits, i.e. a silent skip that reads as
		// a passing verification. It is also pure declaration geometry — knowable
		// at plan time with no deployment knowledge at all.
		if phase != verify.PhasePostBatch {
			// The remediation must name something that works. It did not use to:
			// convertGate filed every declaration in GateSpec.Post, so telling an
			// operator to move a check to post_batch sent them editing YAML for no
			// effect. Position routing is fixed (internal/dsl/gate_position.go), so
			// the advice can now be the truth.
			return fmt.Errorf("gate %q: slo checks execute in the post_batch phase only, got phase %q"+
				" — declare it with position: post_batch (on a step verify block, on"+
				" batches.gate, or in a gates[] entry)", name, phase)
		}
	case "human":
		// A human gate without an approver transport can only block forever
		// or fabricate a pass; neither is acceptable, so require the wiring.
		if rt.Approver == nil {
			return fmt.Errorf("human gate %q requires an approver (supply GateRuntime.Approver via WithGateRuntime)", name)
		}
	}
	return nil
}

// ErrGateNotExecutable reports that a plan declares a verification gate this
// process has no transport for: a human check with no approver wired, or an slo
// check with no Prometheus endpoint.
//
// It is a sentinel in the package that owns the executability judgement so both
// halve of the wiring can name it — internal/wiring refuses the plan, and the
// gRPC layer maps the refusal to a client-actionable FailedPrecondition instead
// of letting it surface as an Internal fault. A refusal nobody can see the class
// of is a refusal that gets "fixed" by widening the error match.
var ErrGateNotExecutable = errors.New("engine: declared verification gate is not executable in this deployment")

// PlanGateBlockers returns one message per inline gate declaration the given
// runtime cannot execute. Callers use it to refuse a plan up front rather than
// let the first gate phase fail after targets were already changed.
func PlanGateBlockers(p *plan.Plan, rt GateRuntime) []string {
	var out []string
	//nolint:errcheck // the sink below never returns an error; walkPlanGates' error is unused by construction
	walkPlanGates(p, func(name string, phase verify.GatePhase, c *dsl.GateCheck) error {
		if err := rt.executabilityProblem(name, phase, c); err != nil {
			out = append(out, err.Error())
		}
		return nil
	})
	return out
}

// gateFromCheck compiles one dsl.GateCheck into a verify.Gate for the given
// phase. Unknown check types, invalid params and missing GateRuntime
// dependencies fail materialisation so that an unexecutable declaration
// surfaces as an explicit run failure instead of a silent pass.
func (rt GateRuntime) gateFromCheck(name string, phase verify.GatePhase, c *dsl.GateCheck) (verify.Gate, error) {
	if c == nil {
		return nil, fmt.Errorf("gate %q: nil check", name)
	}
	if err := rt.executabilityProblem(name, phase, c); err != nil {
		return nil, err
	}
	switch c.Type {
	case "cmd":
		opts := []verify.CommandGateOption{verify.WithExpectedExit(c.ExpectExit)}
		if c.ExpectStdout != "" {
			opts = append(opts, verify.WithExpectedStdout(c.ExpectStdout))
		}
		if c.Timeout != "" {
			if d, err := time.ParseDuration(c.Timeout); err == nil && d > 0 {
				opts = append(opts, verify.WithCommandTimeout(d))
			}
		}
		return verify.NewCommandGate(name, phase, c.Command, opts...), nil

	case "probe":
		// Probes are entirely self-describing: their Params carry every knob
		// (kind/mode/url/host_port/expect_status/script/...) and the gate
		// validates them fail-closed at construction time. No runtime
		// dependency. The legacy Timeout string (when present) seeds
		// timeout_seconds unless Params already set it.
		params := c.Params
		if params == nil {
			params = map[string]any{}
		}
		if _, ok := params["timeout_seconds"]; !ok && c.Timeout != "" {
			if d, err := time.ParseDuration(c.Timeout); err == nil && d > 0 {
				params["timeout_seconds"] = int(d.Seconds())
			}
		}
		return verify.NewProbeGate(name, phase, params), nil

	case "slo":
		return rt.sloGateFromCheck(name, c)

	case "human":
		// executabilityProblem already required the approver and gateFromCheck
		// checked it before dispatching here, so a transport exists. HumanGate
		// validates its own params strictly; hand the mapping over verbatim so
		// its schema stays the single source of truth.
		return verify.NewHumanGate(name, string(phase), rt.Approver, c.Params), nil

	default:
		return nil, fmt.Errorf("gate %q: check type %q is not executable by the engine (supported: cmd|probe|slo|human)", name, c.Type)
	}
}

// sloGateFromCheck compiles a "slo" GateCheck. It is split out of
// gateFromCheck purely to keep that dispatcher readable.
//
// It takes no phase: the endpoint requirement and the post_batch-only rule are
// both enforced by executabilityProblem before it is reached, and SLOGate itself
// carries no phase — the phase comes from the slot the gate was registered under.
func (rt GateRuntime) sloGateFromCheck(name string, c *dsl.GateCheck) (verify.Gate, error) {

	pp, err := parseStrictParams(c.Params, map[string]string{
		"query":           "string",
		"threshold":       "float",
		"comparison":      "string",
		"timeout_seconds": "int",
	})
	if err != nil {
		return nil, fmt.Errorf("gate %q: %w", name, err)
	}

	// The PromQL expression comes from the classic query field; the
	// params mapping may override it.
	query := c.Command
	if q, ok := pp["query"].(string); ok && q != "" {
		query = q
	}
	if query == "" {
		return nil, fmt.Errorf("slo gate %q requires a PromQL query (the check's query field or the \"query\" param)", name)
	}

	rawThreshold, ok := pp["threshold"]
	if !ok {
		return nil, fmt.Errorf("slo gate %q requires a numeric \"threshold\" param", name)
	}
	threshold := rawThreshold.(float64)

	// Comparison operator. The documented LEVEELang spellings are
	// lt|gt|lte|gte (default lte); the historical le/ge/eq spellings are
	// accepted as aliases. Anything else is a hard error — the underlying
	// SLOGate would silently coerce unknown operators to lt, which must
	// never flip a threshold's direction unnoticed.
	comparison := "le"
	if cmp, ok := pp["comparison"].(string); ok && cmp != "" {
		switch cmp {
		case "lt":
			comparison = "lt"
		case "gt":
			comparison = "gt"
		case "lte", "le":
			comparison = "le"
		case "gte", "ge":
			comparison = "ge"
		case "eq":
			comparison = "eq"
		default:
			return nil, fmt.Errorf("slo gate %q: unknown comparison %q (supported: lt|gt|lte|gte, plus aliases le|ge|eq)", name, cmp)
		}
	}

	timeout := 5 * time.Second // documented default for slo timeout_seconds
	if t, ok := pp["timeout_seconds"].(int); ok && t > 0 {
		timeout = time.Duration(t) * time.Second
	}

	return verify.NewSLOGate(name, query, threshold, comparison,
		verify.WithSLOSource(rt.PrometheusURL),
		verify.WithSLOTimeout(timeout),
	), nil
}

// parseStrictParams validates a free-form params mapping against an allowed
// key/type schema and returns a normalised copy. allowed maps each permitted
// key to one of "string", "int", "float" or "bool".
//
// Loose typing: YAML decoding yields int / float64 / bool / string scalars,
// so integer-valued keys additionally accept int64 / uint64 / integral
// float64 values, and float keys accept any of those numeric shapes. Values
// are normalised (ints become int, floats become float64) so callers can
// assert types without further coercion.
//
// An unknown key aborts with an error listing the valid keys, keeping
// misconfiguration loud rather than silently ignored.
func parseStrictParams(params map[string]any, allowed map[string]string) (map[string]any, error) {
	out := make(map[string]any, len(params))
	for k, v := range params {
		typ, ok := allowed[k]
		if !ok {
			valid := make([]string, 0, len(allowed))
			for vk := range allowed {
				valid = append(valid, vk)
			}
			sort.Strings(valid)
			return nil, fmt.Errorf("param %q is not supported (valid keys: %s)", k, strings.Join(valid, ", "))
		}
		switch typ {
		case "string":
			s, ok := v.(string)
			if !ok {
				return nil, fmt.Errorf("param %q must be a string, got %T", k, v)
			}
			out[k] = s
		case "int":
			n, err := looseInt(v)
			if err != nil {
				return nil, fmt.Errorf("param %q: %w", k, err)
			}
			out[k] = n
		case "float":
			f, err := looseFloat(v)
			if err != nil {
				return nil, fmt.Errorf("param %q: %w", k, err)
			}
			out[k] = f
		case "bool":
			b, ok := v.(bool)
			if !ok {
				return nil, fmt.Errorf("param %q must be a boolean, got %T", k, v)
			}
			out[k] = b
		default:
			return nil, fmt.Errorf("internal error: param %q declares unknown type %q", k, typ)
		}
	}
	return out, nil
}

// looseInt coerces the plausible numeric spellings to int.
func looseInt(v any) (int, error) {
	switch n := v.(type) {
	case int:
		return n, nil
	case int64:
		return int(n), nil
	case uint64:
		return int(n), nil
	case float64:
		if n != float64(int(n)) {
			return 0, fmt.Errorf("must be an integer, got %v", n)
		}
		return int(n), nil
	default:
		return 0, fmt.Errorf("must be an integer, got %T", v)
	}
}

// looseFloat coerces the plausible numeric spellings to float64.
func looseFloat(v any) (float64, error) {
	switch n := v.(type) {
	case int:
		return float64(n), nil
	case int64:
		return float64(n), nil
	case uint64:
		return float64(n), nil
	case float64:
		return n, nil
	default:
		return 0, fmt.Errorf("must be a number, got %T", v)
	}
}
