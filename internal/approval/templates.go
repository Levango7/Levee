package approval

// Package approval implements the approval service for LEVEE's change
// pipeline. The templates.go file (T033) defines a library of high-risk
// operation templates — database drop, master/slave switch, firewall
// flush — that the planner matches against workflow steps to auto-raise
// the approval tier and tighten the approver requirements.
//
// A Template is a named, configurable rule that combines:
//
//   - MatchPatterns: the module + action + target substrings that
//     identify the dangerous operation (e.g. pkg.remove on a mysql
//     target).
//   - RequiredLevel: the approval tier the operation must be raised to
//     (always "high" for the built-in templates).
//   - RequiredApprovers / MinApprovers / Timeout: the tightened
//     approval requirements.
//
// The built-in templates are hardcoded in MVP (design doc 4.4.6.3). A
// later phase will load them from YAML; the RegisterTemplate method is
// already in place so that custom templates can be added without
// changing the library code.

import (
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

// --- MatchPattern -----------------------------------------------------------

// MatchPattern defines a single module + action + target match rule. A
// Step matches the pattern when all of the following hold:
//
//   - Module matches exactly (case-sensitive).
//   - Action matches exactly (case-sensitive).
//   - Either Targets is empty (any target matches) OR at least one
//     target substring is contained in step.Target (case-insensitive
//     substring match, so "MySQL" and "mysql" both match "mysql").
type MatchPattern struct {
	// Module is the exact module name, e.g. "pkg", "svc", "shell".
	Module string `json:"module" yaml:"module"`

	// Action is the exact action verb, e.g. "remove", "restart", "exec".
	Action string `json:"action" yaml:"action"`

	// Targets is the optional list of target substrings. When empty,
	// any target matches (including the empty target). When non-empty,
	// at least one substring must be contained in step.Target for the
	// pattern to match. Matching is case-insensitive.
	Targets []string `json:"targets,omitempty" yaml:"targets,omitempty"`
}

// matches reports whether the pattern matches the step. The matching is
// case-insensitive on the target substring to tolerate casing
// differences in user-supplied target identifiers (MySQL vs mysql).
func (p MatchPattern) matches(step Step) bool {
	if p.Module != step.Module {
		return false
	}
	if p.Action != step.Action {
		return false
	}
	if len(p.Targets) == 0 {
		return true
	}
	if step.Target == "" {
		return false
	}
	targetLower := strings.ToLower(step.Target)
	for _, t := range p.Targets {
		if strings.Contains(targetLower, strings.ToLower(t)) {
			return true
		}
	}
	return false
}

// --- Template ---------------------------------------------------------------

// Template is a named, configurable high-risk operation rule. When a
// workflow step matches one of the template's MatchPatterns, the
// planner raises the approval to RequiredLevel and applies the
// approver / timeout requirements.
type Template struct {
	// Name is the unique template identifier, e.g. "database-drop".
	// It is used as the map key in the library and in audit log
	// messages ("step matched template %q").
	Name string `json:"name" yaml:"name"`

	// Description is a human-readable summary of what the template
	// catches and why it is dangerous. Shown in `levee plan
	// --show-templates` output.
	Description string `json:"description" yaml:"description"`

	// MatchPatterns is the list of patterns that identify the
	// operation. A step matches the template when it matches ANY of
	// the patterns (logical OR).
	MatchPatterns []MatchPattern `json:"match_patterns" yaml:"match_patterns"`

	// RequiredLevel is the approval tier the operation must be raised
	// to. For the built-in templates this is always LevelHigh.
	RequiredLevel string `json:"required_level" yaml:"required_level"`

	// RequiredApprovers is the total number of approvers expected to
	// be configured on an approval using this template.
	RequiredApprovers int `json:"required_approvers" yaml:"required_approvers"`

	// MinApprovers is the minimum number of distinct "approve"
	// decisions needed to transition the approval to StatusApproved.
	MinApprovers int `json:"min_approvers" yaml:"min_approvers"`

	// Timeout is the maximum wall-clock duration an approval using
	// this template may remain pending.
	Timeout time.Duration `json:"timeout" yaml:"timeout"`
}

// --- TemplateLibrary --------------------------------------------------------

// TemplateLibrary manages a collection of Templates and provides Match
// to find the first template that matches a step. It is safe for
// concurrent use: the templates map is guarded by an RWMutex so that
// RegisterTemplate / UnregisterTemplate (writers) can run in parallel
// with Get / List / Match / MatchAny (readers).
type TemplateLibrary struct {
	mu        sync.RWMutex
	templates map[string]Template
}

// NewTemplateLibrary returns a library with the built-in templates
// pre-registered. The built-in templates are:
//
//	database-drop        — pkg.remove on db targets        -> high, 2 approvers, 4h
//	master-slave-switch  — svc.restart on mysql/redis      -> high, 2 approvers, 4h
//	firewall-flush        — shell.exec with iptables flush  -> high, 2 approvers, 4h
//
// Callers may add custom templates with RegisterTemplate or replace the
// built-ins by registering a template with the same Name.
func NewTemplateLibrary() *TemplateLibrary {
	lib := &TemplateLibrary{templates: make(map[string]Template)}
	for _, t := range builtinTemplates() {
		lib.templates[t.Name] = t
	}
	return lib
}

// builtinTemplates returns the hardcoded built-in templates. These are
// the high-risk operation patterns documented in the design doc
// (section 4.4.6.3). In MVP they are hardcoded; a later phase will
// load them from YAML (the struct tags are already in place).
func builtinTemplates() []Template {
	const (
		highApprovers = 2
		highTimeout   = 4 * time.Hour
	)
	return []Template{
		{
			Name:        "database-drop",
			Description: "Dropping a database or schema via pkg.remove on a db-related target (mysql/postgres/mongodb/redis/db/database)",
			MatchPatterns: []MatchPattern{
				{
					Module:  "pkg",
					Action:  "remove",
					Targets: []string{"mysql", "postgres", "mongodb", "redis", "db", "database"},
				},
			},
			RequiredLevel:     LevelHigh,
			RequiredApprovers: highApprovers,
			MinApprovers:      highApprovers,
			Timeout:           highTimeout,
		},
		{
			Name:        "master-slave-switch",
			Description: "Restarting a master/slave replication service via svc.restart on mysql/redis/postgres targets",
			MatchPatterns: []MatchPattern{
				{
					Module:  "svc",
					Action:  "restart",
					Targets: []string{"mysql", "redis", "postgres"},
				},
			},
			RequiredLevel:     LevelHigh,
			RequiredApprovers: highApprovers,
			MinApprovers:      highApprovers,
			Timeout:           highTimeout,
		},
		{
			Name:        "firewall-flush",
			Description: "Flushing firewall rules via shell.exec with iptables/ufw/firewall-cmd flush commands",
			MatchPatterns: []MatchPattern{
				{
					Module:  "shell",
					Action:  "exec",
					Targets: []string{"iptables -f", "iptables --flush", "iptables -x", "ufw flush", "firewall-cmd --reload"},
				},
			},
			RequiredLevel:     LevelHigh,
			RequiredApprovers: highApprovers,
			MinApprovers:      highApprovers,
			Timeout:           highTimeout,
		},
	}
}

// RegisterTemplate adds or replaces a template in the library. It
// validates that the template has a non-empty Name and a legal
// RequiredLevel. Re-registering a template with the same Name overwrites
// the previous one — this is the supported way to override a built-in
// template with a custom configuration.
//
// Returns an error when:
//   - t.Name is empty;
//   - t.RequiredLevel is not one of standard / high / emergency.
func (l *TemplateLibrary) RegisterTemplate(t Template) error {
	if t.Name == "" {
		return fmt.Errorf("approval: template name cannot be empty")
	}
	if !validLevel(t.RequiredLevel) {
		return fmt.Errorf("%w: template %q has invalid required_level %q (allowed: standard, high, emergency)",
			ErrInvalidLevel, t.Name, t.RequiredLevel)
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.templates[t.Name] = t
	return nil
}

// UnregisterTemplate removes a template from the library. It is
// primarily useful in tests; production code treats the library as
// append-only. Removing a non-existent template is a no-op.
func (l *TemplateLibrary) UnregisterTemplate(name string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.templates, name)
}

// Get returns the template with the given name. It returns an error
// when the template is not registered.
func (l *TemplateLibrary) Get(name string) (Template, error) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	t, ok := l.templates[name]
	if !ok {
		return Template{}, fmt.Errorf("approval: template %q not found", name)
	}
	return t, nil
}

// List returns all registered templates in sorted order by Name. The
// sorted order makes the output deterministic, which is important for
// `levee plan --show-templates` output and for Match (which iterates
// in the same order so that the matched template is stable when two
// templates could both match).
func (l *TemplateLibrary) List() []Template {
	l.mu.RLock()
	defer l.mu.RUnlock()
	names := make([]string, 0, len(l.templates))
	for n := range l.templates {
		names = append(names, n)
	}
	sort.Strings(names)
	out := make([]Template, 0, len(names))
	for _, n := range names {
		out = append(out, l.templates[n])
	}
	return out
}

// Match returns the first template that matches the step. Templates are
// checked in sorted Name order so that the result is deterministic when
// multiple templates could match (the lexicographically smallest Name
// wins).
//
// Returns (nil, nil) when no template matches — this is the common
// case for ordinary steps and is not an error. Callers should check
// the returned pointer against nil before using it.
func (l *TemplateLibrary) Match(step Step) (*Template, error) {
	for _, t := range l.List() {
		for _, p := range t.MatchPatterns {
			if p.matches(step) {
				// Return a pointer to a copy so the caller cannot
				// mutate the library's internal template through the
				// returned pointer.
				matched := t
				return &matched, nil
			}
		}
	}
	return nil, nil
}

// MatchAny reports whether any registered template matches the step. It
// is a convenience wrapper around Match for callers that only need the
// boolean verdict (e.g. the planner's "does this step need a high-risk
// template?" check).
func (l *TemplateLibrary) MatchAny(step Step) bool {
	t, err := l.Match(step)
	if err != nil {
		return false
	}
	return t != nil
}
