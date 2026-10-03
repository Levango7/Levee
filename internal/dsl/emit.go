package dsl

// emit.go renders a Workflow AST back into LEVEELang YAML — the exact shape
// the parser accepts. It exists for tooling that TRANSLATES into LEVEELang
// (the Ansible compatibility importer being the first caller): an operator
// can review, version and compile the translated workflow through the
// standard pipeline instead of an opaque AST dump.
//
// Fail-closed by construction: every AST field this emitter cannot express
// faithfully makes MarshalWorkflow return an error naming the field. Silent
// dropping would be the worst outcome for a translation tool — the emitted
// workflow would look complete while missing a rollback, an approval gate or
// a snapshot declaration. Extend the emitter (and the round-trip test) when
// a caller needs more; never weaken this to "skip what we don't know".

import (
	"fmt"
	"sort"

	"gopkg.in/yaml.v3"
)

// MarshalWorkflow renders wf as LEVEELang YAML. See the file comment for the
// fail-closed contract: unsupported non-empty fields return an error rather
// than being dropped.
func MarshalWorkflow(wf *Workflow) ([]byte, error) {
	if wf == nil {
		return nil, fmt.Errorf("dsl: marshal workflow: nil workflow")
	}
	if err := checkUnemittable(wf); err != nil {
		return nil, err
	}

	out := map[string]any{}
	emitMeta(out, &wf.Meta)
	if targets := emitTargets(wf.Targets); len(targets) > 0 {
		out["targets"] = targets
	}
	if window := emitWindow(wf.Window); len(window) > 0 {
		out["window"] = window
	}
	if batches := emitBatches(wf.Batches); len(batches) > 0 {
		out["batches"] = batches
	}
	steps, err := emitSteps(wf.Steps)
	if err != nil {
		return nil, err
	}
	if len(steps) > 0 {
		out["steps"] = steps
	}

	data, err := yaml.Marshal(out)
	if err != nil {
		return nil, fmt.Errorf("dsl: marshal workflow: %w", err)
	}
	return data, nil
}

// checkUnemittable refuses every non-empty field the emitter cannot express
// faithfully. Field-by-field so the error names exactly what is missing.
func checkUnemittable(wf *Workflow) error {
	switch {
	case len(wf.Inputs) > 0:
		return fmt.Errorf("dsl: marshal workflow: %d input parameter(s) cannot be emitted yet", len(wf.Inputs))
	case wf.Rollback != nil:
		return fmt.Errorf("dsl: marshal workflow: workflow-level rollback cannot be emitted yet")
	case wf.Approval != nil:
		return fmt.Errorf("dsl: marshal workflow: workflow-level approval cannot be emitted yet")
	case wf.Gate != nil:
		return fmt.Errorf("dsl: marshal workflow: workflow-level gates cannot be emitted yet")
	case wf.Snapshot != nil:
		return fmt.Errorf("dsl: marshal workflow: run snapshot declarations cannot be emitted yet")
	case wf.Batches.Gate != nil:
		return fmt.Errorf("dsl: marshal workflow: batch gate cannot be emitted yet")
	}
	return nil
}

func emitMeta(out map[string]any, meta *WorkflowMeta) {
	if meta.Name != "" {
		out["name"] = meta.Name
	}
	if meta.Version != "" {
		out["version"] = meta.Version
	}
	if meta.Description != "" {
		out["description"] = meta.Description
	}
}

func emitTargets(groups []TargetGroup) []map[string]any {
	targets := make([]map[string]any, 0, len(groups))
	for _, tg := range groups {
		m := map[string]any{}
		if tg.Name != "" {
			m["name"] = tg.Name
		}
		if tg.Type != "" {
			m["type"] = tg.Type
		}
		if len(tg.Hosts) > 0 {
			m["hosts"] = tg.Hosts
		}
		if tg.Query != "" {
			m["query"] = tg.Query
		}
		if tg.MinCount != 0 {
			m["min_count"] = tg.MinCount
		}
		if tg.MaxCount != 0 {
			m["max_count"] = tg.MaxCount
		}
		targets = append(targets, m)
	}
	return targets
}

func emitWindow(w ChangeWindow) map[string]any {
	m := map[string]any{}
	if w.Start != "" {
		m["start"] = w.Start
	}
	if w.End != "" {
		m["end"] = w.End
	}
	if w.Timezone != "" {
		m["timezone"] = w.Timezone
	}
	if w.MaxConcurrency != 0 {
		m["max_concurrency"] = w.MaxConcurrency
	}
	if len(w.Days) > 0 {
		m["days"] = w.Days
	}
	if len(m) == 0 {
		return nil
	}
	return m
}

func emitBatches(b BatchConfig) map[string]any {
	m := map[string]any{}
	if b.Strategy != "" {
		m["strategy"] = b.Strategy
	}
	if b.MaxConcurrency != 0 {
		m["max_concurrency"] = b.MaxConcurrency
	}
	if b.Serial {
		m["serial"] = true
	}
	if len(b.Steps) > 0 {
		m["steps"] = b.Steps
	}
	if len(m) == 0 {
		return nil
	}
	return m
}

func emitSteps(steps []Step) ([]map[string]any, error) {
	out := make([]map[string]any, 0, len(steps))
	for i, s := range steps {
		switch {
		case s.Rollback != nil:
			return nil, fmt.Errorf("dsl: marshal workflow: step %d (%s) carries a rollback declaration that cannot be emitted yet", i, s.Name)
		case s.Approval != nil:
			return nil, fmt.Errorf("dsl: marshal workflow: step %d (%s) carries an approval declaration that cannot be emitted yet", i, s.Name)
		case s.Gate != nil:
			return nil, fmt.Errorf("dsl: marshal workflow: step %d (%s) carries a gate declaration that cannot be emitted yet", i, s.Name)
		}
		m := map[string]any{}
		if s.Name != "" {
			m["name"] = s.Name
		}
		action := s.Module
		if s.Action != "" {
			action = s.Module + "." + s.Action
		}
		if action == "" {
			return nil, fmt.Errorf("dsl: marshal workflow: step %d has neither module nor action", i)
		}
		m["action"] = action
		if len(s.Args) > 0 {
			m["args"] = sortedArgs(s.Args)
		}
		if s.Idempotent {
			m["idempotent"] = true
		}
		if s.Irreversible {
			m["irreversible"] = true
		}
		if s.RequiresReboot {
			m["requires_reboot"] = true
		}
		if len(s.DependsOn) > 0 {
			m["depends_on"] = s.DependsOn
		}
		out = append(out, m)
	}
	return out, nil
}

// sortedArgs copies args into a yaml.Node map with sorted keys so the
// emitted YAML is deterministic (Go map iteration is not) — a translation
// tool's output must be stable enough to diff in version control.
func sortedArgs(args map[string]any) *yaml.Node {
	node := &yaml.Node{Kind: yaml.MappingNode}
	keys := make([]string, 0, len(args))
	for k := range args {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		keyNode := &yaml.Node{Kind: yaml.ScalarNode, Value: k}
		valNode := &yaml.Node{}
		_ = valNode.Encode(args[k])
		node.Content = append(node.Content, keyNode, valNode)
	}
	return node
}
