package main

// cli_tenant_wiring_test.go covers the tenant half of the flag work the
// registration gate cannot see, plus the ID-stability defect the --reason work
// exposed. cli_reference_flags_test.go proves a documented --flag exists; none
// of it proves the flag changes anything. Each case here drives the real RunE,
// so a flag that is declared and then ignored turns red.

import (
	"io"
	"os"
	"path/filepath"
	"regexp"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nexus/levee/internal/tenant"
)

func captureOut(t *testing.T, fn func() error) (string, error) {
	t.Helper()
	orig := os.Stdout
	r, w, err := os.Pipe()
	require.NoError(t, err)
	os.Stdout = w
	runErr := fn()
	require.NoError(t, w.Close())
	os.Stdout = orig
	out, readErr := io.ReadAll(r)
	require.NoError(t, readErr)
	return string(out), runErr
}

// restoreTenantFlagVars resets the package-level option vars this file writes. They
// are process-global by cobra's design, so a value left set leaks into the next
// test in the package — which is how an unrelated "--status nope" from one test
// made another test's `tenant list` fail.

func restoreTenantFlagVars(t *testing.T) {
	t.Helper()
	t.Cleanup(func() {
		tenantListOptStatus = ""
		tenantSuspendOptReason = ""
		tenantCreateOptName = ""
		tenantCreateOptDisplay = ""
		tenantCreateOptMaxTargets = 0
		tenantCreateOptMaxChanges = 0
		tenantCreateOptMaxStorage = 0
		tenantCreateOptMaxAPIRate = 0
	})
}

func tenantIDs(tenants []*tenant.Tenant) []string {
	out := make([]string, 0, len(tenants))
	for _, tt := range tenants {
		out = append(out, tt.ID)
	}
	return out
}

func newTenantFixture(t *testing.T) (acme, beta, gamma *tenant.Tenant) {
	t.Helper()
	tm := tenant.NewTenantManager()
	ctx := t.Context()
	acme, err := tm.Create(ctx, "acme", "", tenant.Quota{})
	require.NoError(t, err)
	beta, err = tm.Create(ctx, "beta", "", tenant.Quota{})
	require.NoError(t, err)
	gamma, err = tm.Create(ctx, "gamma", "", tenant.Quota{})
	require.NoError(t, err)
	return acme, beta, gamma
}

// --- tenant list --status --------------------------------------------------

func TestTenantStatusVocabularyIsSingleSourced(t *testing.T) {
	// The exact list is the point: a fourth lifecycle state that is not added
	// to tenantStatusNames would print as "unknown(3)" and be unfilterable.
	assert.Equal(t, []string{"active", "suspended", "deleted"}, tenant.TenantStatusValues())

	for _, name := range tenant.TenantStatusValues() {
		got, err := tenant.ParseTenantStatus(name)
		require.NoError(t, err, name)
		assert.Equal(t, name, got.String(), "parse and String must agree for %q", name)
	}

	for _, constant := range []tenant.TenantStatus{tenant.TenantActive, tenant.TenantSuspended, tenant.TenantDeleted} {
		assert.Contains(t, tenant.TenantStatusValues(), constant.String(),
			"every declared status must be reachable through the filter vocabulary")
	}

	_, err := tenant.ParseTenantStatus("acitve")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "acitve")
	// The refusal must teach the reader the vocabulary rather than just say no.
	for _, v := range tenant.TenantStatusValues() {
		assert.Contains(t, err.Error(), v)
	}
}

func TestFilterTenantsByStatus(t *testing.T) {
	acme, beta, gamma := newTenantFixture(t)
	acme.Status = tenant.TenantSuspended
	beta.Status = tenant.TenantDeleted

	all := []*tenant.Tenant{acme, beta, gamma}

	t.Run("empty means no filter", func(t *testing.T) {
		got, err := filterTenantsByStatus(all, "")
		require.NoError(t, err)
		assert.Len(t, got, 3)
	})

	for _, tc := range []struct {
		status string
		want   []string
	}{
		{"active", []string{gamma.ID}},
		{"suspended", []string{acme.ID}},
		{"deleted", []string{beta.ID}},
	} {
		got, err := filterTenantsByStatus(all, tc.status)
		require.NoError(t, err, tc.status)
		assert.Equal(t, tc.want, tenantIDs(got), tc.status)
	}

	_, err := filterTenantsByStatus(all, "acitve")
	require.Error(t, err, "a typo must fail loudly instead of printing an empty list")
}

// TestRunTenantListActuallyAppliesStatusFilter drives the command rather than
// the helper, in all three output modes. --quiet prints IDs from the tenant
// slice rather than the rendered rows, so a filter applied only to the rows
// would leave --quiet unfiltered while every row-level assertion stayed green.
func TestRunTenantListActuallyAppliesStatusFilter(t *testing.T) {
	restoreTenantFlagVars(t)
	_, cleanup := setupTenantTestConfig(t)
	defer cleanup()

	createCmd := findSubCmd(findSub("tenant"), "create")
	listCmd := findSubCmd(findSub("tenant"), "list")
	suspendCmd := findSubCmd(findSub("tenant"), "suspend")
	deleteCmd := findSubCmd(findSub("tenant"), "delete")
	require.NotNil(t, createCmd)
	require.NotNil(t, listCmd)

	names := map[string]string{}
	for _, name := range []string{"acme", "beta", "gamma"} {
		tenantCreateOptName = name
		tenantCreateOptDisplay = ""
		tenantCreateOptMaxTargets = 0
		tenantCreateOptMaxChanges = 0
		tenantCreateOptMaxStorage = 0
		tenantCreateOptMaxAPIRate = 0
		require.NoError(t, createCmd.RunE(createCmd, []string{}))
		tm, _, err := loadTenantManager()
		require.NoError(t, err)
		names[name] = mustTenantByName(t, tm, name).ID
	}

	// One tenant per lifecycle state, so any leak across the filter is visible.
	require.NoError(t, suspendCmd.RunE(suspendCmd, []string{names["acme"]}))
	require.NoError(t, deleteCmd.RunE(deleteCmd, []string{names["beta"]}))

	for _, tc := range []struct {
		status string
		want   []string
		miss   []string
	}{
		{status: "", want: []string{names["acme"], names["beta"], names["gamma"]}},
		{status: "active", want: []string{names["gamma"]},
			miss: []string{names["acme"], names["beta"]}},
		{status: "suspended", want: []string{names["acme"]},
			miss: []string{names["beta"], names["gamma"]}},
		{status: "deleted", want: []string{names["beta"]},
			miss: []string{names["acme"], names["gamma"]}},
	} {
		for _, mode := range []struct {
			name  string
			json  bool
			quiet bool
		}{
			{name: "human"},
			{name: "json", json: true},
			// The mode a rows-only filter would have escaped.
			{name: "quiet", quiet: true},
		} {
			tenantListOptStatus = tc.status
			optJSON = mode.json
			optQuiet = mode.quiet
			out, err := captureOut(t, func() error { return listCmd.RunE(listCmd, []string{}) })
			require.NoError(t, err, tc.status+"/"+mode.name)

			for _, id := range tc.want {
				assert.Contains(t, out, id, "%s/%s must list it", tc.status, mode.name)
			}
			for _, id := range tc.miss {
				assert.NotContains(t, out, id, "%s/%s must not leak other statuses", tc.status, mode.name)
			}
		}
	}

	tenantListOptStatus = "nope"
	optJSON, optQuiet = false, false
	_, err := captureOut(t, func() error { return listCmd.RunE(listCmd, []string{}) })
	require.Error(t, err, "an unknown --status must fail the command, not print an empty list")
	assert.Contains(t, err.Error(), "nope")
}

// --- tenant suspend --reason ------------------------------------------------

// TestTenantSuspendReasonRoundTrip proves the reason is not merely echoed: it
// goes through the manager, the tenantRecord DTO and the registry file, and a
// later load still sees it. That mapping layer is where a new field silently
// disappears, because both directions are hand-written copies.
func TestTenantSuspendReasonRoundTrip(t *testing.T) {
	restoreTenantFlagVars(t)
	_, cleanup := setupTenantTestConfig(t)
	defer cleanup()

	createCmd := findSubCmd(findSub("tenant"), "create")
	suspendCmd := findSubCmd(findSub("tenant"), "suspend")
	resumeCmd := findSubCmd(findSub("tenant"), "resume")
	showCmd := findSubCmd(findSub("tenant"), "show")
	require.NotNil(t, createCmd)

	tenantCreateOptName = "acme"
	require.NoError(t, createCmd.RunE(createCmd, []string{}))
	tm, _, err := loadTenantManager()
	require.NoError(t, err)
	id := mustTenantByName(t, tm, "acme").ID

	tenantSuspendOptReason = "合规审查"
	require.NoError(t, suspendCmd.RunE(suspendCmd, []string{id}))

	// Reload from disk: the field must survive serialisation.
	reloaded, _, err := loadTenantManager()
	require.NoError(t, err)
	tt, err := reloaded.Get(id)
	require.NoError(t, err)
	assert.Equal(t, "合规审查", tt.SuspendReason)

	// And it must be visible in the output operators read.
	optJSON = true
	out, err := captureOut(t, func() error { return showCmd.RunE(showCmd, []string{id}) })
	require.NoError(t, err)
	assert.Contains(t, out, "合规审查", "tenant show must surface the recorded reason")

	// Re-suspending with a new reason updates it; re-suspending without one
	// keeps it. Both branches need pinning or "no-op" quietly becomes
	// "overwrite with empty".
	tenantSuspendOptReason = "安全事件"
	require.NoError(t, suspendCmd.RunE(suspendCmd, []string{id}))
	reloaded, _, err = loadTenantManager()
	require.NoError(t, err)
	tt, err = reloaded.Get(id)
	require.NoError(t, err)
	assert.Equal(t, "安全事件", tt.SuspendReason)

	tenantSuspendOptReason = ""
	require.NoError(t, suspendCmd.RunE(suspendCmd, []string{id}))
	reloaded, _, err = loadTenantManager()
	require.NoError(t, err)
	tt, err = reloaded.Get(id)
	require.NoError(t, err)
	assert.Equal(t, "安全事件", tt.SuspendReason, "an omitted --reason must not erase the recorded one")

	// Resume clears it: an active tenant carrying a stale "why we parked it"
	// reads as still parked.
	require.NoError(t, resumeCmd.RunE(resumeCmd, []string{id}))
	reloaded, _, err = loadTenantManager()
	require.NoError(t, err)
	tt, err = reloaded.Get(id)
	require.NoError(t, err)
	assert.Empty(t, tt.SuspendReason)
}

// TestSuspendReasonAbsentFromOldRegistryFileKeepsLoading guards the backward
// half: registries written before this field existed have no suspend_reason
// key, and loading them must not fail.
func TestSuspendReasonAbsentFromOldRegistryFileKeepsLoading(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "tenants.yaml")
	old := "tenants:\n  - id: t-1\n    name: acme\n    display_name: \"\"\n    namespace: tenant-acme\n" +
		"    status: suspended\n    created_at: 2026-01-01T00:00:00Z\n    updated_at: 2026-01-01T00:00:00Z\n" +
		"    max_targets: 0\n    max_concurrent_changes: 0\n    max_storage_mb: 0\n    max_api_rate_per_min: 0\n"
	require.NoError(t, os.WriteFile(path, []byte(old), 0o600))

	reg, err := loadTenantRegistry(path)
	require.NoError(t, err)
	tm, err := registryToManager(reg)
	require.NoError(t, err)
	tt, err := tm.Get("t-1")
	require.NoError(t, err)
	assert.Equal(t, tenant.TenantSuspended, tt.Status)
	assert.Empty(t, tt.SuspendReason)
}

func mustTenantByName(t *testing.T, tm *tenant.TenantManager, name string) *tenant.Tenant {
	t.Helper()
	for _, tt := range tm.List() {
		if tt.Name == name {
			return tt
		}
	}
	t.Fatalf("tenant %q not created", name)
	return nil
}

// --- the ID-stability defect these flags exposed ----------------------------

// TestTenantIDPrintedByCreateResolvesLater pins the defect the --reason work
// walked into: the load path rebuilt every record with Create (which mints a
// fresh ID) and then patched only the struct, leaving tenants.yaml holding the
// old ID while the map key was the new one. `tenant show <printed-id>` answered
// "not found" for a tenant that demonstrably existed, so every ID-addressed
// tenant subcommand was broken across invocations.
func TestTenantIDPrintedByCreateResolvesLater(t *testing.T) {
	restoreTenantFlagVars(t)
	_, cleanup := setupTenantTestConfig(t)
	defer cleanup()

	createCmd := findSubCmd(findSub("tenant"), "create")
	tenantCreateOptName = "acme"
	tenantCreateOptMaxTargets = 7
	out, err := captureOut(t, func() error { return createCmd.RunE(createCmd, []string{}) })
	require.NoError(t, err)

	printed := regexp.MustCompile(`tenant-[0-9a-f]+`).FindString(out)
	require.NotEmpty(t, printed, "create must print the tenant ID it made a fixture of: %q", out)

	// A later invocation reloads from disk; the printed ID must still resolve.
	tm, _, err := loadTenantManager()
	require.NoError(t, err)
	tt, err := tm.Get(printed)
	require.NoError(t, err, "the ID printed by create must stay addressable")
	assert.Equal(t, "acme", tt.Name)

	// The ID-addressed commands must work off that ID too, not just Get.
	showCmd := findSubCmd(findSub("tenant"), "show")
	suspendCmd := findSubCmd(findSub("tenant"), "suspend")
	require.NotNil(t, showCmd)
	require.NotNil(t, suspendCmd)
	_, err = captureOut(t, func() error { return showCmd.RunE(showCmd, []string{printed}) })
	require.NoError(t, err, "tenant show <printed-id>")
	_, err = captureOut(t, func() error { return suspendCmd.RunE(suspendCmd, []string{printed}) })
	require.NoError(t, err, "tenant suspend <printed-id>")

	// Quota lives in a separate map keyed by the same ID, so a renumbered load
	// loses limits without any error: show would read back "unlimited".
	reloaded, _, err := loadTenantManager()
	require.NoError(t, err)
	q, err := reloaded.QuotaManager().GetQuota(printed)
	require.NoError(t, err)
	assert.Equal(t, 7, q.MaxTargets, "the quota must follow the tenant's own ID")
}

// TestTenantIDIsStableAcrossEveryReload goes one step further than the single
// reload above: the ID must not move no matter how many times the registry is
// re-read, because operators paste these IDs into shell history and tickets.
func TestTenantIDIsStableAcrossEveryReload(t *testing.T) {
	restoreTenantFlagVars(t)
	_, cleanup := setupTenantTestConfig(t)
	defer cleanup()

	createCmd := findSubCmd(findSub("tenant"), "create")
	tenantCreateOptName = "acme"
	out, err := captureOut(t, func() error { return createCmd.RunE(createCmd, []string{}) })
	require.NoError(t, err)
	printed := regexp.MustCompile(`tenant-[0-9a-f]+`).FindString(out)
	require.NotEmpty(t, printed)

	for i := 0; i < 3; i++ {
		tm, _, err := loadTenantManager()
		require.NoError(t, err)
		tt, err := tm.Get(printed)
		require.NoError(t, err, "reload %d lost the tenant identity", i)
		assert.Equal(t, printed, tt.ID, "reload %d", i)
		assert.Equal(t, []string{printed}, tenantIDs(tm.List()), "reload %d", i)
	}
}
