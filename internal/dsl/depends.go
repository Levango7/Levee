package dsl

// depends.go consumes the step-level `depends_on` declaration.
//
// Why this file exists: `depends_on` was parsed (parser.go), carried on the AST
// and IR, emitted back (emit.go) and used by shipped examples — but NOTHING
// consumed it. Steps execute serially in the order the plan carries, and the
// plan carried declaration order, so a workflow that declared its steps out of
// dependency order (e.g. `steps: [b (depends_on a), a]`) ran b before a and the
// dependency was silently violated. The error registry even described the
// withdrawn LE071 as "no depends_on in the language", which was not true of the
// parser.
//
// This file makes the declaration mean something with the smallest change the
// MVP execution model allows: a STABLE topological order (steps whose
// dependencies are already met keep their declaration order) plus validation of
// the two ways a dependency graph can be invalid — a reference to a step that
// does not exist, and a cycle. Full DAG execution (parallel steps,
// `{{step.x.output}}` references) remains the V1 item in docs/mvp-tasks.md
// N-18; this is the ordering half, not that feature.
//
// Ordering is a no-op for every shipped example: they were written in
// dependency order, and a stable sort leaves that byte-for-byte equivalent.

import "fmt"

// Dependency problems carry the same codes as the compile-time catalogue so the
// validator and the plan generator report them identically. They are declared
// here (like the other validator codes) rather than imported from
// internal/errors, which would couple the DSL to that package — the code
// literals are what internal/errors/catalogue_producer_test.go scans for.
const (
	// codeDependencyCycle is LE071 (re-registered 2026-10-31): the declared
	// dependencies cannot be satisfied by any order because they contain a
	// cycle.
	codeDependencyCycle = "LE071"
	// codeDependencyUnknown is LE104: depends_on names a step that does not
	// exist. A typo must not be a silent no-op.
	codeDependencyUnknown = "LE104"
)

// OrderStepsByDependsOn returns a copy of steps ordered so every step follows
// the steps it lists in DependsOn. Steps whose dependencies are already met
// keep their declaration order (the sort is stable), so a workflow already
// written in dependency order is returned unchanged.
//
// When the graph is invalid it returns the best-effort order it could build
// plus the problems found. Callers that must not proceed (the plan generator)
// reject on any problem; callers that report (the compile validator) surface
// them as ValidationError values.
func OrderStepsByDependsOn(steps []Step) ([]Step, []ValidationError) {
	if len(steps) == 0 {
		return nil, nil
	}

	// name -> declaration index. First occurrence wins; duplicate step names
	// are a separate defect (spec V23), not this function's to invent.
	index := make(map[string]int, len(steps))
	for i, s := range steps {
		if s.Name == "" {
			continue
		}
		if _, dup := index[s.Name]; !dup {
			index[s.Name] = i
		}
	}

	var problems []ValidationError

	// unmet[i] counts the (de-duplicated, existing) dependencies of step i that
	// have not been placed yet. An unknown reference is reported here and does
	// not add an edge — otherwise a typo would deadlock the whole workflow.
	unmet := make([]int, len(steps))
	dependents := make([][]int, len(steps))
	for i, s := range steps {
		seen := map[int]bool{}
		for _, dep := range s.DependsOn {
			j, ok := index[dep]
			if !ok {
				problems = append(problems, ValidationError{
					Code:    codeDependencyUnknown,
					Field:   fmt.Sprintf("steps[%d].depends_on", i),
					Message: fmt.Sprintf("step %q depends_on %q, which is not a step in this workflow", s.Name, dep),
				})
				continue
			}
			if j == i {
				problems = append(problems, ValidationError{
					Code:    codeDependencyCycle,
					Field:   fmt.Sprintf("steps[%d].depends_on", i),
					Message: fmt.Sprintf("step %q depends on itself", s.Name),
				})
				continue
			}
			if seen[j] {
				continue // the same dependency listed twice is one edge
			}
			seen[j] = true
			unmet[i]++
			dependents[j] = append(dependents[j], i)
		}
	}

	// Stable Kahn: repeatedly place the not-yet-placed step with the lowest
	// declaration index whose dependencies are all placed. "Lowest index"
	// (rather than any ready step) is what makes the sort stable.
	placed := make([]bool, len(steps))
	out := make([]Step, 0, len(steps))
	for len(out) < len(steps) {
		picked := -1
		for i := range steps {
			if placed[i] || unmet[i] != 0 {
				continue
			}
			picked = i
			break
		}
		if picked < 0 {
			break // every remaining step waits on another -> a cycle
		}
		placed[picked] = true
		out = append(out, steps[picked])
		for _, d := range dependents[picked] {
			unmet[d]--
		}
	}
	if len(out) < len(steps) {
		var stuck []string
		for i, s := range steps {
			if !placed[i] {
				stuck = append(stuck, s.Name)
			}
		}
		problems = append(problems, ValidationError{
			Code:    codeDependencyCycle,
			Field:   "steps",
			Message: fmt.Sprintf("step dependencies form a cycle among %v", stuck),
		})
		// Still hand back a total order (declaration order for the stuck
		// steps) so a caller that wants to preview can; callers that must not
		// proceed check the problems first.
		for i := range steps {
			if !placed[i] {
				out = append(out, steps[i])
			}
		}
	}
	return out, problems
}
