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
// override it.
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
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/nexus/levee/internal/state"
	"gopkg.in/yaml.v3"
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

// targetEnvLabelKey mirrors the label the resolver reads
// (grpc.targetEnvLabelKey). It is re-declared here rather than imported
// because grpc imports inventory, not the other way — and one string with a
// test pinning both spellings is cheaper than an import cycle.
const targetEnvLabelKey = "env"
