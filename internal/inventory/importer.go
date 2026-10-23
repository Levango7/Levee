// Package inventory implements asset-management helpers on top of the
// state.Store inventory tables. The importer turns a declarative YAML
// inventory file into idempotent upserts: re-importing the same file
// updates existing rows (matched by hostname+port) instead of duplicating
// them.
package inventory

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/nexus/levee/internal/state"
)

// Store is the subset of state.Store the importer needs.
type Store interface {
	ListInventoryGroups(ctx context.Context) ([]*state.InventoryGroup, error)
	UpsertInventoryGroup(ctx context.Context, group *state.InventoryGroup) error
	FindTargetByAddress(ctx context.Context, hostname string, port int) (*state.Target, error)
	UpsertTarget(ctx context.Context, target *state.Target) error
}

var validChannels = map[string]bool{"ssh": true, "winrm": true}
var validStatuses = map[string]bool{state.StatusActive: true, state.StatusFrozen: true, state.StatusRetired: true}

// GroupDef declares one inventory group. Parent is the NAME of the parent
// group and may reference a group declared later in the file or already
// present in the store.
type GroupDef struct {
	Name   string `yaml:"name" json:"name"`
	Parent string `yaml:"parent,omitempty" json:"parent,omitempty"`
}

// TargetDef declares one managed host.
type TargetDef struct {
	Address       string            `yaml:"address" json:"address"`
	Port          int               `yaml:"port,omitempty" json:"port,omitempty"`
	ChannelType   string            `yaml:"channel_type,omitempty" json:"channel_type,omitempty"`
	Labels        map[string]string `yaml:"labels,omitempty" json:"labels,omitempty"`
	Group         string            `yaml:"group,omitempty" json:"group,omitempty"`
	CredentialRef string            `yaml:"credential_ref,omitempty" json:"credential_ref,omitempty"`
	Status        string            `yaml:"status,omitempty" json:"status,omitempty"`
}

// File is the top-level inventory document.
type File struct {
	Groups  []GroupDef  `yaml:"groups,omitempty" json:"groups,omitempty"`
	Targets []TargetDef `yaml:"targets" json:"targets"`
}

// Summary reports one Import run.
type Summary struct {
	Created int
	Updated int
	Failed  int
	Errors  []string
}

// ParseYAML decodes an inventory document. Unknown top-level keys are
// rejected so typos (e.g. "host" instead of "address") surface loudly.
func ParseYAML(data []byte) (*File, error) {
	f := &File{}
	dec := yaml.NewDecoder(strings.NewReader(string(data)))
	dec.KnownFields(true)
	if err := dec.Decode(f); err != nil && err.Error() != "EOF" {
		return nil, err
	}
	return f, nil
}

// splitAddress accepts "host", "host:port". It does NOT handle IPv6
// brackets in v1; use explicit port field for those.
func splitAddress(addr string) (string, int, error) {
	addr = strings.TrimSpace(addr)
	if addr == "" {
		return "", 0, fmt.Errorf("empty address")
	}
	if i := strings.LastIndex(addr, ":"); i >= 0 {
		host := addr[:i]
		var port int
		if _, err := fmt.Sscanf(addr[i+1:], "%d", &port); err != nil || port <= 0 || port > 65535 {
			return "", 0, fmt.Errorf("invalid port in address %q", addr)
		}
		return host, port, nil
	}
	return addr, 0, nil
}

func newID(prefix string) (string, error) {
	b := make([]byte, 5)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("inventory: generate id: %w", err)
	}
	return prefix + hex.EncodeToString(b), nil
}

// Importer applies inventory files to the store.
type Importer struct {
	store Store
}

// NewImporter returns an Importer backed by store.
func NewImporter(store Store) *Importer { return &Importer{store: store} }

// LabelChange is one row of an import that would leave an EXISTING target with
// different labels than it has stored.
//
// Which label matters is deliberately NOT decided here — the caller owns that
// vocabulary. This reports the merge itself.
type LabelChange struct {
	Row       string // the row's label in the file, for the refusal message
	Host      string
	Port      int
	Stored    map[string]string
	Resulting map[string]string
}

// LabelChanges lists the rows that would change an existing target's labels,
// without writing anything.
//
// Why a preview exists at all: labels carry the environment, and a target's
// environment is what the policy scopes reads and remote execution to (see
// grpc.resource_authz). A re-import that moves a prod host into dev therefore
// hands that machine to everyone the dev environment licenses — and unlike
// registering a new host, it rewrites a row the platform already trusts. The
// refusal has to happen BEFORE the first write: an import that refused halfway
// would leave the inventory half-applied, and ImportTargets promises the
// opposite ("a file that contains one host out of reach imports none of its
// hosts").
//
// The merge is computed by mergedLabels for both this pass and the write, so a
// change to how a re-declaration is interpreted cannot make the preview
// disagree with what actually lands.
func (im *Importer) LabelChanges(ctx context.Context, f *File) ([]LabelChange, error) {
	if f == nil {
		return nil, fmt.Errorf("inventory: nil file")
	}
	var out []LabelChange
	for i, td := range f.Targets {
		host, port, err := targetAddressFor(td)
		if err != nil {
			// importTarget reports the malformed row; a row with no address has no
			// stored row to relabel, so it cannot contribute a label change.
			continue
		}
		status := td.Status
		if status == "" {
			status = state.StatusActive
		}
		if !validStatuses[status] {
			continue // importTarget fails this row; it will not reach a write
		}
		existing, err := im.store.FindTargetByAddress(ctx, host, port)
		if err != nil {
			return nil, err
		}
		if existing == nil {
			continue // a new row has no stored scope to take away
		}
		resulting := mergedLabels(td.Labels, existing.Labels, status, existing.Status)
		if labelsEqual(existing.Labels, resulting) {
			continue
		}
		out = append(out, LabelChange{
			Row:       fmt.Sprintf("targets[%d]", i),
			Host:      host,
			Port:      port,
			Stored:    existing.Labels,
			Resulting: resulting,
		})
	}
	return out, nil
}

// labelsEqual compares two label maps by value. Used only to keep the preview
// from reporting rows that change nothing — a preview that reported every
// re-import would make the caller judge rows it has nothing to decide about.
func labelsEqual(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, av := range a {
		if bv, ok := b[k]; !ok || bv != av {
			return false
		}
	}
	return true
}

// ensureGroup returns the ID of the named group, creating it when missing.
// known maps already-seen names to IDs within this import run.
func (im *Importer) ensureGroup(ctx context.Context, name string, known map[string]string) (string, error) {
	if id, ok := known[name]; ok {
		return id, nil
	}
	existing, err := im.store.ListInventoryGroups(ctx)
	if err != nil {
		return "", err
	}
	for _, g := range existing {
		if g.Name == name {
			known[name] = g.ID
			return g.ID, nil
		}
	}
	grpID, err := newID("grp-")
	if err != nil {
		return "", err
	}
	g := &state.InventoryGroup{ID: grpID, Name: name}
	if err := im.store.UpsertInventoryGroup(ctx, g); err != nil {
		return "", err
	}
	known[name] = g.ID
	return g.ID, nil
}

// Import applies f to the store. Per-row failures are accumulated into the
// returned Summary — one bad entry never aborts the whole file. The work is
// split into importGroups (declarative group tree first) and per-target
// importTarget calls; the Summary accumulates across both phases.
func (im *Importer) Import(ctx context.Context, f *File, defaultGroup string) (*Summary, error) {
	sum := &Summary{}
	if f == nil {
		return nil, fmt.Errorf("inventory: nil file")
	}
	known := map[string]string{}

	im.importGroups(ctx, f, known, sum)

	for i, td := range f.Targets {
		im.importTarget(ctx, td, fmt.Sprintf("targets[%d]", i), defaultGroup, known, sum)
	}
	return sum, nil
}

// importGroups materialises f.Groups (including parent links) into the
// store, recording per-group failures in sum.
func (im *Importer) importGroups(ctx context.Context, f *File, known map[string]string, sum *Summary) {
	for _, gd := range f.Groups {
		if strings.TrimSpace(gd.Name) == "" {
			sum.Failed++
			sum.Errors = append(sum.Errors, "group with empty name")
			continue
		}
		id, err := im.ensureGroup(ctx, gd.Name, known)
		if err != nil {
			sum.Failed++
			sum.Errors = append(sum.Errors, fmt.Sprintf("group %q: %v", gd.Name, err))
			continue
		}
		if gd.Parent != "" {
			parentID, perr := im.ensureGroup(ctx, gd.Parent, known)
			if perr != nil {
				sum.Failed++
				sum.Errors = append(sum.Errors, fmt.Sprintf("group %q parent: %v", gd.Name, perr))
				continue
			}
			if err := im.store.UpsertInventoryGroup(ctx, &state.InventoryGroup{
				ID: id, Name: gd.Name, ParentID: parentID,
			}); err != nil {
				sum.Failed++
				sum.Errors = append(sum.Errors, fmt.Sprintf("group %q: %v", gd.Name, err))
			}
		}
	}
}

// importTarget merges one target declaration into the store: normalises
// address/channel/status defaults, resolves its group, and either updates
// the existing (host,port) row — preserving credential/labels/status where
// the re-declaration is silent — or creates a new one. Failures are
// accumulated into sum.
// targetAddressFor normalises one declaration's address the way every import
// path must: the explicit `address` supplies host and port, a `port:` field
// overrides it, and the default is 22. Extracted so the write pass and the
// label preview cannot disagree about which stored row a declaration refers to
// — a preview that resolved a different address would be answering about a
// different machine.
func targetAddressFor(td TargetDef) (string, int, error) {
	host, port, err := splitAddress(td.Address)
	if err != nil {
		return "", 0, err
	}
	if td.Port > 0 {
		port = td.Port
	}
	if port == 0 {
		port = 22
	}
	return host, port, nil
}

// mergedLabels returns the labels an EXISTING target carries after this
// declaration is applied. The rule lives here, in one place, because the
// preview (LabelChanges) and the write (importTarget) must agree: a preview
// that computed the merge differently from the write would authorise the wrong
// transition.
//
//   - a declaration that names labels REPLACES them;
//   - a declaration that names none keeps the stored ones, but only when it also
//     leaves the status alone. Otherwise the row is being redefined rather than
//     merely re-addressed, and silence there must not silently preserve a scope.
func mergedLabels(incoming, existing map[string]string, incomingStatus, existingStatus string) map[string]string {
	if len(incoming) > 0 {
		return incoming
	}
	if len(existing) > 0 && incomingStatus == existingStatus {
		return existing
	}
	return nil
}

func (im *Importer) importTarget(ctx context.Context, td TargetDef, row string, defaultGroup string, known map[string]string, sum *Summary) {
	host, port, err := targetAddressFor(td)
	if err != nil {
		im.fail(sum, row, err)
		return
	}
	channel := td.ChannelType
	if channel == "" {
		channel = "ssh"
	}
	if !validChannels[channel] {
		im.fail(sum, row, fmt.Errorf("invalid channel_type %q (ssh|winrm)", channel))
		return
	}
	status := td.Status
	if status == "" {
		status = state.StatusActive
	}
	if !validStatuses[status] {
		im.fail(sum, row, fmt.Errorf("invalid status %q", status))
		return
	}

	groupID := ""
	groupName := td.Group
	if groupName == "" {
		groupName = defaultGroup
	}
	if groupName != "" {
		gid, err := im.ensureGroup(ctx, groupName, known)
		if err != nil {
			im.fail(sum, row, err)
			return
		}
		groupID = gid
	}

	existing, err := im.store.FindTargetByAddress(ctx, host, port)
	if err != nil {
		im.fail(sum, row, err)
		return
	}

	tgID, err := newID("tgt-")
	if err != nil {
		im.fail(sum, row, err)
		return
	}
	tg := &state.Target{
		ID:            tgID,
		Hostname:      host,
		Port:          port,
		ChannelType:   channel,
		CredentialRef: td.CredentialRef,
		Labels:        td.Labels,
		GroupID:       groupID,
		Status:        status,
	}
	if existing != nil {
		tg.ID = existing.ID
		if tg.CredentialRef == "" {
			tg.CredentialRef = existing.CredentialRef
		}
		tg.Labels = mergedLabels(td.Labels, existing.Labels, status, existing.Status)
		if td.Status == "" && existing.Status != "" {
			tg.Status = existing.Status // re-import never silently un-freezes
		}
		sum.Updated++
	} else {
		sum.Created++
	}
	if err := im.store.UpsertTarget(ctx, tg); err != nil {
		sum.Failed++
		sum.Errors = append(sum.Errors, fmt.Sprintf("%s %s:%d: %v", row, host, port, err))
		if sum.Created > 0 && existing == nil {
			sum.Created-- // rolled back by the failed upsert
		} else if existing != nil {
			sum.Updated--
		}
	}
}

func (im *Importer) fail(sum *Summary, row string, err error) {
	sum.Failed++
	sum.Errors = append(sum.Errors, fmt.Sprintf("%s: %v", row, err))
}
