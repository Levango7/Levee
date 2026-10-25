// protocol.go is the host side of the JSON-line-over-stdio plugin protocol
// that the shipped example speaks (examples/plugins/http_probe): one JSON
// object per line, request on the plugin's stdin, response on its stdout.
// It exists so `levee plugin test` can perform a LIVE handshake with an
// installed plugin — proving the install→enable→execute chain end to end —
// while the engine-side consumption points (gate/notifier) remain unbuilt.
package plugin

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"
)

// HostRequest is one line the host writes to the plugin's stdin.
type HostRequest struct {
	// Action selects the operation: "meta" (identity handshake), "init"
	// (configuration), "check" (gate check), "close" (shut down cleanly).
	Action string         `json:"action"`
	Config map[string]any `json:"config,omitempty"`
	Params map[string]any `json:"params,omitempty"`
}

// PluginResponse is one line the plugin writes back on stdout.
type PluginResponse struct {
	OK     bool         `json:"ok"`
	Error  string       `json:"error,omitempty"`
	Result *ProbeResult `json:"result,omitempty"`
}

// ProbeResult carries what the example protocol returns for "meta" (plugin
// identity in Details) and for "check" (the gate verdict).
type ProbeResult struct {
	Passed  bool           `json:"passed"`
	Message string         `json:"message"`
	Details map[string]any `json:"details,omitempty"`
}

// probeDeadline bounds one handshake: a plugin that neither answers nor
// exits within it is killed and reported as unresponsive, not hung.
const probeDeadline = 10 * time.Second

// resolvePluginBinary turns a registry-stored path into something safe to
// execute: absolute, pointing at an existing regular file. Registry rows
// are written by `plugin install` (an operator action), but the probe still
// refuses to run anything it cannot positively identify — no PATH lookup,
// no directories.
func resolvePluginBinary(binaryPath string) (string, error) {
	if binaryPath == "" {
		return "", fmt.Errorf("plugin: probe: empty binary path")
	}
	abs, err := filepath.Abs(binaryPath)
	if err != nil {
		return "", fmt.Errorf("plugin: probe: resolve path: %w", err)
	}
	info, err := os.Stat(abs)
	if err != nil {
		return "", fmt.Errorf("plugin: probe: binary not found: %s", binaryPath)
	}
	if info.IsDir() {
		return "", fmt.Errorf("plugin: probe: binary path is a directory: %s", binaryPath)
	}
	return abs, nil
}

// Probe performs one live request/response round against the plugin binary:
// it sends each request as a JSON line, reads one response line per request,
// then closes stdin. The plugin is expected to exit on "close"; Probe waits
// for that exit but enforces the deadline with a kill.
//
// This is deliberately NOT the Sandbox: the sandbox assumes a resident
// gRPC plugin (it discards stdout) and its value — crash monitoring,
// restarts, cgroup limits — is for long-lived processes. A handshake is one
// round trip; a fixed-argv process plus pipes plus a deadline is the honest
// tool for that.
func Probe(ctx context.Context, binaryPath string, requests []HostRequest) ([]PluginResponse, error) {
	bin, err := resolvePluginBinary(binaryPath)
	if err != nil {
		return nil, err
	}

	ctx, cancel := context.WithTimeout(ctx, probeDeadline)
	defer cancel()

	// Fixed argv, no shell: the verified absolute path is argv[0] and the
	// only element. The plugin receives its instructions on stdin (the
	// protocol), never on the command line, so nothing variable reaches
	// the invocation.
	cmd := &exec.Cmd{
		Path: bin,
		Args: []string{bin},
	}
	// A hand-built Cmd carries no context: enforce the deadline by killing
	// the process ourselves when it expires.
	go func() {
		<-ctx.Done()
		_ = cmd.Process.Kill()
	}()
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("plugin: probe: stdin pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("plugin: probe: stdout pipe: %w", err)
	}
	cmd.Stderr = nil

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("plugin: probe: start: %w", err)
	}

	// Write every request up front, then close stdin: the example protocol
	// processes lines sequentially and exits on "close", so the responses
	// drain from the pipe without interleaving writes and reads.
	writeErr := make(chan error, 1)
	go func() {
		defer func() { _ = stdin.Close() }()
		enc := json.NewEncoder(stdin)
		for _, req := range requests {
			if err := enc.Encode(req); err != nil {
				writeErr <- fmt.Errorf("plugin: probe: write request: %w", err)
				return
			}
		}
		writeErr <- nil
	}()

	responses := make([]PluginResponse, 0, len(requests))
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() && len(responses) < len(requests) {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var resp PluginResponse
		if err := json.Unmarshal(line, &resp); err != nil {
			_ = cmd.Wait()
			return nil, fmt.Errorf("plugin: probe: response is not the JSON-line protocol (got %.80q): %w", string(line), err)
		}
		responses = append(responses, resp)
	}

	// The plugin should have exited on "close"; wait for it. A deadline
	// kill surfaces as a non-nil wait error, which we prefer to report as
	// unresponsive rather than as success-with-nothing.
	waitErr := cmd.Wait()
	if werr := <-writeErr; werr != nil {
		return responses, werr
	}
	if ctx.Err() != nil {
		return responses, fmt.Errorf("plugin: probe: plugin did not answer within %s", probeDeadline)
	}
	if waitErr != nil {
		return responses, fmt.Errorf("plugin: probe: plugin exit: %w", waitErr)
	}
	return responses, nil
}

// MetaHandshake is the request set `levee plugin test` sends: identify
// yourself, then shut down. Two responses are expected — the identity and
// the close acknowledgement.
func MetaHandshake() []HostRequest {
	return []HostRequest{
		{Action: "meta"},
		{Action: "close"},
	}
}
