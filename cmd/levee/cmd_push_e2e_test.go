// End-to-end tests for `levee push` (E-2): the in-memory device registry
// (register/devices/unregister), the not-found error paths of send/test, and
// config flag validation. Real APNs/FCM transports need credentials and are
// out of scope; provider init is lazy, so config persistence does not parse
// key material (asserted below).
package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The push commands share the in-process device registry, so their runners
// reset flag values but deliberately keep the manager singleton alive.

func pushFlags() {
	resetDriftFlags()
	resetCalendarFlags()
	resetTargetFlags()
	resetSecretFlags()
	resetPluginFlags()
	resetAuditFlags()
	resetPauseFlags()
	resetChatopsFlags()
	resetSystemFlags()
	resetPushVars()
	resetFlagChangedMarkers()
}

func pushRun(t *testing.T, args ...string) string {
	t.Helper()
	pushFlags()
	out, err := executeCommand(args...)
	require.NoError(t, err, "cli output: %s", out)
	return out
}

func pushRunErr(t *testing.T, args ...string) error {
	t.Helper()
	pushFlags()
	out, err := executeCommand(args...)
	require.Error(t, err, "expected failure, got output: %s", out)
	return err
}

func pushJSON(t *testing.T, args ...string) map[string]any {
	t.Helper()
	pushFlags()
	res, raw, err := executeCommandJSON(args...)
	require.NoError(t, err, "cli output: %s", raw)
	require.NotNil(t, res)
	return res
}

func TestPushE2E_DeviceRegistry(t *testing.T) {
	defer resetRootFlags()
	resetPushFlags(t)
	e := newCLIEnv(t)
	cfg := []string{"--config", e.cfgPath}

	// devices on an empty registry is readable, not an error.
	out := pushRun(t, append([]string{"push", "devices", "--user", "empty-alice"}, cfg...)...)
	assert.NotEmpty(t, trimOut(out))

	// register (human + JSON), platform override to android.
	out = pushRun(t, append([]string{"push", "register", "--user", "alice", "--token", "tok-a"}, cfg...)...)
	assert.Contains(t, out, "alice")
	res := pushJSON(t, append([]string{"push", "register", "--user", "bob",
		"--token", "tok-b", "--platform", "android", "--json"}, cfg...)...)
	require.NotNil(t, res["data"])

	// devices now sees the registry state.
	out = pushRun(t, append([]string{"push", "devices", "--user", "alice"}, cfg...)...)
	assert.Contains(t, out, "alice")

	// unregister finds the registered pair ...
	out = pushRun(t, append([]string{"push", "unregister", "--user", "alice", "--token", "tok-a"}, cfg...)...)
	assert.Contains(t, out, "alice")

	// ... and refuses an unknown pair.
	err := pushRunErr(t, append([]string{"push", "unregister", "--user", "alice", "--token", "nope"}, cfg...)...)
	assert.Contains(t, err.Error(), "not found")

	// Missing required flags rejected.
	err = pushRunErr(t, append([]string{"push", "register", "--user", "x"}, cfg...)...)
	assert.Error(t, err)

	resetPushManagerForTest()
}

func TestPushE2E_SendTestNotFound(t *testing.T) {
	defer resetRootFlags()
	defer resetPushFlags(t)
	e := newCLIEnv(t)
	cfg := []string{"--config", e.cfgPath}

	// No devices registered: send/test fail on the lookup, not the network.
	err := pushRunErr(t, append([]string{"push", "send", "--user", "ghost", "--title", "hello"}, cfg...)...)
	assert.Contains(t, err.Error(), "ghost")
	err = pushRunErr(t, append([]string{"push", "test", "--user", "ghost"}, cfg...)...)
	assert.Error(t, err)

	// send's own required-flag guards run before the lookup.
	err = pushRunErr(t, append([]string{"push", "send", "--title", "no user"}, cfg...)...)
	assert.Error(t, err)
}

func TestPushE2E_ConfigValidation(t *testing.T) {
	defer resetRootFlags()
	defer resetPushFlags(t)
	e := newCLIEnv(t)
	cfg := []string{"--config", e.cfgPath}

	// No flags at all: usage error with the exit=2 marker.
	err := pushRunErr(t, append([]string{"push", "config"}, cfg...)...)
	assert.Contains(t, err.Error(), "[exit=2]")

	// APNs branch requires a readable key file.
	err = pushRunErr(t, append([]string{"push", "config", "--apns-key-file",
		filepath.Join(t.TempDir(), "missing.p8"), "--apns-team-id", "T",
		"--apns-key-id", "K", "--apns-bundle-id", "com.example.app"}, cfg...)...)
	assert.Contains(t, err.Error(), "read")

	// FCM branch likewise for the service-account JSON.
	err = pushRunErr(t, append([]string{"push", "config", "--fcm-key-file",
		filepath.Join(t.TempDir(), "missing.json"), "--fcm-project-id", "p"}, cfg...)...)
	assert.Contains(t, err.Error(), "read")

	// Provider init is LAZY: a present but unparseable key file still saves
	// (it would only fail at first send). The save lands inside the cliEnv
	// data dir — never the developer's real ~/.levee.
	bad := filepath.Join(t.TempDir(), "bad.p8")
	require.NoError(t, os.WriteFile(bad, []byte("not an ec key"), 0o600))
	out := pushRun(t, append([]string{"push", "config", "--apns-key-file", bad,
		"--apns-team-id", "T", "--apns-key-id", "K",
		"--apns-bundle-id", "com.example.app"}, cfg...)...)
	assert.Contains(t, out, "saved")
	assert.FileExists(t, filepath.Join(e.dataDir, "push.json"))
}
