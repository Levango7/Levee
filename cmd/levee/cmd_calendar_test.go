package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nexus/levee/internal/calendar"
)

// =========================================================================
// Command registration & structure
// =========================================================================

func TestCalendarCmdRegistered(t *testing.T) {
	defer resetRootFlags()
	cmd := findSub("calendar")
	require.NotNil(t, cmd, "calendar subcommand should be registered")
	assert.Equal(t, "calendar", cmd.Name())
}

func TestCalendarSubcommands(t *testing.T) {
	defer resetRootFlags()
	cmd := findSub("calendar")
	require.NotNil(t, cmd)

	subNames := []string{"list", "show", "create", "update", "delete", "check"}
	for _, name := range subNames {
		found := false
		for _, sub := range cmd.Commands() {
			if sub.Name() == name {
				found = true
				break
			}
		}
		assert.True(t, found, "calendar should have %q subcommand", name)
	}
}

func TestCalendarListCmdFlags(t *testing.T) {
	defer resetRootFlags()
	cmd := findSubCmd(findSub("calendar"), "list")
	require.NotNil(t, cmd)

	f := cmd.Flags().Lookup("limit")
	require.NotNil(t, f, "list should have --limit flag")
	assert.Equal(t, "0", f.DefValue)
}

func TestCalendarCreateCmdFlags(t *testing.T) {
	defer resetRootFlags()
	cmd := findSubCmd(findSub("calendar"), "create")
	require.NotNil(t, cmd)

	for _, name := range []string{"name", "start", "end", "targets", "frozen", "cron", "repeat"} {
		f := cmd.Flags().Lookup(name)
		require.NotNil(t, f, "create should have --%s flag", name)
	}

	// Required flags.
	for _, name := range []string{"name", "start", "end", "targets"} {
		f := cmd.Flags().Lookup(name)
		_, required := f.Annotations[cobra.BashCompOneRequiredFlag]
		assert.True(t, required, "--%s should be required", name)
	}
}

func TestCalendarShowCmdRequiresArg(t *testing.T) {
	defer resetRootFlags()
	cmd := findSubCmd(findSub("calendar"), "show")
	require.NotNil(t, cmd)

	err := cmd.Args(cmd, []string{})
	assert.Error(t, err)
}

func TestCalendarUpdateCmdRequiresArg(t *testing.T) {
	defer resetRootFlags()
	cmd := findSubCmd(findSub("calendar"), "update")
	require.NotNil(t, cmd)

	err := cmd.Args(cmd, []string{})
	assert.Error(t, err)
}

func TestCalendarDeleteCmdRequiresArg(t *testing.T) {
	defer resetRootFlags()
	cmd := findSubCmd(findSub("calendar"), "delete")
	require.NotNil(t, cmd)

	err := cmd.Args(cmd, []string{})
	assert.Error(t, err)
}

func TestCalendarCheckCmdTargetsRequired(t *testing.T) {
	defer resetRootFlags()
	cmd := findSubCmd(findSub("calendar"), "check")
	require.NotNil(t, cmd)

	f := cmd.Flags().Lookup("targets")
	require.NotNil(t, f)
	_, required := f.Annotations[cobra.BashCompOneRequiredFlag]
	assert.True(t, required, "--targets should be required")
}

// =========================================================================
// Helpers
// =========================================================================

func TestParseTimeFlag_RFC3339(t *testing.T) {
	got, err := parseTimeFlag("2026-08-16T10:00:00Z")
	require.NoError(t, err)
	assert.True(t, got.Equal(time.Date(2026, 8, 16, 10, 0, 0, 0, time.UTC)))
	assert.Equal(t, time.UTC, got.Location())
}

func TestParseTimeFlag_NoTZ(t *testing.T) {
	got, err := parseTimeFlag("2026-08-16T10:00:00")
	require.NoError(t, err)
	assert.Equal(t, time.UTC, got.Location())
}

func TestParseTimeFlag_SpaceSeparated(t *testing.T) {
	got, err := parseTimeFlag("2026-08-16 10:00:00")
	require.NoError(t, err)
	assert.Equal(t, time.UTC, got.Location())
	assert.Equal(t, 10, got.Hour())
}

func TestParseTimeFlag_Empty(t *testing.T) {
	_, err := parseTimeFlag("")
	require.Error(t, err)
}

func TestParseTimeFlag_Invalid(t *testing.T) {
	_, err := parseTimeFlag("not-a-time")
	require.Error(t, err)
}

func TestParseTargetsFlag(t *testing.T) {
	assert.Equal(t, []string{"web", "db"}, parseTargetsFlag("web,db"))
	assert.Equal(t, []string{"web", "db"}, parseTargetsFlag(" web , db "))
	assert.Equal(t, []string{"web"}, parseTargetsFlag("web,,"))
	assert.Nil(t, parseTargetsFlag(""))
}

func TestGenerateCalendarID(t *testing.T) {
	id, err := generateCalendarID()
	require.NoError(t, err)
	assert.True(t, strings.HasPrefix(id, "win-"), "id should have win- prefix, got %s", id)
	assert.Len(t, id, 4+16) // "win-" + 16 hex chars

	// Two calls should produce different IDs.
	id2, err := generateCalendarID()
	require.NoError(t, err)
	assert.NotEqual(t, id, id2)
}

func TestWindowToMap(t *testing.T) {
	// windowToMap defines the JSON field names `levee calendar ... --json`
	// emits, so a rename here breaks external consumers without failing any
	// other test. This replaces an unconditional t.Skip that could never run.
	start := time.Date(2026, 9, 1, 2, 0, 0, 0, time.UTC)
	end := start.Add(2 * time.Hour)
	w := &calendar.Window{
		ID:           "win-1",
		Name:         "nightly",
		StartTime:    start,
		EndTime:      end,
		TargetLabels: []string{"group:web", "env:prod"},
		IsFrozen:     true,
		RepeatRule:   "weekly",
		CronExpr:     "0 2 * * *",
		CreatedAt:    start,
		UpdatedAt:    end,
	}

	m := windowToMap(w)

	assert.Equal(t, "win-1", m["id"])
	assert.Equal(t, "nightly", m["name"])
	assert.Equal(t, start.Format(time.RFC3339), m["start_time"])
	assert.Equal(t, end.Format(time.RFC3339), m["end_time"])
	assert.Equal(t, w.TargetLabels, m["target_labels"])
	assert.Equal(t, true, m["is_frozen"])
	assert.Equal(t, "weekly", m["repeat_rule"])
	assert.Equal(t, "0 2 * * *", m["cron_expr"])
	assert.Equal(t, start.Format(time.RFC3339), m["created_at"])
	assert.Equal(t, end.Format(time.RFC3339), m["updated_at"])
	// Ten keys, no extras: the shape is the contract.
	assert.Len(t, m, 10)
}

// =========================================================================
// Output rendering
// =========================================================================

func TestPrintCalendarListHuman_Empty(t *testing.T) {
	var buf bytes.Buffer
	printCalendarListHuman(&buf, nil)
	assert.Contains(t, buf.String(), "No calendar windows")
}

func TestPrintCalendarListHuman_WithRows(t *testing.T) {
	var buf bytes.Buffer
	rows := []map[string]any{
		{
			"id":            "win-1",
			"name":          "maintenance",
			"start_time":    "2026-08-16T10:00:00Z",
			"end_time":      "2026-08-16T12:00:00Z",
			"is_frozen":     false,
			"target_labels": []string{"web", "db"},
		},
	}
	printCalendarListHuman(&buf, rows)
	out := buf.String()
	assert.Contains(t, out, "win-1")
	assert.Contains(t, out, "maintenance")
	assert.Contains(t, out, "web,db")
}

func TestPrintCalendarShowHuman(t *testing.T) {
	var buf bytes.Buffer
	row := map[string]any{
		"id":            "win-1",
		"name":          "maintenance",
		"start_time":    "2026-08-16T10:00:00Z",
		"end_time":      "2026-08-16T12:00:00Z",
		"is_frozen":     true,
		"target_labels": []string{"web"},
		"repeat_rule":   "weekly",
		"cron_expr":     "0 2 * * 1",
		"created_at":    "2026-08-15T00:00:00Z",
		"updated_at":    "2026-08-15T00:00:00Z",
	}
	printCalendarShowHuman(&buf, row)
	out := buf.String()
	assert.Contains(t, out, "win-1")
	assert.Contains(t, out, "maintenance")
	assert.Contains(t, out, "Frozen:     true")
	assert.Contains(t, out, "Repeat:     weekly")
	assert.Contains(t, out, "Cron:       0 2 * * 1")
}

func TestPrintCalendarCheckHuman_Frozen(t *testing.T) {
	var buf bytes.Buffer
	output := map[string]any{
		"targets":        []string{"web"},
		"now":            "2026-08-16T11:00:00Z",
		"frozen":         true,
		"active_windows": []map[string]any{},
		"active_count":   0,
	}
	printCalendarCheckHuman(&buf, output)
	assert.Contains(t, buf.String(), "FROZEN")
}

func TestPrintCalendarCheckHuman_OK(t *testing.T) {
	var buf bytes.Buffer
	output := map[string]any{
		"targets":        []string{"web"},
		"now":            "2026-08-16T11:00:00Z",
		"frozen":         false,
		"active_windows": []map[string]any{},
		"active_count":   0,
	}
	printCalendarCheckHuman(&buf, output)
	assert.Contains(t, buf.String(), "OK")
}

func TestTargetsString(t *testing.T) {
	assert.Equal(t, "web,db", targetsString([]string{"web", "db"}))
	assert.Equal(t, "web,db", targetsString([]any{"web", "db"}))
	assert.Equal(t, "", targetsString(nil))
	assert.Equal(t, "whatever", targetsString("whatever"))
}

// =========================================================================
// Output envelope
// =========================================================================

func TestCalendarOutputEnvelope(t *testing.T) {
	defer resetRootFlags()
	rows := []map[string]any{
		{"id": "win-1", "name": "maintenance"},
		{"id": "win-2", "name": "freeze"},
	}
	var buf bytes.Buffer
	require.NoError(t, PrintJSON(&buf, map[string]any{
		"data":  rows,
		"meta":  map[string]any{"count": 2},
		"error": nil,
	}))

	var env outputEnvelope
	require.NoError(t, json.Unmarshal(buf.Bytes(), &env))
	require.NotNil(t, env.Data)
	raw, err := json.Marshal(env.Data)
	require.NoError(t, err)
	var result []any
	require.NoError(t, json.Unmarshal(raw, &result))
	assert.Len(t, result, 2)
}
