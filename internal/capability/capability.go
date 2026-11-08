// Package capability owns what a stored capability declaration means: what may
// be declared, how it is written, and how a declaration is matched.
//
// It is a leaf on the dbschema pattern — standard library only — because the two
// places that need it may not depend on each other. internal/cluster writes the
// declaration into cluster_nodes.capabilities for a node's own join; the REST
// gateway matches it for an operator's `?capability=` filter while reading the
// same column through internal/state, and state and cluster are deliberately
// forbidden from importing one another (whichever package created the database
// first must not decide its shape, and neither may lean on the other's
// migration path). A shared matcher in either package would have meant either an
// import cycle or a second copy — and a second copy of a predicate is how two
// surfaces start answering the same question differently.
//
// The representation is a JSON array of module names in a TEXT column, the way
// params and labels are stored: '[]' means nothing declared. Both tables that
// carry it (cluster_nodes and agents) use that shape, so this package serves
// both identity spaces — which are NOT the same set of names (see
// internal/dispatch: run_assignment.owner_node holds a cluster.Node.ID, while
// the agent registry keys on agent ids).
//
// What this package deliberately does not do is route work by module name.
// internal/dispatch places a run on the least-loaded live worker and ignores
// module capabilities; filtering on them at claim time would let a typo'd or
// missing declaration starve a run in silence, which is worse than a node
// attempting a module it lacks and reporting that. A module capability is
// therefore a fact an operator can read and filter, not a scheduling
// constraint — and any surface that displays one should say so rather than
// imply the scheduler honours it.
//
// Regions are the one exception, and they are a different question: which
// network an execution node sits in is not a capability it might typo into
// non-existence, it is a hard reachability fact that a run's targets either
// share or do not (see Regions). dispatch honours region tokens only for runs
// whose targets declare a region, and when no node covers one it refuses
// loudly instead of executing across a data centre boundary (Placement in
// internal/dispatch). Module names remain non-constraining.
package capability

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// Empty is the canonical stored value for "nothing declared". It is also what the
// cluster_nodes column defaults to; internal/cluster's
// TestCapabilitiesColumnDefaultMatchesWhatTheWriterSends keeps those two from
// drifting apart again — the column used to DEFAULT '{}' (an empty object) while
// every writer sent '[]', so the value a reader found depended on which code path
// had created the row.
const Empty = "[]"

// Normalize validates an operator-supplied capability list and renders it as the
// JSON text a declaration column stores. An empty list yields Empty, so
// "declares nothing" has one spelling.
//
// Names are trimmed, duplicates collapse, and an entry that is empty after
// trimming is refused rather than stored: such a name would match no module and
// no filter while still appearing on an operator's card. The agent registry has
// rejected them since v8 for the same reason.
//
// The result is sorted, which makes the stored text a function of the SET. A
// node that restarts with the same capabilities typed in a different order then
// writes the same bytes instead of looking like a changed declaration.
func Normalize(in []string) (string, error) {
	out := make([]string, 0, len(in))
	seen := make(map[string]bool, len(in))
	for _, raw := range in {
		c := strings.TrimSpace(raw)
		if c == "" {
			return "", fmt.Errorf("capability names must not be empty or whitespace-only (got %q in %v)", raw, in)
		}
		// Region tokens are reserved: a malformed one (`region=` or
		// `region=cn north`) would parse to no region at all, so the node
		// would silently look like it reaches nowhere while the operator
		// believes it declared a data centre. Refuse at declaration time —
		// the one place where the operator is still looking.
		if strings.HasPrefix(c, RegionPrefix) {
			name, err := regionName(c)
			if err != nil {
				return "", fmt.Errorf("%w (got %q in %v)", err, raw, in)
			}
			// Canonicalise, do not merely validate: `region= dc-b` and
			// `region=dc-b` name the same data centre, and this package promises
			// the stored text is a function of the SET. Leaving two spellings
			// would also make the sorted order — and therefore what an operator
			// reads back — depend on how the flag was typed.
			c = RegionPrefix + name
		}
		if seen[c] {
			continue
		}
		seen[c] = true
		out = append(out, c)
	}
	sort.Strings(out)
	encoded, err := json.Marshal(out)
	if err != nil {
		// Unreachable for a []string. Returning Empty here would report a node as
		// declaring nothing, which is the failure this package exists to remove.
		return "", fmt.Errorf("encode capabilities: %w", err)
	}
	return string(encoded), nil
}

// Names decodes a stored declaration. An empty string and Empty both mean
// nothing was declared and yield no names. Anything that is not a JSON array of
// names is reported rather than swallowed: a row the code cannot read is a
// deployment whose declarations are being ignored, not an empty one, and '{}' in
// particular is what a column default left behind, not a set of zero names.
func Names(stored string) ([]string, error) {
	trimmed := strings.TrimSpace(stored)
	if trimmed == "" {
		return nil, nil
	}
	var declared []string
	if err := json.Unmarshal([]byte(trimmed), &declared); err != nil {
		return nil, fmt.Errorf("capabilities column is not a JSON string array (%q): %w", trimmed, err)
	}
	out := make([]string, 0, len(declared))
	for _, n := range declared {
		if n == "" {
			continue
		}
		out = append(out, n)
	}
	return out, nil
}

// Has reports whether a stored declaration names want. An unreadable row matches
// nothing, which is the safe direction for a filter: a row this package cannot
// parse does not claim to be what the operator asked for. Callers that must tell
// "no match" from "corrupt data" use Names directly.
func Has(stored, want string) bool {
	if strings.TrimSpace(want) == "" {
		return false
	}
	declared, err := Names(stored)
	if err != nil {
		return false
	}
	for _, n := range declared {
		if n == want {
			return true
		}
	}
	return false
}

// RegionPrefix marks a placement token inside a stored declaration: a node
// declares `region=<name>` for every data centre whose targets it can reach.
// Placement shares the capabilities column on purpose — a separate column
// would need a second migration path in two dialects and a second parser for
// the same question ("what does this node say about where it can reach"),
// which is exactly the drift this package exists to prevent. The prefix is
// therefore reserved: a module may not be named `region=...`.
const RegionPrefix = "region="

// RegionLabel is the inventory label naming the data centre a target lives in.
// It is the other half of RegionPrefix: a node declares the regions it can
// reach, a target declares the region it sits in, and placement compares the
// two names by exact equality (see internal/dispatch). The key and the name
// grammar live together so the node side and the target side cannot drift into
// two spellings of the same fact — the failure mode of which is a run refused
// with "no node reaches dc-b" while an operator is looking at a host labelled
// "dc-b ".
const RegionLabel = "region"

// ValidateRegion canonicalises one region name and reports why it cannot be
// one. Both sides of the placement comparison come through here — a node's
// `region=` token (via Normalize) and a target's inventory `region` label (via
// the importer) — because the two are compared by exact string equality. Two
// validators would let a name be acceptable on one side and not the other,
// which reads at dispatch time as "the node exists but reaches nothing".
//
// Surrounding whitespace is trimmed rather than refused (a YAML label or a
// comma-split flag attracts it); inner whitespace and ',' are refused, since
// neither survives the round trip to a single comparison.
func ValidateRegion(name string) (string, error) {
	r := strings.TrimSpace(name)
	if r == "" {
		return "", fmt.Errorf("region name must not be empty or whitespace-only")
	}
	if strings.ContainsAny(r, ", \t") {
		return "", fmt.Errorf("region name %q must not contain whitespace or ','", r)
	}
	return r, nil
}

// regionName extracts and validates the region part of a reserved token.
func regionName(token string) (string, error) {
	r, err := ValidateRegion(strings.TrimPrefix(strings.TrimSpace(token), RegionPrefix))
	if err != nil {
		return "", fmt.Errorf("%w (token %q)", err, strings.TrimSpace(token))
	}
	return r, nil
}

// Regions returns the data centres a stored declaration says the node can
// reach, in the order they appear in the declaration (Normalize sorts that
// order), without duplicates. An unreadable declaration is reported rather than
// read as "reaches nowhere": the caller that must refuse work needs to tell a
// misconfigured node from a node that declared no region at all.
func Regions(stored string) ([]string, error) {
	names, err := Names(stored)
	if err != nil {
		return nil, err
	}
	var out []string
	seen := make(map[string]bool, len(names))
	for _, n := range names {
		if !strings.HasPrefix(n, RegionPrefix) {
			continue
		}
		r, err := regionName(n)
		if err != nil {
			return nil, err
		}
		if seen[r] {
			continue
		}
		seen[r] = true
		out = append(out, r)
	}
	return out, nil
}

// Reaches reports whether a stored declaration covers every region in want.
// An empty want constrains nothing. An unreadable declaration reaches
// nothing: a node whose declaration cannot be parsed must not be credited with
// access to a data centre it may not be able to reach, and the caller reports
// the parse error separately so the refusal is not mistaken for a missing
// declaration.
func Reaches(stored string, want []string) bool {
	if len(want) == 0 {
		return true
	}
	have, err := Regions(stored)
	if err != nil {
		return false
	}
	set := make(map[string]bool, len(have))
	for _, r := range have {
		set[r] = true
	}
	for _, r := range want {
		if !set[r] {
			return false
		}
	}
	return true
}
