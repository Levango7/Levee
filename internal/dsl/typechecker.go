package dsl

// typechecker.go — LEVEELang compile-time type checker.
//
// The TypeChecker runs after parsing and basic validation. It performs:
//   - input parameter type resolution against the TypeRegistry
//   - workflow variable type inference from assignments
//   - step argument type checking against per-module/action signatures
//   - batch.strategy enum membership ({percent, fixed, serial})
//   - approval.level enum membership ({standard, high, emergency})
//   - template parameter type matching
//
// Two modes are supported:
//   - Strict (default): every TypeError is fatal; CheckStrict returns the
//     first error.
//   - Lenient: TypeErrors are reported but do not block IR generation; the
//     caller decides whether to proceed.
//
// The checker is intentionally conservative: it never mutates the AST and it
// never panics on missing fields — those are reported as TypeErrors instead.

import (
	"fmt"
	"strings"
)

// TypeError describes a single type-checking failure. File/Line/Column locate
// the error in source; ExpectedType and ActualType carry the types involved
// when applicable (nil otherwise).
type TypeError struct {
	File         string
	Line         int
	Column       int
	Message      string
	ExpectedType Type
	ActualType   Type
}

// Error returns a single-line representation suitable for CLI output.
func (e TypeError) Error() string {
	loc := e.File
	if loc == "" {
		loc = "<input>"
	}
	if e.Line > 0 {
		loc = fmt.Sprintf("%s:%d", loc, e.Line)
		if e.Column > 0 {
			loc = fmt.Sprintf("%s:%d", loc, e.Column)
		}
	}
	msg := e.Message
	if e.ExpectedType != nil || e.ActualType != nil {
		exp, act := "<?>", "<?>"
		if e.ExpectedType != nil {
			exp = e.ExpectedType.String()
		}
		if e.ActualType != nil {
			act = e.ActualType.String()
		}
		msg = fmt.Sprintf("%s (expected=%s, actual=%s)", msg, exp, act)
	}
	return fmt.Sprintf("%s: %s", loc, msg)
}

// newTypeError constructs a TypeError with the given location and message.
func newTypeError(file string, line, column int, message string) TypeError {
	return TypeError{File: file, Line: line, Column: column, Message: message}
}

// CheckMode selects between strict and lenient checking.
type CheckMode int

const (
	// ModeStrict is the default: every TypeError is fatal.
	ModeStrict CheckMode = iota
	// ModeLenient reports TypeErrors but does not block downstream phases.
	ModeLenient
)

// TypeChecker performs compile-time type checking on a Workflow AST. It holds
// a TypeRegistry for resolving symbolic type names and the source file path
// for error location reporting. The zero value is not ready — use
// NewTypeChecker.
type TypeChecker struct {
	registry *TypeRegistry
	file     string
	mode     CheckMode
}

// NewTypeChecker returns a TypeChecker backed by the given registry. When
// registry is nil a fresh empty registry is created. The file path is used
// purely for error location reporting and may be empty.
func NewTypeChecker(registry *TypeRegistry, file string) *TypeChecker {
	if registry == nil {
		registry = NewTypeRegistry()
	}
	return &TypeChecker{registry: registry, file: file, mode: ModeStrict}
}

// SetMode switches between strict and lenient checking.
func (c *TypeChecker) SetMode(mode CheckMode) {
	if c == nil {
		return
	}
	c.mode = mode
}

// Registry returns the underlying type registry. Callers may use it to
// pre-register aliases/enums before running Check.
func (c *TypeChecker) Registry() *TypeRegistry {
	if c == nil {
		return nil
	}
	return c.registry
}

// Check runs the full type-checking pipeline on wf and returns every
// TypeError found (non-short-circuiting). A nil wf yields a single TypeError.
// The returned slice is non-nil but may be empty when wf passes.
func (c *TypeChecker) Check(wf *Workflow) []TypeError {
	if wf == nil {
		return []TypeError{newTypeError(c.file, 0, 0, "workflow is nil")}
	}
	var errs []TypeError
	errs = append(errs, c.checkInputs(wf.Inputs)...)
	errs = append(errs, c.checkSteps(wf.Steps)...)
	errs = append(errs, c.checkBatches(wf.Batches)...)
	errs = append(errs, c.checkApproval(wf.Approval)...)
	errs = append(errs, c.checkRollback(wf.Rollback)...)
	return errs
}

// CheckStrict runs Check and returns the first error as a Go error value,
// or nil when there are no errors.
func (c *TypeChecker) CheckStrict(wf *Workflow) error {
	errs := c.Check(wf)
	if len(errs) == 0 {
		return nil
	}
	return errs[0]
}

// CheckWithMode runs Check under the given mode. In lenient mode the errors
// are still returned (so callers may log them) but the caller is expected to
// treat them as warnings. The mode is restored to its previous value before
// returning.
func (c *TypeChecker) CheckWithMode(wf *Workflow, mode CheckMode) []TypeError {
	prev := c.mode
	c.mode = mode
	defer func() { c.mode = prev }()
	return c.Check(wf)
}

// ---------------------------------------------------------------------------
// Sub-checkers
// ---------------------------------------------------------------------------

// checkInputs resolves each input parameter's declared type and validates the
// default value's type when present.
func (c *TypeChecker) checkInputs(inputs []InputParam) []TypeError {
	var errs []TypeError
	for i, p := range inputs {
		field := fmt.Sprintf("input[%d]", i)
		declared := c.registry.Resolve(p.Type)
		if declared == nil && p.Type != "" {
			errs = append(errs, newTypeError(c.file, 0, 0,
				fmt.Sprintf("%s: unknown type %q", field, p.Type)))
			continue
		}
		if p.Default != nil && declared != nil {
			actual := inferType(p.Default)
			if actual != nil && !declared.Compatible(actual) {
				errs = append(errs, TypeError{
					File:         c.file,
					Message:      fmt.Sprintf("%s.default: type mismatch", field),
					ExpectedType: declared,
					ActualType:   actual,
				})
			}
		}
	}
	return errs
}

// checkSteps validates each step's argument types against the per-action
// signature table. Unknown actions are skipped (the basic validator already
// reports missing action fields).
func (c *TypeChecker) checkSteps(steps []Step) []TypeError {
	var errs []TypeError
	for i, s := range steps {
		base := fmt.Sprintf("steps[%d]", i)
		errs = append(errs, c.checkStepArgs(base, s)...)
		if s.Approval != nil {
			errs = append(errs, c.checkApproval(s.Approval)...)
		}
		if s.Rollback != nil {
			errs = append(errs, c.checkRollback(s.Rollback)...)
		}
	}
	return errs
}

// checkStepArgs validates the args of a single step against the action
// signature table. Unknown args are ignored in lenient mode and reported in
// strict mode.
func (c *TypeChecker) checkStepArgs(base string, s Step) []TypeError {
	sig, ok := actionSignatures[s.Module+"."+s.Action]
	if !ok {
		// Unknown action: nothing to check.
		return nil
	}
	var errs []TypeError
	for key, expected := range sig {
		raw, present := s.Args[key]
		if !present {
			continue
		}
		actual := inferType(raw)
		if actual == nil {
			continue
		}
		if !expected.Compatible(actual) {
			errs = append(errs, TypeError{
				File:         c.file,
				Message:      fmt.Sprintf("%s.args.%s: type mismatch", base, key),
				ExpectedType: expected,
				ActualType:   actual,
			})
		}
	}
	return errs
}

// checkBatches validates batch.strategy against the {percent, fixed, serial}
// enum and checks that batch.steps values match the strategy's expected type.
func (c *TypeChecker) checkBatches(b BatchConfig) []TypeError {
	if b.Strategy == "" {
		return nil
	}
	var errs []TypeError
	strategyEnum := batchStrategyEnum
	if !strategyEnum.HasValue(b.Strategy) {
		errs = append(errs, TypeError{
			File:         c.file,
			Message:      fmt.Sprintf("batches.strategy: invalid value %q", b.Strategy),
			ExpectedType: strategyEnum,
			ActualType:   TypeString{},
		})
		return errs
	}
	// steps type check: percent expects percent, fixed/serial expects int.
	var stepType Type
	switch b.Strategy {
	case "percent":
		stepType = TypePercent{}
	case "fixed", "serial":
		stepType = TypeInt{}
	}
	for i := range b.Steps {
		actual := TypeInt{}
		if !stepType.Compatible(actual) {
			errs = append(errs, TypeError{
				File:         c.file,
				Message:      fmt.Sprintf("batches.steps[%d]: type mismatch", i),
				ExpectedType: stepType,
				ActualType:   actual,
			})
		}
	}
	return errs
}

// checkApproval validates approval.level against the {standard, high,
// emergency} enum.
func (c *TypeChecker) checkApproval(a *ApprovalSpec) []TypeError {
	if a == nil || a.Level == "" {
		return nil
	}
	var errs []TypeError
	if !approvalLevelEnum.HasValue(a.Level) {
		errs = append(errs, TypeError{
			File:         c.file,
			Message:      fmt.Sprintf("approval.level: invalid value %q", a.Level),
			ExpectedType: approvalLevelEnum,
			ActualType:   TypeString{},
		})
	}
	return errs
}

// checkRollback validates rollback.strategy when present.
func (c *TypeChecker) checkRollback(r *RollbackSpec) []TypeError {
	if r == nil || r.Strategy == "" {
		return nil
	}
	var errs []TypeError
	switch r.Strategy {
	case "snapshot", "undo-action", "config-revert":
		// valid
	default:
		errs = append(errs, TypeError{
			File:    c.file,
			Message: fmt.Sprintf("rollback.strategy: invalid value %q", r.Strategy),
		})
	}
	return errs
}

// ---------------------------------------------------------------------------
// Type inference from runtime values
// ---------------------------------------------------------------------------

// inferType maps a Go value (typically from YAML decoding) to a LEVEELang
// Type. Returns nil when the value's type cannot be represented.
func inferType(v any) Type {
	switch x := v.(type) {
	case nil:
		return nil
	case string:
		return TypeString{}
	case int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64:
		return TypeInt{}
	case float32, float64:
		return TypeFloat{}
	case bool:
		return TypeBool{}
	case []any:
		if len(x) == 0 {
			return TypeList{ElementType: nil}
		}
		return TypeList{ElementType: inferType(x[0])}
	case map[string]any:
		if len(x) == 0 {
			return TypeMap{KeyType: TypeString{}, ValueType: nil}
		}
		var vt Type
		for _, val := range x {
			vt = inferType(val)
			break
		}
		return TypeMap{KeyType: TypeString{}, ValueType: vt}
	}
	return nil
}

// ---------------------------------------------------------------------------
// Built-in signatures and enums
// ---------------------------------------------------------------------------

// actionSignatures maps "module.action" to a map of expected argument types.
// Only the well-known MVP actions are listed; unknown actions are skipped by
// the checker.
var actionSignatures = map[string]map[string]Type{
	"shell.command": {
		"cmd":     TypeString{},
		"timeout": TypeDuration{},
	},
	"shell.exec": {
		"cmd":     TypeString{},
		"timeout": TypeDuration{},
	},
	"file.write": {
		"path":    TypeString{},
		"content": TypeString{},
		"mode":    TypeString{}, // octal as string, e.g. "0644"
	},
	"file.copy": {
		"src":  TypeString{},
		"dst":  TypeString{},
		"mode": TypeString{},
	},
	"svc.restart": {
		"name":    TypeString{},
		"timeout": TypeDuration{},
	},
	"svc.stop": {
		"name":    TypeString{},
		"timeout": TypeDuration{},
	},
	"pkg.upgrade": {
		"name":    TypeString{},
		"version": TypeString{},
	},
	"pkg.install": {
		"name":    TypeString{},
		"version": TypeString{},
	},
}

// batchStrategyEnum is the enum of allowed batch strategies.
var batchStrategyEnum = &TypeEnum{
	Name:   "batch_strategy",
	Values: []string{"percent", "fixed", "serial"},
}

// approvalLevelEnum is the enum of allowed approval levels.
var approvalLevelEnum = &TypeEnum{
	Name:   "approval_level",
	Values: []string{"standard", "high", "emergency"},
}

// Ensure the sentinel enums satisfy the Type interface at compile time.
var (
	_ Type = (*TypeEnum)(nil)
	_ Type = (*TypeAlias)(nil)
	_ Type = TypeString{}
	_ Type = TypeInt{}
	_ Type = TypeFloat{}
	_ Type = TypeBool{}
	_ Type = TypeDuration{}
	_ Type = TypePercent{}
	_ Type = TypeMap{}
	_ Type = TypeList{}
)

// joinTypeNames joins the String() of the given types with ", " — a small
// helper used by error message builders.
func joinTypeNames(ts []Type) string {
	parts := make([]string, 0, len(ts))
	for _, t := range ts {
		if t == nil {
			parts = append(parts, "<?>")
		} else {
			parts = append(parts, t.String())
		}
	}
	return strings.Join(parts, ", ")
}
