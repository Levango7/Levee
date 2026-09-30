// Package permission role_tree.go implements the role inheritance tree that
// backs hierarchical RBAC for LEVEE. A role may optionally declare a parent
// role; permissions granted to a parent are transitively effective for every
// child. The tree detects cycles on insertion so an invalid configuration
// cannot silently produce infinite permission sets.
//
// The tree is independent of PermissionMatrix: callers may use it standalone
// to compute effective permissions, or wire it in front of a matrix via the
// EffectivePermissions helper. The structure is safe for concurrent reads
// after construction; mutations are guarded by an RWMutex.
package permission

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"gopkg.in/yaml.v3"
)

// Sentinel errors returned by the role tree.
var (
	// ErrEmptyRole is returned when a role name is empty.
	ErrEmptyRole = errors.New("permission: empty role")

	// ErrUnknownRole is returned when a role is not present in the tree.
	ErrUnknownRole = errors.New("permission: unknown role")

	// ErrCycleDetected is returned when adding a parent would create a
	// cycle in the inheritance graph.
	ErrCycleDetected = errors.New("permission: cycle detected")

	// ErrSelfParent is returned when a role is declared as its own parent.
	ErrSelfParent = errors.New("permission: self parent")
)

// RoleTree is a directed acyclic graph of roles where edges point from a
// child role to its parent. A role may have at most one parent (single
// inheritance) but multiple children may share the same parent. The tree
// supports transitive ancestor/descendant queries and effective-permission
// merging.
//
// Permissions are stored as a set of action strings per role. The
// EffectivePermissions method returns the union of permissions granted to
// the role itself and to every ancestor reachable from it.
//
// All public methods are safe for concurrent use.
type RoleTree struct {
	mu sync.RWMutex
	// parent maps child role -> direct parent role. A role with no parent
	// either has no entry or maps to the empty string.
	parent map[string]string
	// children maps parent role -> set of direct child roles. Kept in sync
	// with parent so descendant queries do not need to scan the whole map.
	children map[string]map[string]bool
	// perms maps role -> set of action strings directly granted to that
	// role (excluding inherited permissions).
	perms map[string]map[string]bool
}

// NewRoleTree returns an empty role tree ready to be populated via AddRole
// and GrantPermission.
func NewRoleTree() *RoleTree {
	return &RoleTree{
		parent:   make(map[string]string),
		children: make(map[string]map[string]bool),
		perms:    make(map[string]map[string]bool),
	}
}

// AddRole adds a role to the tree. If parent is non-empty the role is
// declared as a child of parent; the parent role is created implicitly if
// it does not yet exist. AddRole returns an error when:
//   - name is empty (ErrEmptyRole)
//   - name == parent (ErrSelfParent)
//   - adding the edge would create a cycle (ErrCycleDetected)
//
// Re-adding an existing role with the same parent is a no-op. Re-adding
// with a different parent is allowed and re-parents the role, subject to
// the same cycle check.
func (t *RoleTree) AddRole(name, parent string) error {
	if name == "" {
		return ErrEmptyRole
	}
	if parent != "" && name == parent {
		return ErrSelfParent
	}

	t.mu.Lock()
	defer t.mu.Unlock()

	// If the role already exists with the same parent, this is a no-op.
	if existing, ok := t.parent[name]; ok && existing == parent {
		return nil
	}

	// Cycle check: if parent is non-empty, parent must not be reachable
	// from name (i.e. name must not already be an ancestor of parent).
	if parent != "" {
		if reachable(t.parent, parent, name) {
			return fmt.Errorf("%w: role %q is already an ancestor of %q", ErrCycleDetected, name, parent)
		}
	}

	// Detach from previous parent if any.
	if old, ok := t.parent[name]; ok && old != "" {
		if set, ok2 := t.children[old]; ok2 {
			delete(set, name)
		}
	}

	t.parent[name] = parent
	if parent != "" {
		if t.children[parent] == nil {
			t.children[parent] = make(map[string]bool)
		}
		t.children[parent][name] = true
		// Ensure the parent role has a perms entry so it is observable
		// even before any permission is granted to it.
		if t.perms[parent] == nil {
			t.perms[parent] = make(map[string]bool)
		}
	}
	if t.perms[name] == nil {
		t.perms[name] = make(map[string]bool)
	}
	return nil
}

// reachable reports whether target is reachable from start by following
// parent edges. It does not take the lock; callers must hold it. The walk
// is bounded by the number of roles so it always terminates.
func reachable(parent map[string]string, start, target string) bool {
	cur := start
	for i := 0; i < len(parent)+1; i++ {
		if cur == target {
			return true
		}
		next, ok := parent[cur]
		if !ok || next == "" {
			return false
		}
		cur = next
	}
	return false
}

// HasRole reports whether the role exists in the tree.
func (t *RoleTree) HasRole(name string) bool {
	t.mu.RLock()
	defer t.mu.RUnlock()
	_, ok := t.perms[name]
	return ok
}

// Roles returns the sorted list of all role names in the tree.
func (t *RoleTree) Roles() []string {
	t.mu.RLock()
	defer t.mu.RUnlock()
	result := make([]string, 0, len(t.perms))
	for k := range t.perms {
		result = append(result, k)
	}
	sort.Strings(result)
	return result
}

// Parent returns the direct parent of the role, or the empty string when
// the role has no parent. Returns ErrUnknownRole when the role is absent.
func (t *RoleTree) Parent(name string) (string, error) {
	t.mu.RLock()
	defer t.mu.RUnlock()
	if _, ok := t.perms[name]; !ok {
		return "", fmt.Errorf("%w: role %q", ErrUnknownRole, name)
	}
	return t.parent[name], nil
}

// Children returns the sorted list of direct children of the role. Returns
// ErrUnknownRole when the role is absent.
func (t *RoleTree) Children(name string) ([]string, error) {
	t.mu.RLock()
	defer t.mu.RUnlock()
	if _, ok := t.perms[name]; !ok {
		return nil, fmt.Errorf("%w: role %q", ErrUnknownRole, name)
	}
	return sortedKeys(t.children[name]), nil
}

// GetAncestors returns the sorted list of all ancestors of the role
// (parent, grandparent, ...). The walk is transitive and terminates
// because the tree is acyclic. Returns ErrUnknownRole when the role is
// absent.
func (t *RoleTree) GetAncestors(name string) ([]string, error) {
	t.mu.RLock()
	defer t.mu.RUnlock()
	if _, ok := t.perms[name]; !ok {
		return nil, fmt.Errorf("%w: role %q", ErrUnknownRole, name)
	}

	seen := make(map[string]bool)
	cur := t.parent[name]
	for cur != "" {
		if seen[cur] {
			// Defensive: should never happen because cycles are
			// rejected on insertion.
			break
		}
		seen[cur] = true
		next, ok := t.parent[cur]
		if !ok {
			break
		}
		cur = next
	}
	return sortedKeys(seen), nil
}

// GetDescendants returns the sorted list of all descendants of the role
// (children, grandchildren, ...). The walk is breadth-first and
// transitive. Returns ErrUnknownRole when the role is absent.
func (t *RoleTree) GetDescendants(name string) ([]string, error) {
	t.mu.RLock()
	defer t.mu.RUnlock()
	if _, ok := t.perms[name]; !ok {
		return nil, fmt.Errorf("%w: role %q", ErrUnknownRole, name)
	}

	seen := make(map[string]bool)
	queue := []string{name}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		for child := range t.children[cur] {
			if seen[child] {
				continue
			}
			seen[child] = true
			queue = append(queue, child)
		}
	}
	return sortedKeys(seen), nil
}

// GrantPermission grants one or more actions directly to the role. The
// role is created implicitly if it does not yet exist. Empty actions are
// ignored.
func (t *RoleTree) GrantPermission(role string, actions ...string) error {
	if role == "" {
		return ErrEmptyRole
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.perms[role] == nil {
		t.perms[role] = make(map[string]bool)
	}
	for _, a := range actions {
		if a == "" {
			continue
		}
		t.perms[role][a] = true
	}
	return nil
}

// RevokePermission removes one or more actions directly granted to the
// role. Inherited permissions are not affected. Unknown actions are
// ignored.
func (t *RoleTree) RevokePermission(role string, actions ...string) error {
	if role == "" {
		return ErrEmptyRole
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if _, ok := t.perms[role]; !ok {
		return fmt.Errorf("%w: role %q", ErrUnknownRole, role)
	}
	for _, a := range actions {
		delete(t.perms[role], a)
	}
	return nil
}

// DirectPermissions returns the sorted list of actions directly granted
// to the role (excluding inherited permissions). Returns ErrUnknownRole
// when the role is absent.
func (t *RoleTree) DirectPermissions(role string) ([]string, error) {
	t.mu.RLock()
	defer t.mu.RUnlock()
	set, ok := t.perms[role]
	if !ok {
		return nil, fmt.Errorf("%w: role %q", ErrUnknownRole, role)
	}
	return sortedKeys(set), nil
}

// EffectivePermissions returns the sorted union of permissions directly
// granted to the role and to every ancestor reachable from it. This is
// the core of hierarchical RBAC: a child inherits everything its parent
// chain grants.
//
// Returns ErrUnknownRole when the role is absent.
func (t *RoleTree) EffectivePermissions(role string) ([]string, error) {
	t.mu.RLock()
	defer t.mu.RUnlock()
	if _, ok := t.perms[role]; !ok {
		return nil, fmt.Errorf("%w: role %q", ErrUnknownRole, role)
	}

	merged := make(map[string]bool)
	// Start from the role itself and walk up the parent chain.
	cur := role
	for i := 0; i < len(t.parent)+1; i++ {
		for a := range t.perms[cur] {
			merged[a] = true
		}
		next, ok := t.parent[cur]
		if !ok || next == "" {
			break
		}
		cur = next
	}
	return sortedKeys(merged), nil
}

// RemoveRole removes the role from the tree. Children of the role are
// re-parented to the removed role's parent so they continue to inherit
// from the grandparent. Returns ErrUnknownRole when the role is absent.
func (t *RoleTree) RemoveRole(name string) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if _, ok := t.perms[name]; !ok {
		return fmt.Errorf("%w: role %q", ErrUnknownRole, name)
	}

	parent := t.parent[name]
	// Re-parent children.
	for child := range t.children[name] {
		t.parent[child] = parent
		if parent != "" {
			if t.children[parent] == nil {
				t.children[parent] = make(map[string]bool)
			}
			t.children[parent][child] = true
		}
	}
	delete(t.children, name)
	if parent != "" {
		if set, ok := t.children[parent]; ok {
			delete(set, name)
		}
	}
	delete(t.parent, name)
	delete(t.perms, name)
	return nil
}

// Render returns a human-readable indented view of the tree. Roots
// (roles without a parent) are listed first, each followed by its
// descendants indented by two spaces per level. The output is sorted
// for determinism.
func (t *RoleTree) Render() string {
	t.mu.RLock()
	defer t.mu.RUnlock()

	// Find roots: roles whose parent is empty.
	roots := make([]string, 0)
	for r := range t.perms {
		if t.parent[r] == "" {
			roots = append(roots, r)
		}
	}
	sort.Strings(roots)

	var b strings.Builder
	for _, r := range roots {
		renderNode(&b, t, r, 0)
	}
	return b.String()
}

// renderNode writes a single node and recurses into its children. The
// caller holds the read lock.
func renderNode(b *strings.Builder, t *RoleTree, role string, depth int) {
	for i := 0; i < depth; i++ {
		b.WriteString("  ")
	}
	b.WriteString(role)
	actions := sortedKeys(t.perms[role])
	if len(actions) > 0 {
		b.WriteString(" [")
		b.WriteString(strings.Join(actions, ", "))
		b.WriteString("]")
	}
	b.WriteString("\n")
	for _, child := range sortedKeys(t.children[role]) {
		renderNode(b, t, child, depth+1)
	}
}

// sortedKeys returns the sorted keys of a string set map. nil maps yield
// an empty (non-nil) slice so callers can use the result without a nil
// check.
func sortedKeys(m map[string]bool) []string {
	result := make([]string, 0, len(m))
	for k := range m {
		result = append(result, k)
	}
	sort.Strings(result)
	return result
}

// --- YAML loading -----------------------------------------------------------

// roleTreeYAML is the on-disk shape of roles.yaml. It lives here rather than in
// the CLI because the serving process must load the same file the CLI writes:
// a loader in package main is unreachable from internal/*, which is exactly how
// the role tree ended up unused at runtime.
type roleTreeYAML struct {
	Roles []roleYAMLEntry `yaml:"roles"`
}

type roleYAMLEntry struct {
	Name        string   `yaml:"name"`
	Parent      string   `yaml:"parent,omitempty"`
	Permissions []string `yaml:"permissions,omitempty"`
}

// SaveToYAML writes the tree to path with 0600, creating the parent
// directory. Direct permissions only — inheritance is expressed by Parent, so
// a round-trip does not flatten the hierarchy into duplicated grants.
func (t *RoleTree) SaveToYAML(path string) error {
	t.mu.RLock()
	roles := make([]string, 0, len(t.perms))
	for r := range t.perms {
		roles = append(roles, r)
	}
	t.mu.RUnlock()
	sort.Strings(roles)

	cfg := roleTreeYAML{Roles: make([]roleYAMLEntry, 0, len(roles))}
	for _, r := range roles {
		parent, _ := t.Parent(r)
		direct, _ := t.DirectPermissions(r)
		cfg.Roles = append(cfg.Roles, roleYAMLEntry{Name: r, Parent: parent, Permissions: direct})
	}

	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return fmt.Errorf("create role tree dir: %w", err)
	}
	data, err := yaml.Marshal(cfg)
	if err != nil {
		return fmt.Errorf("marshal role tree: %w", err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return fmt.Errorf("write role tree: %w", err)
	}
	return nil
}

// LoadFromYAML replaces the tree with the roles declared in the file at path.
// A missing file leaves the tree empty and returns no error, so callers can
// treat "never configured" as a normal state.
//
// Roles are added in dependency order (a parent before any child that names
// it) rather than file order, so authors may list children first. Entries that
// still cannot be added after a full pass — a parent that never appears —
// surface as an error instead of a silently truncated tree.
func (t *RoleTree) LoadFromYAML(path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("read role tree: %w", err)
	}
	var cfg roleTreeYAML
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return fmt.Errorf("unmarshal role tree: %w", err)
	}

	added := make(map[string]bool)
	for pass := 0; pass < len(cfg.Roles)+1; pass++ {
		progress := false
		for _, r := range cfg.Roles {
			if added[r.Name] {
				continue
			}
			if r.Parent != "" && !added[r.Parent] {
				continue
			}
			if err := t.AddRole(r.Name, r.Parent); err != nil {
				return fmt.Errorf("add role %q: %w", r.Name, err)
			}
			if len(r.Permissions) > 0 {
				if err := t.GrantPermission(r.Name, r.Permissions...); err != nil {
					return fmt.Errorf("grant permissions for %q: %w", r.Name, err)
				}
			}
			added[r.Name] = true
			progress = true
		}
		if !progress {
			break
		}
	}
	for _, r := range cfg.Roles {
		if !added[r.Name] {
			return fmt.Errorf("role %q names parent %q, which is not declared", r.Name, r.Parent)
		}
	}
	return nil
}
