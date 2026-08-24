// step_gates.go — materialisation of inline workflow gate declarations into
// runnable verify.Gate implementations.
//
// A compiled plan carries per-step gate DECLARATIONS (plan.PlanStep.Gate,
// a *dsl.GateSpec). The ClosureRunner's verifier only executes gates that
// are REGISTERED with it, so before a run starts every declaration is
// compiled into a named verify.Gate and registered. From that point the
// existing RunPhase machinery executes them like any other gate.
//
// Fail-closed policy: check types the engine cannot execute (slo, probe,
// human) abort materialisation with an error instead of being silently
// skipped — a declared-but-unexecutable gate must never masquerade as a
// passing one. Command gates execute against GateInput.Channel; when the
// caller supplies no channel the command gate reports Passed=false with a
// "missing channel" reason, which fails the phase honestly.

package engine

import (
	"fmt"
	"time"

	"github.com/nexus/levee/internal/dsl"
	"github.com/nexus/levee/internal/plan"
	"github.com/nexus/levee/internal/verify"
)

// materializeStepGates registers a runnable verify.Gate for every inline
// gate declaration in the plan. Names are deterministic per step, timing
// and index ("step:<name>:pre:<i>"), so re-running a runner over the same
// plan overwrites rather than duplicates.
func materializeStepGates(verifier *verify.GateManager, p *plan.Plan) error {
	for _, b := range p.Batches {
		for _, s := range b.Steps {
			if s.Gate == nil {
				continue
			}
			for i := range s.Gate.Pre {
				g, err := gateFromCheck(fmt.Sprintf("step:%s:pre:%d", s.Name, i), verify.PhasePreApply, &s.Gate.Pre[i])
				if err != nil {
					return err
				}
				verifier.Register(g)
			}
			for i := range s.Gate.Post {
				g, err := gateFromCheck(fmt.Sprintf("step:%s:post:%d", s.Name, i), verify.PhasePostApply, &s.Gate.Post[i])
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
// phase. Only command checks are executable today; everything else fails
// materialisation so that an unsupported declaration surfaces as an explicit
// run failure instead of a silent pass.
func gateFromCheck(name string, phase verify.GatePhase, c *dsl.GateCheck) (verify.Gate, error) {
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
	default:
		return nil, fmt.Errorf("gate %q: check type %q is not executable by the engine (supported: cmd)", name, c.Type)
	}
}
