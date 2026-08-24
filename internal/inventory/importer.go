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

	"github.com/nexus/levee/internal/state"
	"gopkg.in/yaml.v3"
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

func newID(prefix string) string {
	b := make([]byte, 5)
	_, _ = rand.Read(b)
	return prefix + hex.EncodeToString(b)
}

// Importer applies inventory files to the store.
type Importer struct {
	store Store
}

// NewImporter returns an Importer backed by store.
func NewImporter(store Store) *Importer { return &Importer{store: store} }

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
	g := &state.InventoryGroup{ID: newID("grp-"), Name: name}
	if err := im.store.UpsertInventoryGroup(ctx, g); err != nil {
		return "", err
	}
	known[name] = g.ID
	return g.ID, nil
}

// Import applies f to the store. Per-row failures are accumulated into the
// returned Summary — one bad entry never aborts the whole file.
func (im *Importer) Import(ctx context.Context, f *File, defaultGroup string) (*Summary, error) {
	sum := &Summary{}
	if f == nil {
		return nil, fmt.Errorf("inventory: nil file")
	}
	known := map[string]string{}

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

	for i, td := range f.Targets {
		row := fmt.Sprintf("targets[%d]", i)
		host, port, err := splitAddress(td.Address)
		if err != nil {
			im.fail(sum, row, err)
			continue
		}
		if td.Port > 0 {
			port = td.Port
		}
		if port == 0 {
			port = 22
		}
		channel := td.ChannelType
		if channel == "" {
			channel = "ssh"
		}
		if !validChannels[channel] {
			im.fail(sum, row, fmt.Errorf("invalid channel_type %q (ssh|winrm)", channel))
			continue
		}
		status := td.Status
		if status == "" {
			status = state.StatusActive
		}
		if !validStatuses[status] {
			im.fail(sum, row, fmt.Errorf("invalid status %q", status))
			continue
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
				continue
			}
			groupID = gid
		}

		existing, err := im.store.FindTargetByAddress(ctx, host, port)
		if err != nil {
			im.fail(sum, row, err)
			continue
		}

		tg := &state.Target{
			ID:            newID("tgt-"),
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
			if len(tg.Labels) == 0 && len(existing.Labels) > 0 && status == existing.Status {
				tg.Labels = existing.Labels // pure address re-declaration keeps labels
			}
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
			continue
		}
	}
	return sum, nil
}

func (im *Importer) fail(sum *Summary, row string, err error) {
	sum.Failed++
	sum.Errors = append(sum.Errors, fmt.Sprintf("%s: %v", row, err))
}
