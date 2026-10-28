package plugin

import (
	"bufio"
	"context"
	"encoding/base64"
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

// protocolChildMain implements four child personalities:
//
//	"echo"   — a well-behaved plugin: answers "meta" with an identity and
//	           acks "close" (then exits, as the protocol expects).
//	"garbage"— writes a non-JSON line and exits: the host must refuse to
//	           interpret it as a response.
//	"notifier"— a well-behaved notifier: acks "init" (recording the config
//	           keys), answers "notify" ok:false for a refused event and
//	           ok:true otherwise, acks "close".
//	"checker"— a well-behaved gate plugin: acks "init", answers "check"
//	           with a verdict driven by params["pass"] (default pass), and
//	           acks "close".
//	"module" — a well-behaved module plugin: meta declares the action list
//	           and idempotency, "init" is required before "execute" (so the
//	           config handoff is observable), "validate" refuses the verb
//	           "bad_input", "execute" refuses the verb "explode".
//	"module-noactions" — a module plugin whose meta declares no action list:
//	           the host must refuse to register a module that accepts
//	           nothing.
//	"channel" — a well-behaved channel plugin (a session: many requests over
//	           one process): meta identifies it, "connect" requires
//	           params.host and records credential_ref, "exec" answers per cmd
//	           (hang / die / refuse / nopayload / fail / default success),
//	           "upload" stores the base64 payload per path ("/denied"
//	           refuses), "collect" returns that store back (or a synthetic
//	           body for never-uploaded paths), disconnect and close are
//	           acknowledged.
func protocolChildMain(mode string) int {
	scanner := bufio.NewScanner(os.Stdin)
	// The plugin-side contract is lines up to 1 MiB (the examples set this
	// explicitly, and the host's transfer payloads are sized to fit): without
	// it the default 64 KiB cap would kill this child on any real upload
	// frame, which is exactly the failure mode the oversize guard exists to
	// avoid.
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	enc := json.NewEncoder(os.Stdout)
	initialised := false
	credRef := ""
	uploaded := map[string]string{} // channel personality: path -> base64
	for scanner.Scan() {
		var req HostRequest
		if err := json.Unmarshal(scanner.Bytes(), &req); err != nil {
			fmt.Fprintln(os.Stderr, "child: parse:", err)
			return 1
		}
		switch mode {
		case "checker":
			switch req.Action {
			case "init":
				keys := make([]string, 0, len(req.Config))
				for k := range req.Config {
					keys = append(keys, k)
				}
				_ = enc.Encode(PluginResponse{OK: true, Result: &ProbeResult{
					Details: map[string]any{"init_keys": keys},
				}})
			case "check":
				verdict := req.Params["pass"]
				if v, ok := verdict.(string); ok && v == "false" {
					_ = enc.Encode(PluginResponse{OK: true, Result: &ProbeResult{
						Passed:  false,
						Message: "checker says no",
						Details: map[string]any{
							"url":             req.Params["url"],
							"timeout_seconds": req.Params["timeout_seconds"],
						},
					}})
					continue
				}
				_ = enc.Encode(PluginResponse{OK: true, Result: &ProbeResult{
					Passed:  true,
					Message: "checker says yes",
					Details: map[string]any{
						"url":             req.Params["url"],
						"timeout_seconds": req.Params["timeout_seconds"],
					},
				}})
			case "close":
				_ = enc.Encode(PluginResponse{OK: true})
				return 0
			default:
				_ = enc.Encode(PluginResponse{OK: false, Error: "unknown action"})
			}
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
		case "channel":
			switch req.Action {
			case "meta":
				_ = enc.Encode(PluginResponse{OK: true, Result: &ProbeResult{
					Passed:  true,
					Message: "channel-child",
					Details: map[string]any{"name": "channel-child", "type": "channel"},
				}})
			case "init":
				initialised = true
				_ = enc.Encode(PluginResponse{OK: true})
			case "connect":
				host, _ := req.Params["host"].(string)
				if host == "" {
					_ = enc.Encode(PluginResponse{OK: false, Error: "connect requires params.host"})
					continue
				}
				// The boundary this personality pins: plaintext credentials
				// must never reach a channel plugin, only the reference name.
				if _, sent := req.Params["credentials"]; sent {
					_ = enc.Encode(PluginResponse{OK: false, Error: "plaintext credentials must not be sent to a channel plugin"})
					continue
				}
				credRef, _ = req.Params["credential_ref"].(string)
				_ = enc.Encode(PluginResponse{OK: true})
			case "exec":
				if !initialised {
					_ = enc.Encode(PluginResponse{OK: false, Error: "exec before init"})
					continue
				}
				cmd, _ := req.Params["cmd"].(string)
				switch cmd {
				case "hang":
					// Deliberately answer nothing: the host must kill the
					// session rather than wait forever.
				case "pid":
					// Answers with this process's own PID, which is what lets a
					// test tell "the pool reused the session" apart from "the
					// host dialled again".
					_ = enc.Encode(PluginResponse{OK: true, Exec: &ChannelExecResult{
						ExitCode: 0, Stdout: fmt.Sprintf("%d", os.Getpid()), Duration: 1,
					}})
				case "die":
					os.Exit(3)
				case "refuse":
					_ = enc.Encode(PluginResponse{OK: false, Error: "target rejected the command"})
				case "nopayload":
					_ = enc.Encode(PluginResponse{OK: true})
				case "fail":
					_ = enc.Encode(PluginResponse{OK: true, Exec: &ChannelExecResult{
						ExitCode: 2, Stderr: "command failed", Duration: 7,
					}})
				default:
					_ = enc.Encode(PluginResponse{OK: true, Exec: &ChannelExecResult{
						ExitCode: 0,
						Stdout:   "ran " + cmd + " ref=" + credRef,
						Duration: 3,
					}})
				}
			case "upload":
				if !initialised {
					_ = enc.Encode(PluginResponse{OK: false, Error: "upload before init"})
					continue
				}
				path, _ := req.Params["path"].(string)
				if path == "/denied" {
					_ = enc.Encode(PluginResponse{OK: false, Error: "target rejected the write"})
					continue
				}
				data, _ := req.Params["data"].(string)
				uploaded[path] = data
				_ = enc.Encode(PluginResponse{OK: true})
			case "collect":
				path, _ := req.Params["path"].(string)
				if up, ok := uploaded[path]; ok {
					_ = enc.Encode(PluginResponse{OK: true, Collect: &CollectPayload{Data: up}})
					continue
				}
				_ = enc.Encode(PluginResponse{OK: true, Collect: &CollectPayload{
					Data: base64.StdEncoding.EncodeToString([]byte("collected:" + path)),
				}})
			case "disconnect":
				_ = enc.Encode(PluginResponse{OK: true})
			case "close":
				_ = enc.Encode(PluginResponse{OK: true})
				return 0
			default:
				_ = enc.Encode(PluginResponse{OK: false, Error: "unknown action"})
			}
		case "module", "module-noactions":
			switch req.Action {
			case "meta":
				details := map[string]any{"name": "module-child", "idempotent": true}
				if mode == "module" {
					details["actions"] = []any{"apply", "destroy"}
				}
				_ = enc.Encode(PluginResponse{OK: true, Result: &ProbeResult{
					Passed:  true,
					Message: "module-child",
					Details: details,
				}})
			case "init":
				initialised = true
				keys := make([]string, 0, len(req.Config))
				for k := range req.Config {
					keys = append(keys, k)
				}
				_ = enc.Encode(PluginResponse{OK: true, Result: &ProbeResult{
					Details: map[string]any{"init_keys": keys},
				}})
			case "validate":
				if req.Params["action"] == "bad_input" {
					_ = enc.Encode(PluginResponse{OK: false, Error: "args missing region"})
					continue
				}
				_ = enc.Encode(PluginResponse{OK: true})
			case "execute":
				if !initialised {
					_ = enc.Encode(PluginResponse{OK: false, Error: "execute before init"})
					continue
				}
				if req.Params["action"] == "explode" {
					_ = enc.Encode(PluginResponse{OK: false, Error: "upstream API returned 500"})
					continue
				}
				_ = enc.Encode(PluginResponse{OK: true, Module: &ModuleResult{
					ExitCode: 0,
					Stdout:   "applied " + fmt.Sprint(req.Params["action"]),
					Stderr:   "",
					Changed:  true,
					Duration: 42,
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
