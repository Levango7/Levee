package main

// cli_flag_wiring_test.go covers the agent, drift and push wiring behind the
// flags this PR adds to the CLI. The registration gate
// (cli_reference_flags_test.go) only proves a documented --flag exists; these
// cases drive the real RunE so a declared-but-ignored flag turns red.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nexus/levee/internal/agent"
)

// restoreFilterFlagVars resets the agent/drift option vars this file writes.
// They are process-global by cobra's design, so a value left set leaks into the
// next test in the package.
func restoreFilterFlagVars(t *testing.T) {
	t.Helper()
	t.Cleanup(func() {
		agentListOptStatus = ""
		driftOptHost = ""
		driftOptFile = ""
	})
}

// --- agent list --status ----------------------------------------------------

func TestAgentStatusVocabularyIsSingleSourced(t *testing.T) {
	// The reference documented "active / inactive / lost" — none of which the
	// registry can produce. Pinning the exact list is what keeps a fabricated
	// vocabulary from coming back.
	assert.Equal(t, []string{"registered", "idle", "busy", "offline"}, agent.AgentStatusNames())

	for _, name := range agent.AgentStatusNames() {
		got, err := agent.ParseAgentStatus(name)
		require.NoError(t, err, name)
		assert.Equal(t, name, string(got))
	}

	for _, constant := range []agent.AgentStatus{
		agent.StatusRegistered, agent.StatusIdle, agent.StatusBusy, agent.StatusOffline,
	} {
		assert.Contains(t, agent.AgentStatusNames(), string(constant))
	}

	for _, bad := range []string{"active", "inactive", "lost", "", "off"} {
		_, err := agent.ParseAgentStatus(bad)
		assert.Error(t, err, "ParseAgentStatus(%q) must refuse", bad)
	}

	// Case and surrounding space are normalised, matching ParseTenantStatus.
	got, err := agent.ParseAgentStatus("  Idle ")
	require.NoError(t, err)
	assert.Equal(t, agent.StatusIdle, got)
}

func TestRunAgentListActuallyAppliesStatusFilter(t *testing.T) {
	restoreFilterFlagVars(t)
	resetGlobalRegistry()
	defer resetGlobalRegistry()

	registry := getGlobalAgentRegistry()
	require.NoError(t, registry.Register(agent.AgentInfo{ID: "a1", Address: "10.0.0.1:9091", Status: agent.StatusIdle}))
	require.NoError(t, registry.Register(agent.AgentInfo{ID: "b2", Address: "10.0.0.2:9091", Status: agent.StatusBusy}))
	require.NoError(t, registry.Register(agent.AgentInfo{ID: "c3", Address: "10.0.0.3:9091", Status: agent.StatusOffline}))

	listCmd := findSubCmd(findSub("agent"), "list")
	require.NotNil(t, listCmd)

	for _, tc := range []struct {
		status string
		want   []string
		miss   []string
	}{
		{status: "idle", want: []string{"a1"}, miss: []string{"b2", "c3"}},
		{status: "offline", want: []string{"c3"}, miss: []string{"a1", "b2"}},
	} {
		for _, quiet := range []bool{false, true} {
			mode := "human"
			if quiet {
				mode = "quiet"
			}
			agentListOptStatus = tc.status
			optJSON, optQuiet = false, quiet
			out, err := captureOut(t, func() error { return listCmd.RunE(listCmd, []string{}) })
			require.NoError(t, err, tc.status+"/"+mode)
			for _, id := range tc.want {
				assert.Contains(t, out, id, tc.status+"/"+mode)
			}
			for _, id := range tc.miss {
				assert.NotContains(t, out, id, tc.status+"/"+mode+" must not leak other statuses")
			}
		}
	}

	// The vocabulary the docs used to promise must now fail loudly rather than
	// print "No agents found.", which reads as an empty cluster.
	agentListOptStatus = "active"
	optQuiet = false
	_, err := captureOut(t, func() error { return listCmd.RunE(listCmd, []string{}) })
	require.Error(t, err)
	assert.Contains(t, err.Error(), "registered")

	agentListOptStatus = ""
	out, err := captureOut(t, func() error { return listCmd.RunE(listCmd, []string{}) })
	require.NoError(t, err)
	assert.Contains(t, out, "a1")
	assert.Contains(t, out, "b2")
	assert.Contains(t, out, "c3")
}

// --- drift baseline list --host --------------------------------------------

func TestRunDriftBaselineListHostFilter(t *testing.T) {
	restoreFilterFlagVars(t)
	dataDir, cleanup := setupTenantTestConfig(t)
	defer cleanup()

	setCmd := findSubCmd(findSubCmd(findSub("drift"), "baseline"), "set")
	listCmd := findSubCmd(findSubCmd(findSub("drift"), "baseline"), "list")
	require.NotNil(t, setCmd)
	require.NotNil(t, listCmd)

	baselineFile := filepath.Join(dataDir, "baseline.yaml")
	require.NoError(t, os.WriteFile(baselineFile,
		[]byte("items:\n  - check_name: hosts-file\n    type: file_exists\n    path: /etc/hosts\n"), 0o600))

	for _, host := range []string{"web-01", "web-02"} {
		driftOptHost = host
		driftOptFile = baselineFile
		require.NoError(t, setCmd.RunE(setCmd, []string{}))
	}

	optJSON = true
	driftOptHost = "web-01"
	out, err := captureOut(t, func() error { return listCmd.RunE(listCmd, []string{}) })
	require.NoError(t, err)
	assert.Contains(t, out, "web-01")
	assert.NotContains(t, out, "web-02", "--host must narrow the listing")
	assert.Equal(t, 1, decodedCount(t, out), "the reported count must describe the filtered set")

	driftOptHost = ""
	out, err = captureOut(t, func() error { return listCmd.RunE(listCmd, []string{}) })
	require.NoError(t, err)
	assert.Contains(t, out, "web-01")
	assert.Contains(t, out, "web-02", "no --host lists every stored baseline")
	assert.Equal(t, 2, decodedCount(t, out))

	driftOptHost = "web-02"
	out, err = captureOut(t, func() error { return listCmd.RunE(listCmd, []string{}) })
	require.NoError(t, err)
	assert.Contains(t, out, "web-02")
	assert.NotContains(t, out, "web-01")
	assert.Equal(t, 1, decodedCount(t, out))
}

func decodedCount(t *testing.T, out string) int {
	t.Helper()
	var envelope struct {
		Meta struct {
			Count int `json:"count"`
		} `json:"meta"`
	}
	require.NoError(t, json.Unmarshal([]byte(strings.TrimSpace(out)), &envelope), "output: %s", out)
	return envelope.Meta.Count
}

// --- push send --deep-link --------------------------------------------------

func TestPushSendDataPayload(t *testing.T) {
	// The key comes from the mobile client contract already in the codebase
	// (internal/approval/mobile.go), not from a preference in this file.
	assert.Equal(t, map[string]string{"deeplink": "levee://approval/run-123"},
		pushSendData("levee://approval/run-123"))
	assert.Nil(t, pushSendData(""), "no --deep-link must keep the pre-flag nil payload")
}

// TestPushSendPassesBuiltPayloadToTheManager is the call-site half of the
// wiring: a helper that is correct but never handed to SendToUser would still
// deliver no deep link, and no output-level assertion can see that because the
// transport is not configured in tests.
func TestPushSendPassesBuiltPayloadToTheManager(t *testing.T) {
	src, err := os.ReadFile("cmd_push.go")
	require.NoError(t, err)
	assert.Contains(t, string(src),
		"pm.SendToUser(ctx, pushOptUser, pushOptTitle, pushOptBody, pushSendData(pushOptDeepLink))",
		"runPushSend must send the built payload rather than nil")
	assert.NotContains(t, string(src),
		"pm.SendToUser(ctx, pushOptUser, pushOptTitle, pushOptBody, nil)",
		"the hardcoded nil payload must not come back")
}
