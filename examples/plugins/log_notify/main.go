// Command log_notify is an example LEVEE notify plugin. It appends each
// delivery notification to a local log file, demonstrating the notify plugin
// type — the fourth plugin type alongside channel/gate/module.
//
// Like the other example plugins it uses a JSON-over-stdio protocol so it can
// be built and tested without a gRPC dependency. The host sends one request
// per line; the plugin answers exactly one line per request. For notify, the
// host only inspects the top-level ok/error fields (see internal/plugin/
// notifier.go) — the meta handshake reuses the probe result shape.
package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// --- wire types (mirror the host contract) ---------------------------------

// hostRequest is one line the host writes to stdin (internal/plugin/
// protocol.go:23-32).
type hostRequest struct {
	Action string         `json:"action"`
	Config map[string]any `json:"config,omitempty"`
	Params map[string]any `json:"params,omitempty"`
}

// probeResult is the meta handshake payload (internal/plugin/protocol.go:57-63).
type probeResult struct {
	Passed  bool           `json:"passed"`
	Message string         `json:"message"`
	Details map[string]any `json:"details,omitempty"`
}

// pluginResponse is the single-line reply. For notify/init/close only OK and
// Error matter; Result is used solely by the meta request.
type pluginResponse struct {
	OK     bool         `json:"ok"`
	Error  string       `json:"error,omitempty"`
	Result *probeResult `json:"result,omitempty"`
}

// --- metadata ---------------------------------------------------------------

type pluginMeta struct {
	Name        string `json:"name"`
	Version     string `json:"version"`
	Type        string `json:"type"`
	Description string `json:"description"`
}

func meta() pluginMeta {
	return pluginMeta{
		Name:        "log-notify",
		Version:     "1.0.0",
		Type:        "notify",
		Description: "Example notify plugin that appends delivery notifications to a local log file",
	}
}

// --- configuration ----------------------------------------------------------

type config struct {
	Path   string `json:"path" yaml:"path"`
	Format string `json:"format" yaml:"format"`
}

func defaultConfig() config {
	return config{Path: "/tmp/levee-notify.log", Format: "text"}
}

// --- main loop --------------------------------------------------------------

func main() {
	cfg := defaultConfig()
	if err := serve(os.Stdin, os.Stdout, &cfg); err != nil {
		fmt.Fprintf(os.Stderr, "log-notify: %v\n", err)
		os.Exit(1)
	}
}

// serve reads one JSON request per line and writes exactly one JSON response
// per line until EOF or a "close" action. The host's scanner reads one line
// per request, so emitting anything else (e.g. a log line to stdout) would
// desynchronise the stream.
func serve(r io.Reader, w io.Writer, cfg *config) error {
	scanner := bufio.NewScanner(r)
	// Match the host's 1 MiB line limit (internal/plugin/protocol.go:181-182).
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	encoder := json.NewEncoder(w)

	for scanner.Scan() {
		line := scanner.Bytes()
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var req hostRequest
		if err := json.Unmarshal(line, &req); err != nil {
			_ = encoder.Encode(pluginResponse{OK: false, Error: "parse request: " + err.Error()})
			continue
		}
		resp := handle(&req, cfg)
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

// handle dispatches one request. An unknown action is refused with ok:false —
// a silent ok would make an unsupported operation look compliant.
func handle(req *hostRequest, cfg *config) pluginResponse {
	switch req.Action {
	case "meta":
		m := meta()
		return pluginResponse{
			OK: true,
			Result: &probeResult{
				Passed:  true,
				Message: m.Name,
				Details: map[string]any{
					"name": m.Name, "version": m.Version,
					"type": m.Type, "description": m.Description,
				},
			},
		}
	case "init":
		if req.Config == nil {
			return pluginResponse{OK: true}
		}
		if v, ok := req.Config["path"].(string); ok && v != "" {
			cfg.Path = v
		}
		if v, ok := req.Config["format"].(string); ok && v != "" {
			cfg.Format = v
		}
		return pluginResponse{OK: true}
	case "notify":
		if err := deliver(cfg, req.Params); err != nil {
			return pluginResponse{OK: false, Error: err.Error()}
		}
		return pluginResponse{OK: true}
	case "close":
		return pluginResponse{OK: true}
	default:
		return pluginResponse{OK: false, Error: fmt.Sprintf("unknown action %q", req.Action)}
	}
}

// deliver appends one notification to the configured log file as a single
// line. It creates parent directories on demand. Errors are returned to the
// host as ok:false so delivery failures are visible in the run's evidence.
func deliver(cfg *config, params map[string]any) error {
	if cfg.Path == "" {
		return fmt.Errorf("log path is empty")
	}
	if err := os.MkdirAll(filepath.Dir(cfg.Path), 0o750); err != nil {
		return fmt.Errorf("create log dir: %w", err)
	}
	ts := time.Now().UTC().Format(time.RFC3339)
	event := str(params, "event")
	runID := str(params, "run_id")
	level := str(params, "level")
	title := str(params, "title")
	body := str(params, "body")
	recipients := strList(params["recipients"])

	var line string
	if cfg.Format == "json" {
		obj := map[string]any{
			"ts": ts, "event": event, "run_id": runID, "level": level,
			"title": title, "body": body, "recipients": recipients,
		}
		b, err := json.Marshal(obj)
		if err != nil {
			return fmt.Errorf("marshal log line: %w", err)
		}
		line = string(b)
	} else {
		line = fmt.Sprintf("%s level=%s event=%s run_id=%s title=%s body=%s recipients=%s",
			ts, level, event, runID, title, body, strings.Join(recipients, ","))
	}
	// Collapse embedded newlines so one notification is always one log line.
	line = strings.ReplaceAll(line, "\n", "\\n")

	f, err := os.OpenFile(cfg.Path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("open log: %w", err)
	}
	defer func() { _ = f.Close() }()
	if _, err := fmt.Fprintln(f, line); err != nil {
		return fmt.Errorf("write log: %w", err)
	}
	return nil
}

func str(m map[string]any, key string) string {
	v, _ := m[key].(string)
	return v
}

func strList(v any) []string {
	raw, ok := v.([]any)
	if !ok {
		return []string{}
	}
	out := make([]string, 0, len(raw))
	for _, item := range raw {
		if s, ok := item.(string); ok {
			out = append(out, s)
		}
	}
	return out
}
