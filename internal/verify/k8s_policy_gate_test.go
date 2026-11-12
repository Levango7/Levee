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
	// The privileged finding is looked up by ID rather than by position: the
	// policy set reports pod-level findings before container-level ones, and
	// the position of a finding is not a contract.
	var privileged *k8spolicy.Finding
	for i := range findings {
		if findings[i].PolicyID == k8spolicy.PolicyPrivileged {
			privileged = &findings[i]
			break
		}
	}
	require.NotNil(t, privileged, "the privileged finding must be reported, got %v", findings)
	assert.Equal(t, k8spolicy.SeverityCritical, privileged.Severity)
	assert.Contains(t, privileged.Path, "containers[0]")
}

func TestK8sPolicyGatePassesWithWarningsAndShowsThem(t *testing.T) {
	// The workload is hardened in every respect except the image tag, so the
	// gate sees exactly one warning — the point of this test is that a
	// warning is reported without blocking, not how many warnings a bare
	// manifest produces.
	g := NewK8sPolicyGate("k8s-policy", PhasePreApply, []string{`
apiVersion: apps/v1
kind: Deployment
metadata:
  name: web
spec:
  template:
    spec:
      serviceAccountName: web-sa
      containers:
      - name: web
        image: web:latest
        resources:
          limits:
            cpu: "1"
        livenessProbe:
          httpGet:
            path: /healthz
            port: 8080
        readinessProbe:
          httpGet:
            path: /ready
            port: 8080
        securityContext:
          runAsNonRoot: true
          readOnlyRootFilesystem: true
          capabilities:
            drop: ["ALL"]
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
