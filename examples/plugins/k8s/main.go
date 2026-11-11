// Command k8s is a CHANNEL plugin for LEVEE that drives a Kubernetes
// cluster through kubectl.
//
// Why it exists (2026-11-11, D-4 in docs/design-k8s-change-governance.md):
// the gap in LEVEE's cloud-native story is not monitoring and not logging —
// it is that nothing governs a HIGH-RISK ACTION on a workload. Those three
// things (approval, rollback, evidence) already exist in LEVEE's core, and
// they already work over any transport; until now the transports were hosts
// (ssh / winrm / local). A Kubernetes cluster is a perfectly good target for
// the same machinery, so this plugin provides one.
//
// It is deliberately a TRANSPORT, not a platform. It does not sync
// manifests, does not reconcile desired state, does not install operators and
// does not manage GitOps. It makes ONE workload change travel the same
// governed path a shell command on an SSH host already travels.
//
// Verbs:
//
//	meta       — identity (type: channel)
//	init       — the config.yaml captured at install time
//	connect    — params: host / port / type / credential_ref. The host is the
//	             KUBE CONTEXT to use and must be listed in the plugin's
//	             allowed_contexts. No plaintext credentials cross the pipe:
//	             the kubeconfig lives in the plugin's own config.yaml and the
//	             host only passes a credential_ref NAME.
//	exec       — params: cmd. Two command forms are understood (see
//	             parseCommand):
//	               kubectl <verb> [args...]  — a workload change
//	               base64 <resource>        — the host's snapshot-capture
//	                                            idiom (see capture)
//	collect    — params: path (a resource reference); returns the resource
//	             JSON. This is the host's Download over a k8s namespace.
//	upload     — params: path (a resource reference) + data (base64
//	             manifest). This is the host's Upload: the content is applied
//	             with "kubectl apply -f -". It is also the RESTORE half of the
//	             snapshot pair, which is what makes "roll back by restoring
//	             the snapshot" possible without any core change.
//	disconnect — no session state to tear down (kubectl is one-shot per call)
//	close      — exit the process
//
// Install and try it:
//
//	go build -o examples/plugins/k8s/k8s ./examples/plugins/k8s
//	levee plugin install ./examples/plugins/k8s
//	levee plugin enable k8s
//	levee target add kind-levee --channel-type k8s --address kind-levee-s2
//	# then, in a workflow step:
//	#   module: shell, action: exec, args: {cmd: "kubectl scale deployment web --replicas=3"}
//
// Why the command surface is this small: a channel that accepted arbitrary
// kubectl would be an arbitrary-command transport with a cluster attached,
// and the point of the product is that a high-risk action is governed. The
// verb allowlist below is the transport's own floor; the approval chain, the
// policy gate and the snapshot rollback sit ABOVE it and can only narrow
// further, never widen.
package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"
)

// --- wire types (the host's protocol, mirrored) -----------------------------

type hostRequest struct {
	Action string         `json:"action"`
	Config map[string]any `json:"config,omitempty"`
	Params map[string]any `json:"params,omitempty"`
}

type channelExecResult struct {
	ExitCode int    `json:"exit_code"`
	Stdout   string `json:"stdout"`
	Stderr   string `json:"stderr"`
	Duration int64  `json:"duration_ms"`
}

type collectPayload struct {
	Data string `json:"data"`
}

type probeResult struct {
	Passed  bool           `json:"passed"`
	Message string         `json:"message"`
	Details map[string]any `json:"details,omitempty"`
}

type pluginResponse struct {
	OK      bool               `json:"ok"`
	Error   string             `json:"error,omitempty"`
	Result  *probeResult       `json:"result,omitempty"`
	Exec    *channelExecResult `json:"exec,omitempty"`
	Collect *collectPayload    `json:"collect,omitempty"`
}

// --- configuration ----------------------------------------------------------

// config is the plugin's install-time configuration (config.yaml). It holds
// the operator's choices: where the kubeconfig is, which contexts this
// plugin may address, and the floor on what may be done to a cluster.
//
// The kubeconfig path lives HERE and not in a target row on purpose: it is
// the credential carrier (the same reason the ssh channel keeps its keys in
// the operator's environment rather than in the inventory), and the host
// never passes plaintext credentials to a channel plugin — only a
// credential_ref NAME.
//
// The NAMESPACE is not configured here: it comes from the kubeconfig's
// context, which is where an operator already keeps it. One context, one
// namespace, one target — the same "a target IS a place" model the other
// channels have.
type config struct {
	// kubeconfig is the path to the kubeconfig file. Empty means "the
	// operator's default", which kubectl resolves from KUBECONFIG or
	// ~/.kube/config.
	kubeconfig string
	// allowedContexts lists the kube contexts this plugin may address. A
	// target's address must be one of them. An empty list means NO context
	// is allowed: fail-closed, not fail-open.
	allowedContexts []string
	// allowedVerbs is the floor on what may be done. Empty means the
	// built-in default set.
	allowedVerbs []string
	// kubectlPath overrides the kubectl binary location. Empty means
	// "look it up on PATH".
	kubectlPath string
	// timeoutMs bounds one kubectl invocation.
	timeoutMs int
}

// applyConfig folds the host's init payload into the configuration.
func (c *config) applyConfig(raw map[string]any) {
	if v, ok := raw["kubeconfig"].(string); ok {
		c.kubeconfig = strings.TrimSpace(v)
	}
	if v, ok := raw["kubectl_path"].(string); ok {
		c.kubectlPath = strings.TrimSpace(v)
	}
	if v, ok := raw["timeout_ms"].(int); ok && v > 0 {
		c.timeoutMs = v
	}
	c.allowedContexts = stringList(raw["allowed_contexts"])
	c.allowedVerbs = stringList(raw["allowed_verbs"])
}

// stringList reads a YAML/JSON list of strings. Anything else yields an
// empty list — which every caller treats as "allow nothing".
func stringList(raw any) []string {
	items, ok := raw.([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(items))
	for _, it := range items {
		if s, ok := it.(string); ok && strings.TrimSpace(s) != "" {
			out = append(out, strings.TrimSpace(s))
		}
	}
	return out
}

// allowedContext reports whether ctx may be addressed.
func (c config) allowedContext(ctxName string) bool {
	for _, a := range c.allowedContexts {
		if a == ctxName {
			return true
		}
	}
	return false
}

// allowedVerb reports whether verb may be run.
func (c config) allowedVerb(verb string) bool {
	for _, a := range c.allowedVerbs {
		if a == verb {
			return true
		}
	}
	return false
}

// timeout is the per-invocation budget.
func (c config) timeout() time.Duration {
	ms := c.timeoutMs
	if ms <= 0 {
		ms = defaultTimeoutMs
	}
	return time.Duration(ms) * time.Millisecond
}

// defaultTimeoutMs is the timeout used when the config does not set one.
// 30s is long enough for a rollout status wait on a slow node and short
// enough that a hung API server does not hold a run.
const defaultTimeoutMs = 30000

// --- plugin state -----------------------------------------------------------

// plugin holds the state of one plugin process: the configuration and the
// context the current session is bound to.
type plugin struct {
	cfg config
	// ctxName is the kube context the current session is connected to.
	// Empty until connect.
	ctxName string
}

// defaultAllowedVerbs is the transport's own floor when the operator did not
// narrow it further.
//
// The list is the set of verbs that MUTATE A DECLARED WORKLOAD or READ ONE.
// Everything that would turn this channel into an escape hatch is absent on
// purpose:
//
//   - exec / attach / debug / run / cp: arbitrary code execution inside the
//     cluster, or file transfer out of it — indistinguishable from the shell
//     on an SSH host, which is not what a governed change channel is for.
//   - port-forward / proxy: a tunnel to the API server that bypasses every
//     gate above this one.
//   - auth / create token / certificate: credential minting.
//   - config / edit: they rewrite the operator's own kubeconfig.
var defaultAllowedVerbs = []string{
	"get", "describe", "logs", "explain",
	"apply", "patch", "scale", "rollout", "delete", "label", "annotate",
	"wait", "version", "api-resources", "api-versions",
}

// forbiddenFlags are the caller-supplied flags that would redirect the
// target: the kubeconfig to use, the context to use, the API server to talk
// to, the identity to act as, and the namespace to act in. The plugin pins
// all of them, so a caller that tries to set one is refused rather than
// silently overridden — a silent override would teach the operator that the
// flag works.
var forbiddenFlags = map[string]bool{
	"--kubeconfig":               true,
	"--context":                  true,
	"--server":                   true,
	"--namespace":                true,
	"-n":                         true,
	"--user":                     true,
	"--username":                 true,
	"--password":                 true,
	"--token":                    true,
	"--as":                       true,
	"--as-group":                 true,
	"--as-uid":                   true,
	"--certificate-authority":    true,
	"--client-certificate":       true,
	"--client-key":               true,
	"--insecure-skip-tls-verify": true,
	"--exec-credential":          true,
	"--request-timeout":          true,
}

// --- command parsing --------------------------------------------------------

// command is a parsed exec request.
type command struct {
	// verb is the kubectl verb, or "" for the snapshot-capture form.
	verb string
	// args are the kubectl arguments after the verb.
	args []string
	// capture is the resource reference when the command is the host's
	// "base64 <resource>" snapshot-capture idiom.
	capture string
	// captureOnly reports whether this is a capture rather than a kubectl
	// invocation.
	captureOnly bool
}

// parseCommand parses the host's command string.
//
// Two forms are accepted:
//
//	kubectl <verb> [args...]   a kubectl invocation
//	base64 '<resource>'        the host's snapshot-capture idiom
//
// The string is tokenised with quote support (the host's capture idiom
// quotes its path) but WITHOUT any shell semantics: no expansion, no
// substitution, no chaining. A command that tries to use a shell
// metacharacter is refused, because the plugin runs kubectl directly with an
// argument vector — there is no shell for a metacharacter to reach.
func parseCommand(cmd string) (command, error) {
	tokens, err := tokenize(cmd)
	if err != nil {
		return command{}, err
	}
	if len(tokens) == 0 {
		return command{}, errors.New("empty command")
	}

	switch tokens[0] {
	case "kubectl":
		if len(tokens) < 2 {
			return command{}, errors.New("kubectl without a verb")
		}
		verb := tokens[1]
		if strings.HasPrefix(verb, "-") {
			return command{}, fmt.Errorf("kubectl global flag %q before the verb is not accepted; the plugin pins the target itself", verb)
		}
		return command{verb: verb, args: tokens[2:]}, nil

	case "base64":
		if len(tokens) != 2 {
			return command{}, errors.New("base64 expects exactly one resource reference")
		}
		return command{capture: tokens[1], captureOnly: true}, nil

	default:
		return command{}, fmt.Errorf("command %q is not a kubectl invocation; this channel only runs kubectl", tokens[0])
	}
}

// tokenize splits a command line into arguments, honouring single and double
// quotes, and refusing shell metacharacters outright.
//
// Why refuse rather than escape: the plugin never passes the string to a
// shell, so a metacharacter would be a literal argument to kubectl. Letting
// it through would mean the operator's workflow and the plugin's argument
// vector disagree about what the command means, which is exactly the
// ambiguity this plugin exists to remove.
func tokenize(cmd string) ([]string, error) {
	var (
		out   []string
		cur   strings.Builder
		quote rune
		have  bool
	)
	flush := func() {
		if have {
			out = append(out, cur.String())
			cur.Reset()
			have = false
		}
	}
	for i := 0; i < len(cmd); i++ {
		r := rune(cmd[i])
		switch {
		case quote == '\'':
			// POSIX: inside single quotes everything is literal, and the
			// only way out is the closing quote.
			if r == '\'' {
				quote = 0
				continue
			}
			cur.WriteRune(r)
			have = true
		case quote != 0:
			if r == quote {
				quote = 0
				continue
			}
			if r == '\\' && i+1 < len(cmd) {
				i++
				cur.WriteRune(rune(cmd[i]))
				have = true
				continue
			}
			cur.WriteRune(r)
			have = true
		case r == '\\':
			// Outside quotes a backslash escapes the next character. This
			// is what makes the host's own quoting round-trip:
			// shellQuoteSingle embeds a literal quote as '\''. Without the
			// escape rule, that sequence would split into three arguments
			// and the command would mean something else to kubectl than it
			// meant to the host.
			if i+1 >= len(cmd) {
				return nil, errors.New("dangling backslash in command")
			}
			i++
			cur.WriteRune(rune(cmd[i]))
			have = true
		case r == '\'' || r == '"':
			quote = r
			have = true
		case r == ' ' || r == '\t':
			flush()
		case r == ';' || r == '|' || r == '&' || r == '$' || r == '`' ||
			r == '>' || r == '<' || r == '\n' || r == '\r':
			return nil, fmt.Errorf("character %q is not accepted in a command", r)
		default:
			cur.WriteRune(r)
			have = true
		}
	}
	if quote != 0 {
		return nil, errors.New("unterminated quote in command")
	}
	flush()
	return out, nil
}

// checkTargetFlags refuses the flags that would redirect the target.
func checkTargetFlags(args []string) error {
	for _, a := range args {
		flag := a
		if i := strings.Index(a, "="); i >= 0 {
			flag = a[:i]
		}
		if forbiddenFlags[flag] {
			return fmt.Errorf("flag %q is not accepted: this channel pins the kubeconfig, context and namespace itself", flag)
		}
	}
	return nil
}

// --- kubectl invocation -----------------------------------------------------

// kubectlRunner performs one kubectl invocation. The interface exists so the
// tests can assert on the argument vector without a cluster.
type kubectlRunner interface {
	// run executes kubectl with the given arguments, feeding stdin, and
	// returns stdout, stderr and the exit code. A non-zero exit code is a
	// RESULT, not an error: kubectl reports its own failures on stderr.
	run(ctx context.Context, bin string, args []string, stdin []byte) (stdout, stderr string, exitCode int, err error)
}

// execRunner is the real runner: it runs the kubectl binary.
type execRunner struct{}

func (execRunner) run(ctx context.Context, bin string, args []string, stdin []byte) (string, string, int, error) {
	// #nosec G204 -- the binary path is the operator's install-time choice
	// (config.yaml, captured by the host at plugin install), never caller
	// input; the caller-controlled part is the argument vector, which
	// parseCommand tokenizes and the verb/flag policy allowlists.
	cmd := exec.CommandContext(ctx, bin, args...)
	if len(stdin) > 0 {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	var outBuf, errBuf bytes.Buffer
	cmd.Stdout = &outBuf
	cmd.Stderr = &errBuf
	err := cmd.Run()
	if err == nil {
		return outBuf.String(), errBuf.String(), 0, nil
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return outBuf.String(), errBuf.String(), exitErr.ExitCode(), nil
	}
	// The binary could not be started at all (missing, not executable).
	return outBuf.String(), errBuf.String(), -1, err
}

// realRunner is the process-wide runner. It is a variable so the tests can
// substitute a fake kubectl.
var realRunner kubectlRunner = execRunner{}

// --- plugin verbs -----------------------------------------------------------

// handle dispatches one host request.
func (p *plugin) handle(req *hostRequest) pluginResponse {
	switch req.Action {
	case "meta":
		return pluginResponse{OK: true, Result: &probeResult{
			Passed:  true,
			Message: "k8s",
			Details: map[string]any{
				"name":    "k8s",
				"version": "1.0.0",
				"type":    "channel",
			},
		}}

	case "init":
		p.cfg.applyConfig(req.Config)
		if len(p.cfg.allowedVerbs) == 0 {
			p.cfg.allowedVerbs = defaultAllowedVerbs
		}
		return pluginResponse{OK: true}

	case "connect":
		return p.connect(req.Params)

	case "exec":
		return p.exec(req.Params)

	case "collect":
		return p.collect(req.Params)

	case "upload":
		return p.upload(req.Params)

	case "disconnect":
		p.ctxName = ""
		return pluginResponse{OK: true}

	case "close":
		return pluginResponse{OK: true}

	default:
		return pluginResponse{OK: false, Error: fmt.Sprintf("unknown action %q", req.Action)}
	}
}

// connect binds the session to one kube context.
//
// The context comes from the target's address, and it must be one the
// operator allowed. The kubeconfig comes from the plugin's own config: the
// host never hands a channel plugin a secret, only a reference name.
//
// Connect is idempotent by the channel contract: connecting twice to the
// same context is a no-op, and connecting to a DIFFERENT context is
// refused — a session bound to one cluster must not be re-pointed.
func (p *plugin) connect(params map[string]any) pluginResponse {
	host, _ := params["host"].(string)
	host = strings.TrimSpace(host)
	if host == "" {
		return pluginResponse{OK: false, Error: "connect requires params.host (the kube context to use)"}
	}
	if !p.cfg.allowedContext(host) {
		return pluginResponse{OK: false, Error: fmt.Sprintf(
			"context %q is not in this plugin's allowed_contexts", host)}
	}
	if p.ctxName != "" && p.ctxName != host {
		return pluginResponse{OK: false, Error: fmt.Sprintf(
			"session is already bound to context %q and cannot be re-pointed to %q", p.ctxName, host)}
	}
	if p.cfg.kubeconfig != "" {
		if _, err := os.Stat(p.cfg.kubeconfig); err != nil {
			return pluginResponse{OK: false, Error: fmt.Sprintf(
				"kubeconfig %q is not readable: %v", p.cfg.kubeconfig, err)}
		}
	}
	p.ctxName = host
	return pluginResponse{OK: true}
}

// targetArgs builds the pinned kubectl arguments: the operator's kubeconfig
// and the session's context. These come FIRST so a later caller-supplied
// flag could not take precedence — and the caller is forbidden from
// supplying one anyway (checkTargetFlags).
func (p *plugin) targetArgs() []string {
	args := make([]string, 0, 4)
	if p.cfg.kubeconfig != "" {
		args = append(args, "--kubeconfig", p.cfg.kubeconfig)
	}
	if p.ctxName != "" {
		args = append(args, "--context", p.ctxName)
	}
	return args
}

// exec runs one command.
func (p *plugin) exec(params map[string]any) pluginResponse {
	cmdText, _ := params["cmd"].(string)
	cmd, err := parseCommand(cmdText)
	if err != nil {
		return pluginResponse{OK: false, Error: err.Error()}
	}
	if p.ctxName == "" {
		return pluginResponse{OK: false, Error: "exec before connect: the channel is not bound to a context"}
	}

	if cmd.captureOnly {
		return p.capture(cmd.capture)
	}

	if !p.cfg.allowedVerb(cmd.verb) {
		return pluginResponse{OK: false, Error: fmt.Sprintf(
			"kubectl verb %q is not allowed on this channel", cmd.verb)}
	}
	if err := checkTargetFlags(cmd.args); err != nil {
		return pluginResponse{OK: false, Error: err.Error()}
	}

	args := append(p.targetArgs(), cmd.verb)
	args = append(args, cmd.args...)
	return p.runKubectl(args, nil)
}

// capture answers the host's snapshot-capture idiom: "base64 <resource>".
//
// The host captures a target's pre-change state with "base64 <path>" and
// decodes the stdout. For a k8s target the "path" is a RESOURCE REFERENCE
// ("deployment/web"), and the captured bytes are that resource's state.
//
// The capture is JSON, and the server-owned bookkeeping is stripped from it:
// resourceVersion, uid, generation, creationTimestamp, managedFields and the
// whole status block belong to the API server, not to the change. Replaying
// them would make the restore fail with a conflict instead of restoring.
func (p *plugin) capture(ref string) pluginResponse {
	if ref == "" {
		return pluginResponse{OK: false, Error: "capture requires a resource reference"}
	}
	if err := checkTargetFlags([]string{ref}); err != nil {
		return pluginResponse{OK: false, Error: err.Error()}
	}
	args := append(p.targetArgs(), "get", ref, "-o", "json")
	resp := p.runKubectl(args, nil)
	if !resp.OK {
		return resp
	}
	if resp.Exec.ExitCode != 0 {
		return pluginResponse{OK: false, Error: fmt.Sprintf(
			"kubectl get %s failed (exit %d): %s", ref, resp.Exec.ExitCode, strings.TrimSpace(resp.Exec.Stderr))}
	}
	clean, err := stripVolatile([]byte(resp.Exec.Stdout))
	if err != nil {
		return pluginResponse{OK: false, Error: fmt.Sprintf(
			"capture %s: the resource state is not readable as JSON: %v", ref, err)}
	}
	return pluginResponse{OK: true, Exec: &channelExecResult{
		ExitCode: resp.Exec.ExitCode,
		Stdout:   base64.StdEncoding.EncodeToString(clean),
		Stderr:   resp.Exec.Stderr,
		Duration: resp.Exec.Duration,
	}}
}

// collect returns a resource's state (the host's Download over k8s).
func (p *plugin) collect(params map[string]any) pluginResponse {
	ref, _ := params["path"].(string)
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return pluginResponse{OK: false, Error: "collect requires params.path (the resource reference)"}
	}
	// The reference becomes a kubectl argument: a value that parses as a
	// target-redirecting flag must not be able to smuggle itself in here.
	if err := checkTargetFlags([]string{ref}); err != nil {
		return pluginResponse{OK: false, Error: err.Error()}
	}
	args := append(p.targetArgs(), "get", ref, "-o", "json")
	resp := p.runKubectl(args, nil)
	if !resp.OK {
		return resp
	}
	if resp.Exec.ExitCode != 0 {
		return pluginResponse{OK: false, Error: fmt.Sprintf(
			"kubectl get %s failed (exit %d): %s", ref, resp.Exec.ExitCode, strings.TrimSpace(resp.Exec.Stderr))}
	}
	return pluginResponse{OK: true, Collect: &collectPayload{
		Data: base64.StdEncoding.EncodeToString([]byte(resp.Exec.Stdout)),
	}}
}

// upload applies a manifest to a resource (the host's Upload over k8s).
//
// This is the restore half of the snapshot pair: the host stores the bytes it
// captured and pushes them back, so the rollback restores the CAPTURED state
// rather than replaying a reverse command.
func (p *plugin) upload(params map[string]any) pluginResponse {
	ref, _ := params["path"].(string)
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return pluginResponse{OK: false, Error: "upload requires params.path (the resource reference)"}
	}
	encoded, _ := params["data"].(string)
	manifest, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return pluginResponse{OK: false, Error: fmt.Sprintf("upload payload is not base64: %v", err)}
	}
	if len(bytes.TrimSpace(manifest)) == 0 {
		return pluginResponse{OK: false, Error: "upload payload is empty"}
	}
	args := append(p.targetArgs(), "apply", "-f", "-")
	return p.runKubectl(args, manifest)
}

// runKubectl performs one kubectl invocation and wraps the outcome.
func (p *plugin) runKubectl(args []string, stdin []byte) pluginResponse {
	bin := p.cfg.kubectlPath
	if bin == "" {
		bin = "kubectl"
	}
	ctx, cancel := context.WithTimeout(context.Background(), p.cfg.timeout())
	defer cancel()

	start := time.Now()
	stdout, stderr, code, err := realRunner.run(ctx, bin, args, stdin)
	dur := time.Since(start).Milliseconds()
	if err != nil {
		return pluginResponse{OK: false, Error: fmt.Sprintf("kubectl could not be started: %v", err)}
	}
	return pluginResponse{OK: true, Exec: &channelExecResult{
		ExitCode: code,
		Stdout:   stdout,
		Stderr:   stderr,
		Duration: dur,
	}}
}

// stripVolatile removes the API server's bookkeeping fields from a captured
// resource.
//
// The fields listed here are the ones the server owns: a manifest that
// carries them back is rejected (resourceVersion conflict) or fights the
// server (status is not writable). The workload's own declaration — spec,
// labels, annotations — is left exactly as captured, because THAT is the
// state a rollback must restore.
func stripVolatile(manifest []byte) ([]byte, error) {
	var obj map[string]any
	if err := json.Unmarshal(manifest, &obj); err != nil {
		return nil, err
	}
	if meta, ok := obj["metadata"].(map[string]any); ok {
		for _, f := range []string{
			"resourceVersion", "uid", "generation",
			"creationTimestamp", "managedFields", "selfLink",
		} {
			delete(meta, f)
		}
	}
	delete(obj, "status")
	return json.Marshal(obj)
}

// --- main -------------------------------------------------------------------

func main() {
	p := &plugin{}
	if err := serve(os.Stdin, os.Stdout, p); err != nil {
		fmt.Fprintf(os.Stderr, "k8s: %v\n", err)
		os.Exit(1)
	}
}

// serve runs the JSON-line protocol loop.
func serve(r io.Reader, w io.Writer, p *plugin) error {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	enc := json.NewEncoder(w)

	for scanner.Scan() {
		if len(bytes.TrimSpace(scanner.Bytes())) == 0 {
			continue
		}
		var req hostRequest
		if err := json.Unmarshal(scanner.Bytes(), &req); err != nil {
			_ = enc.Encode(pluginResponse{OK: false, Error: fmt.Sprintf("parse request: %v", err)})
			continue
		}
		if err := enc.Encode(p.handle(&req)); err != nil {
			return fmt.Errorf("encode response: %w", err)
		}
		if req.Action == "close" {
			return nil
		}
	}
	return scanner.Err()
}
