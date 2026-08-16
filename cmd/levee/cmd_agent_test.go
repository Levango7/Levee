package main

import (
	"bytes"
	"sync"
	"testing"

	"github.com/nexus/levee/internal/agent"
	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// --- parseCapabilities -----------------------------------------------------

func TestParseCapabilities(t *testing.T) {
	defer resetRootFlags()
	tests := []struct {
		in   string
		want []string
	}{
		{"shell,file,pkg", []string{"shell", "file", "pkg"}},
		{" shell , file ", []string{"shell", "file"}},
		{"", []string{}},
		{"single", []string{"single"}},
		{",,,", []string{}},
	}
	for _, tc := range tests {
		got := parseCapabilities(tc.in)
		assert.Equal(t, tc.want, got, "input=%q", tc.in)
	}
}

// --- agentInfoToMap --------------------------------------------------------

func TestAgentInfoToMap(t *testing.T) {
	defer resetRootFlags()
	info := agent.AgentInfo{
		ID:            "a1",
		Address:       ":9091",
		Capabilities:  []string{"shell"},
		Status:        agent.StatusIdle,
		MaxConcurrent: 4,
		ActiveTasks:   1,
	}
	m := agentInfoToMap(info)
	assert.Equal(t, "a1", m["id"])
	assert.Equal(t, ":9091", m["address"])
	assert.Equal(t, "idle", m["status"])
	assert.Equal(t, 3, m["spare_capacity"])
}

// --- global registry / master client ---------------------------------------

func resetGlobalRegistry() {
	globalAgentRegistry = nil
	globalAgentRegistryOnce = sync.Once{}
	globalMasterClient = nil
	globalMasterClientOnce = sync.Once{}
}

func TestGetGlobalAgentRegistry(t *testing.T) {
	defer resetRootFlags()
	resetGlobalRegistry()
	r1 := getGlobalAgentRegistry()
	r2 := getGlobalAgentRegistry()
	assert.Same(t, r1, r2, "global registry must be a singleton")
}

func TestGetGlobalMasterClient(t *testing.T) {
	defer resetRootFlags()
	resetGlobalRegistry()
	c1 := getGlobalMasterClient()
	c2 := getGlobalMasterClient()
	assert.Same(t, c1, c2, "global master client must be a singleton")
	assert.Same(t, getGlobalAgentRegistry(), c1.Registry())
}

func TestNewInProcessMasterClientIgnoresAddr(t *testing.T) {
	defer resetRootFlags()
	resetGlobalRegistry()
	c1 := newInProcessMasterClient("localhost:9090")
	c2 := newInProcessMasterClient("other:1234")
	assert.Same(t, c1, c2, "address is ignored; same singleton returned")
}

// --- CLI command wiring ----------------------------------------------------

func TestAgentCmdRegistered(t *testing.T) {
	defer resetRootFlags()
	cmd := findSub("agent")
	require.NotNil(t, cmd, "agent command should be registered on root")
	assert.Equal(t, "agent", cmd.Name())
}

func TestAgentCmdHasSubcommands(t *testing.T) {
	defer resetRootFlags()
	cmd := findSub("agent")
	require.NotNil(t, cmd)

	expected := []string{"start", "status", "list", "show", "remove"}
	for _, name := range expected {
		found := false
		for _, sub := range cmd.Commands() {
			if sub.Name() == name {
				found = true
				break
			}
		}
		assert.True(t, found, "agent command should have %q subcommand", name)
	}
}

// --- agent start flags -----------------------------------------------------

func TestAgentStartFlags(t *testing.T) {
	defer resetRootFlags()
	cmd := findSub("agent")
	require.NotNil(t, cmd)

	startCmd := findSubCmd(cmd, "start")
	require.NotNil(t, startCmd)

	flags := []string{"addr", "master", "caps", "max-concurrent", "heartbeat", "id"}
	for _, f := range flags {
		assert.NotNil(t, startCmd.Flag(f), "start command should have --%s flag", f)
	}
}

// --- agent list (human) ----------------------------------------------------

func TestPrintAgentListHumanEmpty(t *testing.T) {
	defer resetRootFlags()
	var buf bytes.Buffer
	printAgentListHuman(&buf, nil)
	assert.Contains(t, buf.String(), "No agents")
}

func TestPrintAgentListHumanWithRows(t *testing.T) {
	defer resetRootFlags()
	rows := []map[string]any{
		{"id": "a1", "address": ":9091", "status": "idle", "active_tasks": 0, "spare_capacity": 2},
		{"id": "a2", "address": ":9092", "status": "busy", "active_tasks": 2, "spare_capacity": 0},
	}
	var buf bytes.Buffer
	printAgentListHuman(&buf, rows)
	out := buf.String()
	assert.Contains(t, out, "a1")
	assert.Contains(t, out, ":9091")
	assert.Contains(t, out, "a2")
	assert.Contains(t, out, "ID")
}

// --- agent show (human) ----------------------------------------------------

func TestPrintAgentShowHuman(t *testing.T) {
	defer resetRootFlags()
	m := map[string]any{
		"id":              "a1",
		"address":         ":9091",
		"status":          "idle",
		"capabilities":    []string{"shell"},
		"last_heartbeat":  "2026-08-16T12:00:00Z",
		"active_tasks":    0,
		"completed_tasks": int64(5),
		"failed_tasks":    int64(1),
		"max_concurrent":  2,
		"spare_capacity":  2,
	}
	var buf bytes.Buffer
	printAgentShowHuman(&buf, m)
	out := buf.String()
	assert.Contains(t, out, "a1")
	assert.Contains(t, out, ":9091")
	assert.Contains(t, out, "idle")
	assert.Contains(t, out, "shell")
}

// --- agent status (human) --------------------------------------------------

func TestPrintAgentStatusHuman(t *testing.T) {
	defer resetRootFlags()
	m := map[string]any{
		"status":          "offline",
		"active_tasks":    0,
		"completed_tasks": int64(0),
		"failed_tasks":    int64(0),
		"max_concurrent":  1,
	}
	var buf bytes.Buffer
	printAgentStatusHuman(&buf, m)
	out := buf.String()
	assert.Contains(t, out, "Status")
	assert.Contains(t, out, "offline")
}

// --- runAgentList / runAgentShow / runAgentRemove via registry -------------

func TestRunAgentListEmpty(t *testing.T) {
	defer resetRootFlags()
	resetGlobalRegistry()

	var buf bytes.Buffer
	oldJSON := optJSON
	optJSON = false
	defer func() { optJSON = oldJSON }()

	// Redirect stdout by calling the run function with a captured
	// output via the human printer directly.
	registry := getGlobalAgentRegistry()
	agents := registry.List()
	rows := make([]map[string]any, 0, len(agents))
	for _, a := range agents {
		rows = append(rows, agentInfoToMap(a))
	}
	printAgentListHuman(&buf, rows)
	assert.Contains(t, buf.String(), "No agents")
}

func TestRunAgentShowMissing(t *testing.T) {
	defer resetRootFlags()
	resetGlobalRegistry()

	err := runAgentShow(nil, []string{"nope"})
	require.Error(t, err)
}

func TestRunAgentRemoveMissing(t *testing.T) {
	defer resetRootFlags()
	resetGlobalRegistry()

	err := runAgentRemove(nil, []string{"nope"})
	require.Error(t, err)
}

func TestRunAgentStatusHuman(t *testing.T) {
	defer resetRootFlags()
	var buf bytes.Buffer
	oldJSON := optJSON
	optJSON = false
	defer func() { optJSON = oldJSON }()

	// Capture stdout by redirecting through the human printer.
	exec := agent.NewAgentExecutor(1, nil)
	st := exec.Stats()
	m := map[string]any{
		"status":          string(agent.StatusOffline),
		"active_tasks":    st.Active,
		"completed_tasks": st.Completed,
		"failed_tasks":    st.Failed,
		"max_concurrent":  exec.MaxConcurrent(),
	}
	printAgentStatusHuman(&buf, m)
	assert.Contains(t, buf.String(), "Status")
}

// --- command structure validation ------------------------------------------

func TestAgentStartCmdNoArgs(t *testing.T) {
	defer resetRootFlags()
	cmd := findSub("agent")
	require.NotNil(t, cmd)
	startCmd := findSubCmd(cmd, "start")
	require.NotNil(t, startCmd)

	err := startCmd.Args(startCmd, []string{"unexpected"})
	assert.Error(t, err, "agent start should not accept positional args")
}

func TestAgentShowCmdRequiresArg(t *testing.T) {
	defer resetRootFlags()
	cmd := findSub("agent")
	require.NotNil(t, cmd)
	showCmd := findSubCmd(cmd, "show")
	require.NotNil(t, showCmd)

	err := showCmd.Args(showCmd, []string{})
	assert.Error(t, err, "agent show requires an agent-id arg")
}

func TestAgentRemoveCmdRequiresArg(t *testing.T) {
	defer resetRootFlags()
	cmd := findSub("agent")
	require.NotNil(t, cmd)
	removeCmd := findSubCmd(cmd, "remove")
	require.NotNil(t, removeCmd)

	err := removeCmd.Args(removeCmd, []string{})
	assert.Error(t, err, "agent remove requires an agent-id arg")
}

// --- cobra command type check ---------------------------------------------

func TestAgentCmdsAreCobra(t *testing.T) {
	defer resetRootFlags()
	cmd := findSub("agent")
	require.NotNil(t, cmd)
	assert.IsType(t, &cobra.Command{}, cmd)
}
