package permission

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNewRoleTree(t *testing.T) {
	tree := NewRoleTree()
	require.NotNil(t, tree)
	assert.Empty(t, tree.Roles())
}

func TestRoleTreeAddRole(t *testing.T) {
	tree := NewRoleTree()
	require.NoError(t, tree.AddRole("admin", ""))
	require.NoError(t, tree.AddRole("sre", "admin"))
	require.NoError(t, tree.AddRole("dev", "sre"))

	assert.True(t, tree.HasRole("admin"))
	assert.True(t, tree.HasRole("sre"))
	assert.True(t, tree.HasRole("dev"))
	assert.False(t, tree.HasRole("nonexistent"))
}

func TestRoleTreeAddRoleEmpty(t *testing.T) {
	tree := NewRoleTree()
	err := tree.AddRole("", "")
	assert.ErrorIs(t, err, ErrEmptyRole)
}

func TestRoleTreeSelfParent(t *testing.T) {
	tree := NewRoleTree()
	err := tree.AddRole("x", "x")
	assert.ErrorIs(t, err, ErrSelfParent)
}

func TestRoleTreeCycleDetection(t *testing.T) {
	tree := NewRoleTree()
	require.NoError(t, tree.AddRole("a", ""))
	require.NoError(t, tree.AddRole("b", "a"))
	require.NoError(t, tree.AddRole("c", "b"))

	// Adding a as a child of c would create a cycle: a -> c -> b -> a.
	err := tree.AddRole("a", "c")
	assert.ErrorIs(t, err, ErrCycleDetected)
}

func TestRoleTreeCycleDetectionLonger(t *testing.T) {
	tree := NewRoleTree()
	require.NoError(t, tree.AddRole("root", ""))
	require.NoError(t, tree.AddRole("mid", "root"))
	require.NoError(t, tree.AddRole("leaf", "mid"))

	// root -> leaf -> mid -> root would cycle.
	err := tree.AddRole("root", "leaf")
	assert.ErrorIs(t, err, ErrCycleDetected)
}

func TestRoleTreeReAddSameParent(t *testing.T) {
	tree := NewRoleTree()
	require.NoError(t, tree.AddRole("a", ""))
	require.NoError(t, tree.AddRole("b", "a"))
	// Re-adding b with the same parent is a no-op.
	require.NoError(t, tree.AddRole("b", "a"))
	parent, err := tree.Parent("b")
	require.NoError(t, err)
	assert.Equal(t, "a", parent)
}

func TestRoleTreeReParent(t *testing.T) {
	tree := NewRoleTree()
	require.NoError(t, tree.AddRole("a", ""))
	require.NoError(t, tree.AddRole("b", ""))
	require.NoError(t, tree.AddRole("c", "a"))

	// Re-parent c to b.
	require.NoError(t, tree.AddRole("c", "b"))
	parent, err := tree.Parent("c")
	require.NoError(t, err)
	assert.Equal(t, "b", parent)

	// a should no longer have c as a child.
	children, err := tree.Children("a")
	require.NoError(t, err)
	assert.NotContains(t, children, "c")

	// b should now have c as a child.
	children, err = tree.Children("b")
	require.NoError(t, err)
	assert.Contains(t, children, "c")
}

func TestRoleTreeGetAncestors(t *testing.T) {
	tree := NewRoleTree()
	require.NoError(t, tree.AddRole("admin", ""))
	require.NoError(t, tree.AddRole("sre", "admin"))
	require.NoError(t, tree.AddRole("dev", "sre"))

	ancestors, err := tree.GetAncestors("dev")
	require.NoError(t, err)
	assert.Equal(t, []string{"admin", "sre"}, ancestors)

	ancestors, err = tree.GetAncestors("admin")
	require.NoError(t, err)
	assert.Empty(t, ancestors)
}

func TestRoleTreeGetDescendants(t *testing.T) {
	tree := NewRoleTree()
	require.NoError(t, tree.AddRole("admin", ""))
	require.NoError(t, tree.AddRole("sre", "admin"))
	require.NoError(t, tree.AddRole("dba", "admin"))
	require.NoError(t, tree.AddRole("dev", "sre"))

	desc, err := tree.GetDescendants("admin")
	require.NoError(t, err)
	assert.Equal(t, []string{"dba", "dev", "sre"}, desc)

	desc, err = tree.GetDescendants("sre")
	require.NoError(t, err)
	assert.Equal(t, []string{"dev"}, desc)

	desc, err = tree.GetDescendants("dev")
	require.NoError(t, err)
	assert.Empty(t, desc)
}

func TestRoleTreeEffectivePermissions(t *testing.T) {
	tree := NewRoleTree()
	require.NoError(t, tree.AddRole("admin", ""))
	require.NoError(t, tree.AddRole("sre", "admin"))
	require.NoError(t, tree.AddRole("dev", "sre"))

	require.NoError(t, tree.GrantPermission("admin", "view", "audit"))
	require.NoError(t, tree.GrantPermission("sre", "apply", "rollback"))
	require.NoError(t, tree.GrantPermission("dev", "plan"))

	// dev inherits from sre and admin.
	eff, err := tree.EffectivePermissions("dev")
	require.NoError(t, err)
	assert.Equal(t, []string{"apply", "audit", "plan", "rollback", "view"}, eff)

	// sre inherits from admin only.
	eff, err = tree.EffectivePermissions("sre")
	require.NoError(t, err)
	assert.Equal(t, []string{"apply", "audit", "rollback", "view"}, eff)

	// admin has only its own.
	eff, err = tree.EffectivePermissions("admin")
	require.NoError(t, err)
	assert.Equal(t, []string{"audit", "view"}, eff)
}

func TestRoleTreeDirectPermissions(t *testing.T) {
	tree := NewRoleTree()
	require.NoError(t, tree.AddRole("sre", ""))
	require.NoError(t, tree.GrantPermission("sre", "apply", "view"))

	direct, err := tree.DirectPermissions("sre")
	require.NoError(t, err)
	assert.Equal(t, []string{"apply", "view"}, direct)

	require.NoError(t, tree.RevokePermission("sre", "view"))
	direct, err = tree.DirectPermissions("sre")
	require.NoError(t, err)
	assert.Equal(t, []string{"apply"}, direct)
}

func TestRoleTreeUnknownRole(t *testing.T) {
	tree := NewRoleTree()
	require.NoError(t, tree.AddRole("sre", ""))

	_, err := tree.Parent("nonexistent")
	assert.ErrorIs(t, err, ErrUnknownRole)

	_, err = tree.GetAncestors("nonexistent")
	assert.ErrorIs(t, err, ErrUnknownRole)

	_, err = tree.GetDescendants("nonexistent")
	assert.ErrorIs(t, err, ErrUnknownRole)

	_, err = tree.EffectivePermissions("nonexistent")
	assert.ErrorIs(t, err, ErrUnknownRole)

	_, err = tree.DirectPermissions("nonexistent")
	assert.ErrorIs(t, err, ErrUnknownRole)
}

func TestRoleTreeRemoveRole(t *testing.T) {
	tree := NewRoleTree()
	require.NoError(t, tree.AddRole("admin", ""))
	require.NoError(t, tree.AddRole("sre", "admin"))
	require.NoError(t, tree.AddRole("dev", "sre"))
	require.NoError(t, tree.GrantPermission("admin", "view"))
	require.NoError(t, tree.GrantPermission("sre", "apply"))

	// Remove sre: dev should be re-parented to admin.
	require.NoError(t, tree.RemoveRole("sre"))
	assert.False(t, tree.HasRole("sre"))

	parent, err := tree.Parent("dev")
	require.NoError(t, err)
	assert.Equal(t, "admin", parent)

	// dev should still inherit admin's view.
	eff, err := tree.EffectivePermissions("dev")
	require.NoError(t, err)
	assert.Contains(t, eff, "view")
}

func TestRoleTreeRemoveUnknownRole(t *testing.T) {
	tree := NewRoleTree()
	err := tree.RemoveRole("nonexistent")
	assert.ErrorIs(t, err, ErrUnknownRole)
}

func TestRoleTreeChildren(t *testing.T) {
	tree := NewRoleTree()
	require.NoError(t, tree.AddRole("admin", ""))
	require.NoError(t, tree.AddRole("sre", "admin"))
	require.NoError(t, tree.AddRole("dba", "admin"))

	children, err := tree.Children("admin")
	require.NoError(t, err)
	assert.Equal(t, []string{"dba", "sre"}, children)

	children, err = tree.Children("sre")
	require.NoError(t, err)
	assert.Empty(t, children)
}

func TestRoleTreeRolesSorted(t *testing.T) {
	tree := NewRoleTree()
	require.NoError(t, tree.AddRole("zeta", ""))
	require.NoError(t, tree.AddRole("alpha", ""))
	require.NoError(t, tree.AddRole("mid", ""))

	roles := tree.Roles()
	assert.Equal(t, []string{"alpha", "mid", "zeta"}, roles)
}

func TestRoleTreeRender(t *testing.T) {
	tree := NewRoleTree()
	require.NoError(t, tree.AddRole("admin", ""))
	require.NoError(t, tree.AddRole("sre", "admin"))
	require.NoError(t, tree.AddRole("dev", "sre"))
	require.NoError(t, tree.GrantPermission("admin", "view"))
	require.NoError(t, tree.GrantPermission("sre", "apply"))

	out := tree.Render()
	assert.Contains(t, out, "admin")
	assert.Contains(t, out, "sre")
	assert.Contains(t, out, "dev")
	assert.Contains(t, out, "view")
	assert.Contains(t, out, "apply")
}

func TestRoleTreeGrantPermissionEmpty(t *testing.T) {
	tree := NewRoleTree()
	err := tree.GrantPermission("", "view")
	assert.ErrorIs(t, err, ErrEmptyRole)
}

func TestRoleTreeRevokePermissionUnknown(t *testing.T) {
	tree := NewRoleTree()
	err := tree.RevokePermission("nonexistent", "view")
	assert.ErrorIs(t, err, ErrUnknownRole)
}

func TestRoleTreeImplicitParentCreation(t *testing.T) {
	tree := NewRoleTree()
	// Adding a child whose parent does not yet exist implicitly creates
	// the parent.
	require.NoError(t, tree.AddRole("child", "parent"))
	assert.True(t, tree.HasRole("parent"))
	assert.True(t, tree.HasRole("child"))
}