// inputs.go implements LEVEELang input-parameter resolution (spec §2.1): the
// step that turns a workflow's `{{input.x}}` references into concrete values.
//
// Design (see notes/levee-review design: docs/leveelang-spec.md §2.1):
//
//   - A declared parameter takes its value from the run's parameter map;
//     failing that, its declared default; failing both, resolution fails. An
//     input carries no value of its own — that is the whole point of the
//     "variables come only from input declarations and step outputs" rule the
//     language exists to enforce over Ansible's implicit layers.
//   - The value is checked against the declared type before it is substituted.
//   - Substitution walks the SAME reference surface the (now retired) LE004
//     advisory used, plus gate parameters and snapshot paths — never a subset,
//     so a reference the compiler can see can never escape resolution.
//   - A reference to an undeclared parameter, or any `{{input.…}}` left over
//     after substitution, is an error rather than a literal handed to a module.
//
// These are PLAN-time failures, not compile diagnostics: a document may
// legitimately declare and reference inputs with no values in hand — the values
// arrive with the run (CreateChange params). The compile channel therefore
// stays silent about inputs, and the governance gate (plan) is where an
// unsatisfied or mistyped input is refused.
package dsl

import (
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Sentinel errors for input resolution, so callers can distinguish the three
// failure modes with errors.Is instead of matching message text.
var (
	// ErrInputMissing: a declared parameter has no supplied value and no default.
	ErrInputMissing = errors.New("dsl: input has no value and no default")
	// ErrInputUndeclared: a reference names a parameter the workflow never declared.
	ErrInputUndeclared = errors.New("dsl: reference to an undeclared input")
	// ErrInputType: a supplied value does not match the declared type.
	ErrInputType = errors.New("dsl: input value does not match its declared type")
)

// inputRefRe matches a reference to a declared input. Whitespace inside the
// braces is allowed because the spec's own examples write both `{{input.x}}`
// and `{{ input.table }}`.
var inputRefRe = regexp.MustCompile(`\{\{\s*input\.\s*([A-Za-z_][A-Za-z0-9_]*)\s*\}\}`)

// leftoverInputRe matches any brace expression still mentioning `input.`. It is
// the post-substitution guard: anything shaped like an input reference that
// inputRefRe did not consume (a malformed name, an expression) must fail rather
// than reach a module as text.
var leftoverInputRe = regexp.MustCompile(`\{\{[^}]*input\.[^}]*\}\}`)

// ResolveInputs substitutes the workflow's declared input values into every
// `{{input.<name>}}` reference and returns the SAME workflow (mutated in place
// — callers own a freshly parsed AST).
//
// values is the run's parameter map (CreateChange params). Only names the
// workflow declares are read, so unrelated parameters travelling in the same
// map (alert_id, target, …) are ignored; that is safe because the map is the
// run's general parameter bag, and the strict checks live on the reference side
// (an undeclared reference is an error) and on required/type.
//
// A workflow that neither declares nor references inputs is returned untouched
// — the common case stays a zero-cost no-op.
func ResolveInputs(wf *Workflow, values map[string]string) (*Workflow, error) {
	if wf == nil {
		return nil, fmt.Errorf("dsl: resolve inputs: nil workflow")
	}
	if len(wf.Inputs) == 0 && !referencesInputs(wf) {
		return wf, nil
	}

	registry := NewTypeRegistry()
	resolved := make(map[string]string, len(wf.Inputs))
	for _, p := range wf.Inputs {
		if p.Name == "" {
			// The validator already reports an unnamed parameter (V23/LE002);
			// resolution must not guess a name to hang a value on.
			continue
		}
		raw, supplied := values[p.Name]
		if !supplied {
			if p.Default == nil {
				return nil, fmt.Errorf("%w: %q", ErrInputMissing, p.Name)
			}
			raw = fmt.Sprintf("%v", p.Default)
		}
		if err := checkInputType(registry, p, raw); err != nil {
			return nil, err
		}
		resolved[p.Name] = raw
	}

	if err := substituteWorkflowInputs(wf, resolved); err != nil {
		return nil, err
	}
	return wf, nil
}

// checkInputType validates raw against the parameter's declared type. An
// unresolvable type name is left to the compile-time type checker (checkInputs)
// rather than judged a second time here — two judges for one slot is how the
// two drift apart.
func checkInputType(registry *TypeRegistry, p InputParam, raw string) error {
	v := strings.TrimSpace(raw)
	// Composite declarations are not resolvable by the type registry (it knows
	// the eight simple names plus registered aliases/enums, not the
	// `map<k,v>` syntax), and the language has chosen no wire format for a
	// composite VALUE. Refuse by shape rather than substitute a string that
	// merely looks like a map into a slot expecting one.
	if typ := strings.TrimSpace(p.Type); strings.HasPrefix(typ, "map<") || strings.HasPrefix(typ, "list<") {
		return fmt.Errorf("%w: input %q: map/list input values are not supported yet", ErrInputType, p.Name)
	}
	declared := registry.Resolve(p.Type)
	if declared == nil {
		// Unknown type name: the compile-time checker owns it (checkInputs);
		// do not judge the same slot twice.
		return nil
	}
	switch resolveBasic(declared).(type) {
	case TypeString:
		return nil
	case TypeInt:
		if _, err := strconv.Atoi(v); err != nil {
			return fmt.Errorf("%w: input %q: %q is not an int", ErrInputType, p.Name, raw)
		}
	case TypeFloat:
		if _, err := strconv.ParseFloat(v, 64); err != nil {
			return fmt.Errorf("%w: input %q: %q is not a float", ErrInputType, p.Name, raw)
		}
	case TypeBool:
		if _, err := strconv.ParseBool(v); err != nil {
			return fmt.Errorf("%w: input %q: %q is not a bool", ErrInputType, p.Name, raw)
		}
	case TypeDuration:
		if _, err := time.ParseDuration(v); err != nil {
			return fmt.Errorf("%w: input %q: %q is not a duration", ErrInputType, p.Name, raw)
		}
	case TypePercent:
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 || n > 100 {
			return fmt.Errorf("%w: input %q: %q is not a percent (0-100)", ErrInputType, p.Name, raw)
		}
	default:
		// Alias to an enum (or an unknown user type): check membership when the
		// target is an enum, otherwise accept (the compile-time checker owns it).
		if e, ok := declared.(*TypeEnum); ok && !e.HasValue(v) {
			return fmt.Errorf("%w: input %q: %q is not a member of %s", ErrInputType, p.Name, raw, e.String())
		}
	}
	return nil
}

// substituteWorkflowInputs walks every place an input reference may appear —
// step args, rollback (undo) step args, step-level gates, workflow-level gates,
// batch gates and snapshot paths — and replaces the references in place.
func substituteWorkflowInputs(wf *Workflow, values map[string]string) error {
	for i := range wf.Steps {
		if err := substituteStepInputs(&wf.Steps[i], values); err != nil {
			return err
		}
	}
	if wf.Gate != nil {
		if err := substituteGateInputs(wf.Gate, values); err != nil {
			return err
		}
	}
	if wf.Batches.Gate != nil {
		if err := substituteGateInputs(wf.Batches.Gate, values); err != nil {
			return err
		}
	}
	if wf.Rollback != nil {
		if err := substituteStrings(wf.Rollback.SnapshotPaths, values); err != nil {
			return err
		}
	}
	return nil
}

// substituteStepInputs resolves one step's args, its rollback steps' args and
// its own gate declarations.
func substituteStepInputs(s *Step, values map[string]string) error {
	if err := substituteArgMap(s.Args, values); err != nil {
		return err
	}
	if s.Gate != nil {
		if err := substituteGateInputs(s.Gate, values); err != nil {
			return err
		}
	}
	if s.Rollback != nil {
		if err := substituteStrings(s.Rollback.SnapshotPaths, values); err != nil {
			return err
		}
		for i := range s.Rollback.Steps {
			// An undo step's own rollback is not a nested-compensation concept
			// the engine honours, so only the undo step's args and gate need
			// substitution.
			if err := substituteArgMap(s.Rollback.Steps[i].Args, values); err != nil {
				return err
			}
			if s.Rollback.Steps[i].Gate != nil {
				if err := substituteGateInputs(s.Rollback.Steps[i].Gate, values); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

// substituteGateInputs resolves the string fields and params of every check in
// a gate spec. The spec parameterises `wait`/`query` through inputs, so gate
// params are part of the reference surface even though the old advisory walk
// did not descend into them.
func substituteGateInputs(g *GateSpec, values map[string]string) error {
	for _, checks := range [][]GateCheck{g.Pre, g.Post, g.Batch} {
		for i := range checks {
			c := &checks[i]
			fields := []*string{&c.Command, &c.ExpectStdout, &c.Timeout}
			for _, f := range fields {
				out, err := substituteInputRefs(*f, values)
				if err != nil {
					return err
				}
				*f = out
			}
			if err := substituteArgMap(c.Params, values); err != nil {
				return err
			}
		}
	}
	return nil
}

// substituteArgMap replaces references in every value of an arg map, recursing
// through nested maps and lists (a reference may sit inside a list or a nested
// map, and missing those would be exactly the silent-literal bug this feature
// exists to remove).
func substituteArgMap(args map[string]any, values map[string]string) error {
	for k, v := range args {
		nv, err := substituteAny(v, values)
		if err != nil {
			return err
		}
		args[k] = nv
	}
	return nil
}

func substituteAny(v any, values map[string]string) (any, error) {
	switch t := v.(type) {
	case string:
		return substituteInputRefs(t, values)
	case map[string]any:
		if err := substituteArgMap(t, values); err != nil {
			return nil, err
		}
		return t, nil
	case []any:
		for i := range t {
			nv, err := substituteAny(t[i], values)
			if err != nil {
				return nil, err
			}
			t[i] = nv
		}
		return t, nil
	default:
		return v, nil
	}
}

func substituteStrings(list []string, values map[string]string) error {
	for i := range list {
		out, err := substituteInputRefs(list[i], values)
		if err != nil {
			return err
		}
		list[i] = out
	}
	return nil
}

// substituteInputRefs replaces every `{{input.name}}` in s. An undeclared name
// is an error, and a brace expression still mentioning `input.` afterwards is an
// error too — the post-condition is that no input reference survives, so no
// literal can reach a module.
func substituteInputRefs(s string, values map[string]string) (string, error) {
	if s == "" || !strings.Contains(s, "input.") {
		return s, nil
	}
	var firstErr error
	out := inputRefRe.ReplaceAllStringFunc(s, func(m string) string {
		name := strings.TrimSpace(inputRefRe.FindStringSubmatch(m)[1])
		v, ok := values[name]
		if !ok {
			if firstErr == nil {
				firstErr = fmt.Errorf("%w: %q", ErrInputUndeclared, name)
			}
			return m
		}
		return v
	})
	if firstErr != nil {
		return s, firstErr
	}
	if leftoverInputRe.MatchString(out) {
		return s, fmt.Errorf("%w: %q still carries an input reference after substitution", ErrInputUndeclared, out)
	}
	return out, nil
}
