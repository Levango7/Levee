package plugin

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The live-handshake test re-executes the test binary as a child speaking
// the JSON-line protocol (the same self-re-execution trick the agent
// registry E2E uses): the child branch is selected by environment, which
// Probe's fixed-argv invocation inherits.
const childEnvKey = "LEVEE_PLUGIN_PROTOCOL_TEST_CHILD"

func TestMain(m *testing.M) {
	if mode := os.Getenv(childEnvKey); mode != "" {
		os.Exit(protocolChildMain(mode))
	}
	os.Exit(m.Run())
}

// protocolChildMain implements three child personalities:
//
//	"echo"   — a well-behaved plugin: answers "meta" with an identity and
//	           acks "close" (then exits, as the protocol expects).
//	"garbage"— writes a non-JSON line and exits: the host must refuse to
//	           interpret it as a response.
//	"notifier"— a well-behaved notifier: acks "init" (recording the config
//	           keys), answers "notify" ok:false for a refused event and
//	           ok:true otherwise, acks "close".
func protocolChildMain(mode string) int {
	scanner := bufio.NewScanner(os.Stdin)
	enc := json.NewEncoder(os.Stdout)
	for scanner.Scan() {
		var req HostRequest
		if err := json.Unmarshal(scanner.Bytes(), &req); err != nil {
			fmt.Fprintln(os.Stderr, "child: parse:", err)
			return 1
		}
		switch mode {
		case "notifier":
			switch req.Action {
			case "init":
				keys := make([]string, 0, len(req.Config))
				for k := range req.Config {
					keys = append(keys, k)
				}
				_ = enc.Encode(PluginResponse{OK: true, Result: &ProbeResult{
					Details: map[string]any{"init_keys": keys},
				}})
			case "notify":
				event, _ := req.Params["event"].(string)
				if event == "refuse_me" {
					_ = enc.Encode(PluginResponse{OK: false, Error: "upstream webhook returned 503"})
					continue
				}
				_ = enc.Encode(PluginResponse{OK: true, Result: &ProbeResult{
					Details: map[string]any{
						"event": event,
						"level": req.Params["level"],
					},
				}})
			case "close":
				_ = enc.Encode(PluginResponse{OK: true})
				return 0
			default:
				_ = enc.Encode(PluginResponse{OK: false, Error: "unknown action"})
			}
		case "echo":
			switch req.Action {
			case "meta":
				_ = enc.Encode(PluginResponse{
					OK: true,
					Result: &ProbeResult{
						Passed:  true,
						Message: "protocol-test-child",
						Details: map[string]any{"version": "1.0.0", "type": "gate"},
					},
				})
			case "close":
				_ = enc.Encode(PluginResponse{OK: true})
				return 0
			default:
				_ = enc.Encode(PluginResponse{OK: false, Error: "unknown action"})
			}
		case "garbage":
			fmt.Println("this is not json")
			return 0
		}
	}
	return 0
}

// A plugin that speaks the protocol answers the handshake with its identity
// and shuts down cleanly on close.
func TestProbeLiveHandshake(t *testing.T) {
	t.Setenv(childEnvKey, "echo")

	responses, err := Probe(context.Background(), os.Args[0], MetaHandshake())
	require.NoError(t, err)
	require.Len(t, responses, 2)

	assert.True(t, responses[0].OK)
	require.NotNil(t, responses[0].Result)
	assert.Equal(t, "protocol-test-child", responses[0].Result.Message)
	assert.Equal(t, "1.0.0", responses[0].Result.Details["version"])
	assert.True(t, responses[1].OK, "close must be acknowledged")
}

// Output that is not the protocol must be refused as such, not parsed into
// a misleading zero-value response.
func TestProbeRefusesNonProtocolOutput(t *testing.T) {
	t.Setenv(childEnvKey, "garbage")

	_, err := Probe(context.Background(), os.Args[0], MetaHandshake())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not the JSON-line protocol")
}

// Registry paths that do not name a regular file are refused before any
// process is started.
func TestProbeRefusesUnusableBinaryPaths(t *testing.T) {
	_, err := Probe(context.Background(), filepath.Join(t.TempDir(), "missing"), MetaHandshake())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "binary not found")

	dir := t.TempDir()
	_, err = Probe(context.Background(), dir, MetaHandshake())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "directory")

	_, err = Probe(context.Background(), "", MetaHandshake())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "empty binary path")
}

// MetaHandshake pins its own shape: the CLI depends on exactly two
// responses arriving in this order.
func TestMetaHandshakeShape(t *testing.T) {
	reqs := MetaHandshake()
	require.Len(t, reqs, 2)
	assert.Equal(t, "meta", reqs[0].Action)
	assert.Equal(t, "close", reqs[1].Action)
}
