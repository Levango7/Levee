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

	"github.com/nexus/levee/internal/tenant"
)

// --- Command registration --------------------------------------------------

func TestTenantCmdRegistered(t *testing.T) {
	defer resetRootFlags()
	cmd := findSub("tenant")
	require.NotNil(t, cmd, "tenant subcommand should be registered")
	assert.Equal(t, "tenant", cmd.Name())
}

func TestTenantSubcommands(t *testing.T) {
	defer resetRootFlags()
	cmd := findSub("tenant")
	require.NotNil(t, cmd)

	expected := []string{"create", "list", "show", "suspend", "resume", "delete", "quota", "usage"}
	for _, name := range expected {
		sub := findSubCmd(cmd, name)
		assert.NotNil(t, sub, "tenant should have %q subcommand", name)
	}
}

func TestTenantCreateCmdFlags(t *testing.T) {
	defer resetRootFlags()
	cmd := findSub("tenant")
	require.NotNil(t, cmd)

	createCmd := findSubCmd(cmd, "create")
	require.NotNil(t, createCmd)

	for _, flag := range []string{"name", "display", "max-targets", "max-changes", "max-storage", "max-api-rate"} {
		f := createCmd.Flags().Lookup(flag)
		require.NotNil(t, f, "tenant create should have --%s flag", flag)
	}

	// --name should be required.
	f := createCmd.Flags().Lookup("name")
	_, required := f.Annotations[cobra.BashCompOneRequiredFlag]
	assert.True(t, required, "--name flag should be required")
}

func TestTenantShowCmdArgs(t *testing.T) {
	defer resetRootFlags()
	cmd := findSub("tenant")
	require.NotNil(t, cmd)

	showCmd := findSubCmd(cmd, "show")
	require.NotNil(t, showCmd)

	err := showCmd.Args(showCmd, []string{})
	assert.Error(t, err, "tenant show should require exactly one arg")

	err = showCmd.Args(showCmd, []string{"id1"})
	assert.NoError(t, err)

	err = showCmd.Args(showCmd, []string{"id1", "id2"})
	assert.Error(t, err)
}

func TestTenantSuspendCmdArgs(t *testing.T) {
	defer resetRootFlags()
	cmd := findSub("tenant")
	require.NotNil(t, cmd)

	suspendCmd := findSubCmd(cmd, "suspend")
	require.NotNil(t, suspendCmd)

	err := suspendCmd.Args(suspendCmd, []string{})
	assert.Error(t, err)
}

func TestTenantResumeCmdArgs(t *testing.T) {
	defer resetRootFlags()
	cmd := findSub("tenant")
	require.NotNil(t, cmd)

	resumeCmd := findSubCmd(cmd, "resume")
	require.NotNil(t, resumeCmd)

	err := resumeCmd.Args(resumeCmd, []string{})
	assert.Error(t, err)
}

func TestTenantDeleteCmdArgs(t *testing.T) {
	defer resetRootFlags()
	cmd := findSub("tenant")
	require.NotNil(t, cmd)

	deleteCmd := findSubCmd(cmd, "delete")
	require.NotNil(t, deleteCmd)

	err := deleteCmd.Args(deleteCmd, []string{})
	assert.Error(t, err)
}

func TestTenantQuotaCmdArgs(t *testing.T) {
	defer resetRootFlags()
	cmd := findSub("tenant")
	require.NotNil(t, cmd)

	quotaCmd := findSubCmd(cmd, "quota")
	require.NotNil(t, quotaCmd)

	err := quotaCmd.Args(quotaCmd, []string{})
	assert.Error(t, err)

	for _, flag := range []string{"max-targets", "max-changes", "max-storage", "max-api-rate"} {
		f := quotaCmd.Flags().Lookup(flag)
		require.NotNil(t, f, "tenant quota should have --%s flag", flag)
	}
}

func TestTenantUsageCmdArgs(t *testing.T) {
	defer resetRootFlags()
	cmd := findSub("tenant")
	require.NotNil(t, cmd)

	usageCmd := findSubCmd(cmd, "usage")
	require.NotNil(t, usageCmd)

	err := usageCmd.Args(usageCmd, []string{})
	assert.Error(t, err)
}

// --- Registry persistence --------------------------------------------------

func TestTenantRegistryLoadSave(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "tenants.yaml")

	reg := &tenantRegistry{
		Tenants: []tenantRecord{
			{
				ID:                   "tenant-1",
				Name:                 "acme",
				DisplayName:          "ACME Corp",
				Namespace:            "tenant-acme",
				Status:               "active",
				MaxTargets:           100,
				MaxConcurrentChanges: 10,
				MaxStorageMB:         500,
				MaxAPIRatePerMin:     60,
			},
		},
	}

	require.NoError(t, saveTenantRegistry(path, reg))

	loaded, err := loadTenantRegistry(path)
	require.NoError(t, err)
	require.Len(t, loaded.Tenants, 1)
	assert.Equal(t, "acme", loaded.Tenants[0].Name)
	assert.Equal(t, 100, loaded.Tenants[0].MaxTargets)
}

func TestTenantRegistryLoadNonExistent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "nonexistent.yaml")

	reg, err := loadTenantRegistry(path)
	require.NoError(t, err)
	assert.Empty(t, reg.Tenants)
}

func TestTenantRegistrySaveCreatesDir(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sub", "deep", "tenants.yaml")

	reg := &tenantRegistry{
		Tenants: []tenantRecord{
			{Name: "acme", Status: "active"},
		},
	}
	require.NoError(t, saveTenantRegistry(path, reg))

	_, err := os.Stat(path)
	require.NoError(t, err)
}

func TestTenantsFilePath(t *testing.T) {
	assert.Equal(t, filepath.Join("/", "data", "tenants.yaml"), tenantsFilePath("/data"))
	assert.Equal(t, filepath.Join("data", "tenants.yaml"), tenantsFilePath("data"))
}

// --- Manager <-> registry conversion ---------------------------------------

func TestManagerToRegistry(t *testing.T) {
	tm := tenant.NewTenantManager()
	_, err := tm.Create(nil, "acme", "ACME", tenant.Quota{
		MaxTargets:           10,
		MaxConcurrentChanges: 5,
		MaxStorageMB:         100,
		MaxAPIRatePerMin:     60,
	})
	require.NoError(t, err)

	reg := managerToRegistry(tm)
	require.Len(t, reg.Tenants, 1)
	rec := reg.Tenants[0]
	assert.Equal(t, "acme", rec.Name)
	assert.Equal(t, "active", rec.Status)
	assert.Equal(t, 10, rec.MaxTargets)
	assert.Equal(t, 5, rec.MaxConcurrentChanges)
	assert.Equal(t, 100, rec.MaxStorageMB)
	assert.Equal(t, 60, rec.MaxAPIRatePerMin)
}

func TestRegistryToManager(t *testing.T) {
	reg := &tenantRegistry{
		Tenants: []tenantRecord{
			{
				ID:                   "tenant-1",
				Name:                 "acme",
				DisplayName:          "ACME",
				Namespace:            "tenant-acme",
				Status:               "active",
				MaxTargets:           10,
				MaxConcurrentChanges: 5,
			},
		},
	}

	tm, err := registryToManager(reg)
	require.NoError(t, err)
	assert.Equal(t, 1, tm.Count())

	tt, err := tm.GetByName("acme")
	require.NoError(t, err)
	assert.Equal(t, "ACME", tt.DisplayName)

	q, err := tm.QuotaManager().GetQuota(tt.ID)
	require.NoError(t, err)
	assert.Equal(t, 10, q.MaxTargets)
	assert.Equal(t, 5, q.MaxConcurrentChanges)
}

func TestRegistryToManagerInvalidStatus(t *testing.T) {
	reg := &tenantRegistry{
		Tenants: []tenantRecord{
			{Name: "acme", Status: "bogus"},
		},
	}
	_, err := registryToManager(reg)
	assert.Error(t, err)
}

func TestRegistryToManagerDuplicateName(t *testing.T) {
	// Two records with the same name; the second should be skipped.
	reg := &tenantRegistry{
		Tenants: []tenantRecord{
			{ID: "t1", Name: "acme", Status: "active"},
			{ID: "t2", Name: "acme", Status: "active"},
		},
	}
	tm, err := registryToManager(reg)
	require.NoError(t, err)
	assert.Equal(t, 1, tm.Count())
}

// --- Output helpers --------------------------------------------------------

func TestTenantToMap(t *testing.T) {
	tt := tenant.NewTenant("acme", "ACME")
	m := tenantToMap(tt)
	assert.Equal(t, tt.ID, m["id"])
	assert.Equal(t, "acme", m["name"])
	assert.Equal(t, "ACME", m["display_name"])
	assert.Equal(t, "tenant-acme", m["namespace"])
	assert.Equal(t, "active", m["status"])
}

func TestPrintTenantListHuman(t *testing.T) {
	defer resetRootFlags()
	rows := []map[string]any{
		{"id": "t1", "name": "acme", "status": "active", "namespace": "tenant-acme"},
		{"id": "t2", "name": "beta", "status": "suspended", "namespace": "tenant-beta"},
	}
	var buf bytes.Buffer
	printTenantListHuman(&buf, rows)
	out := buf.String()
	assert.Contains(t, out, "acme")
	assert.Contains(t, out, "beta")
	assert.Contains(t, out, "active")
	assert.Contains(t, out, "suspended")
}

func TestPrintTenantListHumanEmpty(t *testing.T) {
	defer resetRootFlags()
	var buf bytes.Buffer
	printTenantListHuman(&buf, nil)
	assert.Contains(t, buf.String(), "No tenants found")
}

func TestPrintTenantActionHuman(t *testing.T) {
	defer resetRootFlags()
	output := map[string]any{
		"name":   "acme",
		"id":     "t1",
		"action": "created",
	}
	var buf bytes.Buffer
	printTenantActionHuman(&buf, output)
	assert.Contains(t, buf.String(), "acme")
	assert.Contains(t, buf.String(), "created")
}

func TestPrintTenantDetailHuman(t *testing.T) {
	defer resetRootFlags()
	detail := map[string]any{
		"tenant": map[string]any{
			"name":         "acme",
			"id":           "t1",
			"display_name": "ACME",
			"namespace":    "tenant-acme",
			"status":       "active",
			"created_at":   "2025-01-01",
			"updated_at":   "2025-01-01",
		},
		"quota": map[string]any{
			"max_targets":            10,
			"max_concurrent_changes": 5,
			"max_storage_mb":         100,
			"max_api_rate_per_min":   60,
		},
		"usage": map[string]any{
			"target_count":          3,
			"active_changes":        2,
			"storage_used_mb":       50,
			"api_requests_this_min": 30,
		},
	}
	var buf bytes.Buffer
	printTenantDetailHuman(&buf, detail)
	out := buf.String()
	assert.Contains(t, out, "acme")
	assert.Contains(t, out, "Quota")
	assert.Contains(t, out, "Usage")
}

func TestPrintQuotaHuman(t *testing.T) {
	defer resetRootFlags()
	output := map[string]any{
		"tenant_id":              "t1",
		"max_targets":            10,
		"max_concurrent_changes": 5,
		"max_storage_mb":         100,
		"max_api_rate_per_min":   60,
	}
	var buf bytes.Buffer
	printQuotaHuman(&buf, output)
	out := buf.String()
	assert.Contains(t, out, "t1")
	assert.Contains(t, out, "10")
}

func TestPrintUsageHuman(t *testing.T) {
	defer resetRootFlags()
	output := map[string]any{
		"tenant_id": "t1",
		"usage": map[string]any{
			"target_count":          3,
			"active_changes":        2,
			"storage_used_mb":       50,
			"api_requests_this_min": 30,
		},
		"quota": map[string]any{
			"max_targets":            10,
			"max_concurrent_changes": 5,
			"max_storage_mb":         100,
			"max_api_rate_per_min":   60,
		},
	}
	var buf bytes.Buffer
	printUsageHuman(&buf, output)
	out := buf.String()
	assert.Contains(t, out, "t1")
	assert.Contains(t, out, "3")
}

// --- End-to-end CLI via temp config ----------------------------------------

// setupTenantTestConfig writes a minimal LEVEE config to a temp dir and
// points the global config flag at it. The returned cleanup restores
// the previous flag state.
func setupTenantTestConfig(t *testing.T) (dataDir string, cleanup func()) {
	t.Helper()
	prevConfig := optConfigPath
	prevJSON := optJSON
	prevQuiet := optQuiet

	dataDir = t.TempDir()
	configDir := t.TempDir()
	configPath := filepath.Join(configDir, "config.yaml")
	configYAML := "server:\n  data_dir: " + dataDir + "\n"
	require.NoError(t, os.WriteFile(configPath, []byte(configYAML), 0o644))

	optConfigPath = configPath
	optJSON = false
	optQuiet = false

	cleanup = func() {
		optConfigPath = prevConfig
		optJSON = prevJSON
		optQuiet = prevQuiet
	}
	return dataDir, cleanup
}

func TestTenantCreateListShowE2E(t *testing.T) {
	_, cleanup := setupTenantTestConfig(t)
	defer cleanup()

	// --- create ---
	tenantCreateOptName = "acme"
	tenantCreateOptDisplay = "ACME Corp"
	tenantCreateOptMaxTargets = 100
	tenantCreateOptMaxChanges = 10
	tenantCreateOptMaxStorage = 500
	tenantCreateOptMaxAPIRate = 60

	createCmd := findSubCmd(findSub("tenant"), "create")
	require.NotNil(t, createCmd)
	require.NoError(t, createCmd.RunE(createCmd, []string{}))

	// --- list ---
	listCmd := findSubCmd(findSub("tenant"), "list")
	require.NotNil(t, listCmd)
	require.NoError(t, listCmd.RunE(listCmd, []string{}))

	// --- show by name (should fail) ---
	showCmd := findSubCmd(findSub("tenant"), "show")
	require.NotNil(t, showCmd)
	err := showCmd.RunE(showCmd, []string{"acme"})
	assert.Error(t, err) // names are not ids; show expects an id
}

func TestTenantCreateInvalidNameE2E(t *testing.T) {
	_, cleanup := setupTenantTestConfig(t)
	defer cleanup()

	tenantCreateOptName = "ACME" // uppercase invalid
	tenantCreateOptDisplay = "ACME"
	tenantCreateOptMaxTargets = 0
	tenantCreateOptMaxChanges = 0
	tenantCreateOptMaxStorage = 0
	tenantCreateOptMaxAPIRate = 0

	createCmd := findSubCmd(findSub("tenant"), "create")
	require.NotNil(t, createCmd)
	err := createCmd.RunE(createCmd, []string{})
	assert.Error(t, err)
}

func TestTenantJSONOutputEnvelope(t *testing.T) {
	defer resetRootFlags()
	optJSON = true

	rows := []map[string]any{
		{"id": "t1", "name": "acme", "status": "active", "namespace": "tenant-acme"},
	}
	var buf bytes.Buffer
	require.NoError(t, PrintJSON(&buf, map[string]any{
		"data":  rows,
		"meta":  nil,
		"error": nil,
	}))

	var env outputEnvelope
	require.NoError(t, json.Unmarshal(buf.Bytes(), &env))
	assert.NotNil(t, env.Data)
}

func TestJSONRoundTrip(t *testing.T) {
	in := map[string]any{"a": 1, "b": "two"}
	out, err := jsonRoundTrip(in)
	require.NoError(t, err)
	assert.Equal(t, float64(1), out["a"])
	assert.Equal(t, "two", out["b"])
}
