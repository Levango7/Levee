// protocol.go is the host side of the JSON-line-over-stdio plugin protocol
// that the shipped example speaks (examples/plugins/http_probe): one JSON
// object per line, request on the plugin's stdin, response on its stdout.
// Two consumers live on it: `levee plugin test` performs a live handshake
// with an installed plugin, and the notifier adapter (notifier.go) delivers
// notification messages — together they are the plugin execution surface
// the registry management commands were built for.
package plugin

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"time"
)

// HostRequest is one line the host writes to the plugin's stdin.
type HostRequest struct {
	// Action selects the operation: "meta" (identity handshake), "init"
	// (configuration), "check" (gate check), "notify" (deliver a
	// notification), "close" (shut down cleanly).
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

// notifyDeadline bounds one notification delivery. It is larger than the
// handshake deadline because delivery may include the plugin's own network
// I/O to its upstream (Slack webhook, SMTP relay, ...); it is still a cap,
// not a target — a notifier that routinely needs this long is misconfigured.
const notifyDeadline = 30 * time.Second

// resolvePluginBinary turns a registry-stored path into something safe to
// execute: absolute, pointing at an existing regular file. Registry rows
// are written by `plugin install` (an operator action), but the host still
// refuses to run anything it cannot positively identify — no PATH lookup,
// no directories.
func resolvePluginBinary(binaryPath string) (string, error) {
	if binaryPath == "" {
		return "", fmt.Errorf("plugin: empty binary path")
	}
	abs, err := filepath.Abs(binaryPath)
	if err != nil {
		return "", fmt.Errorf("plugin: resolve path: %w", err)
	}
	info, err := os.Stat(abs)
	if err != nil {
		return "", fmt.Errorf("plugin: binary not found: %s", binaryPath)
	}
	if info.IsDir() {
		return "", fmt.Errorf("plugin: binary path is a directory: %s", binaryPath)
	}
	return abs, nil
}

// pluginProc is one live plugin sub-process speaking the protocol. It owns
// the process, its pipes and one goroutine that kills the process when the
// deadline context expires.
type pluginProc struct {
	cmd     *exec.Cmd
	stdin   io.WriteCloser
	scanner *bufio.Scanner
	enc     *json.Encoder
	cancel  context.CancelFunc
}

// startPluginProc spawns the plugin binary under the deadline carried by
// parent (the caller supplies the deadline via its own WithTimeout) and
// prepares the line protocol on top of its pipes.
func startPluginProc(ctx context.Context, bin string) (*pluginProc, error) {
	ctx, cancel := context.WithCancel(ctx)

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
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
	}()
	stdin, err := cmd.StdinPipe()
	if err != nil {
		cancel()
		return nil, fmt.Errorf("plugin: stdin pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		cancel()
		return nil, fmt.Errorf("plugin: stdout pipe: %w", err)
	}
	cmd.Stderr = nil

	if err := cmd.Start(); err != nil {
		cancel()
		return nil, fmt.Errorf("plugin: start: %w", err)
	}

	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	return &pluginProc{
		cmd:     cmd,
		stdin:   stdin,
		scanner: scanner,
		enc:     json.NewEncoder(stdin),
		cancel:  cancel,
	}, nil
}

// request writes one request line and reads one response line. Requests are
// small and sequential, so the interleaved write/read cannot deadlock on
// pipe buffers.
func (p *pluginProc) request(req HostRequest) (PluginResponse, error) {
	if err := p.enc.Encode(req); err != nil {
		return PluginResponse{}, fmt.Errorf("plugin: write %s request: %w", req.Action, err)
	}
	if !p.scanner.Scan() {
		err := p.scanner.Err()
		if err == nil {
			err = fmt.Errorf("plugin exited before answering %s", req.Action)
		}
		return PluginResponse{}, err
	}
	line := p.scanner.Bytes()
	var resp PluginResponse
	if err := json.Unmarshal(line, &resp); err != nil {
		return PluginResponse{}, fmt.Errorf("plugin: response is not the JSON-line protocol (got %.80q): %w", string(line), err)
	}
	return resp, nil
}

// close asks the plugin to shut down and waits for the process. Delivery
// paths call it best-effort: the deadline kill is the backstop.
func (p *pluginProc) close() {
	_, _ = p.request(HostRequest{Action: "close"})
	_ = p.stdin.Close()
	_ = p.cmd.Wait()
	p.cancel()
}

// Probe performs one live request/response round against the plugin binary
// and reports the responses. This is deliberately NOT the Sandbox: the
// sandbox assumes a resident gRPC plugin (it discards stdout) and its value
// — crash monitoring, restarts, cgroup limits — is for long-lived
// processes. A handshake is one round trip; a fixed-argv process plus pipes
// plus a deadline is the honest tool for that.
func Probe(ctx context.Context, binaryPath string, requests []HostRequest) ([]PluginResponse, error) {
	bin, err := resolvePluginBinary(binaryPath)
	if err != nil {
		return nil, err
	}

	ctx, cancel := context.WithTimeout(ctx, probeDeadline)
	defer cancel()

	proc, err := startPluginProc(ctx, bin)
	if err != nil {
		return nil, err
	}
	defer proc.close()

	responses := make([]PluginResponse, 0, len(requests))
	for _, req := range requests {
		resp, err := proc.request(req)
		if err != nil {
			return responses, err
		}
		responses = append(responses, resp)
	}
	return responses, nil
}

// ParseConfigYAML parses a plugin config blob (from a registry record's
// config_yaml) into a map. It is exported because the notifier adapter and
// the gate-side resolver both need to hand the stored config to the plugin
// process without depending on manager.go internals.
func ParseConfigYAML(s string) (map[string]any, error) {
	return parseConfigYAML(s)
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
