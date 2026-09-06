// End-to-end tests for `levee calendar` (E-2): CRUD against the cliEnv store,
// Changed()-driven update, frozen-window check. Windows use absolute times so
// no clock seam is needed; the check path uses a window that brackets now.
package main

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCalendarE2E_Lifecycle(t *testing.T) {
	defer resetRootFlags()
	e := newCLIEnv(t)
	cfg := []string{"--config", e.cfgPath}

	start := time.Now().UTC().Add(24 * time.Hour).Truncate(time.Second)
	end := start.Add(2 * time.Hour)
	rfc := func(tm time.Time) string { return tm.Format(time.RFC3339) }

	// create (JSON shape, id + repeat/cron fields round-trip).
	res, _, err := freshJSON(t, append([]string{"calendar", "create",
		"--name", "release-window", "--start", rfc(start), "--end", rfc(end),
		"--targets", "prod,staging", "--cron", "0 2 * * 1", "--repeat", "weekly",
		"--json"}, cfg...)...)
	require.NoError(t, err)
	data, ok := res["data"].(map[string]any)
	require.True(t, ok)
	id, ok := data["id"].(string)
	require.True(t, ok, "create JSON must expose id: %v", res)
	assert.Equal(t, "release-window", data["name"])
	assert.Equal(t, "weekly", data["repeat_rule"])

	// Human create variant.
	human := mustRun(t, append([]string{"calendar", "create",
		"--name", "plain", "--start", rfc(start), "--end", rfc(end),
		"--targets", "dev"}, cfg...)...)
	assert.Contains(t, human, "Created window")

	// list (human + JSON + limit).
	out := mustRun(t, append([]string{"calendar", "list"}, cfg...)...)
	assert.Contains(t, out, "release-window")
	res, _, err = freshJSON(t, append([]string{"calendar", "list", "--json", "--limit", "1"}, cfg...)...)
	require.NoError(t, err)
	require.NotNil(t, res)

	// show hit + JSON.
	out = mustRun(t, append([]string{"calendar", "show", id}, cfg...)...)
	assert.Contains(t, out, "release-window")
	res, _, err = freshJSON(t, append([]string{"calendar", "show", id, "--json"}, cfg...)...)
	require.NoError(t, err)
	require.NotNil(t, res)

	// show miss carries the exit=4 marker.
	err = runErr(t, append([]string{"calendar", "show", "win-ghost"}, cfg...)...)
	assert.Contains(t, err.Error(), "not found [exit=4]")

	// update via Changed() flags: name rename, then frozen toggle (JSON).
	out = mustRun(t, append([]string{"calendar", "update", id,
		"--name", "release-renamed"}, cfg...)...)
	assert.Contains(t, out, "release-renamed")
	res, _, err = freshJSON(t, append([]string{"calendar", "update", id,
		"--frozen", "--targets", "prod", "--json"}, cfg...)...)
	require.NoError(t, err)
	data, ok = res["data"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, true, data["is_frozen"])

	// update with a new end time in the human format variant.
	out = mustRun(t, append([]string{"calendar", "update", id, "--start",
		end.Add(-time.Hour).Format("2006-01-02 15:04:05")}, cfg...)...)
	assert.NotEmpty(t, trimOut(out))

	// update without flags / ghost / bad time all fail with markers.
	err = runErr(t, append([]string{"calendar", "update", id}, cfg...)...)
	assert.Contains(t, err.Error(), "nothing to update [exit=2]")
	err = runErr(t, append([]string{"calendar", "update", "win-ghost",
		"--name", "x"}, cfg...)...)
	assert.Contains(t, err.Error(), "not found [exit=4]")
	err = runErr(t, append([]string{"calendar", "update", id,
		"--start", "not-a-time"}, cfg...)...)
	assert.Contains(t, err.Error(), "[exit=2]")

	// delete removes; missing id is documented as not an error.
	out = mustRun(t, append([]string{"calendar", "delete", id}, cfg...)...)
	assert.Contains(t, out, "Deleted window")
	err = runErr(t, append([]string{"calendar", "show", id}, cfg...)...)
	assert.Contains(t, err.Error(), "[exit=4]")
	mustRun(t, append([]string{"calendar", "delete", "win-ghost"}, cfg...)...)
}

func TestCalendarE2E_CreateValidation(t *testing.T) {
	defer resetRootFlags()
	e := newCLIEnv(t)
	cfg := []string{"--config", e.cfgPath}
	now := time.Now().UTC().Truncate(time.Second)
	rfc := func(tm time.Time) string { return tm.Format(time.RFC3339) }

	// end before start is rejected by validateWindow.
	err := runErr(t, append([]string{"calendar", "create", "--name", "bad",
		"--start", rfc(now), "--end", rfc(now.Add(-time.Hour)),
		"--targets", "prod"}, cfg...)...)
	assert.Contains(t, err.Error(), "before start_time")

	// unparseable --start hits the [exit=2] parse guard.
	err = runErr(t, append([]string{"calendar", "create", "--name", "bad",
		"--start", "yesterday", "--end", rfc(now.Add(time.Hour)),
		"--targets", "prod"}, cfg...)...)
	assert.Contains(t, err.Error(), "[exit=2]")

	// missing required flags rejected by cobra.
	err = runErr(t, append([]string{"calendar", "create", "--name", "x"}, cfg...)...)
	assert.Error(t, err)
}

func TestCalendarE2E_Check(t *testing.T) {
	defer resetRootFlags()
	e := newCLIEnv(t)
	cfg := []string{"--config", e.cfgPath}
	now := time.Now().UTC().Truncate(time.Second)
	rfc := func(tm time.Time) string { return tm.Format(time.RFC3339) }

	// Frozen window bracketing "now" for target prod; an unfrozen future
	// window for staging must not freeze its target.
	mustRun(t, append([]string{"calendar", "create", "--name", "freeze",
		"--start", rfc(now.Add(-2 * time.Hour)), "--end", rfc(now.Add(2 * time.Hour)),
		"--targets", "prod", "--frozen"}, cfg...)...)
	mustRun(t, append([]string{"calendar", "create", "--name", "later",
		"--start", rfc(now.Add(72 * time.Hour)), "--end", rfc(now.Add(73 * time.Hour)),
		"--targets", "staging"}, cfg...)...)

	res, _, err := freshJSON(t, append([]string{"calendar", "check",
		"--targets", "prod", "--json"}, cfg...)...)
	require.NoError(t, err)
	data, ok := res["data"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, true, data["frozen"])
	assert.Equal(t, float64(1), data["active_count"])

	// Target with no covering window: not frozen, no active windows.
	res, _, err = freshJSON(t, append([]string{"calendar", "check",
		"--targets", "unlisted", "--json"}, cfg...)...)
	require.NoError(t, err)
	data, ok = res["data"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, false, data["frozen"])
	assert.Equal(t, float64(0), data["active_count"])

	// Human variants: frozen prints "frozen", quiet variant too.
	out := mustRun(t, append([]string{"calendar", "check", "--targets", "prod"}, cfg...)...)
	assert.Contains(t, out, "freeze")
	quiet := mustRun(t, append([]string{"--quiet", "calendar", "check",
		"--targets", "prod"}, cfg...)...)
	assert.Contains(t, quiet, "frozen")
}
