package dsl

// k8spolicy_gate_test.go — the DSL wiring for the S1 k8s workload policy gate.
// The engine materialises the gate from params.manifests (list of manifest
// documents) or params.manifest (a single document) and fails closed on
// anything else (internal/engine). The parser's job is to make both shapes
// writable in YAML and to keep the documents verbatim.

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const k8sPolicyDemoManifest = `apiVersion: apps/v1
kind: Deployment
metadata:
  name: demo
spec:
  template:
    spec:
      containers:
        - name: app
          image: app:1
          securityContext:
            privileged: true
`

func TestParseK8sPolicyGate(t *testing.T) {
	src := `
name: k8spolicy-gate
target:
  hosts: [10.0.0.1]
steps:
  - name: rollout
    action: shell.run
    verify:
      position: post_apply
      k8spolicy:
        manifests:
          - |
` + indentBlock(k8sPolicyDemoManifest, 12) + `
      params:
        note: "blocking set"
gates:
  - position: pre_apply
    k8spolicy:
      manifest: |
` + indentBlock(k8sPolicyDemoManifest, 8) + `
    params:
      note: "pre-flight"
`
	wf, err := NewParser().ParseBytes([]byte(src))
	require.NoError(t, err)

	// List form: documents survive verbatim under params.manifests, with
	// user params riding alongside.
	g := wf.Steps[0].Gate
	require.NotNil(t, g)
	require.Len(t, g.Post, 1)
	assert.Equal(t, "k8spolicy", g.Post[0].Type)
	assert.Equal(t, []string{k8sPolicyDemoManifest}, g.Post[0].Params["manifests"])
	assert.Equal(t, "blocking set", g.Post[0].Params["note"])

	// Single-document form lands under params.manifest.
	require.NotNil(t, wf.Gate)
	require.Len(t, wf.Gate.Pre, 1)
	assert.Equal(t, "k8spolicy", wf.Gate.Pre[0].Type)
	assert.Equal(t, k8sPolicyDemoManifest, wf.Gate.Pre[0].Params["manifest"])
	assert.Equal(t, "pre-flight", wf.Gate.Pre[0].Params["note"])
}

// indentBlock re-indents a block scalar's content to the given column so the
// fixture YAML stays well-formed without hand-counted spaces.
func indentBlock(s string, col int) string {
	pad := strings.Repeat(" ", col)
	return pad + strings.ReplaceAll(strings.TrimSuffix(s, "\n"), "\n", "\n"+pad)
}

// TestParseK8sPolicyGate_DeclaredFieldsWin pins the collision rule: a
// manifests/manifest key in params must not shadow the typed field, or the
// gate would evaluate a document the k8spolicy block never declared.
func TestParseK8sPolicyGate_DeclaredFieldsWin(t *testing.T) {
	src := `
name: k8spolicy-collision
target:
  hosts: [10.0.0.1]
steps:
  - name: rollout
    action: shell.run
    verify:
      k8spolicy:
        manifest: "apiVersion: v1\nkind: Pod\n"
      params:
        manifest: "decoy"
`
	wf, err := NewParser().ParseBytes([]byte(src))
	require.NoError(t, err)
	g := wf.Steps[0].Gate
	require.NotNil(t, g)
	require.Len(t, g.Post, 1)
	assert.Equal(t, "apiVersion: v1\nkind: Pod\n", g.Post[0].Params["manifest"])
}

// TestParseK8sPolicyGate_EmptyBlockNamesTheType keeps an empty k8spolicy
// block honest about what it is: the type is declared, no manifest is, and
// the engine refuses it fail-closed at materialisation.
func TestParseK8sPolicyGate_EmptyBlockNamesTheType(t *testing.T) {
	src := `
name: k8spolicy-empty
target:
  hosts: [10.0.0.1]
steps:
  - name: rollout
    action: shell.run
    verify:
      k8spolicy: {}
`
	wf, err := NewParser().ParseBytes([]byte(src))
	require.NoError(t, err)
	g := wf.Steps[0].Gate
	require.NotNil(t, g)
	require.Len(t, g.Post, 1)
	assert.Equal(t, "k8spolicy", g.Post[0].Type)
	assert.Empty(t, g.Post[0].Params)
}
