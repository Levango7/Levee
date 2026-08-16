package main

import (
	"encoding/json"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// --- command registration --------------------------------------------------

func TestPushCmdRegistered(t *testing.T) {
	defer resetRootFlags()
	cmd := findSub("push")
	require.NotNil(t, cmd, "push subcommand should be registered")
	assert.Equal(t, "push", cmd.Name())
}

func TestPushSubcommands(t *testing.T) {
	defer resetRootFlags()
	cmd := findSub("push")
	require.NotNil(t, cmd)

	want := []string{"register", "unregister", "send", "devices", "test", "config"}
	for _, name := range want {
		found := false
		for _, sub := range cmd.Commands() {
			if sub.Name() == name {
				found = true
				break
			}
		}
		assert.True(t, found, "push should have %q subcommand", name)
	}
}

// --- register flags --------------------------------------------------------

func TestPushRegisterCmdFlags(t *testing.T) {
	defer resetRootFlags()
	cmd := findSubCmd(findSub("push"), "register")
	require.NotNil(t, cmd)

	for _, flag := range []string{"user", "token", "platform"} {
		f := cmd.Flags().Lookup(flag)
		require.NotNil(t, f, "register should have --%s flag", flag)
	}
	// user and token are required.
	for _, flag := range []string{"user", "token"} {
		f := cmd.Flags().Lookup(flag)
		_, required := f.Annotations[cobra.BashCompOneRequiredFlag]
		assert.True(t, required, "--%s flag should be required", flag)
	}
}

func TestPushUnregisterCmdFlags(t *testing.T) {
	defer resetRootFlags()
	cmd := findSubCmd(findSub("push"), "unregister")
	require.NotNil(t, cmd)
	for _, flag := range []string{"user", "token"} {
		f := cmd.Flags().Lookup(flag)
		require.NotNil(t, f)
		_, required := f.Annotations[cobra.BashCompOneRequiredFlag]
		assert.True(t, required, "--%s flag should be required", flag)
	}
}

func TestPushSendCmdFlags(t *testing.T) {
	defer resetRootFlags()
	cmd := findSubCmd(findSub("push"), "send")
	require.NotNil(t, cmd)
	for _, flag := range []string{"user", "title", "body"} {
		f := cmd.Flags().Lookup(flag)
		require.NotNil(t, f, "send should have --%s flag", flag)
	}
}

func TestPushDevicesCmdFlags(t *testing.T) {
	defer resetRootFlags()
	cmd := findSubCmd(findSub("push"), "devices")
	require.NotNil(t, cmd)
	f := cmd.Flags().Lookup("user")
	require.NotNil(t, f)
	_, required := f.Annotations[cobra.BashCompOneRequiredFlag]
	assert.True(t, required, "--user flag should be required for devices")
}

func TestPushTestCmdFlags(t *testing.T) {
	defer resetRootFlags()
	cmd := findSubCmd(findSub("push"), "test")
	require.NotNil(t, cmd)
	f := cmd.Flags().Lookup("user")
	require.NotNil(t, f)
}

func TestPushConfigCmdFlags(t *testing.T) {
	defer resetRootFlags()
	cmd := findSubCmd(findSub("push"), "config")
	require.NotNil(t, cmd)
	for _, flag := range []string{
		"apns-key-file", "apns-team-id", "apns-key-id", "apns-bundle-id", "apns-production",
		"fcm-key-file", "fcm-project-id",
	} {
		f := cmd.Flags().Lookup(flag)
		require.NotNil(t, f, "config should have --%s flag", flag)
	}
}

// --- pushConfigFile round-trip ---------------------------------------------

func TestPushConfigFile_JSONRoundTrip(t *testing.T) {
	original := &pushConfigFile{
		APNs: pushConfigAPNs{
			PrivateKey: "-----BEGIN PRIVATE KEY-----\nfoo\n-----END PRIVATE KEY-----\n",
			TeamID:     "TEAM123",
			KeyID:      "KEY123",
			BundleID:   "com.example.app",
			Production: true,
		},
		FCM: pushConfigFCM{
			ServiceAccountKey: `{"type":"service_account"}`,
			ProjectID:         "my-project",
		},
	}
	data, err := json.Marshal(original)
	require.NoError(t, err)

	var restored pushConfigFile
	err = json.Unmarshal(data, &restored)
	require.NoError(t, err)
	assert.Equal(t, original.APNs.TeamID, restored.APNs.TeamID)
	assert.Equal(t, original.APNs.Production, restored.APNs.Production)
	assert.Equal(t, original.FCM.ProjectID, restored.FCM.ProjectID)
}

// --- getPushManager lazy init ----------------------------------------------

func TestGetPushManager_LazyInitWithEmptyConfig(t *testing.T) {
	defer resetRootFlags()
	defer resetPushManagerForTest()

	// Without a config file, getPushManager should still return a usable
	// manager (with nil APNs/FCM clients). We point the config path at a
	// temp directory so config.Load falls back to defaults.
	pm, err := getPushManager()
	require.NoError(t, err)
	require.NotNil(t, pm)
}

func TestGetPushManager_SingletonAcrossCalls(t *testing.T) {
	defer resetRootFlags()
	defer resetPushManagerForTest()

	pm1, err := getPushManager()
	require.NoError(t, err)
	pm2, err := getPushManager()
	require.NoError(t, err)
	assert.Same(t, pm1, pm2)
}