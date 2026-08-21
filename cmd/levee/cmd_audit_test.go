package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nexus/levee/internal/audit"
	"github.com/nexus/levee/internal/state"
)

func TestAuditCmdRegistered(t *testing.T) {
	defer resetRootFlags()
	cmd := findSub("audit")
	require.NotNil(t, cmd, "audit subcommand should be registered")
	assert.Equal(t, "audit", cmd.Name())
}

func TestAuditSubcommands(t *testing.T) {
	defer resetRootFlags()
	cmd := findSub("audit")
	require.NotNil(t, cmd)

	subNames := []string{"verify", "export", "list", "show"}
	for _, name := range subNames {
		found := false
		for _, sub := range cmd.Commands() {
			if sub.Name() == name {
				found = true
				break
			}
		}
		assert.True(t, found, "audit should have %q subcommand", name)
	}
}

func TestAuditVerifyCmdRequiresArg(t *testing.T) {
	defer resetRootFlags()
	cmd := findSub("audit")
	require.NotNil(t, cmd)

	verifyCmd := findSubCmd(cmd, "verify")
	require.NotNil(t, verifyCmd)

	err := verifyCmd.Args(verifyCmd, []string{})
	assert.Error(t, err, "audit verify requires run-id arg")
}

func TestAuditExportCmdRequiresArg(t *testing.T) {
	defer resetRootFlags()
	cmd := findSub("audit")
	require.NotNil(t, cmd)

	exportCmd := findSubCmd(cmd, "export")
	require.NotNil(t, exportCmd)

	err := exportCmd.Args(exportCmd, []string{})
	assert.Error(t, err, "audit export requires run-id arg")
}

func TestAuditExportCmdFormatFlag(t *testing.T) {
	defer resetRootFlags()
	cmd := findSub("audit")
	require.NotNil(t, cmd)

	exportCmd := findSubCmd(cmd, "export")
	require.NotNil(t, exportCmd)

	f := exportCmd.Flags().Lookup("format")
	require.NotNil(t, f, "audit export should have --format flag")
	assert.Equal(t, "json", f.DefValue, "format flag should default to json")
}

func TestAuditListCmdRequiresArg(t *testing.T) {
	defer resetRootFlags()
	cmd := findSub("audit")
	require.NotNil(t, cmd)

	listCmd := findSubCmd(cmd, "list")
	require.NotNil(t, listCmd)

	err := listCmd.Args(listCmd, []string{})
	assert.Error(t, err, "audit list requires run-id arg")
}

func TestAuditShowCmdRequiresArg(t *testing.T) {
	defer resetRootFlags()
	cmd := findSub("audit")
	require.NotNil(t, cmd)

	showCmd := findSubCmd(cmd, "show")
	require.NotNil(t, showCmd)

	err := showCmd.Args(showCmd, []string{})
	assert.Error(t, err, "audit show requires trace-id arg")
}

func TestAuditVerifyExitCode(t *testing.T) {
	defer resetRootFlags()

	err := fmt.Errorf("chain verification failed for run %q: %d of %d records tampered [exit=6]",
		"run-001", 1, 5)
	assert.Equal(t, 6, exitCodeFor(err))
}

func TestAuditOutputEnvelope(t *testing.T) {
	defer resetRootFlags()

	data := map[string]any{
		"run_id": "run-001",
		"valid":  true,
		"count":  5,
	}

	var buf bytes.Buffer
	require.NoError(t, PrintJSON(&buf, map[string]any{
		"data":  data,
		"meta":  nil,
		"error": nil,
	}))

	var env outputEnvelope
	require.NoError(t, json.Unmarshal(buf.Bytes(), &env))
	assert.NotNil(t, env.Data)
}

func TestPrintAuditVerifyHuman(t *testing.T) {
	defer resetRootFlags()

	output := map[string]any{
		"run_id":   "run-001",
		"valid":    true,
		"count":    5,
		"failures": []audit.ChainFailure{},
	}

	var buf bytes.Buffer
	printAuditVerifyHuman(&buf, output)
	assert.Contains(t, buf.String(), "run-001")
	assert.Contains(t, buf.String(), "Valid")
}

func TestPrintAuditVerifyHumanWithFailures(t *testing.T) {
	defer resetRootFlags()

	output := map[string]any{
		"run_id": "run-001",
		"valid":  false,
		"count":  5,
		"failures": []audit.ChainFailure{
			{TraceID: "trace-001", Index: 2, Type: audit.FailureHashMismatch},
		},
	}

	var buf bytes.Buffer
	printAuditVerifyHuman(&buf, output)
	assert.Contains(t, buf.String(), "Failures")
	assert.Contains(t, buf.String(), "trace-001")
}

func TestPrintAuditListHuman(t *testing.T) {
	defer resetRootFlags()

	rows := []map[string]any{
		{"id": "trace-001", "event": "step_execute", "actor": "system", "timestamp": "2026-08-15T10:00:00Z"},
	}

	var buf bytes.Buffer
	printAuditListHuman(&buf, rows)
	assert.Contains(t, buf.String(), "trace-001")
	assert.Contains(t, buf.String(), "step_execute")
}

func TestPrintAuditListHumanEmpty(t *testing.T) {
	defer resetRootFlags()

	var buf bytes.Buffer
	printAuditListHuman(&buf, nil)
	assert.Contains(t, buf.String(), "No traces found")
}

func TestPrintAuditShowHuman(t *testing.T) {
	defer resetRootFlags()

	trace := &state.Trace{
		ID:        "trace-001",
		RunID:     "run-001",
		Event:     "step_execute",
		Actor:     "system",
		Detail:    `{"step":"restart"}`,
		PrevHash:  "abc",
		CurrHash:  "def",
		Timestamp: state.Trace{}.Timestamp,
	}

	var buf bytes.Buffer
	printAuditShowHuman(&buf, trace)
	assert.Contains(t, buf.String(), "trace-001")
	assert.Contains(t, buf.String(), "step_execute")
	assert.Contains(t, buf.String(), "system")
}

func TestExportTracesCSV(t *testing.T) {
	defer resetRootFlags()

	traces := []*state.Trace{
		{
			ID:        "trace-001",
			RunID:     "run-001",
			Event:     "step_execute",
			Actor:     "system",
			Detail:    `{"step":"restart"}`,
			PrevHash:  "",
			CurrHash:  "abc123",
			Timestamp: state.Trace{}.Timestamp,
		},
	}

	var buf bytes.Buffer
	require.NoError(t, exportTracesCSV(&buf, traces))
	assert.Contains(t, buf.String(), "id,run_id,event")
	assert.Contains(t, buf.String(), "trace-001")
}
