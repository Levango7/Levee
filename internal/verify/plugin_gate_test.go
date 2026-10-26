// plugin_gate_test.go pins the plugin gate: the verdict mapping from the
// plugin protocol to GateResult, the init/check refusal surfacing, and the
// resolver failure path. The live child is the test binary re-executed with
// a "checker" personality (protocol_test.go's TestMain), so the real
// subprocess path — spawn, init, check, close — is exercised end to end.
package verify

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestMain gives this test binary a plugin-child personality: when re-executed
// with LEVEE_PLUGIN_PROTOCOL_TEST_CHILD set, it speaks the JSON-line protocol
// (init/check/close) instead of running the suite. The host side of the
// plugin gate spawns this binary as the plugin, so the child must answer the
// protocol here. The plugin package's protocol_test.go has the same hook for
// its own tests; each package's test binary needs its own because Go test
// binaries are per-package.
func TestMain(m *testing.M) {
	if mode := os.Getenv("LEVEE_PLUGIN_PROTOCOL_TEST_CHILD"); mode != "" {
		os.Exit(verifyChildMain(mode))
	}
	os.Exit(m.Run())
}

func verifyChildMain(mode string) int {
	scanner := bufio.NewScanner(os.Stdin)
	enc := json.NewEncoder(os.Stdout)
	for scanner.Scan() {
		var req pluginHostRequest
		if err := json.Unmarshal(scanner.Bytes(), &req); err != nil {
			fmt.Fprintln(os.Stderr, "child: parse:", err)
			return 1
		}
		switch mode {
		case "checker":
			switch req.Action {
			case "init":
				_ = enc.Encode(pluginHostResponse{OK: true})
			case "check":
				verdict := req.Params["pass"]
				if v, ok := verdict.(string); ok && v == "false" {
					_ = enc.Encode(pluginHostResponse{OK: true, Result: &pluginHostResult{
						Passed:  false,
						Message: "checker says no",
						Details: map[string]any{
							"url":             req.Params["url"],
							"timeout_seconds": req.Params["timeout_seconds"],
						},
					}})
					continue
				}
				_ = enc.Encode(pluginHostResponse{OK: true, Result: &pluginHostResult{
					Passed:  true,
					Message: "checker says yes",
					Details: map[string]any{
						"url":             req.Params["url"],
						"timeout_seconds": req.Params["timeout_seconds"],
					},
				}})
			case "close":
				_ = enc.Encode(pluginHostResponse{OK: true})
				return 0
			default:
				_ = enc.Encode(pluginHostResponse{OK: false, Error: "unknown action"})
			}
		case "garbage":
			fmt.Println("this is not json")
			return 0
		case "notifier":
			switch req.Action {
			case "init":
				_ = enc.Encode(pluginHostResponse{OK: true})
			case "notify":
				event, _ := req.Params["event"].(string)
				if event == "refuse_me" {
					_ = enc.Encode(pluginHostResponse{OK: false, Error: "upstream webhook returned 503"})
					continue
				}
				_ = enc.Encode(pluginHostResponse{OK: true, Result: &pluginHostResult{
					Details: map[string]any{"event": event},
				}})
			case "close":
				_ = enc.Encode(pluginHostResponse{OK: true})
				return 0
			default:
				_ = enc.Encode(pluginHostResponse{OK: false, Error: "unknown action"})
			}
		}
	}
	return 0
}

// pluginHostRequest/pluginHostResponse mirror the wire shapes the host side
// declares in the plugin package, without importing it (this file is in
// verify; the protocol types are unexported there except the ones verify
// itself uses).
type pluginHostRequest struct {
	Action string         `json:"action"`
	Config map[string]any `json:"config,omitempty"`
	Params map[string]any `json:"params,omitempty"`
}

type pluginHostResponse struct {
	OK     bool              `json:"ok"`
	Error  string            `json:"error,omitempty"`
	Result *pluginHostResult `json:"result,omitempty"`
}

type pluginHostResult struct {
	Passed  bool           `json:"passed"`
	Message string         `json:"message"`
	Details map[string]any `json:"details,omitempty"`
}

// countingResolver records the name it was asked for and returns the test
// binary as the plugin path (the child is this binary itself).
func countingResolver(t *testing.T, seen *string) PluginResolver {
	return func(pluginName string) (PluginSpec, error) {
		*seen = pluginName
		return PluginSpec{
			Name:       pluginName,
			BinaryPath: os.Args[0],
			Config:     map[string]any{"endpoint": "http://example.invalid/hook"},
		}, nil
	}
}

// A plugin that answers ok:true with a passing verdict maps to Passed=true,
// and its message and details survive into the GateResult.
func TestPluginGateCheckMapsPassingVerdict(t *testing.T) {
	t.Setenv("LEVEE_PLUGIN_PROTOCOL_TEST_CHILD", "checker")
	seen := ""
	g := NewPluginGate("post-batch-http", PhasePostBatch, "http-probe",
		map[string]any{"url": "https://example.com"}, "", countingResolver(t, &seen))

	res, err := g.Check(context.Background(), GateInput{})
	require.NoError(t, err)
	assert.True(t, res.Passed)
	assert.Equal(t, "checker says yes", res.Message)
	assert.Equal(t, "http-probe", seen)
}

// A plugin verdict of passed:false is a real gate failure — not an error,
// so the manager records it as a result and triggers the phase failure
// action rather than a "gate could not run" path.
func TestPluginGateCheckMapsFailingVerdict(t *testing.T) {
	t.Setenv("LEVEE_PLUGIN_PROTOCOL_TEST_CHILD", "checker")
	seen := ""
	g := NewPluginGate("post-batch-http", PhasePostBatch, "http-probe",
		map[string]any{"url": "https://example.com", "pass": "false"}, "", countingResolver(t, &seen))

	res, err := g.Check(context.Background(), GateInput{})
	require.NoError(t, err)
	assert.False(t, res.Passed)
	assert.Equal(t, "checker says no", res.Message)
}

// A plugin that refuses the check (ok:false) is a gate that could not run,
// surfaced as an error the manager turns into a failed phase — never a pass.
func TestPluginGateCheckSurfacesPluginRefusal(t *testing.T) {
	t.Setenv("LEVEE_PLUGIN_PROTOCOL_TEST_CHILD", "notifier") // notifier refuses "check"
	seen := ""
	g := NewPluginGate("post-batch-http", PhasePostBatch, "http-probe",
		map[string]any{"url": "https://example.com"}, "", countingResolver(t, &seen))

	res, err := g.Check(context.Background(), GateInput{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "http-probe")
	assert.Contains(t, err.Error(), "refused check")
	assert.False(t, res.Passed)
}

// A resolver failure (plugin gone, registry missing) is an error, never a
// pass: the declaration named a plugin that this deployment cannot execute.
func TestPluginGateCheckFailsWhenResolverFails(t *testing.T) {
	g := NewPluginGate("post-batch-http", PhasePostBatch, "vanished",
		map[string]any{}, "", func(pluginName string) (PluginSpec, error) {
			return PluginSpec{}, context.DeadlineExceeded
		})

	_, err := g.Check(context.Background(), GateInput{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "vanished")
}

// A nil resolver is a runtime failure, not a construction error: the
// plan-time refusal (executabilityProblem) already caught declarations when
// no resolver was wired, so reaching Check with a nil resolver means the
// registry disappeared between plan and run — an honest failure.
func TestPluginGateCheckNilResolverFails(t *testing.T) {
	g := NewPluginGate("post-batch-http", PhasePostBatch, "http-probe",
		map[string]any{}, "", nil)

	_, err := g.Check(context.Background(), GateInput{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no plugin resolver")
}

// A plugin name that resolves to a binary which speaks garbage is refused as
// a protocol breach rather than misread.
func TestPluginGateCheckRefusesGarbageOutput(t *testing.T) {
	t.Setenv("LEVEE_PLUGIN_PROTOCOL_TEST_CHILD", "garbage")
	seen := ""
	g := NewPluginGate("post-batch-http", PhasePostBatch, "http-probe",
		map[string]any{}, "", countingResolver(t, &seen))

	_, err := g.Check(context.Background(), GateInput{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not the JSON-line protocol")
}

// The declared timeout is injected into the check params as timeout_seconds
// so plugins that honour it (like the example) can bound their work. The
// checker child echoes timeout_seconds back in its response details, so the
// assertion proves the injection happened rather than only that the check
// passed.
func TestPluginGateCheckInjectsTimeoutSeconds(t *testing.T) {
	t.Setenv("LEVEE_PLUGIN_PROTOCOL_TEST_CHILD", "checker")
	seen := ""
	g := NewPluginGate("post-batch-http", PhasePostBatch, "http-probe",
		map[string]any{"url": "https://example.com"}, "15s", countingResolver(t, &seen))

	res, err := g.Check(context.Background(), GateInput{})
	require.NoError(t, err)
	assert.True(t, res.Passed)
	require.NotNil(t, res.Details)
	assert.Equal(t, float64(15), res.Details["timeout_seconds"], "the 15s declaration must arrive as timeout_seconds=15")
}
