package k8spolicy

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// deployment wraps a pod spec into a Deployment manifest.
func deployment(podSpec string) string {
	return "apiVersion: apps/v1\nkind: Deployment\nmetadata:\n  name: web\nspec:\n  template:\n    spec:\n" + podSpec
}

func TestEvaluateNoFindingsOnSafeWorkload(t *testing.T) {
	manifest := deployment(`      serviceAccountName: web-sa
      containers:
      - name: web
        image: registry.example.com/web:1.4.2
        resources:
          limits:
            cpu: "1"
            memory: 512Mi
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
          allowPrivilegeEscalation: false
          readOnlyRootFilesystem: true
          capabilities:
            drop: ["ALL"]
`)
	findings, err := Evaluate([]string{manifest})
	require.NoError(t, err)
	assert.Empty(t, findings, "a hardened workload must not produce findings")
}

func TestEvaluatePolicyMatrix(t *testing.T) {
	cases := []struct {
		policy   string
		severity Severity
		manifest string
		pathHas  string
	}{
		{
			policy:   PolicyPrivileged,
			severity: SeverityCritical,
			manifest: deployment("      containers:\n      - name: web\n        image: web:1\n        securityContext:\n          privileged: true\n"),
			pathHas:  "securityContext.privileged",
		},
		{
			policy:   PolicyAllowPrivilegeEscal,
			severity: SeverityCritical,
			manifest: deployment("      containers:\n      - name: web\n        image: web:1\n        securityContext:\n          allowPrivilegeEscalation: true\n"),
			pathHas:  "allowPrivilegeEscalation",
		},
		{
			policy:   PolicyHostPath,
			severity: SeverityCritical,
			manifest: deployment("      volumes:\n      - name: host\n        hostPath:\n          path: /\n      containers:\n      - name: web\n        image: web:1\n"),
			pathHas:  "hostPath",
		},
		{
			policy:   PolicyHostNamespace,
			severity: SeverityCritical,
			manifest: deployment("      hostNetwork: true\n      containers:\n      - name: web\n        image: web:1\n"),
			pathHas:  "hostNetwork",
		},
		{
			policy:   PolicyRunAsRoot,
			severity: SeverityCritical,
			manifest: deployment("      securityContext:\n        runAsUser: 0\n      containers:\n      - name: web\n        image: web:1\n"),
			pathHas:  "runAsUser",
		},
		{
			policy:   PolicyAddCapabilities,
			severity: SeverityCritical,
			manifest: deployment("      containers:\n      - name: web\n        image: web:1\n        securityContext:\n          capabilities:\n            add: [\"ALL\"]\n"),
			pathHas:  "capabilities.add",
		},
		{
			policy:   PolicyUnpinnedImage,
			severity: SeverityWarning,
			manifest: deployment("      containers:\n      - name: web\n        image: web:latest\n        resources:\n          limits:\n            cpu: \"1\"\n"),
			pathHas:  ".image",
		},
		{
			policy:   PolicyNoResourceLimits,
			severity: SeverityWarning,
			manifest: deployment("      containers:\n      - name: web\n        image: web:1.2.3\n        securityContext:\n          runAsNonRoot: true\n"),
			pathHas:  "resources.limits",
		},
		{
			policy:   PolicyNoNonRoot,
			severity: SeverityWarning,
			manifest: deployment("      containers:\n      - name: web\n        image: web:1.2.3\n        resources:\n          limits:\n            cpu: \"1\"\n"),
			pathHas:  "runAsNonRoot",
		},
		{
			policy:   PolicyAutomountSAToken,
			severity: SeverityWarning,
			manifest: deployment("      automountServiceAccountToken: true\n      containers:\n      - name: web\n        image: web:1.2.3\n        resources:\n          limits:\n            cpu: \"1\"\n        securityContext:\n          runAsNonRoot: true\n"),
			pathHas:  "automountServiceAccountToken",
		},
		{
			policy:   PolicyHostPort,
			severity: SeverityWarning,
			manifest: deployment("      containers:\n      - name: web\n        image: web:1.2.3\n        resources:\n          limits:\n            cpu: \"1\"\n        securityContext:\n          runAsNonRoot: true\n        ports:\n        - containerPort: 80\n          hostPort: 80\n"),
			pathHas:  "hostPort",
		},
		{
			policy:   PolicyProcMount,
			severity: SeverityCritical,
			manifest: deployment("      securityContext:\n        procMount: Unmasked\n      containers:\n      - name: web\n        image: web:1.2.3\n"),
			pathHas:  "procMount",
		},
		{
			policy:   PolicyDefaultSA,
			severity: SeverityWarning,
			manifest: deployment("      containers:\n      - name: web\n        image: web:1.2.3\n"),
			pathHas:  "serviceAccountName",
		},
		{
			policy:   PolicySecretLiteralEnv,
			severity: SeverityCritical,
			manifest: deployment("      containers:\n      - name: web\n        image: web:1.2.3\n        env:\n        - name: DB_PASSWORD\n          value: hunter2\n"),
			pathHas:  "env[0].value",
		},
		{
			policy:   PolicyReadOnlyRootFS,
			severity: SeverityWarning,
			manifest: deployment("      containers:\n      - name: web\n        image: web:1.2.3\n        securityContext:\n          runAsNonRoot: true\n"),
			pathHas:  "readOnlyRootFilesystem",
		},
		{
			policy:   PolicyNoDropCapabilities,
			severity: SeverityWarning,
			manifest: deployment("      containers:\n      - name: web\n        image: web:1.2.3\n        securityContext:\n          runAsNonRoot: true\n"),
			pathHas:  "capabilities.drop",
		},
		{
			policy:   PolicyNoLivenessProbe,
			severity: SeverityWarning,
			manifest: deployment("      containers:\n      - name: web\n        image: web:1.2.3\n"),
			pathHas:  "livenessProbe",
		},
		{
			policy:   PolicyNoReadinessProbe,
			severity: SeverityWarning,
			manifest: deployment("      containers:\n      - name: web\n        image: web:1.2.3\n"),
			pathHas:  "readinessProbe",
		},
		{
			policy:   PolicyInsecureRegistry,
			severity: SeverityWarning,
			manifest: deployment("      containers:\n      - name: web\n        image: http://registry.internal/app:1.2.3\n"),
			pathHas:  ".image",
		},
	}

	for _, tc := range cases {
		t.Run(tc.policy, func(t *testing.T) {
			findings, err := Evaluate([]string{tc.manifest})
			require.NoError(t, err)
			require.NotEmpty(t, findings, "policy %s must fire", tc.policy)

			var hit *Finding
			for i := range findings {
				if findings[i].PolicyID == tc.policy {
					hit = &findings[i]
					break
				}
			}
			require.NotNil(t, hit, "policy %s must be reported, got %v", tc.policy, findings)
			assert.Equal(t, tc.severity, hit.Severity)
			assert.Contains(t, hit.Path, tc.pathHas)
			assert.Equal(t, "Deployment", hit.Kind)
			assert.Equal(t, "web", hit.Name)
			assert.NotEmpty(t, hit.Message, "a finding must carry a reason")
			assert.NotEmpty(t, hit.Remediation, "a finding must carry a remedy")
		})
	}
}

func TestEvaluateOnlyFlagsCredentialLookingEnvNames(t *testing.T) {
	manifest := deployment("      containers:\n      - name: web\n        image: web:1.2.3\n        env:\n        - name: LOG_LEVEL\n          value: info\n        - name: DB_PASSWORD\n          valueFrom:\n            secretKeyRef:\n              name: db\n              key: password\n")
	findings, err := Evaluate([]string{manifest})
	require.NoError(t, err)
	for _, f := range findings {
		assert.NotEqual(t, PolicySecretLiteralEnv, f.PolicyID,
			"a plain value under a non-credential name, or a value taken from a Secret, is not a credential leak")
	}
}

func TestEvaluateSkipsNonWorkloadKinds(t *testing.T) {
	findings, err := Evaluate([]string{
		"apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: cfg\ndata:\n  a: b\n",
		"apiVersion: v1\nkind: Service\nmetadata:\n  name: web\nspec:\n  ports:\n  - port: 80\n",
	})
	require.NoError(t, err)
	assert.Empty(t, findings, "non-workload kinds carry no pod spec and must be skipped")
}

func TestEvaluateHandlesMultipleDocuments(t *testing.T) {
	manifests := []string{`
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
---
apiVersion: v1
kind: Pod
metadata:
  name: side
spec:
  hostNetwork: true
  containers:
  - name: side
    image: side:1
`}
	findings, err := Evaluate(manifests)
	require.NoError(t, err)
	ids := map[string]bool{}
	for _, f := range findings {
		ids[f.PolicyID] = true
	}
	assert.True(t, ids[PolicyPrivileged], "the Deployment's violation must be found")
	assert.True(t, ids[PolicyHostNamespace], "the Pod's violation must be found")
}

func TestEvaluateCronJobTemplate(t *testing.T) {
	manifest := "apiVersion: batch/v1\nkind: CronJob\nmetadata:\n  name: nightly\nspec:\n  jobTemplate:\n    spec:\n      template:\n        spec:\n          containers:\n          - name: job\n            image: job:latest\n"
	findings, err := Evaluate([]string{manifest})
	require.NoError(t, err)
	require.NotEmpty(t, findings)
	assert.Equal(t, "CronJob", findings[0].Kind)
}

func TestEvaluateParseErrorIsAnError(t *testing.T) {
	findings, err := Evaluate([]string{"kind: Deployment\n  bad indentation: [\n"})
	require.Error(t, err, "an unparsable manifest must be an error, not a pass")
	assert.Nil(t, findings)
	assert.True(t, strings.Contains(err.Error(), "k8spolicy"), "the error must name the package")
}

func TestImageTag(t *testing.T) {
	assert.Equal(t, "1.4.2", imageTag("registry.example.com/web:1.4.2"))
	assert.Equal(t, "latest", imageTag("registry.example.com/web:latest"))
	assert.Equal(t, "", imageTag("registry.example.com/web"))
	assert.Equal(t, "sha256", imageTag("registry.example.com/web@sha256:abc"))
}
