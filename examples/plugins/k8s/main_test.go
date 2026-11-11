package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeRunner records the kubectl invocation and replays canned output. It
// replaces realRunner so the tests assert on the ARGUMENT VECTOR — the
// contract this plugin exists to enforce — without needing a cluster.
type fakeRunner struct {
	mu     sync.Mutex
	args   []string
	stdin  []byte
	stdout string
	stderr string
	code   int
}

func (f *fakeRunner) run(_ context.Context, _ string, args []string, stdin []byte) (string, string, int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.args = args
	f.stdin = stdin
	return f.stdout, f.stderr, f.code, nil
}

// useRunner installs r for the duration of the test and returns it.
func useRunner(t *testing.T, r *fakeRunner) *fakeRunner {
	t.Helper()
	old := realRunner
	realRunner = r
	t.Cleanup(func() { realRunner = old })
	return r
}

// connectedPlugin returns a plugin that has been inited with cfg and bound to
// ctxName — the state every exec/collect/upload test needs.
func connectedPlugin(t *testing.T, cfg map[string]any, ctxName string) *plugin {
	t.Helper()
	p := &plugin{}
	resp := p.handle(&hostRequest{Action: "init", Config: cfg})
	require.True(t, resp.OK, "init refused: %s", resp.Error)
	resp = p.handle(&hostRequest{Action: "connect", Params: map[string]any{"host": ctxName}})
	require.True(t, resp.OK, "connect refused: %s", resp.Error)
	return p
}

// --- tokenizer --------------------------------------------------------------

func TestTokenizeSplitsAndHonoursQuotes(t *testing.T) {
	tokens, err := tokenize(`kubectl scale deployment web --replicas=3`)
	require.NoError(t, err)
	assert.Equal(t, []string{"kubectl", "scale", "deployment", "web", "--replicas=3"}, tokens)
}

// The host's snapshotter quotes the resource reference with single quotes
// (shellQuoteSingle); the plugin must read back exactly the path it wrote.
func TestTokenizeReadsBackHostSingleQuoting(t *testing.T) {
	tokens, err := tokenize(`base64 'deployment/web'`)
	require.NoError(t, err)
	assert.Equal(t, []string{"base64", "deployment/web"}, tokens)
}

func TestTokenizeRefusesShellMetacharacters(t *testing.T) {
	for _, cmd := range []string{
		"kubectl get pods; rm -rf /",
		"kubectl get pods | cat",
		"kubectl get pods && kubectl delete ns prod",
		"kubectl get pods $(whoami)",
		"kubectl get pods `id`",
		"kubectl get pods > /tmp/x",
	} {
		_, err := tokenize(cmd)
		require.Error(t, err, "command %q must be refused", cmd)
	}
}

func TestTokenizeRefusesUnterminatedQuoteAndDanglingBackslash(t *testing.T) {
	_, err := tokenize(`base64 'deployment/web`)
	require.Error(t, err)
	_, err = tokenize(`kubectl get deployment web \`)
	require.Error(t, err)
}

// --- parseCommand -----------------------------------------------------------

func TestParseCommandKubectlForm(t *testing.T) {
	cmd, err := parseCommand("kubectl scale deployment web --replicas=3")
	require.NoError(t, err)
	assert.Equal(t, "scale", cmd.verb)
	assert.Equal(t, []string{"deployment", "web", "--replicas=3"}, cmd.args)
	assert.Empty(t, cmd.capture)
}

func TestParseCommandCaptureForm(t *testing.T) {
	cmd, err := parseCommand(`base64 'deployment/web'`)
	require.NoError(t, err)
	assert.True(t, cmd.captureOnly)
	assert.Equal(t, "deployment/web", cmd.capture)
}

func TestParseCommandRefusals(t *testing.T) {
	cases := []struct {
		name string
		cmd  string
	}{
		{"empty", "   "},
		{"not kubectl", "sh -c something"},
		{"kubectl without verb", "kubectl"},
		{"global flag before verb", "kubectl --kubeconfig=/other get pods"},
		{"capture needs exactly one ref", "base64 deployment/web extra"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := parseCommand(tc.cmd)
			require.Error(t, err)
		})
	}
}

// --- target-flag policy -----------------------------------------------------

func TestCheckTargetFlagsRefusesTargetRewrites(t *testing.T) {
	for _, flag := range []string{
		"--kubeconfig=/other", "--kubeconfig", "--context=other", "--context",
		"--server=https://other", "-n", "--namespace=other", "--namespace",
		"--token=x", "--as=system:admin", "--as-group=x", "--as-uid=x",
		"--insecure-skip-tls-verify", "--request-timeout=1h",
	} {
		err := checkTargetFlags([]string{"deployment", "web", flag})
		require.Error(t, err, "flag %q must be refused", flag)
	}
}

func TestCheckTargetFlagsAllowsWorkloadArguments(t *testing.T) {
	require.NoError(t, checkTargetFlags([]string{
		"deployment", "web", "--replicas=3", "-o", "json", "--timeout=30s",
	}))
}

// --- plugin verbs -----------------------------------------------------------

func TestConnectRequiresListedContext(t *testing.T) {
	p := &plugin{}
	p.handle(&hostRequest{Action: "init", Config: map[string]any{
		"allowed_contexts": []any{"kind-levee-s2"},
	}})

	resp := p.handle(&hostRequest{Action: "connect", Params: map[string]any{"host": "prod-cluster"}})
	require.False(t, resp.OK, "a context outside the allowlist must be refused")
	assert.Contains(t, resp.Error, "allowed_contexts")

	resp = p.handle(&hostRequest{Action: "connect", Params: map[string]any{"host": "kind-levee-s2"}})
	require.True(t, resp.OK)
}

func TestConnectRefusesRepointingASession(t *testing.T) {
	p := connectedPlugin(t, map[string]any{
		"allowed_contexts": []any{"kind-levee-s2", "kind-levee-b"},
	}, "kind-levee-s2")

	resp := p.handle(&hostRequest{Action: "connect", Params: map[string]any{"host": "kind-levee-b"}})
	require.False(t, resp.OK, "a session bound to one cluster must not be re-pointed")
}

func TestConnectRefusesUnreadableKubeconfig(t *testing.T) {
	p := &plugin{}
	p.handle(&hostRequest{Action: "init", Config: map[string]any{
		"allowed_contexts": []any{"kind-levee-s2"},
		"kubeconfig":       t.TempDir() + "/nope.yaml",
	}})
	resp := p.handle(&hostRequest{Action: "connect", Params: map[string]any{"host": "kind-levee-s2"}})
	require.False(t, resp.OK)
	assert.Contains(t, resp.Error, "kubeconfig")
}

func TestExecInjectsPinnedTargetBeforeTheVerb(t *testing.T) {
	fake := useRunner(t, &fakeRunner{stdout: "{}"})
	kubeconfig := t.TempDir() + "/kubeconfig"
	require.NoError(t, os.WriteFile(kubeconfig, []byte("apiVersion: v1\n"), 0o600))
	p := connectedPlugin(t, map[string]any{
		"allowed_contexts": []any{"kind-levee-s2"},
		"kubeconfig":       kubeconfig,
	}, "kind-levee-s2")

	resp := p.handle(&hostRequest{Action: "exec", Params: map[string]any{
		"cmd": "kubectl scale deployment web --replicas=3",
	}})
	require.True(t, resp.OK, "exec refused: %s", resp.Error)
	require.NotNil(t, resp.Exec)
	assert.Equal(t, 0, resp.Exec.ExitCode)

	// The pinned flags must come BEFORE the verb: a caller-supplied flag
	// could otherwise take precedence (kubectl honours the last occurrence).
	assert.Equal(t, []string{
		"--kubeconfig", kubeconfig,
		"--context", "kind-levee-s2",
		"scale", "deployment", "web", "--replicas=3",
	}, fake.args)
}

func TestExecRefusesVerbsThatTurnTheChannelIntoAnEscapeHatch(t *testing.T) {
	p := connectedPlugin(t, map[string]any{
		"allowed_contexts": []any{"kind-levee-s2"},
	}, "kind-levee-s2")
	for _, cmd := range []string{
		"kubectl exec web -- ls",
		"kubectl port-forward pod/web 8080",
		"kubectl proxy",
		"kubectl cp web:/etc/passwd ./x",
		"kubectl run x --image=busybox",
		"kubectl auth can-i --list",
		"kubectl config use-context prod",
		"kubectl edit deployment web",
	} {
		resp := p.handle(&hostRequest{Action: "exec", Params: map[string]any{"cmd": cmd}})
		require.False(t, resp.OK, "command %q must be refused", cmd)
	}
}

func TestExecRefusesTargetRewritesInsideTheCommand(t *testing.T) {
	p := connectedPlugin(t, map[string]any{
		"allowed_contexts": []any{"kind-levee-s2"},
	}, "kind-levee-s2")
	resp := p.handle(&hostRequest{Action: "exec", Params: map[string]any{
		"cmd": "kubectl scale deployment web --replicas=3 --namespace=other",
	}})
	require.False(t, resp.OK, "the caller must not be able to move the change to another namespace")
}

func TestExecBeforeConnectIsRefused(t *testing.T) {
	useRunner(t, &fakeRunner{stdout: "{}"})
	p := &plugin{}
	p.handle(&hostRequest{Action: "init", Config: map[string]any{
		"allowed_contexts": []any{"kind-levee-s2"},
	}})
	resp := p.handle(&hostRequest{Action: "exec", Params: map[string]any{
		"cmd": "kubectl get pods",
	}})
	require.False(t, resp.OK)
	assert.Contains(t, resp.Error, "before connect")
}

// A captured manifest must not carry the server's bookkeeping: replaying
// resourceVersion makes the restore conflict instead of restoring.
func TestCaptureStripsServerOwnedFields(t *testing.T) {
	captured := `{
	  "apiVersion": "apps/v1",
	  "kind": "Deployment",
	  "metadata": {
	    "name": "web",
	    "namespace": "default",
	    "uid": "8f0c-...",
	    "resourceVersion": "424242",
	    "generation": 7,
	    "creationTimestamp": "2026-11-11T05:00:00Z",
	    "labels": {"app": "web"},
	    "managedFields": [{"manager": "kubectl"}]
	  },
	  "spec": {"replicas": 1, "selector": {"matchLabels": {"app": "web"}}},
	  "status": {"replicas": 1, "readyReplicas": 1}
	}`
	useRunner(t, &fakeRunner{stdout: captured})
	p := connectedPlugin(t, map[string]any{
		"allowed_contexts": []any{"kind-levee-s2"},
	}, "kind-levee-s2")

	resp := p.handle(&hostRequest{Action: "exec", Params: map[string]any{
		"cmd": `base64 'deployment/web'`,
	}})
	require.True(t, resp.OK, "capture refused: %s", resp.Error)
	require.NotNil(t, resp.Exec)

	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(resp.Exec.Stdout))
	require.NoError(t, err, "the host decodes capture stdout as base64")

	var obj map[string]any
	require.NoError(t, json.Unmarshal(raw, &obj))
	meta, ok := obj["metadata"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "web", meta["name"], "the workload identity must survive the strip")
	assert.Equal(t, "Deployment", obj["kind"])
	for _, gone := range []string{"resourceVersion", "uid", "generation", "creationTimestamp", "managedFields"} {
		assert.NotContains(t, meta, gone, "server-owned field %s must be stripped", gone)
	}
	assert.NotContains(t, obj, "status", "status belongs to the server, not to the change")
	spec, ok := obj["spec"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, float64(1), spec["replicas"], "the workload declaration must survive the strip")
}

// The host's snapshotter treats a non-zero exit as a failed capture and
// aborts the run; the plugin must surface kubectl's failure as a refusal,
// never as an empty success.
func TestCaptureSurfacesKubectlFailure(t *testing.T) {
	useRunner(t, &fakeRunner{stderr: `Error from server (NotFound)`, code: 1})
	p := connectedPlugin(t, map[string]any{
		"allowed_contexts": []any{"kind-levee-s2"},
	}, "kind-levee-s2")

	resp := p.handle(&hostRequest{Action: "exec", Params: map[string]any{
		"cmd": `base64 'deployment/web'`,
	}})
	require.False(t, resp.OK, "a failed get is not a clean capture")
	assert.Contains(t, resp.Error, "NotFound")
}

func TestUploadAppliesTheCapturedManifest(t *testing.T) {
	manifest := []byte(`{"apiVersion":"apps/v1","kind":"Deployment","metadata":{"name":"web"},"spec":{"replicas":1}}`)
	fake := useRunner(t, &fakeRunner{stdout: "{}"})
	p := connectedPlugin(t, map[string]any{
		"allowed_contexts": []any{"kind-levee-s2"},
	}, "kind-levee-s2")

	resp := p.handle(&hostRequest{Action: "upload", Params: map[string]any{
		"path": "deployment/web",
		"data": base64.StdEncoding.EncodeToString(manifest),
	}})
	require.True(t, resp.OK, "upload refused: %s", resp.Error)
	assert.Equal(t, []string{"apply", "-f", "-"}, fake.args[len(fake.args)-3:])
	assert.Equal(t, manifest, fake.stdin, "the captured bytes must be handed to kubectl verbatim")
}

func TestUploadRefusesEmptyAndNonBase64Payloads(t *testing.T) {
	useRunner(t, &fakeRunner{stdout: "{}"})
	p := connectedPlugin(t, map[string]any{
		"allowed_contexts": []any{"kind-levee-s2"},
	}, "kind-levee-s2")

	resp := p.handle(&hostRequest{Action: "upload", Params: map[string]any{
		"path": "deployment/web", "data": "not base64 !!",
	}})
	require.False(t, resp.OK)
	resp = p.handle(&hostRequest{Action: "upload", Params: map[string]any{
		"path": "deployment/web", "data": base64.StdEncoding.EncodeToString([]byte("   ")),
	}})
	require.False(t, resp.OK, "an empty restore payload is not a success")
}

func TestCollectRefusesFlagShapedReferences(t *testing.T) {
	useRunner(t, &fakeRunner{stdout: "{}"})
	p := connectedPlugin(t, map[string]any{
		"allowed_contexts": []any{"kind-levee-s2"},
	}, "kind-levee-s2")

	resp := p.handle(&hostRequest{Action: "collect", Params: map[string]any{
		"path": "--kubeconfig=/other",
	}})
	require.False(t, resp.OK, "a reference that parses as a target-redirecting flag must be refused")
}

// --- protocol loop ----------------------------------------------------------

func TestServeRoundTrip(t *testing.T) {
	fake := useRunner(t, &fakeRunner{stdout: `{"kind":"Deployment"}`})
	p := &plugin{}

	in := strings.Join([]string{
		`{"action":"meta"}`,
		`{"action":"init","config":{"allowed_contexts":["kind-levee-s2"]}}`,
		`{"action":"connect","params":{"host":"kind-levee-s2"}}`,
		`{"action":"exec","params":{"cmd":"kubectl get deployment web -o json"}}`,
		`{"action":"close"}`,
		`{"action":"meta"}`,
	}, "\n") + "\n"

	var out bytes.Buffer
	require.NoError(t, serve(strings.NewReader(in), &out, p))

	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	require.Len(t, lines, 5, "the loop must stop after close, not answer trailing input")
	require.True(t, fake.called(), "exec must have reached kubectl")
}

func (f *fakeRunner) called() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.args != nil
}
