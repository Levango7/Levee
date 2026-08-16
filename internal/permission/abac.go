
// Package permission abac.go implements the Attribute-Based Access Control
// engine. ABAC policies are expressed as
//
//	<effect> <action> on <resource> when <label-condition>
//
// where effect is "allow" or "deny", action and resource follow the same
// wildcard rules as Policy, and label-condition is a boolean expression
// over resource/subject labels. The condition grammar supports:
//
//   - comparison:    key = value, key != value
//   - membership:    key in [a, b, c], key not_in [a, b, c]
//   - conjunction:   <expr> AND <expr>
//   - disjunction:   <expr> OR <expr>
//   - grouping:      ( <expr> )
//
// Keys are dotted paths into the label map (e.g. "target.env",
// "change.risk"). The engine returns (allowed, reason) so callers can
// surface a human-readable explanation for audit logs.
package permission

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
)

// Sentinel errors returned by the ABAC layer.
var (
	// ErrInvalidCondition is returned when a label condition cannot be
	// parsed.
	ErrInvalidCondition = errors.New("permission: invalid condition")

	// ErrUnknownOperator is returned when a condition uses an operator
	// other than =, !=, in, not_in.
	ErrUnknownOperator = errors.New("permission: unknown operator")
)

// LabelOperator is the comparison operator of a label condition atom.
type LabelOperator string

const (
	// OpEq is "key = value".
	OpEq LabelOperator = "="
	// OpNeq is "key != value".
	OpNeq LabelOperator = "!="
	// OpIn is "key in [v1, v2, ...]".
	OpIn LabelOperator = "in"
	// OpNotIn is "key not_in [v1, v2, ...]".
	OpNotIn LabelOperator = "not_in"
)

// LabelCondition is a boolean expression over a label map. It is either a
// single atom (leaf) or a binary combination of two sub-conditions
// (AND/OR). The zero value is a tautology (always true) so an empty
// condition matches everything.
type LabelCondition struct {
	// leaf is non-nil when this condition is an atom.
	leaf *labelAtom
	// left/right are set when this condition is a binary combination.
	left  *LabelCondition
	right *LabelCondition
	// op is "AND" or "OR" for binary conditions.
	op string
}

// labelAtom is a single comparison: key <op> value(s).
type labelAtom struct {
	key    string
	op     LabelOperator
	values []string
}

// Evaluate reports whether the condition holds for the given labels.
// Missing keys evaluate to the empty string for = / != and to "not in
// the set" for in / not_in. The zero LabelCondition (no leaf, no left)
// is a tautology.
func (c *LabelCondition) Evaluate(labels map[string]string) (bool, error) {
	if c == nil {
		return true, nil
	}
	if c.leaf != nil {
		return c.leaf.evaluate(labels), nil
	}
	if c.left != nil && c.right != nil {
		lv, err := c.left.Evaluate(labels)
		if err != nil {
			return false, err
		}
		rv, err := c.right.Evaluate(labels)
		if err != nil {
			return false, err
		}
		switch c.op {
		case "AND":
			return lv && rv, nil
		case "OR":
			return lv || rv, nil
		default:
			return false, fmt.Errorf("%w: unknown combinator %q", ErrInvalidCondition, c.op)
		}
	}
	// Tautology.
	return true, nil
}

// evaluate evaluates a single atom against the labels.
func (a *labelAtom) evaluate(labels map[string]string) bool {
	v := labels[a.key]
	switch a.op {
	case OpEq:
		return v == a.values[0]
	case OpNeq:
		return v != a.values[0]
	case OpIn:
		for _, want := range a.values {
			if v == want {
				return true
			}
		}
		return false
	case OpNotIn:
		for _, want := range a.values {
			if v == want {
				return false
			}
		}
		return true
	default:
		return false
	}
}

// String returns a canonical, human-readable representation of the
// condition. Useful for audit logs and debug output.
func (c *LabelCondition) String() string {
	if c == nil {
		return ""
	}
	if c.leaf != nil {
		return c.leaf.String()
	}
	if c.left != nil && c.right != nil {
		return fmt.Sprintf("(%s %s %s)", c.left.String(), c.op, c.right.String())
	}
	return ""
}

// String returns a canonical representation of an atom.
func (a *labelAtom) String() string {
	switch a.op {
	case OpIn, OpNotIn:
		return fmt.Sprintf("%s %s [%s]", a.key, a.op, strings.Join(a.values, ", "))
	default:
		return fmt.Sprintf("%s %s %s", a.key, a.op, a.values[0])
	}
}

// ParseLabelCondition parses a condition string into a LabelCondition.
// The grammar is documented in the package comment. Returns
// ErrInvalidCondition (wrapped) on any syntax error.
//
// Examples:
//   - "target.env = prod"
//   - "change.risk != low"
//   - "target.env in [prod, staging]"
//   - "change.risk not_in [low, medium]"
//   - "target.env = prod AND change.risk = high"
//   - "target.env = prod OR target.env = staging"
//   - "(target.env = prod AND change.risk = high) OR target.env = dev"
func ParseLabelCondition(s string) (LabelCondition, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return LabelCondition{}, nil
	}
	p := &conditionParser{src: s}
	c, err := p.parseOr()
	if err != nil {
		return LabelCondition{}, err
	}
	if p.pos < len(p.src) {
		return LabelCondition{}, fmt.Errorf("%w: trailing input at position %d", ErrInvalidCondition, p.pos)
	}
	return c, nil
}

// conditionParser is a tiny recursive-descent parser for the condition
// grammar. It keeps the source, the current position, and nothing else.
type conditionParser struct {
	src string
	pos int
}

// parseOr parses an OR expression: parseAnd ("OR" parseAnd)*.
func (p *conditionParser) parseOr() (LabelCondition, error) {
	left, err := p.parseAnd()
	if err != nil {
		return LabelCondition{}, err
	}
	for {
		p.skipSpaces()
		if !p.consumeKeyword("OR") {
			break
		}
		p.skipSpaces()
		right, err := p.parseAnd()
		if err != nil {
			return LabelCondition{}, err
		}
		// Copy left and right into fresh heap allocations so the new
		// left does not alias the loop variable (which would create a
		// self-referential cycle and blow the stack on Evaluate).
		leftCopy := left
		rightCopy := right
		left = LabelCondition{
			left:  &leftCopy,
			right: &rightCopy,
			op:    "OR",
		}
	}
	return left, nil
}

// parseAnd parses an AND expression: parseAtom ("AND" parseAtom)*.
func (p *conditionParser) parseAnd() (LabelCondition, error) {
	left, err := p.parseAtom()
	if err != nil {
		return LabelCondition{}, err
	}
	for {
		p.skipSpaces()
		if !p.consumeKeyword("AND") {
			break
		}
		p.skipSpaces()
		right, err := p.parseAtom()
		if err != nil {
			return LabelCondition{}, err
		}
		// Copy left and right into fresh heap allocations so the new
		// left does not alias the loop variable (which would create a
		// self-referential cycle and blow the stack on Evaluate).
		leftCopy := left
		rightCopy := right
		left = LabelCondition{
			left:  &leftCopy,
			right: &rightCopy,
			op:    "AND",
		}
	}
	return left, nil
}

// parseAtom parses a single comparison or a parenthesised sub-expression.
func (p *conditionParser) parseAtom() (LabelCondition, error) {
	p.skipSpaces()
	if p.pos < len(p.src) && p.src[p.pos] == '(' {
		p.pos++
		inner, err := p.parseOr()
		if err != nil {
			return LabelCondition{}, err
		}
		p.skipSpaces()
		if p.pos >= len(p.src) || p.src[p.pos] != ')' {
			return LabelCondition{}, fmt.Errorf("%w: missing closing paren at position %d", ErrInvalidCondition, p.pos)
		}
		p.pos++
		return inner, nil
	}
	return p.parseComparison()
}

// parseComparison parses a single key <op> value(s) atom.
func (p *conditionParser) parseComparison() (LabelCondition, error) {
	key, err := p.parseIdent()
	if err != nil {
		return LabelCondition{}, err
	}
	p.skipSpaces()
	op, err := p.parseOperator()
	if err != nil {
		return LabelCondition{}, err
	}
	p.skipSpaces()
	var values []string
	if op == OpIn || op == OpNotIn {
		values, err = p.parseList()
	} else {
		var v string
		v, err = p.parseValue()
		values = []string{v}
	}
	if err != nil {
		return LabelCondition{}, err
	}
	return LabelCondition{leaf: &labelAtom{key: key, op: op, values: values}}, nil
}

// parseIdent reads a dotted identifier (e.g. "target.env").
func (p *conditionParser) parseIdent() (string, error) {
	start := p.pos
	for p.pos < len(p.src) {
		c := p.src[p.pos]
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '.' || c == '_' || c == '-' {
			p.pos++
			continue
		}
		break
	}
	if p.pos == start {
		return "", fmt.Errorf("%w: expected identifier at position %d", ErrInvalidCondition, p.pos)
	}
	return p.src[start:p.pos], nil
}

// parseOperator reads one of =, !=, in, not_in.
func (p *conditionParser) parseOperator() (LabelOperator, error) {
	if p.pos >= len(p.src) {
		return "", fmt.Errorf("%w: expected operator at position %d", ErrInvalidCondition, p.pos)
	}
	// Two-char operators first.
	if p.pos+1 < len(p.src) {
		two := p.src[p.pos : p.pos+2]
		if two == "!=" {
			p.pos += 2
			return OpNeq, nil
		}
	}
	if p.src[p.pos] == '=' {
		p.pos++
		return OpEq, nil
	}
	// Keyword operators.
	for _, kw := range []string{"not_in", "in"} {
		if p.consumeKeyword(kw) {
			return LabelOperator(kw), nil
		}
	}
	return "", fmt.Errorf("%w: unknown operator at position %d", ErrUnknownOperator, p.pos)
}

// parseValue reads a single value: an identifier or a quoted string.
func (p *conditionParser) parseValue() (string, error) {
	p.skipSpaces()
	if p.pos >= len(p.src) {
		return "", fmt.Errorf("%w: expected value at position %d", ErrInvalidCondition, p.pos)
	}
	if p.src[p.pos] == '"' || p.src[p.pos] == '\'' {
		quote := p.src[p.pos]
		p.pos++
		start := p.pos
		for p.pos < len(p.src) && p.src[p.pos] != quote {
			p.pos++
		}
		if p.pos >= len(p.src) {
			return "", fmt.Errorf("%w: unterminated string at position %d", ErrInvalidCondition, start)
		}
		v := p.src[start:p.pos]
		p.pos++
		return v, nil
	}
	return p.parseIdent()
}

// parseList reads a bracketed list of values: [a, b, c].
func (p *conditionParser) parseList() ([]string, error) {
	p.skipSpaces()
	if p.pos >= len(p.src) || p.src[p.pos] != '[' {
		return nil, fmt.Errorf("%w: expected '[' at position %d", ErrInvalidCondition, p.pos)
	}
	p.pos++
	var values []string
	for {
		p.skipSpaces()
		if p.pos < len(p.src) && p.src[p.pos] == ']' {
			p.pos++
			break
		}
		v, err := p.parseValue()
		if err != nil {
			return nil, err
		}
		values = append(values, v)
		p.skipSpaces()
		if p.pos < len(p.src) && p.src[p.pos] == ',' {
			p.pos++
			continue
		}
		p.skipSpaces()
		if p.pos < len(p.src) && p.src[p.pos] == ']' {
			p.pos++
			break
		}
		return nil, fmt.Errorf("%w: expected ',' or ']' at position %d", ErrInvalidCondition, p.pos)
	}
	if len(values) == 0 {
		return nil, fmt.Errorf("%w: empty list", ErrInvalidCondition)
	}
	return values, nil
}

// skipSpaces advances past whitespace.
func (p *conditionParser) skipSpaces() {
	for p.pos < len(p.src) && (p.src[p.pos] == ' ' || p.src[p.pos] == '\t') {
		p.pos++
	}
}

// consumeKeyword reports whether the next token is the given keyword and,
// when it is, advances past it. A keyword must be followed by whitespace,
// an operator character, or end-of-input so that "in" does not match the
// prefix of "input".
func (p *conditionParser) consumeKeyword(kw string) bool {
	if p.pos+len(kw) > len(p.src) {
		return false
	}
	if p.src[p.pos:p.pos+len(kw)] != kw {
		return false
	}
	next := p.pos + len(kw)
	if next >= len(p.src) {
		p.pos = next
		return true
	}
	c := p.src[next]
	if c == ' ' || c == '\t' || c == '(' || c == '[' || c == '"' || c == '\'' {
		p.pos = next
		return true
	}
	return false
}

// --- ABACEngine -------------------------------------------------------------

// ABACEngine wraps a PolicySet and provides label-based access control
// with human-readable explanations. It is the high-level entry point
// used by the orchestrator and the CLI `rbac check` command.
//
// The engine is safe for concurrent use after construction.
type ABACEngine struct {
	mu       sync.RWMutex
	policies *PolicySet
}

// NewABACEngine returns an engine backed by the given policy set. If
// policies is nil an empty set is created so the engine can be used
// immediately (and will deny everything by default).
func NewABACEngine(policies *PolicySet) *ABACEngine {
	if policies == nil {
		policies = NewPolicySet()
	}
	return &ABACEngine{policies: policies}
}

// Policies returns the underlying policy set. Callers may mutate it
// (e.g. add policies) but should use the engine's own methods when
// possible so that cache invalidation hooks can fire.
func (e *ABACEngine) Policies() *PolicySet {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.policies
}

// SetPolicies swaps the policy set. The old set is discarded.
func (e *ABACEngine) SetPolicies(ps *PolicySet) {
	if ps == nil {
		ps = NewPolicySet()
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	e.policies = ps
}

// Evaluate decides whether subject may perform action on resource given
// the supplied labels. It returns (allowed, reason). The reason is a
// short human-readable string suitable for audit logs; on denial it
// explains which policy caused the rejection.
//
// The decision follows deny-wins semantics:
//  1. If any matching deny policy fires, return (false, "denied by
//     policy <id>").
//  2. If any matching allow policy fires, return (true, "allowed by
//     policy <id>").
//  3. If no policy matches, return (false, "no matching policy").
func (e *ABACEngine) Evaluate(subject, action, resource string, labels map[string]string) (bool, string) {
	e.mu.RLock()
	defer e.mu.RUnlock()


	var (
		allowID  string
		denyID   string
		anyError error
	)
	for _, p := range e.policies.policies {
		ok, err := p.Matches(resource, action, labels)
		if err != nil {
			if anyError == nil {
				anyError = err
			}
			continue
		}
		if !ok {
			continue
		}
		if p.Effect == EffectDeny {
			denyID = p.ID
			break
		}
		if allowID == "" {
			allowID = p.ID
		}
	}

	if denyID != "" {
		return false, fmt.Sprintf("denied by policy %q", denyID)
	}
	if allowID != "" {
		return true, fmt.Sprintf("allowed by policy %q", allowID)
	}
	if anyError != nil {
		return false, fmt.Sprintf("evaluation error: %v", anyError)
	}
	return false, "no matching policy"
}

// Explain is a convenience wrapper around Evaluate that returns a longer
// explanation including the matched policies. It is intended for the CLI
// `rbac check --verbose` path.
func (e *ABACEngine) Explain(subject, action, resource string, labels map[string]string) string {
	allowed, reason := e.Evaluate(subject, action, resource, labels)
	decision := "DENY"
	if allowed {
		decision = "ALLOW"
	}

	var b strings.Builder
	fmt.Fprintf(&b, "Decision: %s\n", decision)
	fmt.Fprintf(&b, "Reason:   %s\n", reason)
	fmt.Fprintf(&b, "Subject:  %s\n", subject)
	fmt.Fprintf(&b, "Action:   %s\n", action)
	fmt.Fprintf(&b, "Resource: %s\n", resource)
	if len(labels) > 0 {
		keys := make([]string, 0, len(labels))
		for k := range labels {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		b.WriteString("Labels:\n")
		for _, k := range keys {
			fmt.Fprintf(&b, "  %s = %s\n", k, labels[k])
		}
	}
	return b.String()
}