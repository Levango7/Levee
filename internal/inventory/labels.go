package inventory

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/nexus/levee/internal/capability"
	"github.com/nexus/levee/internal/state"
)

// canonicalLabels returns the label set a declaration will store, with the
// reserved region label validated and canonicalised.
//
// Why here and not at the display surfaces: the region value is compared with a
// node's declaration by exact string equality (internal/dispatch), so a stored
// "dc-b " or "cn north" leaves a host that no node can ever match. The run is
// then refused at dispatch and the refusal blames the node set, while the typo
// sits in the inventory. Import is the moment the operator is still looking at
// the file that contains it, so the file is refused there instead.
//
// Other label keys are the deployment's own vocabulary and pass through
// untouched. The returned map is a copy only when a value needed rewriting, so
// the parsed document is never mutated behind the caller's back.
func canonicalLabels(labels map[string]string) (map[string]string, error) {
	if labels == nil {
		return nil, nil
	}
	raw, ok := labels[capability.RegionLabel]
	if !ok {
		return labels, nil
	}
	name, err := capability.ValidateRegion(raw)
	if err != nil {
		return nil, fmt.Errorf("label %q: %w", capability.RegionLabel, err)
	}
	if name == raw {
		return labels, nil
	}
	out := make(map[string]string, len(labels))
	for k, v := range labels {
		out[k] = v
	}
	out[capability.RegionLabel] = name
	return out, nil
}

// SetTargetLabels applies a label edit to one stored target and returns the row
// as written. Keys the caller did not name are preserved — this is an edit, not
// the replacement an inventory import performs — so a host can gain a region
// label without the caller having to restate its whole label set.
//
// Two refusals, both deliberate:
//
//   - The env label is not editable here. A target's environment is what the
//     policy scopes reads and remote execution to, and target-ownership.yaml
//     governs it on the import path (EnvConflicts). A CLI that could rewrite it
//     directly would be the bypass of that check, so the operator is sent to
//     `levee target import` — where a contradiction is refused unless it is
//     explicitly overridden and audited.
//   - A region value must be a region name (capability.ValidateRegion), for the
//     same reason import validates it.
//
// changed reports whether the row was written: removing a key that is not there
// is a no-op rather than an error (re-running an edit script is ordinary), and
// so is setting a key to the value it already has.
func SetTargetLabels(ctx context.Context, store Store, host string, port int, set map[string]string, unset []string) (*state.Target, bool, error) {
	for k := range set {
		if strings.TrimSpace(k) == targetEnvLabelKey {
			return nil, false, errEnvLabelNotEditable()
		}
	}
	for _, k := range unset {
		if strings.TrimSpace(k) == targetEnvLabelKey {
			return nil, false, errEnvLabelNotEditable()
		}
	}

	row, err := store.FindTargetByAddress(ctx, host, port)
	if err != nil {
		return nil, false, err
	}
	if row == nil {
		return nil, false, fmt.Errorf("no target at %s:%d", host, port)
	}

	merged := make(map[string]string, len(row.Labels)+len(set))
	for k, v := range row.Labels {
		merged[k] = v
	}
	for k, v := range set {
		key := strings.TrimSpace(k)
		if key == "" {
			return nil, false, fmt.Errorf("label key must not be empty (got %q)", k)
		}
		merged[key] = v
	}
	for _, k := range unset {
		delete(merged, strings.TrimSpace(k))
	}
	if len(merged) == 0 {
		merged = nil
	}
	canonical, err := canonicalLabels(merged)
	if err != nil {
		return nil, false, err
	}
	if labelsEqual(row.Labels, canonical) {
		return row, false, nil
	}
	row.Labels = canonical
	if err := store.UpsertTarget(ctx, row); err != nil {
		return nil, false, err
	}
	return row, true, nil
}

func errEnvLabelNotEditable() error {
	return fmt.Errorf("the %q label is owned by %s: change it with `levee target import` "+
		"(an import refuses an env that contradicts the registry unless --allow-env-conflict is passed and audited)",
		targetEnvLabelKey, OwnershipFileName)
}

// FormatLabels renders a label set the way `levee target list` does: sorted, one
// `k=v` per key. Shared so a row printed after an edit is recognisable as the
// row the operator just saw in the listing.
func FormatLabels(labels map[string]string) string {
	if len(labels) == 0 {
		return ""
	}
	pairs := make([]string, 0, len(labels))
	for k, v := range labels {
		pairs = append(pairs, k+"="+v)
	}
	sort.Strings(pairs)
	return strings.Join(pairs, ",")
}
