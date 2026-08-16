// Command http_probe is an example LEVEE gate plugin that performs HTTP
// health checks against a configurable URL. It demonstrates the plugin
// structure: a sub-process that reads its manifest (plugin.yaml) and
// configuration (config.yaml) from its working directory and serves
// gate-check requests over its stdin/stdout (or gRPC in a full
// implementation).
//
// This example uses a simplified JSON-over-stdio protocol so that it can
// be built and tested without a gRPC dependency. In a production plugin
// the communication would use the gRPC service defined in
// proto/plugin.proto.
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

// --- Plugin metadata --------------------------------------------------------

// pluginMeta is the static metadata for this plugin. It must match the
// plugin.yaml manifest.
type pluginMeta struct {
	Name        string `json:"name"`
	Version     string `json:"version"`
	Type        string `json:"type"`
	Description string `json:"description"`
}

func meta() pluginMeta {
	return pluginMeta{
		Name:        "http-probe",
		Version:     "1.0.0",
		Type:        "gate",
		Description: "HTTP probe gate that checks a URL's status code and response body",
	}
}

// --- Plugin configuration ---------------------------------------------------

// config holds the runtime configuration parsed from config.yaml.
type config struct {
	Timeout    string `json:"timeout" yaml:"timeout"`
	Retry      int    `json:"retry" yaml:"retry"`
	ExpectCode int    `json:"expect_code" yaml:"expect_code"`
	ExpectBody string `json:"expect_body" yaml:"expect_body"`
}

func defaultConfig() config {
	return config{
		Timeout:    "10s",
		Retry:      3,
		ExpectCode: 200,
		ExpectBody: "",
	}
}

// --- Gate check protocol ----------------------------------------------------

// checkRequest is the JSON request sent by the host to the plugin.
type checkRequest struct {
	// Action identifies the operation: "init", "check", "close".
	Action string `json:"action"`

	// Config is the plugin configuration, sent with "init".
	Config map[string]any `json:"config,omitempty"`

	// Params are the gate-specific parameters, sent with "check".
	Params map[string]any `json:"params,omitempty"`
}

// checkResponse is the JSON response sent by the plugin to the host.
type checkResponse struct {
	// OK reports whether the operation succeeded.
	OK bool `json:"ok"`

	// Error is a human-readable error message when OK is false.
	Error string `json:"error,omitempty"`

	// Result is the gate check result, populated for "check".
	Result *gateResult `json:"result,omitempty"`
}

// gateResult is the outcome of an HTTP probe.
type gateResult struct {
	Passed  bool           `json:"passed"`
	Message string         `json:"message"`
	Details map[string]any `json:"details,omitempty"`
}

// --- Main loop --------------------------------------------------------------

func main() {
	cfg := defaultConfig()
	if err := serve(os.Stdin, os.Stdout, &cfg); err != nil {
		fmt.Fprintf(os.Stderr, "http-probe: %v\n", err)
		os.Exit(1)
	}
}

// serve reads JSON requests from r and writes JSON responses to w until
// EOF or a "close" action. It is the plugin's main loop.
func serve(r io.Reader, w io.Writer, cfg *config) error {
	scanner := bufio.NewScanner(r)
	// Allow large lines (up to 1MB) for big config payloads.
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	encoder := json.NewEncoder(w)

	for scanner.Scan() {
		line := scanner.Bytes()
		if len(strings.TrimSpace(string(line))) == 0 {
			continue
		}

		var req checkRequest
		if err := json.Unmarshal(line, &req); err != nil {
			_ = encoder.Encode(checkResponse{
				OK:    false,
				Error: fmt.Sprintf("parse request: %v", err),
			})
			continue
		}

		resp := handleRequest(&req, cfg)
		if err := encoder.Encode(resp); err != nil {
			return fmt.Errorf("encode response: %w", err)
		}

		if req.Action == "close" {
			return nil
		}
	}

	if err := scanner.Err(); err != nil {
		return fmt.Errorf("read stdin: %w", err)
	}
	return nil
}

// handleRequest dispatches a single request to the appropriate handler.
func handleRequest(req *checkRequest, cfg *config) checkResponse {
	switch req.Action {
	case "init":
		return handleInit(req, cfg)
	case "check":
		return handleCheck(req, cfg)
	case "close":
		return checkResponse{OK: true}
	case "meta":
		return checkResponse{
			OK: true,
			Result: &gateResult{
				Passed:  true,
				Message: meta().Name,
				Details: map[string]any{
					"name":        meta().Name,
					"version":     meta().Version,
					"type":        meta().Type,
					"description": meta().Description,
				},
			},
		}
	default:
		return checkResponse{OK: false, Error: fmt.Sprintf("unknown action %q", req.Action)}
	}
}

// handleInit applies the host-supplied configuration to the plugin.
func handleInit(req *checkRequest, cfg *config) checkResponse {
	if req.Config == nil {
		return checkResponse{OK: true}
	}

	if v, ok := req.Config["timeout"].(string); ok {
		cfg.Timeout = v
	}
	if v, ok := req.Config["retry"]; ok {
		if n, ok := toInt(v); ok {
			cfg.Retry = n
		}
	}
	if v, ok := req.Config["expect_code"]; ok {
		if n, ok := toInt(v); ok {
			cfg.ExpectCode = n
		}
	}
	if v, ok := req.Config["expect_body"].(string); ok {
		cfg.ExpectBody = v
	}

	return checkResponse{OK: true}
}

// handleCheck performs the HTTP probe.
func handleCheck(req *checkRequest, cfg *config) checkResponse {
	url, _ := req.Params["url"].(string)
	if url == "" {
		return checkResponse{OK: false, Error: "missing required param \"url\""}
	}

	timeout, err := time.ParseDuration(cfg.Timeout)
	if err != nil {
		timeout = 10 * time.Second
	}

	expectCode := cfg.ExpectCode
	if v, ok := req.Params["expect_code"]; ok {
		if n, ok := toInt(v); ok {
			expectCode = n
		}
	}

	expectBody := cfg.ExpectBody
	if v, ok := req.Params["expect_body"].(string); ok {
		expectBody = v
	}

	retries := cfg.Retry
	if retries < 0 {
		retries = 0
	}

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	var lastErr error
	var resp *http.Response
	for attempt := 0; attempt <= retries; attempt++ {
		var reqErr error
		resp, reqErr = http.Get(url)
		if reqErr == nil {
			break
		}
		lastErr = reqErr
		if attempt < retries {
			select {
			case <-ctx.Done():
				lastErr = ctx.Err()
				break
			case <-time.After(time.Duration(attempt+1) * time.Second):
			}
		}
	}
	if lastErr != nil {
		return checkResponse{
			OK: true,
			Result: &gateResult{
				Passed:  false,
				Message: fmt.Sprintf("HTTP request failed: %v", lastErr),
				Details: map[string]any{
					"url":     url,
					"attempt": retries + 1,
				},
			},
		}
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)

	passed := resp.StatusCode == expectCode
	if expectBody != "" {
		passed = passed && strings.Contains(string(body), expectBody)
	}

	message := fmt.Sprintf("HTTP %d (expected %d)", resp.StatusCode, expectCode)
	if !passed {
		if resp.StatusCode != expectCode {
			message = fmt.Sprintf("status code %d != expected %d", resp.StatusCode, expectCode)
		} else if expectBody != "" {
			message = fmt.Sprintf("response body does not contain %q", expectBody)
		}
	}

	return checkResponse{
		OK: true,
		Result: &gateResult{
			Passed:  passed,
			Message: message,
			Details: map[string]any{
				"url":         url,
				"status_code": resp.StatusCode,
				"expect_code": expectCode,
				"body_length": len(body),
				"attempt":     retries + 1,
			},
		},
	}
}

// toInt converts a numeric value (from JSON) to an int. JSON numbers
// arrive as float64.
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
