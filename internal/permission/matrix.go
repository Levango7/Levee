// Package permission implements the team × environment permission matrix
// for LEVEE. It defines which team can perform which actions on which
// environment, loaded from a YAML or JSON configuration file.
//
// The matrix is two-dimensional:
//   - Team: e.g. sre, dba, network, security, platform
//   - Environment: e.g. dev, staging, prod, emergency
//
// Each (team, env) pair maps to a set of allowed actions. The matrix
// supports wildcards ("*") for both team and env, an admin super-set
// rule, and explicit revoke that takes precedence over grant.
package permission

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"sync"

	"gopkg.in/yaml.v3"
)

// Supported actions. These are the operations that can be authorised by
// the permission matrix.
const (
	ActionPlan      = "plan"
	ActionApply     = "apply"
	ActionApprove   = "approve"
	ActionRollback  = "rollback"
	ActionPause     = "pause"
	ActionResume    = "resume"
	ActionPauseAll  = "pause_all"
	ActionResumeAll = "resume_all"
	ActionCancel    = "cancel"
	ActionView      = "view"
	ActionAdmin     = "admin"
)

// Wildcard matches all teams or all environments when used as the team
// or env argument in Grant/Revoke, or when present in a config file.
const Wildcard = "*"

// AllActions lists every action recognised by the matrix, including
// ActionAdmin. It is used to expand the admin super-set rule.
var AllActions = []string{
	ActionPlan,
	ActionApply,
	ActionApprove,
	ActionRollback,
	ActionPause,
	ActionResume,
	ActionPauseAll,
	ActionResumeAll,
	ActionCancel,
	ActionView,
	ActionAdmin,
}

// Sentinel errors returned by the permission matrix.
var (
	ErrEmptyTeam     = errors.New("permission: empty team")
	ErrEmptyEnv      = errors.New("permission: empty environment")
	ErrEmptyAction   = errors.New("permission: empty action")
	ErrConfigInvalid = errors.New("permission: invalid config")
	ErrUnknownTeam   = errors.New("permission: unknown team")
	ErrUnknownEnv    = errors.New("permission: unknown environment")
)

// PermissionMatrix is the team × environment permission matrix. It
// defines which team can perform which actions on which environment.
// The matrix is loaded from a configuration file (YAML or JSON) and is
// purely in-memory.
//
// Resolution order for Allow(team, env, action):
//  1. If the action is explicitly revoked (directly or via wildcard),
//     return false.
//  2. If the action is explicitly granted (directly or via wildcard),
//     return true.
//  3. If admin is granted (directly or via wildcard) and action is not
//     admin, return true (admin super-set).
//  4. Otherwise return false.
type PermissionMatrix struct {
	// mu guards grants and revokes against concurrent reads and writes.
	// Read operations (Allow, ActionsFor, Teams, Environments) acquire the
	// read lock; write operations (Grant, Revoke, LoadFromConfig) acquire
	// the write lock. The mutex is a named field (not embedded) to keep
	// the locking surface explicit and avoid leaking Lock/Unlock methods
	// on the public API.
	mu sync.RWMutex
	// grants tracks explicitly granted permissions:
	// team → env → action → true.
	grants map[string]map[string]map[string]bool
	// revokes tracks explicitly revoked permissions:
	// team → env → action → true. Revokes take precedence over grants.
	revokes map[string]map[string]map[string]bool
}

// PermissionConfig is the configuration file format for the permission
// matrix. It is a list of team rules, each containing a list of
// environment permissions.
type PermissionConfig struct {
	Teams []TeamRule `yaml:"teams" json:"teams"`
}

// TeamRule defines the permissions for a single team across one or more
// environments.
type TeamRule struct {
	Name         string          `yaml:"name" json:"name"`
	Environments []EnvPermission `yaml:"environments" json:"environments"`
}

// EnvPermission lists the actions allowed for a team on a single
// environment.
type EnvPermission struct {
	Name    string   `yaml:"name" json:"name"`
	Actions []string `yaml:"actions" json:"actions"`
}

// NewPermissionMatrix creates an empty permission matrix ready to be
// populated via LoadFromConfig, LoadFromYAML, LoadFromJSON, or
// Grant/Revoke calls.
func NewPermissionMatrix() *PermissionMatrix {
	return &PermissionMatrix{
		grants:  make(map[string]map[string]map[string]bool),
		revokes: make(map[string]map[string]map[string]bool),
	}
}

// LoadFromConfig populates the matrix from a PermissionConfig struct.
// It resets any existing rules in the matrix (both grants and revokes)
// before loading. Only grants are populated from the config; use Revoke
// to add explicit denials after loading.
func (m *PermissionMatrix) LoadFromConfig(cfg PermissionConfig) error {
	if len(cfg.Teams) == 0 {
		return fmt.Errorf("%w: no teams defined", ErrConfigInvalid)
	}

	// Build the new grants map outside the critical section to minimise
	// the time the write lock is held. Revokes are always reset on load.
	newGrants := make(map[string]map[string]map[string]bool)
	for _, team := range cfg.Teams {
		if team.Name == "" {
			return fmt.Errorf("%w: team name is empty", ErrConfigInvalid)
		}
		for _, env := range team.Environments {
			if env.Name == "" {
				return fmt.Errorf("%w: environment name is empty for team %q", ErrConfigInvalid, team.Name)
			}
			for _, action := range env.Actions {
				if action == "" {
					return fmt.Errorf("%w: action is empty for team %q env %q", ErrConfigInvalid, team.Name, env.Name)
				}
				if newGrants[team.Name] == nil {
					newGrants[team.Name] = make(map[string]map[string]bool)
				}
				if newGrants[team.Name][env.Name] == nil {
					newGrants[team.Name][env.Name] = make(map[string]bool)
				}
				newGrants[team.Name][env.Name][action] = true
			}
		}
	}

	// Swap in the rebuilt maps under the write lock.
	m.mu.Lock()
	defer m.mu.Unlock()
	m.grants = newGrants
	m.revokes = make(map[string]map[string]map[string]bool)
	return nil
}

// LoadFromYAML loads the matrix from a YAML file at the given path. The
// file must have the structure described by PermissionConfig.
func (m *PermissionMatrix) LoadFromYAML(path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read yaml: %w", err)
	}
	var cfg PermissionConfig
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return fmt.Errorf("unmarshal yaml: %w", err)
	}
	return m.LoadFromConfig(cfg)
}

// LoadFromJSON loads the matrix from a JSON byte slice. The JSON must
// have the structure described by PermissionConfig.
func (m *PermissionMatrix) LoadFromJSON(data []byte) error {
	var cfg PermissionConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		return fmt.Errorf("unmarshal json: %w", err)
	}
	return m.LoadFromConfig(cfg)
}

// Allow reports whether the given team is allowed to perform the given
// action on the given environment. The check respects wildcards
// (team="*" or env="*" in the rule set) and the admin super-set rule.
//
// Empty team, env, or action always returns false.
func (m *PermissionMatrix) Allow(team, env, action string) bool {
	if team == "" || env == "" || action == "" {
		return false
	}

	m.mu.RLock()
	defer m.mu.RUnlock()

	// 1. Explicit revoke takes precedence over everything.
	if m.lookup(m.revokes, team, env, action) {
		return false
	}

	// 2. Explicit grant.
	if m.lookup(m.grants, team, env, action) {
		return true
	}

	// 3. Admin super-set: if the team has admin on the env (directly or
	//    via wildcard), all non-admin actions are allowed — unless the
	//    specific action was explicitly revoked (checked in step 1).
	if action != ActionAdmin && m.lookup(m.grants, team, env, ActionAdmin) {
		return true
	}

	return false
}

// lookup checks the given rule set for a match, honouring wildcards. It
// checks four combinations in order: exact match, wildcard team, wildcard
// env, and wildcard both. Returns true if any combination matches.
func (m *PermissionMatrix) lookup(rules map[string]map[string]map[string]bool, team, env, action string) bool {
	candidates := [4]struct{ t, e string }{
		{team, env},
		{Wildcard, env},
		{team, Wildcard},
		{Wildcard, Wildcard},
	}
	for _, c := range candidates {
		if envs, ok := rules[c.t]; ok {
			if acts, ok2 := envs[c.e]; ok2 {
				if acts[action] {
					return true
				}
			}
		}
	}
	return false
}

// AllowAny reports whether the team is allowed to perform at least one of
// the given actions on the environment. Returns false if no actions are
// provided.
func (m *PermissionMatrix) AllowAny(team, env string, actions ...string) bool {
	for _, a := range actions {
		if m.Allow(team, env, a) {
			return true
		}
	}
	return false
}

// AllowAll reports whether the team is allowed to perform all of the
// given actions on the environment. Returns false if no actions are
// provided.
func (m *PermissionMatrix) AllowAll(team, env string, actions ...string) bool {
	if len(actions) == 0 {
		return false
	}
	for _, a := range actions {
		if !m.Allow(team, env, a) {
			return false
		}
	}
	return true
}

// ActionsFor returns the sorted list of actions allowed for the team on
// the environment. It expands the admin super-set: if the team has admin
// (directly or via wildcard), all actions are included except those
// explicitly revoked.
//
// Returns nil if team or env is empty.
func (m *PermissionMatrix) ActionsFor(team, env string) []string {
	if team == "" || env == "" {
		return nil
	}

	m.mu.RLock()
	defer m.mu.RUnlock()

	seen := make(map[string]bool)

	// Collect directly granted actions (including via wildcards).
	for _, action := range m.collectGrants(team, env) {
		if !m.lookup(m.revokes, team, env, action) {
			seen[action] = true
		}
	}

	// Admin super-set expansion: if admin is granted, all actions are
	// allowed except those explicitly revoked.
	if m.lookup(m.grants, team, env, ActionAdmin) {
		for _, a := range AllActions {
			if !m.lookup(m.revokes, team, env, a) {
				seen[a] = true
			}
		}
	}

	result := make([]string, 0, len(seen))
	for a := range seen {
		result = append(result, a)
	}
	sort.Strings(result)
	return result
}

// collectGrants returns all actions explicitly granted to the team on the
// env, honouring wildcards. Duplicates are removed.
func (m *PermissionMatrix) collectGrants(team, env string) []string {
	var result []string
	seen := make(map[string]bool)

	candidates := [4]struct{ t, e string }{
		{team, env},
		{Wildcard, env},
		{team, Wildcard},
		{Wildcard, Wildcard},
	}
	for _, c := range candidates {
		if envs, ok := m.grants[c.t]; ok {
			if acts, ok2 := envs[c.e]; ok2 {
				for a := range acts {
					if !seen[a] {
						seen[a] = true
						result = append(result, a)
					}
				}
			}
		}
	}
	return result
}

// Teams returns the sorted list of all team names defined in the matrix
// (excluding the wildcard "*"). Teams from both grants and revokes are
// included.
func (m *PermissionMatrix) Teams() []string {
	m.mu.RLock()
	defer m.mu.RUnlock()

	seen := make(map[string]bool)
	for _, rules := range []map[string]map[string]map[string]bool{m.grants, m.revokes} {
		for t := range rules {
			if t != Wildcard {
				seen[t] = true
			}
		}
	}
	result := make([]string, 0, len(seen))
	for t := range seen {
		result = append(result, t)
	}
	sort.Strings(result)
	return result
}

// Environments returns the sorted list of all environment names defined
// in the matrix (excluding the wildcard "*"). Environments from both
// grants and revokes are included.
func (m *PermissionMatrix) Environments() []string {
	m.mu.RLock()
	defer m.mu.RUnlock()

	seen := make(map[string]bool)
	for _, rules := range []map[string]map[string]map[string]bool{m.grants, m.revokes} {
		for _, envs := range rules {
			for e := range envs {
				if e != Wildcard {
					seen[e] = true
				}
			}
		}
	}
	result := make([]string, 0, len(seen))
	for e := range seen {
		result = append(result, e)
	}
	sort.Strings(result)
	return result
}

// Grant grants the action to the team on the environment. Use the
// Wildcard ("*") for team or env to grant broadly. Empty team, env, or
// action is ignored.
func (m *PermissionMatrix) Grant(team, env, action string) {
	if team == "" || env == "" || action == "" {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.grant(team, env, action)
}

// grant is the internal helper that adds a grant entry without
// validation.
func (m *PermissionMatrix) grant(team, env, action string) {
	if m.grants[team] == nil {
		m.grants[team] = make(map[string]map[string]bool)
	}
	if m.grants[team][env] == nil {
		m.grants[team][env] = make(map[string]bool)
	}
	m.grants[team][env][action] = true
}

// Revoke revokes the action from the team on the environment. A revoke
// takes precedence over a grant, even a wildcard grant. Use the
// Wildcard ("*") for team or env to revoke broadly. Empty team, env, or
// action is ignored.
func (m *PermissionMatrix) Revoke(team, env, action string) {
	if team == "" || env == "" || action == "" {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.revoke(team, env, action)
}

// revoke is the internal helper that adds a revoke entry without
// validation.
func (m *PermissionMatrix) revoke(team, env, action string) {
	if m.revokes[team] == nil {
		m.revokes[team] = make(map[string]map[string]bool)
	}
	if m.revokes[team][env] == nil {
		m.revokes[team][env] = make(map[string]bool)
	}
	m.revokes[team][env][action] = true
}
