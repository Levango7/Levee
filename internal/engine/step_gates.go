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
}

// materializeStepGates registers a runnable verify.Gate for every inline
// gate declaration in the plan. Names are deterministic per step, timing
// and index ("step:<name>:pre:<i>", "step:<name>:batch:<i>"), so re-running
// a runner over the same plan overwrites rather than duplicates.
func materializeStepGates(verifier *verify.GateManager, p *plan.Plan, rt GateRuntime) error {
	for _, b := range p.Batches {
		for _, s := range b.Steps {
			if s.Gate == nil {
				continue
			}
			for i := range s.Gate.Pre {
				g, err := gateFromCheck(fmt.Sprintf("step:%s:pre:%d", s.Name, i), verify.PhasePreApply, &s.Gate.Pre[i], rt)
				if err != nil {
					return err
				}
				verifier.Register(g)
			}
			// Batch-timing checks run after EVERY batch completes, so they
			// register under the post-batch phase.
			for i := range s.Gate.Batch {
				g, err := gateFromCheck(fmt.Sprintf("step:%s:batch:%d", s.Name, i), verify.PhasePostBatch, &s.Gate.Batch[i], rt)
				if err != nil {
					return err
				}
				verifier.Register(g)
			}
			for i := range s.Gate.Post {
				g, err := gateFromCheck(fmt.Sprintf("step:%s:post:%d", s.Name, i), verify.PhasePostApply, &s.Gate.Post[i], rt)
				if err != nil {
					return err
				}
				verifier.Register(g)
			}
		}
	}
	return nil
}

// gateFromCheck compiles one dsl.GateCheck into a verify.Gate for the given
// phase. Unknown check types, invalid params and missing GateRuntime
// dependencies fail materialisation so that an unexecutable declaration
// surfaces as an explicit run failure instead of a silent pass.
func gateFromCheck(name string, phase verify.GatePhase, c *dsl.GateCheck, rt GateRuntime) (verify.Gate, error) {
	if c == nil {
		return nil, fmt.Errorf("gate %q: nil check", name)
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
		// Without a Prometheus endpoint there is no honest way to evaluate an
		// SLO threshold, so materialisation aborts instead of registering a
		// gate that would guess.
		if rt.PrometheusURL == "" {
			return nil, fmt.Errorf("slo gate %q requires verify.prometheus_url configuration", name)
		}
		// The verify.SLOGate is bound to the post-batch phase; a declaration
		// asking for any other timing would register a gate RunPhase never
		// sees (silent skip), so reject it up front.
		if phase != verify.PhasePostBatch {
			return nil, fmt.Errorf("gate %q: slo checks execute in the post_batch phase only, got phase %q", name, phase)
		}

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

	case "human":
		// A human gate without an approver transport can only block forever
		// or fabricate a pass; neither is acceptable, so require the runtime
		// wiring up front.
		if rt.Approver == nil {
			return nil, fmt.Errorf("human gate %q requires an approver (supply GateRuntime.Approver via WithGateRuntime)", name)
		}
		// HumanGate validates its own params strictly; hand the mapping over
		// verbatim so its schema stays the single source of truth.
		return verify.NewHumanGate(name, string(phase), rt.Approver, c.Params), nil

	default:
		return nil, fmt.Errorf("gate %q: check type %q is not executable by the engine (supported: cmd|probe|slo|human)", name, c.Type)
	}
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
