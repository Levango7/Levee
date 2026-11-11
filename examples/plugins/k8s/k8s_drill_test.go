package main

// k8s_drill_test.go runs the plugin's channel contract against a REAL
// cluster. It is the kind-based half of D-4 S2's acceptance
// (docs/design-k8s-change-governance.md): snapshot capture → workload change
// → restore by snapshot, through the plugin's own exec/upload path and a real
// kubectl.
//
// Gated on LEVEE_K8S_DRILL_CONTEXT (the kube context to drill against) — the
// same opt-in pattern as the PostgreSQL drill (LEVEE_PG_TEST_DSN): a cluster
// is an operator-provided fixture, not something CI owns. Without the
// variable the drill skips, and the contract tests in main_test.go carry the
// CI-side proof with a fake kubectl.

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestK8sChannelDrill(t *testing.T) {
	ctxName := strings.TrimSpace(os.Getenv("LEVEE_K8S_DRILL_CONTEXT"))
	if ctxName == "" {
		t.Skip("LEVEE_K8S_DRILL_CONTEXT not set; skipping the real-cluster k8s channel drill")
	}
	kubeconfig := strings.TrimSpace(os.Getenv("LEVEE_K8S_DRILL_KUBECONFIG"))
	kubectlBin := strings.TrimSpace(os.Getenv("LEVEE_K8S_DRILL_KUBECTL"))

	name := "levee-drill-" + drillSuffix()

	// Harness-side fixture: a one-replica deployment to drill against. It
	// lives in the context's default namespace — the plugin pins namespace
	// to the kubeconfig context and refuses caller-supplied -n, so the
	// fixture must live where the context resolves.
	applyHarness(t, kubeconfig, kubectlBin, ctxName, deploymentManifest(name))
	t.Cleanup(func() { deleteHarness(t, kubeconfig, kubectlBin, ctxName, name) })

	p := &plugin{}
	resp := p.handle(&hostRequest{Action: "init", Config: map[string]any{
		"allowed_contexts": []any{ctxName},
		"kubeconfig":       kubeconfig,
		"kubectl_path":     kubectlBin,
	}})
	require.True(t, resp.OK, "init refused: %s", resp.Error)
	resp = p.handle(&hostRequest{Action: "connect", Params: map[string]any{"host": ctxName}})
	require.True(t, resp.OK, "connect refused: %s", resp.Error)

	// --- scenario: snapshot capture -------------------------------------
	// The host's snapshotter asks for "base64 <resource>"; the plugin must
	// answer with the resource's state, server bookkeeping stripped.
	resp = p.handle(&hostRequest{Action: "exec", Params: map[string]any{
		"cmd": "base64 'deployment/" + name + "'",
	}})
	require.True(t, resp.OK, "capture refused: %s", resp.Error)
	snapshot, err := base64.StdEncoding.DecodeString(strings.TrimSpace(resp.Exec.Stdout))
	require.NoError(t, err, "the host decodes capture stdout as base64")
	original := replicasOf(t, snapshot)
	require.Equal(t, int32(1), original, "the fixture starts at one replica")

	// --- scenario: the governed change happens --------------------------
	resp = p.handle(&hostRequest{Action: "exec", Params: map[string]any{
		"cmd": "kubectl scale deployment " + name + " --replicas=3",
	}})
	require.True(t, resp.OK, "scale refused: %s", resp.Error)
	require.Equal(t, 0, resp.Exec.ExitCode, "scale failed: %s", resp.Exec.Stderr)
	require.Eventually(t, func() bool {
		return replicasOf(t, harnessJSON(t, kubeconfig, kubectlBin, ctxName, name)) == 3
	}, 30*time.Second, 500*time.Millisecond, "the cluster must reflect the change")

	// --- scenario: rollback restores the SNAPSHOT ------------------------
	resp = p.handle(&hostRequest{Action: "upload", Params: map[string]any{
		"path": "deployment/" + name,
		"data": base64.StdEncoding.EncodeToString(snapshot),
	}})
	require.True(t, resp.OK, "restore refused: %s", resp.Error)
	require.Eventually(t, func() bool {
		return replicasOf(t, harnessJSON(t, kubeconfig, kubectlBin, ctxName, name)) == original
	}, 30*time.Second, 500*time.Millisecond,
		"rollback must put the workload back to the captured state (replicas=%d)", original)

	assert.NotContains(t, string(snapshot), "resourceVersion",
		"the captured snapshot must not carry the server's bookkeeping")
}

// --- harness (direct kubectl, NOT through the plugin) -----------------------

func harnessKubectl(t *testing.T, kubeconfig, kubectlBin, ctxName string, args ...string) (string, string) {
	t.Helper()
	full := make([]string, 0, len(args)+4)
	if kubeconfig != "" {
		full = append(full, "--kubeconfig", kubeconfig)
	}
	if ctxName != "" {
		full = append(full, "--context", ctxName)
	}
	full = append(full, args...)
	bin := kubectlBin
	if bin == "" {
		bin = "kubectl"
	}
	cmd := exec.Command(bin, full...)
	var out, errOut strings.Builder
	cmd.Stdout = &out
	cmd.Stderr = &errOut
	err := cmd.Run()
	require.NoError(t, err, "harness kubectl %v failed: %s", args, errOut.String())
	return out.String(), errOut.String()
}

func applyHarness(t *testing.T, kubeconfig, kubectlBin, ctxName, manifest string) {
	t.Helper()
	full := make([]string, 0, 8)
	if kubeconfig != "" {
		full = append(full, "--kubeconfig", kubeconfig)
	}
	if ctxName != "" {
		full = append(full, "--context", ctxName)
	}
	full = append(full, "apply", "-f", "-")
	bin := kubectlBin
	if bin == "" {
		bin = "kubectl"
	}
	cmd := exec.Command(bin, full...)
	cmd.Stdin = strings.NewReader(manifest)
	var out, errOut strings.Builder
	cmd.Stdout = &out
	cmd.Stderr = &errOut
	err := cmd.Run()
	require.NoError(t, err, "harness kubectl apply failed: %s", errOut.String())
}

func deleteHarness(t *testing.T, kubeconfig, kubectlBin, ctxName, name string) {
	t.Helper()
	full := make([]string, 0, 8)
	if kubeconfig != "" {
		full = append(full, "--kubeconfig", kubeconfig)
	}
	if ctxName != "" {
		full = append(full, "--context", ctxName)
	}
	full = append(full, "delete", "deployment", name, "--ignore-not-found", "--wait=false")
	bin := kubectlBin
	if bin == "" {
		bin = "kubectl"
	}
	_ = exec.Command(bin, full...).Run()
}

func harnessJSON(t *testing.T, kubeconfig, kubectlBin, ctxName, name string) []byte {
	t.Helper()
	out, _ := harnessKubectl(t, kubeconfig, kubectlBin, ctxName, "get", "deployment/"+name, "-o", "json")
	return []byte(out)
}

// replicasOf reads spec.replicas out of a Deployment manifest.
func replicasOf(t *testing.T, manifest []byte) int32 {
	t.Helper()
	var obj struct {
		Spec struct {
			Replicas int32 `json:"replicas"`
		} `json:"spec"`
	}
	require.NoError(t, json.Unmarshal(manifest, &obj))
	return obj.Spec.Replicas
}

// deploymentManifest is a minimal one-replica Deployment. The image never
// needs to pull: the drill asserts on the OBJECT's state, not on pods.
func deploymentManifest(name string) string {
	return fmt.Sprintf(`{
	  "apiVersion": "apps/v1",
	  "kind": "Deployment",
	  "metadata": {"name": %s, "labels": {"app": %s, "levee-drill": "true"}},
	  "spec": {
	    "replicas": 1,
	    "selector": {"matchLabels": {"app": %s}},
	    "template": {
	      "metadata": {"labels": {"app": %s}},
	      "spec": {"containers": [{"name": "app", "image": "registry.k8s.io/pause:3.10"}]}
	    }
	  }
	}`, strconv.Quote(name), strconv.Quote(name), strconv.Quote(name), strconv.Quote(name))
}

// drillSuffix makes the fixture name unique so concurrent drills (or a
// leftover from a crashed one) cannot collide.
func drillSuffix() string {
	return fmt.Sprintf("%d", time.Now().UnixNano()%1_000_000_0)
}
