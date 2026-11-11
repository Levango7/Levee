package verify

import (
	"context"
	"fmt"
	"strings"

	"github.com/nexus/levee/internal/k8spolicy"
)

// K8sPolicyGate refuses a change whose Kubernetes workload manifests violate
// the high-risk policy set (internal/k8spolicy), so a workload change is
// governed by the same approval chain as everything else LEVEE changes.
//
// Behaviour, in line with the rest of this package:
//   - A manifest that cannot be parsed is a REFUSAL, not a pass. The gate
//     cannot know what it failed to read, and "I could not check" must never
//     read as "it is fine".
//   - A critical finding blocks. Warnings do not block but are carried in the
//     message and in Details, so a passing gate still shows what it saw.
//   - The refusal says WHICH policy and WHERE, because a refusal the operator
//     cannot act on is a refusal that gets worked around.
//
// LIMITATIONS (deliberate):
//   - The gate evaluates manifests as given. It does not resolve a workload
//     that a change only mutates in place (e.g. a scale of an existing
//     Deployment) — the caller has to pass the resulting manifest.
//   - It evaluates workloads only; Services, ConfigMaps, CRDs and the rest are
//     not interpreted (see internal/k8spolicy's boundary note).
type K8sPolicyGate struct {
	name      string
	phase     GatePhase
	manifests []string
}

// NewK8sPolicyGate returns a gate that checks the given manifests. An empty
// phase means PhasePreApply (the moment where a refusal is cheapest).
func NewK8sPolicyGate(name string, phase GatePhase, manifests []string) *K8sPolicyGate {
	if phase == "" {
		phase = PhasePreApply
	}
	return &K8sPolicyGate{name: name, phase: phase, manifests: manifests}
}

// Name returns the gate's registry key.
func (g *K8sPolicyGate) Name() string { return g.name }

// Phase returns the phase this gate runs in.
func (g *K8sPolicyGate) Phase() GatePhase { return g.phase }

// Check evaluates the manifests and reports the verdict.
func (g *K8sPolicyGate) Check(_ context.Context, _ GateInput) (GateResult, error) {
	findings, err := k8spolicy.Evaluate(g.manifests)
	if err != nil {
		// Fail closed: an unreadable manifest is not a clean manifest.
		return GateResult{
			Passed: false,
			Message: fmt.Sprintf("k8s policy gate %q could not evaluate the manifests: %v",
				g.name, err),
			Details: map[string]any{
				"reason": "manifest_unparsable",
				"error":  err.Error(),
			},
		}, nil
	}

	critical := make([]k8spolicy.Finding, 0, len(findings))
	warnings := 0
	for _, f := range findings {
		if f.Severity == k8spolicy.SeverityCritical {
			critical = append(critical, f)
			continue
		}
		warnings++
	}

	if len(critical) > 0 {
		parts := make([]string, 0, 3)
		for i, f := range critical {
			if i == 3 {
				parts = append(parts, fmt.Sprintf("(+%d more)", len(critical)-3))
				break
			}
			parts = append(parts, fmt.Sprintf("%s at %s", f.PolicyID, f.Path))
		}
		return GateResult{
			Passed: false,
			Message: fmt.Sprintf("k8s policy gate %q found %d critical policy violation(s): %s",
				g.name, len(critical), strings.Join(parts, "; ")),
			Details: map[string]any{
				"findings":  findings,
				"critical":  len(critical),
				"warnings":  warnings,
				"gate_name": g.name,
			},
		}, nil
	}

	msg := fmt.Sprintf("k8s policy gate %q passed", g.name)
	if warnings > 0 {
		msg = fmt.Sprintf("k8s policy gate %q passed with %d warning(s)", g.name, warnings)
	}
	return GateResult{
		Passed:  true,
		Message: msg,
		Details: map[string]any{
			"findings":  findings,
			"critical":  0,
			"warnings":  warnings,
			"gate_name": g.name,
		},
	}, nil
}
