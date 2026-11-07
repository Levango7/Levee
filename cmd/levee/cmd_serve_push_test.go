package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// testAPNsPrivateKeyPEM is a fresh P-256 key in PKCS8 PEM form, generated
// per test run: a long-lived key must not be committed to the repository.
func testAPNsPrivateKeyPEM(t *testing.T) string {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	der, err := x509.MarshalPKCS8PrivateKey(key)
	require.NoError(t, err)
	return string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}))
}

// The daemon's push wiring is the second half of A-3: the push manager used
// to be hard-coded to nil in cmd_serve.go, so an approval request never
// produced a notification no matter how the deployment was configured.
//
// These cases pin the wiring itself — whether a manager is handed to the
// mobile service, and which deep-link origin is used. The transports
// themselves are covered in internal/push against local HTTP servers.

// writePushConfig writes a push configuration into dataDir.
func writePushConfig(t *testing.T, dataDir string, cfg pushConfigFile) {
	t.Helper()
	require.NoError(t, os.MkdirAll(dataDir, 0o750))
	data, err := json.MarshalIndent(cfg, "", "  ")
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(dataDir, "push.json"), data, 0o600))
}

// TestNewPushRuntime_NoCredentialsIsNotConfigured: with no credentials the
// deployment has no push channel. The runtime says so through
// configured=false — a manager that silently drops messages must not be
// handed to the daemon as if it could deliver.
func TestNewPushRuntime_NoCredentialsIsNotConfigured(t *testing.T) {
	rt, err := newPushRuntimeFromDataDir(t.TempDir())
	require.NoError(t, err)
	assert.False(t, rt.configured, "no credentials means no push channel")
	assert.NotNil(t, rt.manager, "the CLI still needs a manager to enroll devices")
}

// A deployment with APNs credentials has a channel, so the runtime reports
// itself as configured.
func TestNewPushRuntime_APNsCredentialsAreConfigured(t *testing.T) {
	dir := t.TempDir()
	writePushConfig(t, dir, pushConfigFile{
		APNs: pushConfigAPNs{
			PrivateKey: testAPNsPrivateKeyPEM(t),
			TeamID:     "TEAMID1234",
			KeyID:      "KEYID12345",
			BundleID:   "com.example.levee",
		},
	})

	rt, err := newPushRuntimeFromDataDir(dir)
	require.NoError(t, err)
	assert.True(t, rt.configured)
	assert.NotNil(t, rt.manager)
}

// The base URL is the deployment's public origin for web deep links.
func TestNewPushRuntime_ReadsBaseURL(t *testing.T) {
	dir := t.TempDir()
	writePushConfig(t, dir, pushConfigFile{BaseURL: "https://levee.example.com"})

	rt, err := newPushRuntimeFromDataDir(dir)
	require.NoError(t, err)
	assert.Equal(t, "https://levee.example.com", rt.baseURL)
}

// A push configuration that cannot be parsed is an error: starting up as if
// no credentials were configured would silently disable the feature.
func TestNewPushRuntime_CorruptConfigIsAnError(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "push.json"), []byte("{not json"), 0o600))

	_, err := newPushRuntimeFromDataDir(dir)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "parse push config")
}

// TestInstallServeMobileApproval_NoCredentialsMeansNoPushManager is the
// regression guard for the hard-coded nil: a deployment without push
// credentials gets a nil manager, so the mobile service says "push
// disabled" rather than pretending to deliver.
func TestInstallServeMobileApproval_NoCredentialsMeansNoPushManager(t *testing.T) {
	svc, manager := installServeMobileApproval(nil, t.TempDir())
	require.NotNil(t, svc)
	assert.Nil(t, manager, "without credentials the daemon must not hold a push manager")
}

// With credentials configured, the daemon DOES get a push manager — this is
// the wiring that was missing.
func TestInstallServeMobileApproval_WithCredentialsGetsPushManager(t *testing.T) {
	dir := t.TempDir()
	writePushConfig(t, dir, pushConfigFile{
		APNs: pushConfigAPNs{
			PrivateKey: testAPNsPrivateKeyPEM(t),
			TeamID:     "TEAMID1234",
			KeyID:      "KEYID12345",
			BundleID:   "com.example.levee",
		},
	})

	svc, manager := installServeMobileApproval(nil, dir)
	require.NotNil(t, svc)
	assert.NotNil(t, manager, "a configured deployment must get a push manager")
}

// The device registry the daemon reads is the one the CLI writes: a device
// enrolled with `levee push register` must be visible to the daemon.
func TestInstallServeMobileApproval_SharesTheCLIDeviceRegistry(t *testing.T) {
	dir := t.TempDir()
	writePushConfig(t, dir, pushConfigFile{
		APNs: pushConfigAPNs{
			PrivateKey: testAPNsPrivateKeyPEM(t),
			TeamID:     "TEAMID1234",
			KeyID:      "KEYID12345",
			BundleID:   "com.example.levee",
		},
	})

	rt, err := newPushRuntimeFromDataDir(dir)
	require.NoError(t, err)
	require.NotNil(t, rt.manager)
	require.NoError(t, rt.manager.RegisterDevice("alice", "tok-a", "ios"))

	// A second runtime over the same data dir — the daemon starting after
	// the CLI enrolled the device.
	rt2, err := newPushRuntimeFromDataDir(dir)
	require.NoError(t, err)
	require.NotNil(t, rt2.manager)
	devices := rt2.manager.ListDevices("alice")
	require.Len(t, devices, 1, "the daemon must see the devices the CLI enrolled")
	assert.Equal(t, "tok-a", devices[0].Token)
}

// TestInstallServeMobileApproval_BrokenConfigDoesNotDisableTheDaemon: a
// push configuration that cannot be read or parsed must not take the control
// plane down. Push delivery is best-effort — the approval is recorded either
// way — so the daemon starts with push disabled and an error in the log.
func TestInstallServeMobileApproval_BrokenConfigDoesNotDisableTheDaemon(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "push.json"), []byte("{not json"), 0o600))

	svc, manager := installServeMobileApproval(nil, dir)
	require.NotNil(t, svc, "the daemon must still get a working mobile service")
	assert.Nil(t, manager, "push is disabled when its configuration is unusable")
}

// A push configuration whose key material cannot be parsed is also
// non-fatal for the daemon (the same reason as the corrupt file).
func TestInstallServeMobileApproval_UnparseableKeyDoesNotDisableTheDaemon(t *testing.T) {
	dir := t.TempDir()
	writePushConfig(t, dir, pushConfigFile{
		APNs: pushConfigAPNs{
			PrivateKey: "-----BEGIN PRIVATE KEY-----\nnot a key\n-----END PRIVATE KEY-----\n",
			TeamID:     "TEAMID1234",
			KeyID:      "KEYID12345",
			BundleID:   "com.example.levee",
		},
	})

	svc, manager := installServeMobileApproval(nil, dir)
	require.NotNil(t, svc)
	assert.Nil(t, manager)
}
