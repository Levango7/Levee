// notifier_test.go pins the notifier adapter: the mapping from
// notify.Message onto the plugin wire shape, the init hand-over of the
// registry config, the refusal surfacing, and the record validation. The
// live child is the test binary re-executed with a "notifier" personality
// (see protocol_test.go's TestMain).
package plugin

import (
	"context"
	"os"
	"testing"

	"github.com/nexus/levee/internal/notify"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func testNotifierRecord(binaryPath string) *RegistryRecord {
	return &RegistryRecord{
		Name:       "test-notify",
		Type:       TypeNotifier,
		State:      StateEnabled,
		BinaryPath: binaryPath,
		ConfigYAML: "endpoint: http://example.invalid/hook\nformat: text\n",
	}
}

// A delivered message reaches the plugin with the interface's field
// spellings, and the registry config arrives via init before it.
func TestPluginNotifierSendDeliversAndHandsOverConfig(t *testing.T) {
	t.Setenv(childEnvKey, "notifier")
	n, err := NewPluginNotifier(testNotifierRecord(os.Args[0]))
	require.NoError(t, err)

	msg, err := notify.NewMessage("run_completed", "run-1", notify.LevelInfo,
		"Run finished", "all batches passed")
	require.NoError(t, err)
	msg.Recipients = []notify.Recipient{notify.Channel("ops", "ops")}

	require.NoError(t, n.Send(context.Background(), msg))
}

// A plugin refusal (ok:false) surfaces as an error carrying the plugin's
// own words, not a silent success.
func TestPluginNotifierSendSurfacesPluginRefusal(t *testing.T) {
	t.Setenv(childEnvKey, "notifier")
	n, err := NewPluginNotifier(testNotifierRecord(os.Args[0]))
	require.NoError(t, err)

	msg, err := notify.NewMessage("refuse_me", "run-1", notify.LevelError,
		"should be refused", "")
	require.NoError(t, err)
	msg.Recipients = []notify.Recipient{notify.Channel("ops", "ops")}

	err = n.Send(context.Background(), msg)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "test-notify")
	assert.Contains(t, err.Error(), "upstream webhook returned 503")
}

// Record validation happens before any process starts: wrong type, wrong
// state and nil are refused with the respective sentinels.
func TestPluginNotifierRecordValidation(t *testing.T) {
	_, err := NewPluginNotifier(nil)
	require.Error(t, err)

	gate := testNotifierRecord(os.Args[0])
	gate.Type = TypeGate
	_, err = NewPluginNotifier(gate)
	require.ErrorIs(t, err, ErrInvalidPluginType)

	disabled := testNotifierRecord(os.Args[0])
	disabled.State = StateDisabled
	_, err = NewPluginNotifier(disabled)
	require.ErrorIs(t, err, ErrPluginNotEnabled)
}

// A binary that vanished after enable is a per-delivery error, not a
// construction-time snapshot.
func TestPluginNotifierSendMissingBinaryFailsDelivery(t *testing.T) {
	n, err := NewPluginNotifier(testNotifierRecord(t.TempDir() + "/gone"))
	require.NoError(t, err)

	msg, err := notify.NewMessage("run_completed", "run-1", notify.LevelInfo, "t", "b")
	require.NoError(t, err)
	msg.Recipients = []notify.Recipient{notify.Channel("ops", "ops")}

	err = n.Send(context.Background(), msg)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "binary not found")
}

// The wire mapping is the plugin interface's JSON contract: recipients
// collapse to their IDs, level renders as its word, metadata survives.
func TestNotifyParamsMapping(t *testing.T) {
	msg, err := notify.NewMessage("gate_failed", "run-9", notify.LevelWarning,
		"Gate failed", "post_batch check on batch 2")
	require.NoError(t, err)
	msg.Recipients = []notify.Recipient{
		notify.Channel("ops", "ops"),
		{Type: notify.RecipientOncall, ID: "alice", Name: "Alice"},
	}
	msg.Metadata = map[string]string{"host": "web-01"}

	params := notifyParams(msg)
	assert.Equal(t, "gate_failed", params["event"])
	assert.Equal(t, "run-9", params["run_id"])
	assert.Equal(t, "warning", params["level"])
	assert.Equal(t, []string{"ops", "alice"}, params["recipients"])
	assert.Equal(t, map[string]any{"host": "web-01"}, params["metadata"])
}
