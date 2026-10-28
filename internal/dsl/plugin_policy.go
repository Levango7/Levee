// Plugin action classification: how an installed module plugin's manifest
// classified each action it accepts. The classification is three-state:
//
//   - destructive: declared in the manifest's destructive_actions. Judged
//     irreversible exactly like the built-in inherent vocabulary, and therefore
//     requires the workflow-level allow_irreversible whitelist.
//   - reversible: declared in the manifest's reversible_actions. The only state
//     that lets a plugin action pass without the whitelist.
//   - unclassified: an action the plugin accepts but its manifest puts in
//     neither list. Judged irreversible (fail-closed), because "the manifest
//     did not say" is not evidence of safety.
//
// This type lives in the language package for the same reason as
// inherent_irreversible.go: the compile gate, the plan-time gate, and the
// executor's checker all read the same verdict and must never disagree. It
// carries deployment facts (which plugins are installed and how they classified
// their actions), so callers build it from the plugin registry — a stable
// install-time artifact — and never by executing a plugin.
package dsl

import (
	"sort"
	"strings"
)

// PluginActionPolicy is an immutable, normalized view of the installed module
// plugins' action declarations. The zero value is valid and claims nothing:
// every step is judged by the language vocabulary alone.
type PluginActionPolicy struct {
	destructive map[string]struct{}
	reversible  map[string]struct{}
	modules     map[string]struct{}
}

// NewPluginActionPolicy normalizes the three declaration lists into a policy.
// Entries are "<plugin>.<action>" pairs for destructive/reversible and bare
// plugin names for modules. Malformed keys are dropped: a pair that names no
// step can never match one, so keeping it would only masquerade as coverage.
// Duplicates are collapsed.
func NewPluginActionPolicy(destructive, reversible, modules []string) PluginActionPolicy {
	p := PluginActionPolicy{
		destructive: make(map[string]struct{}, len(destructive)),
		reversible:  make(map[string]struct{}, len(reversible)),
		modules:     make(map[string]struct{}, len(modules)),
	}
	for _, key := range destructive {
		if key, ok := normalizeActionKey(key); ok {
			p.destructive[key] = struct{}{}
		}
	}
	for _, key := range reversible {
		if key, ok := normalizeActionKey(key); ok {
			p.reversible[key] = struct{}{}
		}
	}
	for _, name := range modules {
		name = strings.TrimSpace(name)
		if name == "" || strings.Contains(name, ".") {
			continue
		}
		p.modules[name] = struct{}{}
	}
	return p
}

// Destructive returns the declared destructive pairs, sorted. The slice is a
// copy.
func (p PluginActionPolicy) Destructive() []string { return sortedKeys(p.destructive) }

// Reversible returns the declared reversible pairs, sorted. The slice is a
// copy.
func (p PluginActionPolicy) Reversible() []string { return sortedKeys(p.reversible) }

// Modules returns the installed module-plugin names, sorted. The slice is a
// copy.
func (p PluginActionPolicy) Modules() []string { return sortedKeys(p.modules) }

// IsEmpty reports whether the policy carries no declaration at all. A caller
// may pass an empty policy anywhere a policy is accepted; it simply makes no
// plugin-specific claims.
func (p PluginActionPolicy) IsEmpty() bool {
	return len(p.destructive) == 0 && len(p.reversible) == 0 && len(p.modules) == 0
}

// JudgesIrreversible reports whether the plugin policy alone judges
// module.action irreversible. It does not consider the explicit author
// declaration or the built-in vocabulary; callers weigh those first, matching
// IrreversibleChecker.Check's priority order.
//
// A destructive declaration returns true even when the pair also appears in
// reversible — install-time validation refuses that overlap, and if a corrupt
// record still carries it, the destructive reading is the safe one.
func (p PluginActionPolicy) JudgesIrreversible(module, action string) bool {
	key := module + "." + action
	if _, ok := p.destructive[key]; ok {
		return true
	}
	if _, isModule := p.modules[module]; !isModule {
		return false
	}
	_, declaredReversible := p.reversible[key]
	return !declaredReversible
}

// normalizeActionKey trims a "<module>.<action>" pair and rejects entries whose
// dot is missing or at an edge. This is the same shape splitWhitelistKey
// enforces on allow_irreversible entries.
func normalizeActionKey(key string) (string, bool) {
	key = strings.TrimSpace(key)
	module, action, ok := splitWhitelistKey(key)
	if !ok {
		return "", false
	}
	return module + "." + action, true
}

func sortedKeys(set map[string]struct{}) []string {
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
