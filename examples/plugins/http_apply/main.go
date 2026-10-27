// Command http-apply is an example MODULE plugin for LEVEE.
//
// It speaks the host's JSON-line protocol on stdin/stdout (one request object
// per line, one response per line) and manages a resource through a vendor
// REST API — the case the executor's channel-free modules exist for: the
// plugin owns its transport, so the host dials no SSH/WinRM session for it and
// only passes the step's inventory target as addressing.
//
// Verbs:
//
//	meta     — identity + the module surface: details.actions (required) and
//	           details.idempotent (optional, default false)
//	init     — the config.yaml stored next to plugin.yaml at install time
//	validate — the host calls this BEFORE execute; refuse a malformed step
//	execute  — run one action; the answer carries ModuleResult
//	           (exit_code/stdout/stderr/changed/duration_ms)
//
// Install and try it:
//
//	go build -o examples/plugins/http_apply/http-apply ./examples/plugins/http_apply
//	levee plugin install ./examples/plugins/http_apply
//	levee plugin enable http-apply
//	# then, in a workflow step: module: http-apply, action: apply,
//	#                          args: {url: "https://api.example/v1/buckets/a"}
//
// A workflow step's module name IS the registry name, so `module: http-apply`
// dispatches here once serve has booted with the plugin enabled.
package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

// --- wire types (the host's protocol, mirrored) -----------------------------

type hostRequest struct {
	Action string         `json:"action"`
	Config map[string]any `json:"config,omitempty"`
	Params map[string]any `json:"params,omitempty"`
}

type moduleResult struct {
	ExitCode int            `json:"exit_code"`
	Stdout   string         `json:"stdout"`
	Stderr   string         `json:"stderr"`
	Changed  bool           `json:"changed"`
	Duration int64          `json:"duration_ms"`
	Details  map[string]any `json:"details,omitempty"`
}

type probeResult struct {
	Passed  bool           `json:"passed"`
	Message string         `json:"message"`
	Details map[string]any `json:"details,omitempty"`
}

type pluginResponse struct {
	OK     bool          `json:"ok"`
	Error  string        `json:"error,omitempty"`
	Result *probeResult  `json:"result,omitempty"` // meta
	Module *moduleResult `json:"module,omitempty"` // execute
}

// --- configuration ---------------------------------------------------------

type config struct {
	Timeout   time.Duration
	AuthToken string
}

func defaultConfig() config {
	return config{Timeout: 10 * time.Second}
}

// --- main ------------------------------------------------------------------

func main() {
	cfg := defaultConfig()
	if err := serve(os.Stdin, os.Stdout, &cfg); err != nil {
		fmt.Fprintf(os.Stderr, "http-apply: %v\n", err)
		os.Exit(1)
	}
}

func serve(r io.Reader, w io.Writer, cfg *config) error {
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
		if err := enc.Encode(handle(&req, cfg)); err != nil {
			return fmt.Errorf("encode response: %w", err)
		}
		if req.Action == "close" {
			return nil
		}
	}
	return scanner.Err()
}

// handle dispatches one request. Unknown verbs are refused: a host that speaks
// a newer protocol must not read a silent "ok" as compliance.
func handle(req *hostRequest, cfg *config) pluginResponse {
	switch req.Action {
	case "meta":
		return pluginResponse{OK: true, Result: &probeResult{
			Passed:  true,
			Message: "http-apply",
			Details: map[string]any{
				"name":    "http-apply",
				"version": "1.0.0",
				"type":    "module",
				// The declared surface: the host rejects a step whose
				// action is not in this list before starting a process.
				"actions":    []any{"apply", "destroy"},
				"idempotent": true, // PUT/DELETE of the same URL converges
			},
		}}
	case "init":
		if v, ok := req.Config["timeout_seconds"]; ok {
			if n, ok := toInt(v); ok && n > 0 {
				cfg.Timeout = time.Duration(n) * time.Second
			}
		}
		if v, ok := req.Config["auth_token"].(string); ok {
			cfg.AuthToken = v
		}
		return pluginResponse{OK: true}
	case "validate":
		if err := validate(req.Params); err != nil {
			return pluginResponse{OK: false, Error: err.Error()}
		}
		return pluginResponse{OK: true}
	case "execute":
		return execute(req.Params, cfg)
	case "close":
		return pluginResponse{OK: true}
	default:
		return pluginResponse{OK: false, Error: fmt.Sprintf("unknown action %q", req.Action)}
	}
}

// validate runs before anything is mutated: args are checked here so a typo
// fails the step without a request ever leaving the host.
func validate(params map[string]any) error {
	action, _ := params["action"].(string)
	switch action {
	case "apply", "destroy":
	default:
		return fmt.Errorf("unsupported action %q (declared: apply, destroy)", action)
	}
	args, _ := params["args"].(map[string]any)
	url, _ := args["url"].(string)
	if strings.TrimSpace(url) == "" {
		return fmt.Errorf("action %q requires args.url", action)
	}
	if !strings.HasPrefix(url, "http://") && !strings.HasPrefix(url, "https://") {
		return fmt.Errorf("args.url %q must be an absolute http(s) URL", url)
	}
	return nil
}

// execute performs the request and maps the outcome:
//
//	transport failure        → ok:false (the action could not run at all)
//	2xx                      → exit_code 0, changed true
//	non-2xx                  → exit_code 1 (it ran and failed; the host's
//	                           step error quotes stderr, audit keeps the code)
func execute(params map[string]any, cfg *config) pluginResponse {
	action, _ := params["action"].(string)
	args, _ := params["args"].(map[string]any)
	url, _ := args["url"].(string)

	method := http.MethodPut
	if action == "destroy" {
		method = http.MethodDelete
	}

	start := time.Now()
	req, err := http.NewRequest(method, url, nil)
	if err != nil {
		return pluginResponse{OK: false, Error: fmt.Sprintf("build request: %v", err)}
	}
	if cfg.AuthToken != "" {
		req.Header.Set("Authorization", "Bearer "+cfg.AuthToken)
	}
	client := &http.Client{Timeout: cfg.Timeout}
	resp, err := client.Do(req)
	if err != nil {
		return pluginResponse{OK: false, Error: fmt.Sprintf("%s %s: %v", method, url, err)}
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))

	out := &moduleResult{
		Stdout:   fmt.Sprintf("%s %s -> %d\n%s", method, url, resp.StatusCode, strings.TrimSpace(string(body))),
		Duration: time.Since(start).Milliseconds(),
		Changed:  true,
		Details:  map[string]any{"status": resp.StatusCode, "method": method},
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		out.ExitCode = 1
		out.Stderr = fmt.Sprintf("vendor API returned %d", resp.StatusCode)
	}
	return pluginResponse{OK: true, Module: out}
}

// toInt accepts the numeric shapes a YAML/JSON config can produce.
func toInt(v any) (int, bool) {
	switch n := v.(type) {
	case int:
		return n, true
	case int64:
		return int(n), true
	case float64:
		return int(n), true
	}
	return 0, false
}
