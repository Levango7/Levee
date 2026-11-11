package verify

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nexus/levee/internal/k8spolicy"
)

func TestK8sPolicyGateBlocksOnCriticalFinding(t *testing.T) {
	g := NewK8sPolicyGate("k8s-policy", PhasePreApply, []string{`
apiVersion: apps/v1
kind: Deployment
metadata:
  name: web
spec:
  template:
    spec:
      containers:
      - name: web
        image: web:1
        securityContext:
          privileged: true
`})

	res, err := g.Check(context.Background(), GateInput{})
	require.NoError(t, err)
	assert.False(t, res.Passed, "a critical policy violation must block")
	assert.Contains(t, res.Message, "k8s/privileged", "the refusal must name the policy")
	assert.Contains(t, res.Message, "containers[0]", "the refusal must point at the field")
	assert.Equal(t, 1, res.Details["critical"])
	findings, ok := res.Details["findings"].([]k8spolicy.Finding)
	require.True(t, ok)
	require.NotEmpty(t, findings)
	assert.Equal(t, k8spolicy.PolicyPrivileged, findings[0].PolicyID)
}

func TestK8sPolicyGatePassesWithWarningsAndShowsThem(t *testing.T) {
	g := NewK8sPolicyGate("k8s-policy", PhasePreApply, []string{`
apiVersion: apps/v1
kind: Deployment
metadata:
  name: web
spec:
  template:
    spec:
      containers:
      - name: web
        image: web:latest
        resources:
          limits:
            cpu: "1"
        securityContext:
          runAsNonRoot: true
`})

	res, err := g.Check(context.Background(), GateInput{})
	require.NoError(t, err)
	assert.True(t, res.Passed, "a warning must not block")
	assert.Contains(t, res.Message, "warning", "the pass message must still show what it saw")
	assert.Equal(t, 0, res.Details["critical"])
	assert.Equal(t, 1, res.Details["warnings"])
}

func TestK8sPolicyGateFailsClosedOnUnparsableManifest(t *testing.T) {
	g := NewK8sPolicyGate("k8s-policy", PhasePreApply, []string{"kind: Deployment\n  broken: [\n"})

	res, err := g.Check(context.Background(), GateInput{})
	require.NoError(t, err)
	assert.False(t, res.Passed, "a manifest that cannot be read must not pass")
	assert.Equal(t, "manifest_unparsable", res.Details["reason"])
}

func TestK8sPolicyGateDefaultsToPreApply(t *testing.T) {
	g := NewK8sPolicyGate("k8s-policy", "", nil)
	assert.Equal(t, PhasePreApply, g.Phase())
	assert.Equal(t, "k8s-policy", g.Name())
}
