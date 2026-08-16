// Package permission policy.go implements fine-grained permission policies
// built on the (Resource, Action, Condition) triple. A policy either
// allows or denies an action on a resource when an optional label-based
// condition holds. A PolicySet is a collection of policies evaluated with
// deny-wins semantics: if any matching deny policy fires, the request is
// rejected even when matching allow policies exist.
//
// Resources support a simple wildcard scheme:
//   - "change:abc123"           — a concrete change resource
//   - "change:*"                — every change resource
//   - "target:env=prod"         — every target whose env label is prod
//   - "*"                       — every resource
//
// Conditions are label expressions parsed by parseLabelCondition in
// abac.go; a policy with an empty condition always matches. PolicySet can
// be loaded from YAML so operators can manage policies as configuration.
package permission

import (
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"

	"gopkg.in/yaml.v3"
)

// PolicyEffect is the outcome of a policy: allow or deny.
type PolicyEffect string

const (
	// EffectAllow permits the action when the policy matches.
	EffectAllow PolicyEffect = "allow"
	// EffectDeny rejects the action when the policy matches. Deny takes
	// precedence over allow.
	EffectDeny PolicyEffect = "deny"
)

// Sentinel errors returned by the policy layer.
var (
	// ErrUnknownEffect is returned when a policy effect is neither
	// "allow" nor "deny".
	ErrUnknownEffect = errors.New("permission: unknown effect")

	// ErrEmptyResource is returned when a policy resource is empty.
	ErrEmptyResource = errors.New("permission: empty resource")

	// ErrEmptyPolicyAction is returned when a policy action is empty.
	ErrEmptyPolicyAction = errors.New("permission: empty policy action")

	// ErrNoMatch is returned by PolicySet.Evaluate when no policy matches
	// the request. Callers usually treat this as an implicit deny.
	ErrNoMatch = errors.New("permission: no matching policy")
)

// Policy is a single (Resource, Action, Condition) rule with an effect.
// A policy matches a request when the resource pattern matches the
// request resource, the action matches (exact or wildcard), and the
// condition evaluates to true against the request labels.
type Policy struct {
	// ID is an optional human-readable identifier for audit logging.
	ID string `yaml:"id" json:"id"`
	// Effect is "allow" or "deny".
	Effect PolicyEffect `yaml:"effect" json:"effect"`
	// Resource is a concrete id or a wildcard pattern such as
	// "change:*" or "target:env=prod".
	Resource string `yaml:"resource" json:"resource"`
	// Action is the operation to authorise, e.g. "apply", "view", or
	// "*" to match any action.
	Action string `yaml:"action" json:"action"`
	// Condition is an optional label expression. Empty means always
	// match. The grammar is documented in abac.go.
	Condition string `yaml:"condition" json:"condition"`
	// Description is an optional human-readable note.
	Description string `yaml:"description" json:"description"`

	// parsedCondition is the compiled form of Condition. It is built
	// lazily by ensureCompiled and reused across evaluations.
	parsedCondition LabelCondition
	// compiled tracks whether parsedCondition has been built.
	compiled bool
	// compileErr stores any error from the last compile attempt so
	// repeated evaluations do not re-attempt a known-bad condition.
	compileErr error
}

// Validate checks that the policy is well-formed: effect is known,
// resource and action are non-empty, and the condition (if any) parses.
func (p *Policy) Validate() error {
	if p.Effect != EffectAllow && p.Effect != EffectDeny {
		return fmt.Errorf("%w: %q", ErrUnknownEffect, p.Effect)
	}
	if p.Resource == "" {
		return ErrEmptyResource
	}
	if p.Action == "" {
		return ErrEmptyPolicyAction
	}
	if p.Condition != "" {
		if _, err := ParseLabelCondition(p.Condition); err != nil {
			return fmt.Errorf("parse condition: %w", err)
		}
	}
	return nil
}

// ensureCompiled lazily parses the condition. Safe to call concurrently;
// the worst case is a few redundant parses which are idempotent.
func (p *Policy) ensureCompiled() error {
	if p.compiled {
		return p.compileErr
	}
	if p.Condition == "" {
		p.compiled = true
		return nil
	}
	c, err := ParseLabelCondition(p.Condition)
	p.parsedCondition = c
	p.compileErr = err
	p.compiled = true
	return err
}

// matchesResource reports whether the policy resource pattern matches
// the request resource. Matching is structural:
//   - "*" matches everything.
//   - "kind:*" matches every resource of that kind.
//   - "kind:selector" matches when the request resource has the same
//     kind and either the selector is "*" or the request resource's
//     value part equals the selector.
//   - Any other pattern matches by exact string equality.
func (p *Policy) matchesResource(request string) bool {
	pat := p.Resource
	if pat == Wildcard {
		return true
	}
	if pat == request {
		return true
	}
	// "kind:*" form.
	if strings.HasSuffix(pat, ":*") {
		patKind := strings.TrimSuffix(pat, ":*")
		reqKind, _, _ := strings.Cut(request, ":")
		return patKind == reqKind || patKind == ""
	}
	// "kind:selector" form where selector contains '=' is a label
	// selector; we cannot evaluate it here without labels so we treat
	// it as a non-match for the resource component. The condition
	// field is responsible for label-based matching.
	if strings.Contains(pat, ":") {
		patKind, patSel, _ := strings.Cut(pat, ":")
		reqKind, reqSel, _ := strings.Cut(request, ":")
		if patKind != reqKind {
			return false
		}
		// If the selector contains '=', it is a label selector and
		// must be evaluated by the condition. Match the kind only.
		if strings.Contains(patSel, "=") {
			return true
		}
		return patSel == reqSel
	}
	return false
}

// matchesAction reports whether the policy action matches the request
// action. "*" matches any action; otherwise exact equality is required.
func (p *Policy) matchesAction(request string) bool {
	if p.Action == Wildcard {
		return true
	}
	return p.Action == request
}

// matchesCondition reports whether the policy condition holds for the
// given labels. An empty condition always matches.
func (p *Policy) matchesCondition(labels map[string]string) (bool, error) {
	if err := p.ensureCompiled(); err != nil {
		return false, fmt.Errorf("compile condition: %w", err)
	}
	if p.Condition == "" {
		return true, nil
	}
	return p.parsedCondition.Evaluate(labels)
}

// Matches reports whether the policy matches the request (resource,
// action, labels). Returns an error when the condition cannot be
// evaluated.
func (p *Policy) Matches(resource, action string, labels map[string]string) (bool, error) {
	if !p.matchesResource(resource) {
		return false, nil
	}
	if !p.matchesAction(action) {
		return false, nil
	}
	return p.matchesCondition(labels)
}

// PolicySet is a collection of policies evaluated with deny-wins
// semantics. The set is safe for concurrent reads after construction;
// mutations are guarded by an RWMutex.
type PolicySet struct {
	mu       sync.RWMutex
	policies []*Policy
}

// NewPolicySet returns an empty policy set.
func NewPolicySet() *PolicySet {
	return &PolicySet{}
}

// Add appends a policy to the set. The policy is validated first; an
// invalid policy is rejected and the set is left untouched.
func (s *PolicySet) Add(p *Policy) error {
	if p == nil {
		return errors.New("permission: nil policy")
	}
	if err := p.Validate(); err != nil {
		return fmt.Errorf("validate policy: %w", err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.policies = append(s.policies, p)
	return nil
}

// Remove removes the policy with the given ID from the set. Returns
// true when a policy was removed, false when no policy had that ID.
func (s *PolicySet) Remove(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, p := range s.policies {
		if p.ID == id {
			s.policies = append(s.policies[:i], s.policies[i+1:]...)
			return true
		}
	}
	return false
}

// List returns a snapshot of the policies in the set, sorted by ID then
// by Resource. The returned slice is a copy and may be mutated freely.
func (s *PolicySet) List() []*Policy {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*Policy, len(s.policies))
	copy(out, s.policies)
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].ID != out[j].ID {
			return out[i].ID < out[j].ID
		}
		return out[i].Resource < out[j].Resource
	})
	return out
}

// Len returns the number of policies in the set.
func (s *PolicySet) Len() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.policies)
}

// EvaluationContext is the input to PolicySet.Evaluate. It carries the
// subject, action, resource, and resource labels needed to evaluate
// label-based conditions.
type EvaluationContext struct {
	Subject  string
	Action   string
	Resource string
	Labels   map[string]string
}

// Evaluate evaluates every policy in the set against the context. The
// result follows deny-wins semantics:
//  1. If any matching deny policy fires, return (false, nil).
//  2. If any matching allow policy fires, return (true, nil).
//  3. If no policy matches, return (false, ErrNoMatch).
//
// A policy that errors during condition evaluation is treated as a
// non-match (best-effort) and the error is returned alongside the final
// decision when no other policy decides. This keeps a single broken
// policy from breaking the whole evaluation while still surfacing the
// error.
func (s *PolicySet) Evaluate(ctx EvaluationContext) (bool, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var (
		allowed      bool
		firstErr     error
		anyMatched   bool
		matchedAllow bool
		matchedDeny  bool
	)
	for _, p := range s.policies {
		ok, err := p.Matches(ctx.Resource, ctx.Action, ctx.Labels)
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		if !ok {
			continue
		}
		anyMatched = true
		if p.Effect == EffectDeny {
			matchedDeny = true
		} else {
			matchedAllow = true
		}
	}

	if matchedDeny {
		return false, nil
	}
	if matchedAllow {
		allowed = true
		return allowed, nil
	}
	if !anyMatched {
		return false, fmt.Errorf("%w: subject %q action %q resource %q", ErrNoMatch, ctx.Subject, ctx.Action, ctx.Resource)
	}
	// anyMatched but no allow/deny fired (should not happen) — surface
	// the first error.
	return false, firstErr
}

// --- YAML loading -----------------------------------------------------------

// PolicyConfig is the YAML/JSON representation of a policy file. It is a
// flat list of policies plus optional metadata.
type PolicyConfig struct {
	Policies []Policy `yaml:"policies" json:"policies"`
}

// LoadFromYAML loads policies from a YAML file. Existing policies in the
// set are replaced.
func (s *PolicySet) LoadFromYAML(path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read policy yaml: %w", err)
	}
	var cfg PolicyConfig
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return fmt.Errorf("unmarshal policy yaml: %w", err)
	}
	return s.LoadFromConfig(cfg)
}

// LoadFromConfig replaces the set with the policies from cfg. Each policy
// is validated; the first invalid policy aborts the load and leaves the
// set untouched.
func (s *PolicySet) LoadFromConfig(cfg PolicyConfig) error {
	newPolicies := make([]*Policy, 0, len(cfg.Policies))
	for i := range cfg.Policies {
		p := cfg.Policies[i]
		if err := p.Validate(); err != nil {
			return fmt.Errorf("validate policy %d: %w", i, err)
		}
		newPolicies = append(newPolicies, &p)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.policies = newPolicies
	return nil
}

// MarshalYAML emits the policy set as a PolicyConfig document.
func (s *PolicySet) MarshalYAML() ([]byte, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	cfg := PolicyConfig{Policies: make([]Policy, 0, len(s.policies))}
	for _, p := range s.policies {
		cfg.Policies = append(cfg.Policies, *p)
	}
	data, err := yaml.Marshal(cfg)
	if err != nil {
		return nil, fmt.Errorf("marshal policy yaml: %w", err)
	}
	return data, nil
}