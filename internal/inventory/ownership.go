// ownership.go is the platform-side fact source for "which environment does
// this host belong to".
//
// Why it exists: a target's `labels["env"]` is caller-asserted — the policy can
// answer "may you claim hosts in this environment" but never "is this really a
// prod machine" (see grpc.resource_authz and the ImportTargets stored-env guard,
// which closes the ONE path that could rewrite an existing row). This file is
// the missing input, named in the ledger as needing a platform-side ownership
// fact source; the shape follows alert-sources.yaml, the repo's precedent: a
// registration decides what a thing belongs to, and nothing in a request can
// override it. The ONE exception is deliberate and per-call: ImportTargets can
// carry allow_env_conflict, which writes the file's declaration anyway and
// leaves an audit row. There is no serve-level switch, because one that stayed
// on would make the check decorative.
//
// The file lives in the data directory (not the main config, which gets pasted
// into tickets and reviews) and is OPTIONAL: no file means "the platform has no
// ownership facts", which keeps the labels-and-default-env behaviour for
// deployments that have not opted in. A file that exists but cannot be parsed,
// or declares a host without an environment, is an ERROR — half an ownership
// fact is worse than none, and a deployment that opted in and broke must not
// silently degrade to caller-asserted labels.
//
// The key is the HOSTNAME, not the target id: ids are generated (tgt-...) and
// change on re-registration, while the hostname is what an operator knows, what
// an alert's `instance` label carries, and what survives an import that replaces
// a row. The same hostname cannot be declared twice; that conflict is an error
// rather than a last-write, because two rows naming one host is exactly the
// ambiguity this file exists to end.
package inventory

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/nexus/levee/internal/state"
)

// OwnershipFileName is the optional host-ownership registry within the LEVEE
// data directory.
const OwnershipFileName = "target-ownership.yaml"

// HostOwnership is one registered fact: this hostname belongs to this
// environment.
type HostOwnership struct {
	// Hostname is the inventory hostname the fact is about (the same string
	// FindTargetByAddress matches on).
	Hostname string `yaml:"hostname"`
	// Environment is the platform-side answer for which environment this host
	// belongs to.
	Environment string `yaml:"environment"`
}

// OwnershipRegistry is an immutable snapshot of target-ownership.yaml. A nil
// registry and an empty one mean the same thing: no platform-side ownership
// facts configured.
type OwnershipRegistry struct {
	byHost map[string]string
}

// Configured reports whether any host is registered. This is the switch between
// "consult the registry first, labels as fallback" and the historical
// labels-only resolution.
func (r *OwnershipRegistry) Configured() bool {
	return r != nil && len(r.byHost) > 0
}

// Lookup returns the registered environment for a hostname, if any.
func (r *OwnershipRegistry) Lookup(hostname string) (string, bool) {
	if r == nil {
		return "", false
	}
	env, ok := r.byHost[strings.ToLower(strings.TrimSpace(hostname))]
	return env, ok
}

// Hostnames returns the registered hostnames in no particular order, for
// startup logging and the consistency report.
func (r *OwnershipRegistry) Hostnames() []string {
	if r == nil {
		return nil
	}
	names := make([]string, 0, len(r.byHost))
	for h := range r.byHost {
		names = append(names, h)
	}
	return names
}

// ownershipFile is the on-disk shape. A top-level `hosts:` list leaves room for
// later keys without a breaking change.
type ownershipFile struct {
	Hosts []HostOwnership `yaml:"hosts"`
}

// LoadOwnership reads dataDir/target-ownership.yaml. A missing file yields an
// empty, usable registry and no error (the optional-file posture above). A file
// that exists but cannot be parsed, or that declares a host without an
// environment, is an error.
func LoadOwnership(dataDir string) (*OwnershipRegistry, error) {
	reg := &OwnershipRegistry{byHost: map[string]string{}}
	if strings.TrimSpace(dataDir) == "" {
		return reg, nil
	}
	path := filepath.Join(dataDir, OwnershipFileName)
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return reg, nil
		}
		return nil, fmt.Errorf("read target ownership: %w", err)
	}
	var f ownershipFile
	if err := yaml.Unmarshal(raw, &f); err != nil {
		return nil, fmt.Errorf("parse %s: %w", OwnershipFileName, err)
	}
	for i, h := range f.Hosts {
		host := strings.ToLower(strings.TrimSpace(h.Hostname))
		env := strings.TrimSpace(h.Environment)
		switch {
		case host == "":
			return nil, fmt.Errorf("%s: hosts[%d] has no hostname", OwnershipFileName, i)
		case env == "":
			return nil, fmt.Errorf("%s: host %q has no environment (its ownership would be unattributable)", OwnershipFileName, h.Hostname)
		}
		if _, dup := reg.byHost[host]; dup {
			return nil, fmt.Errorf("%s: host %q is declared twice (two facts about one host is the ambiguity this file exists to end)", OwnershipFileName, host)
		}
		reg.byHost[host] = env
	}
	return reg, nil
}

// LabelConflict is one host whose registry fact and inventory label disagree.
//
// Which one "wins" at policy time is settled (the registry — see
// grpc.TargetEnvResolver's resolution order); a conflict matters because the
// console's target table and every labels-based filter still SHOW the label,
// so an operator reading the inventory sees "dev" while policy judges "prod".
// Surfacing that gap at startup is what keeps the registry from quietly
// drifting away from the rows it governs.
type LabelConflict struct {
	Hostname    string
	RegistryEnv string
	LabelEnv    string // "" when the row declares none
}

// LabelConflicts compares the registry against an inventory snapshot and
// returns every registered host whose row's labels["env"] disagrees (including
// rows that declare no env at all). Registered hosts with NO inventory row are
// not conflicts — "registered ahead of an import" is the supported flow (see
// grpc's resolver test); the audit is about rows that exist and disagree.
//
// The comparison is by the resolved label value, not by map identity: a row
// with extra labels but the same env is consistent.
func LabelConflicts(reg *OwnershipRegistry, rows []*state.Target) []LabelConflict {
	if reg == nil || !reg.Configured() || len(rows) == 0 {
		return nil
	}
	var out []LabelConflict
	for _, row := range rows {
		if row == nil {
			continue
		}
		regEnv, ok := reg.Lookup(row.Hostname)
		if !ok {
			continue // not ours to judge
		}
		labelEnv := row.Labels[targetEnvLabelKey]
		if labelEnv == regEnv {
			continue
		}
		out = append(out, LabelConflict{
			Hostname:    row.Hostname,
			RegistryEnv: regEnv,
			LabelEnv:    labelEnv,
		})
	}
	return out
}

// EnvConflict is one target declaration whose environment would disagree with
// the platform-side ownership fact.
//
// Row is the declaration's position in the inventory file ("targets[2]"), kept
// because a hostname alone does not tell an operator which line to fix when a
// file bundles many hosts. DeclaredEnv is the value the write would LEAVE on
// the row — for an existing target that is the merged result (see
// mergedLabels), not merely what this file spells out, so a labels-free
// re-import that preserves a stale value is judged like one that writes it.
type EnvConflict struct {
	Row         string
	Host        string
	Port        int
	RegistryEnv string
	DeclaredEnv string
}

// String renders one conflict as an operator-facing sentence. The empty
// declared value is spelled out rather than left as `""`, because "the row
// carries no env at all" is a distinct (and common) way to disagree with the
// registry.
func (c EnvConflict) String() string {
	declared := c.DeclaredEnv
	if declared == "" {
		declared = "(none)"
	}
	prefix := ""
	if c.Row != "" {
		prefix = c.Row + " "
	}
	return fmt.Sprintf("%s%s:%d would carry env %q but %s registers it as %q",
		prefix, c.Host, c.Port, declared, OwnershipFileName, c.RegistryEnv)
}

// FormatEnvConflicts renders a refusal message without letting one bad file
// produce an unbounded error string. The cap is the same "name enough to act,
// count the rest" shape used elsewhere in the repo's refusal messages.
func FormatEnvConflicts(conflicts []EnvConflict) string {
	const max = 5
	parts := make([]string, 0, len(conflicts))
	for i, c := range conflicts {
		if i == max {
			parts = append(parts, fmt.Sprintf("and %d more", len(conflicts)-max))
			break
		}
		parts = append(parts, c.String())
	}
	return strings.Join(parts, "; ")
}

// EnvConflictHosts renders a bounded host:port list for an audit row's Target
// column — the only place the audit schema can name WHICH hosts an override
// covered, since it has no detail column. A large import must not produce an
// unbounded string, so the tail is a count.
func EnvConflictHosts(conflicts []EnvConflict) string {
	const max = 8
	parts := make([]string, 0, len(conflicts))
	for i, c := range conflicts {
		if i == max {
			parts = append(parts, fmt.Sprintf("+%d more", len(conflicts)-max))
			break
		}
		parts = append(parts, fmt.Sprintf("%s:%d", c.Host, c.Port))
	}
	return strings.Join(parts, ",")
}

// EnvConflicts reports every declaration whose resulting env label would
// contradict target-ownership.yaml.
//
// Why the merge is applied first: an import that omits labels still writes
// whatever mergedLabels preserves, so judging only td.Labels would miss the
// stale row a "labels-free" re-import keeps alive. Rows the importer will fail
// (bad address or status) are skipped for the same reason LabelChanges skips
// them: they reach no write.
//
// A nil or empty registry returns nil. No registry means the deployment has
// not opted into platform-side ownership facts, and this check must not invent
// one from labels or the default environment.
func (im *Importer) EnvConflicts(ctx context.Context, reg *OwnershipRegistry, f *File) ([]EnvConflict, error) {
	if f == nil {
		return nil, fmt.Errorf("inventory: nil file")
	}
	if !reg.Configured() {
		return nil, nil
	}
	var out []EnvConflict
	for i, td := range f.Targets {
		host, port, err := targetAddressFor(td)
		if err != nil {
			continue
		}
		regEnv, ok := reg.Lookup(host)
		if !ok {
			continue // the registry does not declare this host; labels remain the only fact
		}
		status := td.Status
		if status == "" {
			status = state.StatusActive
		}
		if !validStatuses[status] {
			continue
		}
		// A row the importer will reject for its channel reaches no write
		// either; reporting it as an ownership conflict would blame the wrong
		// field and block the rest of the file.
		channel := td.ChannelType
		if channel == "" {
			channel = "ssh"
		}
		if !validChannels[channel] {
			continue
		}
		resulting := td.Labels
		existing, err := im.store.FindTargetByAddress(ctx, host, port)
		if err != nil {
			return nil, err
		}
		if existing != nil {
			resulting = mergedLabels(td.Labels, existing.Labels, status, existing.Status)
		}
		declared := resulting[targetEnvLabelKey]
		if declared == regEnv {
			continue
		}
		out = append(out, EnvConflict{
			Row:         fmt.Sprintf("targets[%d]", i),
			Host:        host,
			Port:        port,
			RegistryEnv: regEnv,
			DeclaredEnv: declared,
		})
	}
	return out, nil
}

// targetEnvLabelKey mirrors the label the resolver reads
// (grpc.targetEnvLabelKey). It is re-declared here rather than imported
// because grpc imports inventory, not the other way — and one string with a
// test pinning both spellings is cheaper than an import cycle.
const targetEnvLabelKey = "env"
