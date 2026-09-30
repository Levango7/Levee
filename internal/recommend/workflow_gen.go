package recommend

// workflow_gen.go implements the WorkflowGenerator that produces LEVEELang
// workflow YAML drafts from fix proposals, knowledge-base matches and LLM
// responses. The generated YAML follows the LEVEELang schema:
//
//	name: auto-fix-<target>-<timestamp>
//	description: <from recommendation>
//	target:
//	  hosts:
//	    - <target>
//	window:
//	  duration: 30m
//	  approval: standard
//	batches:
//	  - name: batch-1
//	    targets: all
//	    steps:
//	      - name: <step-name>
//	        module: <module>
//	        action: <action>
//	        args:
//	          <key>: <value>
//	rollback:
//	  - name: rollback-<step-name>
//	    module: <module>
//	    action: <reverse-action>
//
// The generator is intentionally pure: it does not call the LLM or the
// knowledge base. It is safe for concurrent use because it carries no
// mutable state.

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/nexus/levee/internal/log"
)

// --- Sentinel errors --------------------------------------------------------

var (
	// ErrEmptyTarget is returned by Generate when the target is empty.
	ErrEmptyTarget = errors.New("recommend: empty target")
	// ErrNoSteps is returned when no fix steps can be derived.
	ErrNoSteps = errors.New("recommend: no steps")
)

// --- FixStep ----------------------------------------------------------------

// FixStep represents a single fix action to be encoded as a LEVEELang step.
type FixStep struct {
	Name        string            `json:"name"`
	Module      string            `json:"module"`
	Action      string            `json:"action"`
	Target      string            `json:"target"`
	Args        map[string]string `json:"args"`
	Description string            `json:"description"`
}

// --- WorkflowGenerator ------------------------------------------------------

// WorkflowGenerator generates LEVEELang workflow YAML drafts. It is safe for
// concurrent use.
type WorkflowGenerator struct {
	log *slog.Logger
}

// NewWorkflowGenerator creates a WorkflowGenerator wired to the package
// singleton logger.
func NewWorkflowGenerator() *WorkflowGenerator {
	return &WorkflowGenerator{log: log.With("component", "workflow_gen")}
}

// Generate generates a LEVEELang workflow YAML from a list of fix steps.
// It returns ErrEmptyTarget when target is empty and ErrNoSteps when steps
// is empty.
func (g *WorkflowGenerator) Generate(target string, steps []FixStep) (string, error) {
	if target == "" {
		return "", ErrEmptyTarget
	}
	if len(steps) == 0 {
		return "", ErrNoSteps
	}
	return g.generate(target, steps), nil
}

// generate builds the YAML string. The caller has already validated inputs.
//
// The emitted document must satisfy dsl.NewParser().ParseBytes — that parser
// is the ONLY way a workflow can reach the governed path (PlanChange ->
// plan.NewGenerator -> plan_hash -> approval), so a draft that does not
// parse here is a draft that can never be applied. The shapes below are
// pinned by TestGenerate_ProducesParseableWorkflow.
//
//   - `batches` is a MAP (strategy / steps / max_concurrency), not a list of
//     named batch objects. Batch division is the generator's job, not the
//     document's.
//   - `rollback` is declared PER STEP (yamlStepRaw.rollback), because that is
//     what the compensation ledger attributes compensations to. A top-level
//     rollback list has nowhere to attach.
//   - `target` uses `type` + `hosts`. Note that `target.query` is validated
//     for non-emptiness but has NO resolver behind it: the executed target
//     set comes from PlanChangeRequest.target_hosts, not from this document.
//
// An earlier version emitted a private dialect (named batch objects, a
// top-level rollback list, an approval key under window) that no workflow in
// the tree accepts; internal/autoplanner existed to hand-parse that broken
// output and has since been removed. Do not reintroduce a private dialect
// here — if the schema needs a new field, add it to internal/dsl and
// regenerate the spec.
func (g *WorkflowGenerator) generate(target string, steps []FixStep) string {
	var b strings.Builder
	name := fmt.Sprintf("auto-fix-%s-%d", sanitizeName(target), time.Now().Unix())
	fmt.Fprintf(&b, "name: %s\n", name)
	fmt.Fprintf(&b, "description: %s\n", yamlScalar(fmt.Sprintf("Auto-generated fix workflow for %s", target)))
	fmt.Fprintf(&b, "target:\n")
	fmt.Fprintf(&b, "  type: host\n")
	fmt.Fprintf(&b, "  hosts:\n")
	fmt.Fprintf(&b, "    - %s\n", yamlScalar(target))
	fmt.Fprintf(&b, "batches:\n")
	fmt.Fprintf(&b, "  strategy: serial\n")
	fmt.Fprintf(&b, "steps:\n")
	for _, s := range steps {
		writeStep(&b, s)
	}
	return b.String()
}

// writeStep writes a single step entry, including its rollback declaration.
//
// A step with no derivable reverse action gets NO rollback block at all,
// rather than a "noop" one. A noop rollback is the worst of both worlds: the
// plan looks covered (rollback != nil, so the compensation-gap check passes)
// while reversing nothing. Omitting it makes the step an honest compensation
// gap, which the verdict reports — see internal/rollback: "no rollback spec".
func writeStep(b *strings.Builder, s FixStep) {
	name := orDefault(s.Name, "unnamed-step")
	fmt.Fprintf(b, "  - name: %s\n", yamlScalar(name))
	fmt.Fprintf(b, "    module: %s\n", yamlScalar(orDefault(s.Module, "shell")))
	fmt.Fprintf(b, "    action: %s\n", yamlScalar(orDefault(s.Action, "run")))
	if s.Description != "" {
		fmt.Fprintf(b, "    description: %s\n", yamlScalar(s.Description))
	}
	if len(s.Args) > 0 {
		fmt.Fprintf(b, "    args:\n")
		for _, k := range sortedKeys(s.Args) {
			fmt.Fprintf(b, "      %s: %s\n", k, yamlScalar(s.Args[k]))
		}
	} else {
		fmt.Fprintf(b, "    args: {}\n")
	}
	writeRollback(b, s)
}

// writeRollback writes the step's rollback block, or nothing when no reverse
// action is derivable. See writeStep for why a noop is never emitted.
func writeRollback(b *strings.Builder, s FixStep) {
	rev, ok := reverseAction(s.Action)
	if !ok {
		return
	}
	fmt.Fprintf(b, "    rollback:\n")
	fmt.Fprintf(b, "      strategy: undo-action\n")
	fmt.Fprintf(b, "      steps:\n")
	fmt.Fprintf(b, "        - name: %s\n", yamlScalar("rollback-"+orDefault(s.Name, "unnamed-step")))
	fmt.Fprintf(b, "          module: %s\n", yamlScalar(orDefault(s.Module, "shell")))
	fmt.Fprintf(b, "          action: %s\n", yamlScalar(rev))
}

// reverseAction returns the reverse action for a given action and whether one
// could be derived at all. An unknown action returns ok=false: inventing a
// noop would mask a missing compensation as a present one.
func reverseAction(action string) (string, bool) {
	switch strings.ToLower(action) {
	case "restart":
		return "restart", true
	case "stop":
		return "start", true
	case "start":
		return "stop", true
	case "remove", "delete":
		return "restore", true
	case "copy", "write":
		return "restore", true
	case "install":
		return "uninstall", true
	case "uninstall":
		return "install", true
	default:
		return "", false
	}
}

// GenerateFromMatch generates a workflow from a knowledge base match. The
// match's Source is introspected (HistoricalIncident, Runbook or FixPattern)
// and converted into FixSteps.
func (g *WorkflowGenerator) GenerateFromMatch(target string, match *Match) (string, error) {
	if target == "" {
		return "", ErrEmptyTarget
	}
	if match == nil {
		return "", ErrNoSteps
	}
	steps := stepsFromMatch(match)
	if len(steps) == 0 {
		return "", ErrNoSteps
	}
	return g.generate(target, steps), nil
}

// stepsFromMatch extracts FixSteps from a Match based on its Type.
func stepsFromMatch(match *Match) []FixStep {
	if match == nil {
		return nil
	}
	switch match.Type {
	case MatchTypeIncident:
		inc, ok := match.Source.(HistoricalIncident)
		if !ok {
			return nil
		}
		return stepsFromIncident(inc)
	case MatchTypeRunbook:
		rb, ok := match.Source.(Runbook)
		if !ok {
			return nil
		}
		return stepsFromRunbook(rb)
	case MatchTypePattern:
		p, ok := match.Source.(FixPattern)
		if !ok {
			return nil
		}
		return stepsFromPattern(p)
	}
	return nil
}

// stepsFromIncident builds steps from a HistoricalIncident using tag-driven
// heuristics.
func stepsFromIncident(inc HistoricalIncident) []FixStep {
	tags := lowerTags(inc.Tags)
	if containsAny(tags, "java", "oom", "memory", "jvm") {
		return []FixStep{{
			Name:        "restart-service",
			Module:      "svc",
			Action:      "restart",
			Target:      "java-app",
			Description: inc.Resolution,
			Args:        map[string]string{"service": "java-app"},
		}}
	}
	if containsAny(tags, "disk", "storage", "logs") {
		return []FixStep{{
			Name:        "clean-disk",
			Module:      "shell",
			Action:      "run",
			Target:      "/var/log",
			Description: inc.Resolution,
			Args:        map[string]string{"cmd": "find /var/log -name '*.gz' -mtime +7 -delete"},
		}}
	}
	if containsAny(tags, "network", "partition") {
		return []FixStep{{
			Name:        "restart-network",
			Module:      "svc",
			Action:      "restart",
			Target:      "networking",
			Description: inc.Resolution,
			Args:        map[string]string{"service": "networking"},
		}}
	}
	if containsAny(tags, "config", "drift") {
		return []FixStep{{
			Name:        "rollback-config",
			Module:      "file",
			Action:      "copy",
			Target:      "last-good",
			Description: inc.Resolution,
			Args:        map[string]string{"baseline": "last-good"},
		}}
	}
	return []FixStep{{
		Name:        "apply-resolution",
		Module:      "shell",
		Action:      "run",
		Description: inc.Resolution,
		Args:        map[string]string{"cmd": "echo " + yamlScalar(inc.Resolution)},
	}}
}

// stepsFromRunbook builds steps from a Runbook's step list.
func stepsFromRunbook(rb Runbook) []FixStep {
	steps := make([]FixStep, 0, len(rb.Steps))
	for _, s := range rb.Steps {
		step := FixStep{
			Name:        s.Action,
			Module:      "shell",
			Action:      "run",
			Description: s.Description,
		}
		if s.Command != "" {
			step.Args = map[string]string{"cmd": s.Command}
		}
		steps = append(steps, step)
	}
	return steps
}

// stepsFromPattern builds steps from a FixPattern using tag-driven heuristics.
func stepsFromPattern(p FixPattern) []FixStep {
	tags := lowerTags(p.Tags)
	if containsAny(tags, "java", "oom", "memory") {
		return []FixStep{{
			Name:        "restart-service",
			Module:      "svc",
			Action:      "restart",
			Target:      "java-app",
			Description: p.Fix,
			Args:        map[string]string{"service": "java-app"},
		}}
	}
	if containsAny(tags, "disk", "storage", "logs") {
		return []FixStep{{
			Name:        "clean-disk",
			Module:      "shell",
			Action:      "run",
			Target:      "/var/log",
			Description: p.Fix,
			Args:        map[string]string{"cmd": "find /var/log -name '*.gz' -mtime +7 -delete"},
		}}
	}
	if containsAny(tags, "database", "pool", "connection") {
		return []FixStep{{
			Name:        "tune-pool",
			Module:      "file",
			Action:      "copy",
			Description: p.Fix,
			Args:        map[string]string{"pool": "primary"},
		}}
	}
	return []FixStep{{
		Name:        "apply-fix",
		Module:      "shell",
		Action:      "run",
		Description: p.Fix,
	}}
}

// GenerateFromLLM generates a workflow from an LLM response. The response is
// expected to be a JSON object with a "steps" array; when parsing fails or
// yields no steps the raw text is wrapped in a single review step so the
// operator can still inspect it.
func (g *WorkflowGenerator) GenerateFromLLM(target string, llmResponse string) (string, error) {
	if target == "" {
		return "", ErrEmptyTarget
	}
	steps := parseLLMSteps(llmResponse)
	if len(steps) == 0 {
		steps = []FixStep{{
			Name:        "review-llm-output",
			Module:      "shell",
			Action:      "run",
			Description: "Review the LLM-generated fix proposal",
			Args:        map[string]string{"proposal": llmResponse},
		}}
	}
	return g.generate(target, steps), nil
}

// parseLLMSteps attempts to parse the LLM response as a JSON object with a
// "steps" field. Returns nil on any failure.
func parseLLMSteps(response string) []FixStep {
	response = strings.TrimSpace(response)
	if response == "" {
		return nil
	}
	var proposal struct {
		Steps []FixStep `json:"steps"`
	}
	if err := json.Unmarshal([]byte(response), &proposal); err != nil {
		return nil
	}
	if len(proposal.Steps) == 0 {
		return nil
	}
	for i := range proposal.Steps {
		if proposal.Steps[i].Module == "" {
			proposal.Steps[i].Module = "shell"
		}
		if proposal.Steps[i].Action == "" {
			proposal.Steps[i].Action = "run"
		}
	}
	return proposal.Steps
}

// --- YAML helpers -----------------------------------------------------------

// orDefault returns s when non-empty, otherwise def.
func orDefault(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

// sanitizeName replaces characters that are unsafe in a LEVEELang workflow
// name with hyphens.
func sanitizeName(s string) string {
	if s == "" {
		return "unknown"
	}
	var out strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-' || r == '.':
			out.WriteRune(r)
		default:
			out.WriteRune('-')
		}
	}
	return out.String()
}

// yamlScalar renders a string as a YAML scalar, quoting it when it contains
// characters that would otherwise be misinterpreted by the YAML parser.
func yamlScalar(s string) string {
	if s == "" {
		return "\"\""
	}
	if strings.ContainsAny(s, ":#{}[]&*!|>'\"\\\n") {
		return strconv.Quote(s)
	}
	return s
}

// sortedKeys returns the keys of m in lexical order.
func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// lowerTags returns a lower-cased copy of tags.
func lowerTags(tags []string) []string {
	out := make([]string, len(tags))
	for i, t := range tags {
		out[i] = strings.ToLower(t)
	}
	return out
}

// containsAny reports whether haystack contains any of the needles.
func containsAny(haystack []string, needles ...string) bool {
	set := make(map[string]struct{}, len(haystack))
	for _, h := range haystack {
		set[h] = struct{}{}
	}
	for _, n := range needles {
		if _, ok := set[n]; ok {
			return true
		}
	}
	return false
}
