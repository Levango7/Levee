// Package verify implements LEVEE's verification gate framework.
//
// This file implements ProbeGate, a parameterised reachability / health
// probe covering three kinds:
//
//   - http:  GET a URL (optionally expanding "{target}" over every target)
//     and require the response status to fall inside an expected range,
//     optionally also matching the body by substring or regex;
//   - tcp:   dial host:port and require the connection to open;
//   - script: upload an operator-authored script through the channel and
//     execute it remotely, judging it by its exit code.
//
// Each kind runs in one of two modes:
//
//   - direct (default): the gate acts from the orchestrator's own network
//     position (net/http client or net.DialTimeout);
//   - remote: the gate drives the check through the GateInput.Channel so
//     that the probe originates from the target's network position. Remote
//     modes require a live channel; without one the gate fails honestly
//     ("missing channel") rather than fabricating a pass.
//
// TRUST LEVEL: like CommandGate commands, probe parameters that end up
// inside a shell command line (url, host_port, interpreter, script) come
// from compiled plans authored by operators. They are NOT subject to the
// validateGateCommand metacharacter blacklist — a probe URL or a multiline
// script cannot express itself under those restrictions. This mirrors the
// shell-module trust level: plan authors already hold full control over
// executed step commands, so probes add no new privilege. The blacklist is
// defence-in-depth for less-trusted plan sources and simply does not apply
// to this gate's construction.
//
// Configuration errors (unknown params, bad types, contradictory settings)
// do not panic the constructor: they are recorded at construction time and
// surfaced by Check fail-closed (Passed=false plus an error), mirroring
// CommandGate.policyErr. A misconfigured probe must never masquerade as a
// passing one.
package verify

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"net"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/nexus/levee/internal/channel"
	"github.com/nexus/levee/internal/log"
)

// ProbeGate defaults. The defaults are conservative so that a misconfigured
// gate fails fast rather than hanging the pipeline.
const (
	// DefaultProbeTimeout is the default per-attempt timeout when the
	// "timeout_seconds" param is not supplied.
	DefaultProbeTimeout = 10 * time.Second

	// DefaultProbeInterpreter is the interpreter used to run "script" kind
	// probes when "interpreter" is not supplied.
	DefaultProbeInterpreter = "sh"

	// DefaultProbeExpectStatusLo / Hi bound the default HTTP status window
	// when "expect_status" is not supplied (i.e. any 2xx passes).
	DefaultProbeExpectStatusLo = 200
	DefaultProbeExpectStatusHi = 299

	// DefaultProbeExpectExit is the expected exit code of "script" kind
	// probes when the "expect_exit" param is not supplied.
	DefaultProbeExpectExit = 0

	// ProbeTargetPlaceholder is the token expanded over every
	// GateInput.TargetIDs entry in http URLs and tcp host_port values.
	ProbeTargetPlaceholder = "{target}"

	// RemoteTCPCheckTimeoutSeconds bounds the bash /dev/tcp dial on the
	// remote side via coreutils timeout(1). POSIX best-effort: it relies on
	// bash's virtual /dev/tcp and a GNU/BSD timeout binary being present.
	RemoteTCPCheckTimeoutSeconds = 5
)

// validProbeParamKeys lists every accepted ProbeGate parameter, sorted, for
// inclusion in validation error messages. Keep in sync with applyParams.
var validProbeParamKeys = []string{
	"body_contains",
	"body_regex",
	"expect_exit",
	"expect_status",
	"host_port",
	"interpreter",
	"kind",
	"mode",
	"port_from_target",
	"script",
	"timeout_seconds",
	"url",
}

// ProbeGate is a parameterised reachability / health probe (http | tcp |
// script, direct | remote). It is safe for concurrent use: all mutable state
// is confined to a single Check call.
type ProbeGate struct {
	name  string
	phase GatePhase

	kind             string // "http" | "tcp" | "script"
	mode             string // "direct" | "remote"
	url              string // http only; may contain "{target}"
	hostPort         string // tcp only; "host:port", may contain "{target}"
	portFromTarget   bool   // tcp only; parse "host:port" out of each TargetID
	expectStatusLo   int    // http only, inclusive lower bound
	expectStatusHi   int    // http only, inclusive upper bound
	bodyContains     string // http direct only, optional substring match
	bodyRegex        *regexp.Regexp
	bodyRegexSrc     string // original regex text for the audit trail
	script           string // script kind, multiline, uploaded then executed
	interpreter      string // script kind, default "sh"
	timeout          time.Duration
	expectExit       int // script kind, default 0
	httpClientDirect *http.Client

	// paramsErr holds the first configuration violation found while applying
	// the params map. It is set at construction time and surfaced by Check
	// (fail-closed) so that a misconfigured probe can never report a pass.
	paramsErr error
}

// NewProbeGate returns a ProbeGate with the given name, phase and parameter
// map. All options are applied up-front: the params map is validated and
// parsed here, and any violation (unknown key, wrong type, contradictory
// settings, missing required "kind") is stored and later reported by Check
// as Passed=false plus an error. Construction therefore never fails, but a
// misconfigured gate always fails closed.
func NewProbeGate(name string, phase GatePhase, params map[string]any) *ProbeGate {
	g := &ProbeGate{
		name:             name,
		phase:            phase,
		mode:             "direct",
		expectStatusLo:   DefaultProbeExpectStatusLo,
		expectStatusHi:   DefaultProbeExpectStatusHi,
		interpreter:      DefaultProbeInterpreter,
		timeout:          DefaultProbeTimeout,
		expectExit:       DefaultProbeExpectExit,
		httpClientDirect: &http.Client{},
	}
	g.paramsErr = g.applyParams(params)
	return g
}

// applyParams validates and applies the params map strictly. Unknown keys
// are rejected with an error listing the valid keys; known keys are
// type-checked loosely (YAML numbers may arrive as int / int64 / float64)
// and cross-checked against the selected kind/mode. The first violation is
// returned; later keys are not examined.
func (g *ProbeGate) applyParams(params map[string]any) error {
	for k, v := range params {
		var err error
		switch k {
		case "kind":
			g.kind, err = paramString(v, k)
			if err == nil {
				switch g.kind {
				case "http", "tcp", "script":
				default:
					err = fmt.Errorf(`probe param "kind" must be one of http|tcp|script, got %q`, g.kind)
				}
			}
		case "mode":
			g.mode, err = paramString(v, k)
			if err == nil {
				switch g.mode {
				case "direct", "remote":
				default:
					err = fmt.Errorf(`probe param "mode" must be one of direct|remote, got %q`, g.mode)
				}
			}
		case "url":
			g.url, err = paramString(v, k)
		case "host_port":
			g.hostPort, err = paramString(v, k)
		case "port_from_target":
			g.portFromTarget, err = paramBool(v, k)
		case "expect_status":
			g.expectStatusLo, g.expectStatusHi, err = parseExpectStatus(v)
		case "body_contains":
			g.bodyContains, err = paramString(v, k)
		case "body_regex":
			g.bodyRegexSrc, err = paramString(v, k)
			if err == nil {
				g.bodyRegex, err = regexp.Compile(g.bodyRegexSrc)
				if err != nil {
					err = fmt.Errorf("probe param %q is not a valid regular expression: %w", k, err)
				}
			}
		case "script":
			g.script, err = paramString(v, k)
		case "interpreter":
			g.interpreter, err = paramString(v, k)
		case "timeout_seconds":
			g.timeout, err = paramDurationSeconds(v, k)
		case "expect_exit":
			g.expectExit, err = paramInt(v, k)
		default:
			return fmt.Errorf("probe param %q is not supported (valid keys: %s)", k, strings.Join(validProbeParamKeys, ", "))
		}
		if err != nil {
			return err
		}
	}

	// Cross-field validation. Everything below is fail-closed: the error is
	// surfaced by Check, never silently ignored.

	if g.kind == "" {
		return fmt.Errorf(`probe param "kind" is required (one of http|tcp|script)`)
	}

	if g.portFromTarget && g.kind != "tcp" {
		return fmt.Errorf(`probe param "port_from_target" applies to kind=tcp only`)
	}

	switch g.kind {
	case "http":
		if g.url == "" {
			return fmt.Errorf(`probe param "url" is required for kind=http`)
		}
		if g.script != "" {
			return fmt.Errorf(`probe param "script" applies to kind=script only`)
		}
		if g.hostPort != "" {
			return fmt.Errorf(`probe param "host_port" applies to kind=tcp only`)
		}
		if g.bodyContains != "" || g.bodyRegex != nil {
			// Body inspection reads the response payload directly, which only
			// the direct http client does today; curl -w in remote mode
			// reports the status code alone.
			if g.mode != "direct" {
				return fmt.Errorf(`probe params "body_contains"/"body_regex" apply to mode=direct only`)
			}
		}
	case "tcp":
		if g.url != "" {
			return fmt.Errorf(`probe param "url" applies to kind=http only`)
		}
		if g.hostPort == "" && !g.portFromTarget {
			return fmt.Errorf(`kind=tcp requires "host_port" or "port_from_target"`)
		}
		if g.hostPort != "" && g.portFromTarget {
			return fmt.Errorf(`kind=tcp accepts either "host_port" or "port_from_target", not both`)
		}
	case "script":
		if g.script == "" {
			return fmt.Errorf(`probe param "script" is required for kind=script`)
		}
		if g.url != "" {
			return fmt.Errorf(`probe param "url" applies to kind=http only`)
		}
		if g.hostPort != "" {
			return fmt.Errorf(`probe param "host_port" applies to kind=tcp only`)
		}
	}

	return nil
}

// Name returns the gate's unique identifier.
func (g *ProbeGate) Name() string { return g.name }

// Phase returns the phase at which this gate runs.
func (g *ProbeGate) Phase() GatePhase { return g.phase }

// ParamsError returns the configuration violation detected at construction
// time, or nil when the params map was valid.
func (g *ProbeGate) ParamsError() error { return g.paramsErr }

// Check runs the probe according to its configured kind and mode.
//
// A nil error with Passed == true means the probe succeeded; a nil error
// with Passed == false means the probe ran (or was correctly rejected) and
// did not pass; a non-nil error accompanies a fail-closed rejection
// (misconfiguration) or an infrastructure failure such as a broken channel.
func (g *ProbeGate) Check(ctx context.Context, input GateInput) (GateResult, error) {
	// Honour an already-cancelled context up front, mirroring CommandGate.
	if err := ctx.Err(); err != nil {
		return GateResult{
			Passed:  false,
			Message: fmt.Sprintf("probe gate %q cancelled before run: %v", g.name, err),
			Details: map[string]any{
				"gate":   "probe",
				"name":   g.name,
				"reason": "context_cancelled",
				"cause":  err.Error(),
			},
		}, nil
	}

	// Fail closed on configuration violations before touching the network
	// or the channel (mirror CommandGate.policyErr).
	if g.paramsErr != nil {
		return GateResult{
			Passed:  false,
			Message: fmt.Sprintf("probe gate %q rejected invalid params: %v", g.name, g.paramsErr),
			Details: map[string]any{
				"gate":   "probe",
				"name":   g.name,
				"reason": "invalid_params",
				"cause":  g.paramsErr.Error(),
			},
		}, fmt.Errorf("probe gate %q: invalid params: %w", g.name, g.paramsErr)
	}

	start := time.Now()
	details := map[string]any{
		"gate":   "probe",
		"name":   g.name,
		"kind":   g.kind,
		"mode":   g.mode,
		"run_id": input.RunID,
	}

	var (
		res GateResult
		err error
	)
	switch g.kind {
	case "http":
		res, err = g.checkHTTP(ctx, input, details)
	case "tcp":
		res, err = g.checkTCP(ctx, input, details)
	case "script":
		res, err = g.checkScript(ctx, input, details)
	default:
		// Unreachable: applyParams rejects an empty/unknown kind, but keep
		// the default branch total so the gate can never panic.
		res = GateResult{
			Passed:  false,
			Message: fmt.Sprintf("probe gate %q has unsupported kind %q", g.name, g.kind),
			Details: details,
		}
	}
	if res.Latency == 0 {
		res.Latency = time.Since(start)
	}
	return res, err
}

// checkHTTP executes the http probe. In direct mode it GETs the URL with a
// per-attempt timeout; in remote mode it shells out to curl on the target
// through the channel (POSIX best-effort).
func (g *ProbeGate) checkHTTP(ctx context.Context, input GateInput, details map[string]any) (GateResult, error) {
	fail := func(reason, format string, args ...any) (GateResult, error) {
		details["reason"] = reason
		return GateResult{
			Passed:  false,
			Message: fmt.Sprintf("probe gate %q: %s", g.name, fmt.Sprintf(format, args...)),
			Details: details,
		}, nil
	}

	if g.mode == "remote" {
		// Remote http probe via curl. POSIX best-effort: assumes curl(1) is
		// installed on the target; -f makes non-2xx responses fail, -w prints
		// the effective status code for the audit trail.
		if res, rerr := g.missingChannelResult(input); res != nil {
			return *res, rerr
		}
		cmd := fmt.Sprintf("curl -fsS -o /dev/null -w '%%{http_code}' '%s'", g.url)
		details["command"] = cmd
		out, res, xerr := g.execOnChannel(ctx, input.Channel, cmd)
		if xerr != nil {
			return *out, xerr
		}
		return g.judgeRemoteHTTP(res, details)
	}

	// Direct mode: expand "{target}" over EVERY target so that multi-target
	// workflows get one request per target; an empty target list degrades to
	// a single unexpanded request.
	urls := expandTargets(g.url, input.TargetIDs)
	details["url"] = urls
	for i, u := range urls {
		label := probeTargetLabel(i, input.TargetIDs)
		passed, status, why := g.probeOneHTTP(ctx, u)
		if !passed {
			if len(input.TargetIDs) > 0 {
				return fail("status_or_body_mismatch", "target %q failed (%s)", label, why)
			}
			return fail("status_or_body_mismatch", "%s", why)
		}
		details["last_status"] = status
	}
	details["reason"] = "all_targets_ok"
	return GateResult{
		Passed:  true,
		Message: fmt.Sprintf("probe gate %q passed: %d/%d http probe(s) ok", g.name, len(urls), len(urls)),
		Details: details,
	}, nil
}

// probeOneHTTP performs a single direct GET and judges status / body. It
// never returns a terminal error: transport problems are reported as a
// failed probe with the cause embedded in the message.
func (g *ProbeGate) probeOneHTTP(ctx context.Context, rawURL string) (passed bool, status int, why string) {
	attemptCtx, cancel := context.WithTimeout(ctx, g.timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(attemptCtx, http.MethodGet, rawURL, nil)
	if err != nil {
		return false, 0, fmt.Sprintf("build request for %q: %v", rawURL, err)
	}
	resp, err := g.httpClientDirect.Do(req)
	if err != nil {
		return false, 0, fmt.Sprintf("GET %q: %v", rawURL, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode < g.expectStatusLo || resp.StatusCode > g.expectStatusHi {
		return false, resp.StatusCode,
			fmt.Sprintf("status %d outside expected range %d-%d", resp.StatusCode, g.expectStatusLo, g.expectStatusHi)
	}

	if g.bodyContains != "" || g.bodyRegex != nil {
		body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20)) // 1 MiB cap keeps a runaway endpoint from exhausting memory
		if err != nil {
			return false, resp.StatusCode, fmt.Sprintf("read body: %v", err)
		}
		if g.bodyContains != "" && !strings.Contains(string(body), g.bodyContains) {
			return false, resp.StatusCode, fmt.Sprintf("body does not contain %q", g.bodyContains)
		}
		if g.bodyRegex != nil && !g.bodyRegex.Match(body) {
			return false, resp.StatusCode, fmt.Sprintf("body does not match regex %q", g.bodyRegexSrc)
		}
	}
	return true, resp.StatusCode, ""
}

// checkTCP executes the tcp probe. In direct mode it dials host:port with a
// timeout; in remote mode it opens the connection from the target via bash's
// virtual /dev/tcp (POSIX best-effort, see package comment).
func (g *ProbeGate) checkTCP(ctx context.Context, input GateInput, details map[string]any) (GateResult, error) {
	addrs, unresolved, err := g.resolveTCPAddrs(input)
	if err != nil {
		return GateResult{
			Passed:  false,
			Message: fmt.Sprintf("probe gate %q: %v", g.name, err),
			Details: map[string]any{
				"gate":   "probe",
				"name":   g.name,
				"reason": "invalid_params",
				"cause":  err.Error(),
			},
		}, fmt.Errorf("probe gate %q: %w", g.name, err)
	}
	if len(unresolved) > 0 {
		return GateResult{
			Passed:  false,
			Message: fmt.Sprintf("probe gate %q: targets without host:port: %s", g.name, strings.Join(unresolved, ", ")),
			Details: map[string]any{
				"gate":       "probe",
				"name":       g.name,
				"reason":     "unresolved_targets",
				"unresolved": unresolved,
			},
		}, nil
	}
	details["addrs"] = addrs

	if g.mode == "remote" {
		if res, rerr := g.missingChannelResult(input); res != nil {
			return *res, rerr
		}
		for _, addr := range addrs {
			// POSIX best-effort remote tcp check: bash /dev/tcp opens a TCP
			// connection; timeout(1) bounds the attempt. Exit 0 means the
			// connection was established and closed cleanly.
			cmd := fmt.Sprintf("timeout %d bash -c 'exec 3<>/dev/tcp/%s'", RemoteTCPCheckTimeoutSeconds, addr)
			details["command"] = cmd
			out, res, xerr := g.execOnChannel(ctx, input.Channel, cmd)
			if xerr != nil {
				return *out, xerr
			}
			if res.ExitCode != 0 {
				details["reason"] = "connect_failed_remote"
				details["addr"] = addr
				details["exit_code"] = res.ExitCode
				return GateResult{
					Passed:  false,
					Message: fmt.Sprintf("probe gate %q: remote tcp connect to %s failed (exit=%d)", g.name, addr, res.ExitCode),
					Details: details,
				}, nil
			}
		}
		details["reason"] = "all_addrs_ok"
		return GateResult{
			Passed:  true,
			Message: fmt.Sprintf("probe gate %q passed: %d remote tcp connect(s) ok", g.name, len(addrs)),
			Details: details,
		}, nil
	}

	// Direct mode.
	for _, addr := range addrs {
		dialCtx, cancel := context.WithTimeout(ctx, g.timeout)
		var d net.Dialer
		conn, derr := d.DialContext(dialCtx, "tcp", addr)
		cancel()
		if derr != nil {
			details["reason"] = "connect_failed_direct"
			details["addr"] = addr
			details["cause"] = derr.Error()
			return GateResult{
				Passed:  false,
				Message: fmt.Sprintf("probe gate %q: tcp connect to %s failed: %v", g.name, addr, derr),
				Details: details,
			}, nil
		}
		_ = conn.Close()
	}
	details["reason"] = "all_addrs_ok"
	return GateResult{
		Passed:  true,
		Message: fmt.Sprintf("probe gate %q passed: %d tcp connect(s) ok", g.name, len(addrs)),
		Details: details,
	}, nil
}

// resolveTCPAddrs computes the host:port list a tcp probe should test.
// With port_from_target the address comes from each TargetID ("host:port");
// otherwise host_port is used literally, expanding "{target}" over the
// target list (empty list => one unexpanded attempt).
func (g *ProbeGate) resolveTCPAddrs(input GateInput) (addrs []string, unresolved []string, err error) {
	if g.portFromTarget {
		if len(input.TargetIDs) == 0 {
			return nil, nil, fmt.Errorf(`port_from_target requires at least one target in GateInput.TargetIDs`)
		}
		for _, t := range input.TargetIDs {
			if !strings.Contains(t, ":") {
				unresolved = append(unresolved, t)
				continue
			}
			addrs = append(addrs, t)
		}
		return addrs, unresolved, nil
	}
	return expandTargets(g.hostPort, input.TargetIDs), nil, nil
}

// checkScript uploads the operator-authored script to a private temp path on
// the target and executes it with the configured interpreter, preserving its
// exit code while cleaning up the file either way. The result is judged by
// comparing the exit code against the "expect_exit" param (default 0).
func (g *ProbeGate) checkScript(ctx context.Context, input GateInput, details map[string]any) (GateResult, error) {
	if res, rerr := g.missingChannelResult(input); res != nil {
		return *res, rerr
	}

	path := "/tmp/.levee-probe-" + randHex8()
	if err := input.Channel.Upload(ctx, path, strings.NewReader(g.script)); err != nil {
		return GateResult{
			Passed:  false,
			Message: fmt.Sprintf("probe gate %q: upload script to %s: %v", g.name, path, err),
			Details: map[string]any{
				"gate":   "probe",
				"name":   g.name,
				"reason": "upload_error",
				"path":   path,
				"cause":  err.Error(),
			},
		}, err
	}
	details["path"] = path
	details["interpreter"] = g.interpreter

	// rc=$? captures the script's exit code; the cleanup rm runs regardless
	// of the outcome and the captured rc becomes this Exec's exit code.
	cmd := fmt.Sprintf("%s %s; rc=$?; rm -f %s; exit $rc", g.interpreter, path, path)
	details["command"] = cmd
	out, res, err := g.execOnChannel(ctx, input.Channel, cmd)
	if err != nil {
		return *out, err
	}

	details["exit_code"] = res.ExitCode
	details["stdout"] = res.Stdout
	details["stderr"] = res.Stderr
	if res.ExitCode != g.expectExit {
		details["reason"] = "exit_code_mismatch"
		details["expected_exit"] = g.expectExit
		return GateResult{
			Passed:  false,
			Message: fmt.Sprintf("probe gate %q: script exited %d, want %d", g.name, res.ExitCode, g.expectExit),
			Details: details,
		}, nil
	}
	details["reason"] = "match"
	return GateResult{
		Passed:  true,
		Message: fmt.Sprintf("probe gate %q passed: script exited %d", g.name, res.ExitCode),
		Details: details,
	}, nil
}

// missingChannelResult returns the fail-closed result and error when the
// gate requires a live channel but GateInput.Channel is nil, mirroring
// CommandGate's wording so audit trails read the same across gate types.
// It returns (nil, nil) when a channel is available.
func (g *ProbeGate) missingChannelResult(input GateInput) (*GateResult, error) {
	if input.Channel == nil {
		return &GateResult{
			Passed:  false,
			Message: fmt.Sprintf("probe gate %q has no channel", g.name),
			Details: map[string]any{
				"gate":   "probe",
				"name":   g.name,
				"reason": "missing_channel",
			},
		}, fmt.Errorf("probe gate %q: channel is nil", g.name)
	}
	return nil, nil
}

// execOnChannel runs cmd on the target through the channel with the gate's
// per-attempt timeout applied on top of the caller's context.
func (g *ProbeGate) execOnChannel(ctx context.Context, ch channel.Channel, cmd string) (*GateResult, *channel.ExecResult, error) {
	execCtx, cancel := context.WithTimeout(ctx, g.timeout)
	defer cancel()

	start := time.Now()
	res, err := ch.Exec(execCtx, cmd)
	if res != nil {
		res.Duration = time.Since(start)
	}
	if err != nil {
		log.Warn("probe gate exec failed",
			"gate", g.name,
			"cmd", cmd,
			"err", err)
		return &GateResult{
			Passed:  false,
			Message: fmt.Sprintf("probe gate %q: exec failed: %v", g.name, err),
			Details: map[string]any{
				"gate":   "probe",
				"name":   g.name,
				"reason": "exec_error",
				"cmd":    cmd,
				"cause":  err.Error(),
			},
		}, res, err
	}
	return nil, res, nil
}

// judgeRemoteHTTP evaluates the output of the remote curl http probe. The
// gate passes when the command exited 0 and curl printed a 2xx status code;
// both conditions together are POSIX best-effort evidence that the endpoint
// answered successfully.
func (g *ProbeGate) judgeRemoteHTTP(res *channel.ExecResult, details map[string]any) (GateResult, error) {
	details["exit_code"] = res.ExitCode
	details["stdout"] = res.Stdout
	code := strings.TrimSpace(res.Stdout)
	if res.ExitCode != 0 {
		details["reason"] = "curl_failed"
		return GateResult{
			Passed:  false,
			Message: fmt.Sprintf("probe gate %q: remote curl failed (exit=%d)", g.name, res.ExitCode),
			Details: details,
		}, nil
	}
	if len(code) == 0 || code[0] != '2' {
		details["reason"] = "status_not_2xx"
		details["reported_status"] = code
		return GateResult{
			Passed:  false,
			Message: fmt.Sprintf("probe gate %q: remote probe reported non-2xx status %q", g.name, code),
			Details: details,
		}, nil
	}
	details["reason"] = "all_targets_ok"
	return GateResult{
		Passed:  true,
		Message: fmt.Sprintf("probe gate %q passed: remote probe reported status %s", g.name, code),
		Details: details,
	}, nil
}

// expandTargets substitutes ProbeTargetPlaceholder in template with each
// target id. An empty target list yields a single entry with the template
// unchanged, so a gate without targets still probes exactly once.
func expandTargets(template string, targets []string) []string {
	if len(targets) == 0 {
		return []string{template}
	}
	out := make([]string, 0, len(targets))
	for _, t := range targets {
		out = append(out, strings.ReplaceAll(template, ProbeTargetPlaceholder, t))
	}
	return out
}

// probeTargetLabel returns the audit-trail label for the i-th expansion: the
// target id when available, otherwise the index.
func probeTargetLabel(i int, targets []string) string {
	if i < len(targets) {
		return targets[i]
	}
	return fmt.Sprintf("#%d", i)
}

// randHex8 returns 8 hex characters from crypto/rand, falling back to a
// timestamp-derived value if the CSPRNG is unavailable (mirroring
// newRunID's defensive posture). Used only for temp-file uniqueness.
func randHex8() string {
	b := make([]byte, 4)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("%08x", time.Now().UnixNano()&0xffffffff)
	}
	return hex.EncodeToString(b)
}

// --- loose param coercions ---------------------------------------------------
//
// YAML decoding produces int / float64 / bool / string values; these helpers
// accept the plausible spellings of each scalar and reject everything else
// with a stable, actionable message.

func paramString(v any, key string) (string, error) {
	s, ok := v.(string)
	if !ok {
		return "", fmt.Errorf("probe param %q must be a string, got %T", key, v)
	}
	return s, nil
}

func paramBool(v any, key string) (bool, error) {
	b, ok := v.(bool)
	if !ok {
		return false, fmt.Errorf("probe param %q must be a boolean, got %T", key, v)
	}
	return b, nil
}

func paramInt(v any, key string) (int, error) {
	switch n := v.(type) {
	case int:
		return n, nil
	case int64:
		return int(n), nil
	case uint64:
		return int(n), nil
	case float64:
		if n != float64(int(n)) {
			return 0, fmt.Errorf("probe param %q must be an integer, got %v", key, n)
		}
		return int(n), nil
	default:
		return 0, fmt.Errorf("probe param %q must be an integer, got %T", key, v)
	}
}

// paramDurationSeconds converts a positive number of seconds to a Duration.
func paramDurationSeconds(v any, key string) (time.Duration, error) {
	n, err := paramInt(v, key)
	if err != nil {
		return 0, err
	}
	if n <= 0 {
		return 0, fmt.Errorf("probe param %q must be > 0, got %d", key, n)
	}
	return time.Duration(n) * time.Second, nil
}

// parseExpectStatus accepts "200-299" style ranges, single codes as strings
// ("200") and bare integers (200). The returned bounds are inclusive.
func parseExpectStatus(v any) (lo, hi int, err error) {
	lo, hi = DefaultProbeExpectStatusLo, DefaultProbeExpectStatusHi
	switch val := v.(type) {
	case string:
		s := strings.TrimSpace(val)
		if loS, hiS, ok := strings.Cut(s, "-"); ok {
			lo, err = strconvAtoi(loS, "expect_status")
			if err != nil {
				return lo, hi, err
			}
			hi, err = strconvAtoi(hiS, "expect_status")
			if err != nil {
				return lo, hi, err
			}
		} else {
			lo, err = strconvAtoi(s, "expect_status")
			if err != nil {
				return lo, hi, err
			}
			hi = lo
		}
	case int:
		lo, hi = val, val
	case int64:
		lo, hi = int(val), int(val)
	case float64:
		lo, hi = int(val), int(val)
	default:
		return lo, hi, fmt.Errorf("probe param \"expect_status\" must be a string or integer, got %T", v)
	}
	if lo <= 0 || hi <= 0 || lo > hi {
		return lo, hi, fmt.Errorf(`probe param "expect_status" must be a positive range low-high (e.g. "200-299"), got %v`, v)
	}
	return lo, hi, nil
}

func strconvAtoi(s string, key string) (int, error) {
	var n int
	if s == "" {
		return 0, fmt.Errorf("probe param %q contains an empty number", key)
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c < '0' || c > '9' {
			return 0, fmt.Errorf("probe param %q must look like \"200\" or \"200-299\", got %q", key, s)
		}
		n = n*10 + int(c-'0')
	}
	return n, nil
}
