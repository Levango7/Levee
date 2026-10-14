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
// What this package deliberately does not do is route work. internal/dispatch
// places a run on the least-loaded live worker and knows nothing about
// capabilities, and it should stay that way until a run declares what it needs:
// filtering at claim time would let a typo'd or missing declaration starve a run
// in silence, which is worse than a node attempting a module it lacks and
// reporting that. A capability is therefore a fact an operator can read and
// filter, not a scheduling constraint — and any surface that displays one should
// say so rather than imply the scheduler honours it.
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
