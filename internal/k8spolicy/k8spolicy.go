// Package k8spolicy evaluates Kubernetes workload manifests against a set of
// high-risk policies, so a change that touches a cluster workload can be
// refused (or warned about) BEFORE it is applied.
//
// Why it exists (2026-11-11, owner directive "补云原生能力"): the cloud-native
// gap in LEVEE is not monitoring and not logging — it is that nothing governs
// the high-risk part of a workload change. This package is the policy half of
// that story; the execution half (changing a workload through the approval
// chain) is tracked separately in docs/design-k8s-change-governance.md (S2).
//
// Boundaries, deliberate:
//   - No Kubernetes dependency. Manifests are read as generic YAML maps, so
//     this package does not pin a client-go version and stays small.
//   - Only the fields this policy set cares about are interpreted; unknown
//     fields are ignored rather than guessed.
//   - A manifest that does not parse is an ERROR, not a pass. The caller fails
//     closed — the house rule (see internal/verify: a gate that cannot run
//     reports Passed=false rather than auto-passing).
//   - Findings are evidence, not verdicts: the caller decides whether a
//     critical finding blocks and whether warnings are only shown.
package k8spolicy

import (
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"
)

// Severity classifies a finding. Critical findings describe a manifest that
// grants more privilege than the workload needs; warnings describe a manifest
// that is merely unpinned or unbounded.
type Severity string

const (
	// SeverityCritical marks a finding that should block the change.
	SeverityCritical Severity = "critical"
	// SeverityWarning marks a finding worth surfacing but not blocking.
	SeverityWarning Severity = "warning"
)

// Finding is one policy violation found in one workload manifest.
//
// Path locates the offending field inside the manifest (e.g.
// "spec.template.spec.containers[0].securityContext.privileged") so the
// operator can jump straight to it; Message says what is wrong in words;
// Remediation says what to do instead. A refusal without a reason is a
// refusal the operator cannot act on, so all three are always populated.
type Finding struct {
	PolicyID    string   `json:"policy_id"`
	Severity    Severity `json:"severity"`
	Kind        string   `json:"kind"`
	Name        string   `json:"name"`
	Path        string   `json:"path"`
	Message     string   `json:"message"`
	Remediation string   `json:"remediation"`
}

// PolicyID values. They are stable identifiers: the audit trail and any
// suppression list refer to them, so renaming one is a contract change.
const (
	PolicyPrivileged          = "k8s/privileged"
	PolicyAllowPrivilegeEscal = "k8s/allow-privilege-escalation"
	PolicyHostPath            = "k8s/host-path"
	PolicyHostNamespace       = "k8s/host-namespace"
	PolicyRunAsRoot           = "k8s/run-as-root"
	PolicyAddCapabilities     = "k8s/add-capabilities"
	PolicyUnpinnedImage       = "k8s/unpinned-image"
	PolicyNoResourceLimits    = "k8s/no-resource-limits"
	PolicyNoNonRoot           = "k8s/no-non-root"
	PolicyAutomountSAToken    = "k8s/automount-service-account-token" // #nosec G101 -- policy ID constant that surfaces in audit/evidence output; the "token" here names the K8s feature, not a credential
	PolicyHostPort            = "k8s/host-port"

	// The second batch (2026-11-12) closes the gaps the first one left
	// between "the container asks for too much" and "the container is safe to
	// run": the node's /proc, the identity the pod inherits, credentials
	// written in clear text, and the operational properties that decide
	// whether a wedged workload is noticed at all.
	PolicyProcMount          = "k8s/proc-mount"
	PolicyDefaultSA          = "k8s/default-service-account"
	PolicySecretLiteralEnv   = "k8s/secret-literal-env"
	PolicyReadOnlyRootFS     = "k8s/read-only-root-filesystem"
	PolicyNoDropCapabilities = "k8s/no-drop-capabilities"
	PolicyNoLivenessProbe    = "k8s/no-liveness-probe"
	PolicyNoReadinessProbe   = "k8s/no-readiness-probe"
	PolicyInsecureRegistry   = "k8s/insecure-registry"
)

// workloadKinds are the kinds whose pod template this package evaluates.
// Everything else (Service, ConfigMap, Secret, Ingress, CRDs, ...) carries no
// pod spec and is skipped — a policy engine that invents verdicts for kinds
// it does not understand is worse than one that stays quiet.
var workloadKinds = map[string]bool{
	"Deployment":            true,
	"StatefulSet":           true,
	"DaemonSet":             true,
	"ReplicaSet":            true,
	"ReplicationController": true,
	"Job":                   true,
	"CronJob":               true,
	"Pod":                   true,
}

// Evaluate checks every YAML document in manifests and returns the findings.
//
// Each element of manifests is one manifest source; a source may hold several
// documents separated by "---". The error return is non-nil when a document
// could not be parsed — the caller must treat that as a refusal, not as
// "no findings".
func Evaluate(manifests []string) ([]Finding, error) {
	var findings []Finding
	for _, src := range manifests {
		for _, doc := range splitDocuments(src) {
			if strings.TrimSpace(doc) == "" {
				continue
			}
			var obj map[string]any
			if err := yaml.Unmarshal([]byte(doc), &obj); err != nil {
				return nil, fmt.Errorf("k8spolicy: parse manifest: %w", err)
			}
			if len(obj) == 0 {
				continue
			}
			kind, _ := obj["kind"].(string)
			if !workloadKinds[kind] {
				continue
			}
			name := digString(obj, "metadata", "name")
			pod, ok := podSpecOf(obj, kind)
			if !ok {
				continue
			}
			findings = append(findings, checkPodSpec(kind, name, pod)...)
		}
	}
	return findings, nil
}

// splitDocuments splits a YAML source on document separators.
func splitDocuments(src string) []string {
	lines := strings.Split(src, "\n")
	var docs []string
	var cur []string
	for _, ln := range lines {
		if strings.TrimRight(ln, " \t\r") == "---" {
			docs = append(docs, strings.Join(cur, "\n"))
			cur = nil
			continue
		}
		cur = append(cur, ln)
	}
	docs = append(docs, strings.Join(cur, "\n"))
	return docs
}

// podSpecOf returns the pod spec of a workload manifest. CronJob nests it
// under spec.jobTemplate.spec.template; the other kinds use spec.template,
// except Pod which uses spec directly.
func podSpecOf(obj map[string]any, kind string) (map[string]any, bool) {
	switch kind {
	case "CronJob":
		return digMap(obj, "spec", "jobTemplate", "spec", "template", "spec")
	case "Pod":
		return digMap(obj, "spec")
	default:
		return digMap(obj, "spec", "template", "spec")
	}
}

// checkPodSpec runs the policy set against one pod spec.
//
// The work is split into pod-level and per-container checks because a single
// function covering both exceeded the repository's cyclomatic-complexity
// floor (gocyclo 30): the split is not cosmetic, it separates "the pod
// grants the container too much" from "the container asks for too much",
// which are different reviews of the same manifest.
func checkPodSpec(kind, name string, pod map[string]any) []Finding {
	s := &findingSink{kind: kind, name: name}
	s.checkPodLevel(pod)
	s.checkContainers(pod)
	return s.out
}

// findingSink accumulates the findings of one workload. It exists so the
// per-check helpers can report without threading a closure through every
// signature.
type findingSink struct {
	kind string
	name string
	out  []Finding
}

// add records one finding against the workload this sink belongs to.
func (s *findingSink) add(id string, sev Severity, path, msg, fix string) {
	s.out = append(s.out, Finding{
		PolicyID:    id,
		Severity:    sev,
		Kind:        s.kind,
		Name:        s.name,
		Path:        path,
		Message:     msg,
		Remediation: fix,
	})
}

// checkPodLevel evaluates the settings that belong to the pod itself: what
// the pod shares with the node, what it mounts from the node, and which
// identity it runs under.
func (s *findingSink) checkPodLevel(pod map[string]any) {
	// Sharing a host namespace breaks the isolation the workload was
	// scheduled for.
	for _, ns := range []struct {
		field string
		label string
	}{
		{"hostNetwork", "the host network"},
		{"hostPID", "the host PID namespace"},
		{"hostIPC", "the host IPC namespace"},
	} {
		if b, ok := digBool(pod, ns.field); ok && b {
			s.add(PolicyHostNamespace, SeverityCritical,
				"spec.template.spec."+ns.field,
				"pod shares "+ns.label+" (host namespaces are shared with the node)",
				"remove "+ns.field+" or set it to false")
		}
	}

	// A mounted hostPath lets the workload write outside the container
	// filesystem.
	if vols, ok := digSlice(pod, "volumes"); ok {
		for i, v := range vols {
			vm, ok := v.(map[string]any)
			if !ok {
				continue
			}
			if _, has := digMap(vm, "hostPath"); has {
				s.add(PolicyHostPath, SeverityCritical,
					fmt.Sprintf("spec.template.spec.volumes[%d].hostPath", i),
					"volume mounts a hostPath (the workload can read/write the node filesystem)",
					"use emptyDir, a PVC, or a projected volume instead of hostPath")
			}
		}
	}

	// A service account token is mounted by default in many clusters; the
	// workload usually does not need it.
	if b, ok := digBool(pod, "automountServiceAccountToken"); ok && b {
		s.add(PolicyAutomountSAToken, SeverityWarning,
			"spec.template.spec.automountServiceAccountToken",
			"pod mounts the service account token (it grants the workload the API identity of that account)",
			"set automountServiceAccountToken: false unless the workload calls the Kubernetes API")
	}

	// A root user is rarely intended.
	if u, ok := digInt(pod, "securityContext", "runAsUser"); ok && u == 0 {
		s.add(PolicyRunAsRoot, SeverityCritical,
			"spec.template.spec.securityContext.runAsUser",
			"pod runs as UID 0 (root inside the container)",
			"set runAsNonRoot: true and a non-zero runAsUser")
	}

	// Unmasked /proc hands the workload the node's process information.
	if pm := digString(pod, "securityContext", "procMount"); pm != "" && strings.EqualFold(pm, "Unmasked") {
		s.add(PolicyProcMount, SeverityCritical,
			"spec.template.spec.securityContext.procMount",
			"pod masks /proc as Unmasked (it exposes the node's process information to the workload)",
			"remove procMount or set it to Default")
	}

	// A pod that names no service account runs as the namespace's default
	// one. That is only harmless when no token is mounted at all.
	_, named := dig(pod, "serviceAccountName")
	_, legacy := dig(pod, "serviceAccount")
	mount, hasMount := digBool(pod, "automountServiceAccountToken")
	if !named && !legacy && !(hasMount && !mount) {
		s.add(PolicyDefaultSA, SeverityWarning,
			"spec.template.spec.serviceAccountName",
			"pod runs as the namespace's default service account (the workload inherits an identity it did not ask for)",
			"declare a dedicated serviceAccountName, or set automountServiceAccountToken: false")
	}
}

// checkContainers evaluates every container of the pod spec (init containers
// first — they run with the same security context).
func (s *findingSink) checkContainers(pod map[string]any) {
	for i, c := range containersOf(pod) {
		cm, ok := c.(map[string]any)
		if !ok {
			continue
		}
		base := fmt.Sprintf("spec.template.spec.containers[%d]", i)
		s.checkContainerPrivilege(cm, base)
		s.checkContainerHygiene(pod, cm, base)
		s.checkContainerOps(cm, base)
		s.checkContainerSecrets(cm, base)
	}
}

// checkContainerOps evaluates the operational properties that decide whether a
// failing workload is noticed and contained: probes, a writable root, and the
// capability set the container keeps.
//
// They are warnings rather than critical findings on purpose. A workload
// without a readiness probe is a reliability gap, not an escalation of
// privilege; the gate reports it so the operator can decide.
func (s *findingSink) checkContainerOps(cm map[string]any, base string) {
	if _, ok := digMap(cm, "livenessProbe"); !ok {
		s.add(PolicyNoLivenessProbe, SeverityWarning,
			base+".livenessProbe",
			"container declares no livenessProbe (a wedged container keeps serving traffic)",
			"add a livenessProbe that fails when the workload is no longer serving")
	}
	if _, ok := digMap(cm, "readinessProbe"); !ok {
		s.add(PolicyNoReadinessProbe, SeverityWarning,
			base+".readinessProbe",
			"container declares no readinessProbe (traffic reaches it before it is ready)",
			"add a readinessProbe that fails until the workload is ready")
	}
	if ro, ok := digBool(cm, "securityContext", "readOnlyRootFilesystem"); !ok || !ro {
		s.add(PolicyReadOnlyRootFS, SeverityWarning,
			base+".securityContext.readOnlyRootFilesystem",
			"container root filesystem is writable (a compromise persists on the node)",
			"set readOnlyRootFilesystem: true and mount writable paths as emptyDir")
	}
	if !dropsAllCapabilities(cm) {
		s.add(PolicyNoDropCapabilities, SeverityWarning,
			base+".securityContext.capabilities.drop",
			"container does not drop all capabilities (the default capability set stays available to it)",
			"set securityContext.capabilities.drop: [\"ALL\"] and add back only what the workload needs")
	}
}

// checkContainerSecrets evaluates how the container receives its credentials
// and where its image comes from.
//
// A credential written as a literal env value is a critical finding: the value
// lives in the manifest, and the manifest lives in git.
func (s *findingSink) checkContainerSecrets(cm map[string]any, base string) {
	if envs, ok := digSlice(cm, "env"); ok {
		for i, e := range envs {
			em, ok := e.(map[string]any)
			if !ok {
				continue
			}
			name := digString(em, "name")
			if !looksLikeCredential(name) {
				continue
			}
			if _, has := dig(em, "value"); !has {
				continue
			}
			if _, fromSecret := dig(em, "valueFrom"); fromSecret {
				continue
			}
			s.add(PolicySecretLiteralEnv, SeverityCritical,
				fmt.Sprintf("%s.env[%d].value", base, i),
				"environment variable "+name+" is set as a literal value (the credential is stored in the manifest in clear text)",
				"reference a Secret with valueFrom.secretKeyRef and keep the value out of the manifest")
		}
	}
	if img := digString(cm, "image"); strings.HasPrefix(strings.ToLower(img), "http://") {
		s.add(PolicyInsecureRegistry, SeverityWarning,
			base+".image",
			"image is pulled over plaintext HTTP (the workload content is not verified in transit)",
			"pull from an https:// registry or a private registry that serves TLS")
	}
}

// dropsAllCapabilities reports whether the container drops ALL capabilities,
// the baseline the container runtime starts from.
func dropsAllCapabilities(cm map[string]any) bool {
	drops, ok := digSlice(cm, "securityContext", "capabilities", "drop")
	if !ok {
		return false
	}
	for _, d := range drops {
		if s, _ := d.(string); strings.EqualFold(s, "ALL") {
			return true
		}
	}
	return false
}

// credentialNameParts are the name fragments that mark an environment
// variable as carrying a credential. The check is by name on purpose: a
// literal value under a credential-looking name is the case worth refusing,
// and a literal LOG_LEVEL is not worth a finding.
var credentialNameParts = []string{ // #nosec G101 -- the entries are NAMES of environment variables, not credentials; this list is what tells the policy which names to treat as credential-bearing
	"PASSWORD", "PASSWD", "TOKEN", "SECRET", "CREDENTIAL",
	"APIKEY", "API_KEY", "PRIVATE_KEY", "ACCESS_KEY", "ACCESSKEY",
}

// looksLikeCredential reports whether an environment variable name reads as a
// credential.
func looksLikeCredential(name string) bool {
	upper := strings.ToUpper(name)
	for _, part := range credentialNameParts {
		if strings.Contains(upper, part) {
			return true
		}
	}
	return false
}

// checkContainerPrivilege evaluates how much of the node's authority one
// container asks for: privileged mode, privilege escalation, root and
// dangerous capabilities.
func (s *findingSink) checkContainerPrivilege(cm map[string]any, base string) {
	if b, ok := digBool(cm, "securityContext", "privileged"); ok && b {
		s.add(PolicyPrivileged, SeverityCritical,
			base+".securityContext.privileged",
			"container runs privileged (it gets the host's capabilities)",
			"drop privileged; grant only the specific capabilities it needs")
	}
	if b, ok := digBool(cm, "securityContext", "allowPrivilegeEscalation"); ok && b {
		s.add(PolicyAllowPrivilegeEscal, SeverityCritical,
			base+".securityContext.allowPrivilegeEscalation",
			"container allows privilege escalation",
			"set allowPrivilegeEscalation: false")
	}
	if u, ok := digInt(cm, "securityContext", "runAsUser"); ok && u == 0 {
		s.add(PolicyRunAsRoot, SeverityCritical,
			base+".securityContext.runAsUser",
			"container runs as UID 0 (root inside the container)",
			"set runAsNonRoot: true and a non-zero runAsUser")
	}
	caps, ok := digSlice(cm, "securityContext", "capabilities", "add")
	if !ok {
		return
	}
	for _, c := range caps {
		cs, _ := c.(string)
		switch strings.ToUpper(cs) {
		case "ALL", "SYS_ADMIN":
			s.add(PolicyAddCapabilities, SeverityCritical,
				base+".securityContext.capabilities.add",
				"container adds the "+cs+" capability",
				"drop "+cs+"; grant only the capabilities the workload needs")
		}
	}
}

// checkContainerHygiene evaluates the operational properties of one
// container: whether its content is pinned, whether it is bounded, and
// whether it is reachable in ways that bypass the Service.
func (s *findingSink) checkContainerHygiene(pod, cm map[string]any, base string) {
	// Image pinning: a moving tag means the workload content is not
	// reviewable, so "what was approved" cannot be reconstructed.
	if img := digString(cm, "image"); img != "" {
		tag := imageTag(img)
		if tag == "" || tag == "latest" {
			s.add(PolicyUnpinnedImage, SeverityWarning,
				base+".image",
				"image "+img+" is not pinned to an immutable version",
				"pin the image to a tag or a digest (registry/name@sha256:...)")
		}
	}

	// Resource limits: an unbounded container can starve its node.
	if _, ok := digMap(cm, "resources", "limits"); !ok {
		s.add(PolicyNoResourceLimits, SeverityWarning,
			base+".resources.limits",
			"container declares no resource limits (it can exhaust the node)",
			"set requests and limits for cpu and memory")
	}

	// Non-root: neither the pod nor the container says it must not be root.
	_, podNonRoot := digBool(pod, "securityContext", "runAsNonRoot")
	_, ctrNonRoot := digBool(cm, "securityContext", "runAsNonRoot")
	_, ctrUser := digMap(cm, "securityContext", "runAsUser")
	if !podNonRoot && !ctrNonRoot && !ctrUser {
		s.add(PolicyNoNonRoot, SeverityWarning,
			base+".securityContext.runAsNonRoot",
			"container does not declare runAsNonRoot (it may run as root)",
			"set runAsNonRoot: true at the pod or container level")
	}

	// hostPort binds a node port directly.
	ports, ok := digSlice(cm, "ports")
	if !ok {
		return
	}
	for pi, p := range ports {
		pm, ok := p.(map[string]any)
		if !ok {
			continue
		}
		// hostPort is a port number, not a mapping: presence is the
		// signal, so it is read as a value rather than as a map.
		if _, has := dig(pm, "hostPort"); has {
			s.add(PolicyHostPort, SeverityWarning,
				fmt.Sprintf("%s.ports[%d].hostPort", base, pi),
				"container publishes a hostPort (it binds the node directly and bypasses the Service)",
				"drop hostPort and reach the workload through its Service")
		}
	}
}

// containersOf returns the container list of a pod spec: initContainers first
// (they run with the same security context), then the regular containers.
func containersOf(pod map[string]any) []any {
	var out []any
	if inits, ok := digSlice(pod, "initContainers"); ok {
		out = append(out, inits...)
	}
	if ctrs, ok := digSlice(pod, "containers"); ok {
		out = append(out, ctrs...)
	}
	return out
}

// imageTag returns the tag part of an image reference, or "" when the
// reference carries no tag (implicitly :latest) or is a digest.
func imageTag(image string) string {
	if strings.Contains(image, "@") {
		return "sha256"
	}
	slash := strings.LastIndex(image, "/")
	colon := strings.LastIndex(image, ":")
	if colon <= slash {
		return ""
	}
	return image[colon+1:]
}

// --- generic map navigation helpers (yaml.v3 gives map[string]any) ---

func dig(m map[string]any, path ...string) (any, bool) {
	cur := any(m)
	for _, p := range path {
		cm, ok := cur.(map[string]any)
		if !ok {
			return nil, false
		}
		cur, ok = cm[p]
		if !ok {
			return nil, false
		}
	}
	return cur, true
}

func digMap(m map[string]any, path ...string) (map[string]any, bool) {
	v, ok := dig(m, path...)
	if !ok {
		return nil, false
	}
	out, ok := v.(map[string]any)
	return out, ok
}

func digSlice(m map[string]any, path ...string) ([]any, bool) {
	v, ok := dig(m, path...)
	if !ok {
		return nil, false
	}
	out, ok := v.([]any)
	return out, ok
}

func digString(m map[string]any, path ...string) string {
	v, ok := dig(m, path...)
	if !ok {
		return ""
	}
	s, _ := v.(string)
	return s
}

func digBool(m map[string]any, path ...string) (bool, bool) {
	v, ok := dig(m, path...)
	if !ok {
		return false, false
	}
	b, ok := v.(bool)
	return b, ok
}

func digInt(m map[string]any, path ...string) (int, bool) {
	v, ok := dig(m, path...)
	if !ok {
		return 0, false
	}
	switch n := v.(type) {
	case int:
		return n, true
	case int64:
		return int(n), true
	case float64:
		return int(n), true
	default:
		return 0, false
	}
}
