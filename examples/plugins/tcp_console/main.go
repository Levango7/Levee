// Command tcp-console is an example CHANNEL plugin for LEVEE.
//
// It speaks the host's JSON-line protocol on stdin/stdout and drives a
// line-oriented TCP console — the shape of a device, a serial bridge or a
// terminal server: connect once, then send command lines and read replies
// until a terminator appears.
//
// A channel plugin is the one consumption point that is a SESSION, not a
// one-shot: the host starts this process at Connect, sends many exec requests
// over the same pipes, and tears it down at Close. Keep the connection in a
// variable — that is the whole reason the process stays alive.
//
// Verbs:
//
//	meta       — identity (type: channel)
//	init       — the config.yaml captured at install time
//	connect    — params: host / port / type / credential_ref (NEVER plaintext
//	             credentials: authenticate from your own config)
//	exec       — params: cmd; answer with the `exec` payload
//	             {exit_code, stdout, stderr, duration_ms}
//	collect    — return bytes as `collect: {data: <base64>}`; this example
//	             refuses it, which is the honest answer for a transport that
//	             has no file semantics
//	upload     — params: path / data (base64, ≤512 KiB per frame); this
//	             example refuses it for the same reason
//	disconnect — close the session cleanly
//	close      — exit the process
//
// Install and try it against any line server — README.md has a five-line
// Python console that answers every line with "OK <line>":
//
//	go build -o examples/plugins/tcp_console/tcp-console ./examples/plugins/tcp_console
//	levee plugin install ./examples/plugins/tcp_console
//	levee plugin enable tcp-console
//	levee target add console-1 --channel-type tcp-console --host 127.0.0.1 --port 9999
//	# then a workflow step: module: shell, action: exec, args: {cmd: "show version"}
package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
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

type channelExecResult struct {
	ExitCode int    `json:"exit_code"`
	Stdout   string `json:"stdout"`
	Stderr   string `json:"stderr"`
	Duration int64  `json:"duration_ms"`
}

type probeResult struct {
	Passed  bool           `json:"passed"`
	Message string         `json:"message"`
	Details map[string]any `json:"details,omitempty"`
}

type pluginResponse struct {
	OK     bool               `json:"ok"`
	Error  string             `json:"error,omitempty"`
	Result *probeResult       `json:"result,omitempty"` // meta
	Exec   *channelExecResult `json:"exec,omitempty"`   // exec
}

// --- plugin state -----------------------------------------------------------

type plugin struct {
	connectTimeout time.Duration
	readTimeout    time.Duration
	terminator     string

	conn   net.Conn
	reader *bufio.Reader
}

func newPlugin() *plugin {
	return &plugin{
		connectTimeout: 3 * time.Second,
		readTimeout:    5 * time.Second,
		terminator:     "OK",
	}
}

// --- main ------------------------------------------------------------------

func main() {
	p := newPlugin()
	if err := serve(os.Stdin, os.Stdout, p); err != nil {
		fmt.Fprintf(os.Stderr, "tcp-console: %v\n", err)
		os.Exit(1)
	}
}

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

func (p *plugin) handle(req *hostRequest) pluginResponse {
	switch req.Action {
	case "meta":
		return pluginResponse{OK: true, Result: &probeResult{
			Passed:  true,
			Message: "tcp-console",
			Details: map[string]any{"name": "tcp-console", "version": "1.0.0", "type": "channel"},
		}}
	case "init":
		applyConfig(p, req.Config)
		return pluginResponse{OK: true}
	case "connect":
		return p.connect(req.Params)
	case "exec":
		return p.exec(req.Params)
	case "collect":
		// Honest refusal: this transport has no file semantics. The host
		// surfaces it as the step error; it does not silently return empty.
		return pluginResponse{OK: false, Error: "tcp-console has no file semantics; collect is not supported"}
	case "upload":
		// Same refusal, other direction. A transport that COULD write files
		// would decode params.data (base64) and write it to params.path —
		// within the 512 KiB frame the host enforces.
		return pluginResponse{OK: false, Error: "tcp-console has no file semantics; upload is not supported"}
	case "disconnect":
		p.disconnect()
		return pluginResponse{OK: true}
	case "close":
		p.disconnect()
		return pluginResponse{OK: true}
	default:
		return pluginResponse{OK: false, Error: fmt.Sprintf("unknown action %q", req.Action)}
	}
}

func applyConfig(p *plugin, cfg map[string]any) {
	if v, ok := toInt(cfg["connect_timeout_ms"]); ok && v > 0 {
		p.connectTimeout = time.Duration(v) * time.Millisecond
	}
	if v, ok := toInt(cfg["read_timeout_ms"]); ok && v > 0 {
		p.readTimeout = time.Duration(v) * time.Millisecond
	}
	if v, ok := cfg["terminator"].(string); ok && v != "" {
		p.terminator = v
	}
}

// connect dials the console. The host hands over host/port/type and — when the
// inventory row names one — credential_ref; plaintext credentials never cross
// the pipe, so authentication (a login line, a token header) belongs in
// config.yaml and is performed here.
func (p *plugin) connect(params map[string]any) pluginResponse {
	if p.conn != nil {
		return pluginResponse{OK: true} // idempotent by contract
	}
	host, _ := params["host"].(string)
	if host == "" {
		return pluginResponse{OK: false, Error: "connect requires params.host"}
	}
	port, _ := toInt(params["port"])
	if port == 0 {
		port = 23 // telnet's default: console servers almost always speak it
	}

	conn, err := net.DialTimeout("tcp", fmt.Sprintf("%s:%d", host, port), p.connectTimeout)
	if err != nil {
		return pluginResponse{OK: false, Error: fmt.Sprintf("dial %s:%d: %v", host, port, err)}
	}
	p.conn = conn
	p.reader = bufio.NewReader(conn)
	return pluginResponse{OK: true}
}

// exec writes one command line and reads until the terminator appears (or the
// read deadline fires). The exit code distinguishes "the console answered" (0)
// from "the console did not answer in time" (1) — a transport failure is
// reported by returning ok:false instead.
func (p *plugin) exec(params map[string]any) pluginResponse {
	if p.conn == nil {
		return pluginResponse{OK: false, Error: "exec before connect"}
	}
	cmd, _ := params["cmd"].(string)
	if strings.TrimSpace(cmd) == "" {
		return pluginResponse{OK: false, Error: "exec requires params.cmd"}
	}

	start := time.Now()
	if err := p.conn.SetWriteDeadline(time.Now().Add(p.connectTimeout)); err != nil {
		return pluginResponse{OK: false, Error: fmt.Sprintf("set write deadline: %v", err)}
	}
	if _, err := io.WriteString(p.conn, cmd+"\n"); err != nil {
		return pluginResponse{OK: false, Error: fmt.Sprintf("write: %v", err)}
	}

	if err := p.conn.SetReadDeadline(time.Now().Add(p.readTimeout)); err != nil {
		return pluginResponse{OK: false, Error: fmt.Sprintf("set read deadline: %v", err)}
	}
	var out strings.Builder
	for {
		line, err := p.reader.ReadString('\n')
		out.WriteString(line)
		if strings.Contains(line, p.terminator) {
			return pluginResponse{OK: true, Exec: &channelExecResult{
				ExitCode: 0, Stdout: out.String(), Duration: time.Since(start).Milliseconds(),
			}}
		}
		if err != nil {
			if ne, ok := err.(net.Error); ok && ne.Timeout() {
				return pluginResponse{OK: true, Exec: &channelExecResult{
					ExitCode: 1, Stdout: out.String(),
					Stderr:   fmt.Sprintf("no %q terminator within %s", p.terminator, p.readTimeout),
					Duration: time.Since(start).Milliseconds(),
				}}
			}
			return pluginResponse{OK: false, Error: fmt.Sprintf("read: %v", err)}
		}
	}
}

func (p *plugin) disconnect() {
	if p.conn != nil {
		_ = p.conn.Close()
		p.conn = nil
		p.reader = nil
	}
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
