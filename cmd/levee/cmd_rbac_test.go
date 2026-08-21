package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nexus/levee/internal/permission"
)

// --- Command registration tests --------------------------------------------

func TestRBACCmdRegistered(t *testing.T) {
	defer resetRootFlags()
	cmd := findSub("rbac")
	require.NotNil(t, cmd, "rbac subcommand should be registered")
	assert.Equal(t, "rbac", cmd.Name())
}

func TestRBACSubcommands(t *testing.T) {
	defer resetRootFlags()
	cmd := findSub("rbac")
	require.NotNil(t, cmd)

	expected := []string{"role", "policy", "check", "tree"}
	for _, name := range expected {
		found := false
		for _, sub := range cmd.Commands() {
			if sub.Name() == name {
				found = true
				break
			}
		}
		assert.True(t, found, "rbac should have %q subcommand", name)
	}
}

func TestRBACRoleSubcommands(t *testing.T) {
	defer resetRootFlags()
	cmd := findSubCmd(findSub("rbac"), "role")
	require.NotNil(t, cmd)

	expected := []string{"list", "add", "remove"}
	for _, name := range expected {
		found := false
		for _, sub := range cmd.Commands() {
			if sub.Name() == name {
				found = true
				break
			}
		}
		assert.True(t, found, "rbac role should have %q subcommand", name)
	}
}

func TestRBACPolicySubcommands(t *testing.T) {
	defer resetRootFlags()
	cmd := findSubCmd(findSub("rbac"), "policy")
	require.NotNil(t, cmd)

	expected := []string{"list", "add", "remove"}
	for _, name := range expected {
		found := false
		for _, sub := range cmd.Commands() {
			if sub.Name() == name {
				found = true
				break
			}
		}
		assert.True(t, found, "rbac policy should have %q subcommand", name)
	}
}

func TestRBACRoleAddFlags(t *testing.T) {
	defer resetRootFlags()
	addCmd := findSubCmd(findSubCmd(findSub("rbac"), "role"), "add")
	require.NotNil(t, addCmd)

	f := addCmd.Flags().Lookup("name")
	require.NotNil(t, f, "role add should have --name flag")
	_, required := f.Annotations[cobra.BashCompOneRequiredFlag]
	assert.True(t, required, "--name should be required")

	f = addCmd.Flags().Lookup("parent")
	require.NotNil(t, f, "role add should have --parent flag")
}

func TestRBACPolicyAddFlags(t *testing.T) {
	defer resetRootFlags()
	addCmd := findSubCmd(findSubCmd(findSub("rbac"), "policy"), "add")
	require.NotNil(t, addCmd)

	for _, flag := range []string{"id", "effect", "resource", "action", "condition", "description"} {
		f := addCmd.Flags().Lookup(flag)
		require.NotNil(t, f, "policy add should have --%s flag", flag)
	}
	for _, flag := range []string{"id", "resource", "action"} {
		f := addCmd.Flags().Lookup(flag)
		_, required := f.Annotations[cobra.BashCompOneRequiredFlag]
		assert.True(t, required, "--%s should be required", flag)
	}
}

func TestRBACCheckFlags(t *testing.T) {
	defer resetRootFlags()
	checkCmd := findSubCmd(findSub("rbac"), "check")
	require.NotNil(t, checkCmd)

	for _, flag := range []string{"user", "action", "resource", "label", "verbose"} {
		f := checkCmd.Flags().Lookup(flag)
		require.NotNil(t, f, "check should have --%s flag", flag)
	}
	for _, flag := range []string{"user", "action", "resource"} {
		f := checkCmd.Flags().Lookup(flag)
		_, required := f.Annotations[cobra.BashCompOneRequiredFlag]
		assert.True(t, required, "--%s should be required", flag)
	}
}

// --- Helper function tests --------------------------------------------------

func TestParseLabelFlags(t *testing.T) {
	flags := []string{"target.env=prod", "change.risk=high", "malformed", "=novalue"}
	labels := parseLabelFlags(flags)
	assert.Equal(t, "prod", labels["target.env"])
	assert.Equal(t, "high", labels["change.risk"])
	assert.NotContains(t, labels, "malformed")
	assert.NotContains(t, labels, "")
}

func TestParseLabelFlagsEmpty(t *testing.T) {
	labels := parseLabelFlags(nil)
	assert.NotNil(t, labels)
	assert.Empty(t, labels)
}

func TestLoadRBACRoleTreeNonExistent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "roles.yaml")
	tree, err := loadRBACRoleTree(path)
	require.NoError(t, err)
	assert.NotNil(t, tree)
	assert.Empty(t, tree.Roles())
}

func TestSaveLoadRBACRoleTree(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "roles.yaml")

	tree := permission.NewRoleTree()
	require.NoError(t, tree.AddRole("admin", ""))
	require.NoError(t, tree.AddRole("sre", "admin"))
	require.NoError(t, tree.GrantPermission("admin", "view", "audit"))
	require.NoError(t, tree.GrantPermission("sre", "apply"))

	require.NoError(t, saveRBACRoleTree(path, tree))

	loaded, err := loadRBACRoleTree(path)
	require.NoError(t, err)
	assert.True(t, loaded.HasRole("admin"))
	assert.True(t, loaded.HasRole("sre"))

	parent, err := loaded.Parent("sre")
	require.NoError(t, err)
	assert.Equal(t, "admin", parent)

	eff, err := loaded.EffectivePermissions("sre")
	require.NoError(t, err)
	assert.Contains(t, eff, "view")
	assert.Contains(t, eff, "audit")
	assert.Contains(t, eff, "apply")
}

func TestLoadRBACPolicySetNonExistent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "policies.yaml")
	ps, err := loadRBACPolicySet(path)
	require.NoError(t, err)
	assert.NotNil(t, ps)
	assert.Equal(t, 0, ps.Len())
}

func TestSaveLoadRBACPolicySet(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "policies.yaml")

	ps := permission.NewPolicySet()
	require.NoError(t, ps.Add(&permission.Policy{
		ID:       "p1",
		Effect:   permission.EffectAllow,
		Resource: "change:*",
		Action:   "apply",
	}))
	require.NoError(t, ps.Add(&permission.Policy{
		ID:        "p2",
		Effect:    permission.EffectDeny,
		Resource:  "change:*",
		Action:    "apply",
		Condition: "change.risk = high",
	}))

	require.NoError(t, saveRBACPolicySet(path, ps))

	loaded, err := loadRBACPolicySet(path)
	require.NoError(t, err)
	assert.Equal(t, 2, loaded.Len())
}

func TestSaveRBACRoleTreeCreatesDir(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sub", "deep", "roles.yaml")

	tree := permission.NewRoleTree()
	require.NoError(t, tree.AddRole("admin", ""))
	require.NoError(t, saveRBACRoleTree(path, tree))

	_, err := os.Stat(path)
	require.NoError(t, err)
}

func TestSaveRBACPolicySetCreatesDir(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sub", "deep", "policies.yaml")

	ps := permission.NewPolicySet()
	require.NoError(t, ps.Add(&permission.Policy{
		ID: "p1", Effect: permission.EffectAllow, Resource: "*", Action: "view",
	}))
	require.NoError(t, saveRBACPolicySet(path, ps))

	_, err := os.Stat(path)
	require.NoError(t, err)
}

// --- Printer tests ----------------------------------------------------------

func TestPrintRBACRoleListHuman(t *testing.T) {
	rows := []map[string]any{
		{
			"role":      "sre",
			"parent":    "admin",
			"direct":    []string{"apply"},
			"effective": []string{"apply", "view"},
		},
	}
	var buf bytes.Buffer
	printRBACRoleListHuman(&buf, rows)
	out := buf.String()
	assert.Contains(t, out, "sre")
	assert.Contains(t, out, "admin")
	assert.Contains(t, out, "apply")
	assert.Contains(t, out, "view")
}

func TestPrintRBACRoleListHumanEmpty(t *testing.T) {
	var buf bytes.Buffer
	printRBACRoleListHuman(&buf, nil)
	assert.Contains(t, buf.String(), "No roles")
}

func TestPrintRBACPolicyListHuman(t *testing.T) {
	rows := []map[string]any{
		{
			"id":        "p1",
			"effect":    "allow",
			"resource":  "change:*",
			"action":    "apply",
			"condition": "target.env = prod",
		},
	}
	var buf bytes.Buffer
	printRBACPolicyListHuman(&buf, rows)
	out := buf.String()
	assert.Contains(t, out, "p1")
	assert.Contains(t, out, "allow")
	assert.Contains(t, out, "change:*")
	assert.Contains(t, out, "apply")
	assert.Contains(t, out, "target.env = prod")
}

func TestPrintRBACPolicyListHumanEmpty(t *testing.T) {
	var buf bytes.Buffer
	printRBACPolicyListHuman(&buf, nil)
	assert.Contains(t, buf.String(), "No policies")
}

// --- Integration tests via CLI ---------------------------------------------

// withTestDataDir sets LEVEE_SERVER_DATA_DIR to a temp directory for the
// duration of the test so that RBAC commands read/write to an isolated
// location. It also resets all RBAC command flag globals to avoid state
// leaking between tests. It returns the cleanup function.
func withTestDataDir(t *testing.T) string {
	t.Helper()
	resetRBACFlags()
	dir := t.TempDir()
	orig := os.Getenv("LEVEE_SERVER_DATA_DIR")
	os.Setenv("LEVEE_SERVER_DATA_DIR", dir)
	t.Cleanup(func() {
		if orig == "" {
			os.Unsetenv("LEVEE_SERVER_DATA_DIR")
		} else {
			os.Setenv("LEVEE_SERVER_DATA_DIR", orig)
		}
	})
	return dir
}

// resetRBACFlags restores the RBAC command option variables to their
// zero values. Tests that flip RBAC flags call it to avoid leaking state
// into sibling tests.
func resetRBACFlags() {
	rbacRoleAddOptName = ""
	rbacRoleAddOptParent = ""
	rbacRoleRemoveOptID = ""
	rbacPolicyAddOptID = ""
	rbacPolicyAddOptEffect = "allow"
	rbacPolicyAddOptResource = ""
	rbacPolicyAddOptAction = ""
	rbacPolicyAddOptCondition = ""
	rbacPolicyAddOptDesc = ""
	rbacPolicyRemoveOptID = ""
	rbacCheckOptUser = ""
	rbacCheckOptAction = ""
	rbacCheckOptResource = ""
	rbacCheckOptLabels = nil
	rbacCheckOptVerbose = false
}

func TestRbacRoleListEmpty(t *testing.T) {
	defer resetRootFlags()
	withTestDataDir(t)

	out, err := executeCommand("rbac", "role", "list", "--json")
	require.NoError(t, err)

	var env outputEnvelope
	require.NoError(t, json.Unmarshal([]byte(out), &env))
	assert.NotNil(t, env.Data)
}

func TestRbacRoleAddAndList(t *testing.T) {
	defer resetRootFlags()
	withTestDataDir(t)

	// Add admin role.
	_, err := executeCommand("rbac", "role", "add", "--name", "admin")
	require.NoError(t, err)

	// Add sre role with parent.
	_, err = executeCommand("rbac", "role", "add", "--name", "sre", "--parent", "admin")
	require.NoError(t, err)

	// List roles.
	out, err := executeCommand("rbac", "role", "list", "--json")
	require.NoError(t, err)

	var env outputEnvelope
	require.NoError(t, json.Unmarshal([]byte(out), &env))
	data, ok := env.Data.([]any)
	require.True(t, ok)
	assert.Len(t, data, 2)
}

func TestRbacRoleAddCycle(t *testing.T) {
	defer resetRootFlags()
	withTestDataDir(t)

	_, err := executeCommand("rbac", "role", "add", "--name", "a")
	require.NoError(t, err)
	_, err = executeCommand("rbac", "role", "add", "--name", "b", "--parent", "a")
	require.NoError(t, err)

	// Adding a as child of b would cycle.
	_, err = executeCommand("rbac", "role", "add", "--name", "a", "--parent", "b")
	require.Error(t, err)
}

func TestRbacPolicyAddAndList(t *testing.T) {
	defer resetRootFlags()
	withTestDataDir(t)

	_, err := executeCommand("rbac", "policy", "add",
		"--id", "p1", "--effect", "allow",
		"--resource", "change:*", "--action", "apply")
	require.NoError(t, err)

	out, err := executeCommand("rbac", "policy", "list", "--json")
	require.NoError(t, err)

	var env outputEnvelope
	require.NoError(t, json.Unmarshal([]byte(out), &env))
	data, ok := env.Data.([]any)
	require.True(t, ok)
	require.Len(t, data, 1)

	row := data[0].(map[string]any)
	assert.Equal(t, "p1", row["id"])
	assert.Equal(t, "allow", row["effect"])
	assert.Equal(t, "change:*", row["resource"])
	assert.Equal(t, "apply", row["action"])
}

func TestRbacPolicyAddInvalid(t *testing.T) {
	defer resetRootFlags()
	withTestDataDir(t)

	_, err := executeCommand("rbac", "policy", "add",
		"--id", "p1", "--effect", "maybe",
		"--resource", "*", "--action", "view")
	require.Error(t, err)
}

func TestRbacPolicyRemove(t *testing.T) {
	defer resetRootFlags()
	withTestDataDir(t)

	_, err := executeCommand("rbac", "policy", "add",
		"--id", "p1", "--resource", "*", "--action", "view")
	require.NoError(t, err)

	_, err = executeCommand("rbac", "policy", "remove", "--id", "p1")
	require.NoError(t, err)

	// List should be empty.
	out, err := executeCommand("rbac", "policy", "list", "--json")
	require.NoError(t, err)
	var env outputEnvelope
	require.NoError(t, json.Unmarshal([]byte(out), &env))
	data, _ := env.Data.([]any)
	assert.Empty(t, data)
}

func TestRbacPolicyRemoveNotFound(t *testing.T) {
	defer resetRootFlags()
	withTestDataDir(t)

	_, err := executeCommand("rbac", "policy", "remove", "--id", "nonexistent")
	require.Error(t, err)
}

func TestRbacCheckAllow(t *testing.T) {
	defer resetRootFlags()
	withTestDataDir(t)

	_, err := executeCommand("rbac", "policy", "add",
		"--id", "allow-view", "--effect", "allow",
		"--resource", "change:*", "--action", "view")
	require.NoError(t, err)

	out, err := executeCommand("rbac", "check",
		"--user", "alice", "--action", "view", "--resource", "change:123",
		"--json")
	require.NoError(t, err)

	var env outputEnvelope
	require.NoError(t, json.Unmarshal([]byte(out), &env))
	data, ok := env.Data.(map[string]any)
	require.True(t, ok)
	assert.True(t, data["allowed"].(bool))
}

func TestRbacCheckDeny(t *testing.T) {
	defer resetRootFlags()
	withTestDataDir(t)

	_, err := executeCommand("rbac", "policy", "add",
		"--id", "deny-prod", "--effect", "deny",
		"--resource", "target:*", "--action", "apply",
		"--condition", "target.env = prod")
	require.NoError(t, err)

	out, err := executeCommand("rbac", "check",
		"--user", "alice", "--action", "apply", "--resource", "target:abc",
		"--label", "target.env=prod", "--json")
	require.NoError(t, err)

	var env outputEnvelope
	require.NoError(t, json.Unmarshal([]byte(out), &env))
	data, ok := env.Data.(map[string]any)
	require.True(t, ok)
	assert.False(t, data["allowed"].(bool))
}

func TestRbacCheckNoMatch(t *testing.T) {
	defer resetRootFlags()
	withTestDataDir(t)

	out, err := executeCommand("rbac", "check",
		"--user", "alice", "--action", "view", "--resource", "change:1",
		"--json")
	require.NoError(t, err)

	var env outputEnvelope
	require.NoError(t, json.Unmarshal([]byte(out), &env))
	data, ok := env.Data.(map[string]any)
	require.True(t, ok)
	assert.False(t, data["allowed"].(bool))
}

func TestRbacCheckVerbose(t *testing.T) {
	defer resetRootFlags()
	withTestDataDir(t)

	_, err := executeCommand("rbac", "policy", "add",
		"--id", "allow-view", "--effect", "allow",
		"--resource", "*", "--action", "view")
	require.NoError(t, err)

	out, err := executeCommand("rbac", "check",
		"--user", "alice", "--action", "view", "--resource", "change:1",
		"--label", "target.env=prod", "--verbose")
	require.NoError(t, err)
	assert.Contains(t, out, "ALLOW")
	assert.Contains(t, out, "alice")
	assert.Contains(t, out, "target.env")
}

func TestRbacTreeEmpty(t *testing.T) {
	defer resetRootFlags()
	withTestDataDir(t)

	out, err := executeCommand("rbac", "tree")
	require.NoError(t, err)
	// Empty tree renders nothing.
	assert.Empty(t, out)
}

func TestRbacTreeWithRoles(t *testing.T) {
	defer resetRootFlags()
	withTestDataDir(t)

	_, err := executeCommand("rbac", "role", "add", "--name", "admin")
	require.NoError(t, err)
	_, err = executeCommand("rbac", "role", "add", "--name", "sre", "--parent", "admin")
	require.NoError(t, err)

	out, err := executeCommand("rbac", "tree")
	require.NoError(t, err)
	assert.Contains(t, out, "admin")
	assert.Contains(t, out, "sre")
}

func TestRbacTreeJSON(t *testing.T) {
	defer resetRootFlags()
	withTestDataDir(t)

	_, err := executeCommand("rbac", "role", "add", "--name", "admin")
	require.NoError(t, err)

	out, err := executeCommand("rbac", "tree", "--json")
	require.NoError(t, err)

	var env outputEnvelope
	require.NoError(t, json.Unmarshal([]byte(out), &env))
	assert.NotNil(t, env.Data)
}

func TestRbacRoleRemove(t *testing.T) {
	defer resetRootFlags()
	withTestDataDir(t)

	_, err := executeCommand("rbac", "role", "add", "--name", "admin")
	require.NoError(t, err)

	_, err = executeCommand("rbac", "role", "remove", "--name", "admin")
	require.NoError(t, err)

	// List should be empty.
	out, err := executeCommand("rbac", "role", "list", "--json")
	require.NoError(t, err)
	var env outputEnvelope
	require.NoError(t, json.Unmarshal([]byte(out), &env))
	data, _ := env.Data.([]any)
	assert.Empty(t, data)
}
