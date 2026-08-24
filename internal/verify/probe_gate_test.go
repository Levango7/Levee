package verify

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// --- tests: construction and strict params ---------------------------------

func TestProbeGateImplementsGateInterface(t *testing.T) {
	var _ Gate = (*ProbeGate)(nil)

	g := NewProbeGate("health", PhasePostApply, map[string]any{"kind": "http", "url": "http://localhost/"})
	assert.Equal(t, "health", g.Name())
	assert.Equal(t, PhasePostApply, g.Phase())
	require.NoError(t, g.ParamsError())
}

func TestProbeGateUnknownParamFailsClosed(t *testing.T) {
	g := NewProbeGate("bad", PhasePostBatch, map[string]any{
		"kind":  "http",
		"url":   "http://localhost/",
		"karma": 9000, // not a valid key
	})

	res, err := g.Check(context.Background(), GateInput{RunID: "run-1"})
	require.Error(t, err)
	assert.False(t, res.Passed)
	assert.Contains(t, err.Error(), `"karma"`)
	for _, k := range validProbeParamKeys {
		assert.Contains(t, err.Error(), k, "error must list every valid key")
	}
}

func TestProbeGateMissingKindFailsClosed(t *testing.T) {
	g := NewProbeGate("kindless", PhasePreApply, map[string]any{"url": "http://localhost/"})
	res, err := g.Check(context.Background(), GateInput{})
	require.Error(t, err)
	assert.False(t, res.Passed)
	assert.Contains(t, err.Error(), `"kind" is required`)
}

func TestProbeGateContradictoryParamsFailClosed(t *testing.T) {
	cases := []struct {
		name   string
		params map[string]any
		want   string
	}{
		{"tcp without address", map[string]any{"kind": "tcp"}, `host_port`},
		{"url on tcp", map[string]any{"kind": "tcp", "host_port": "h:1", "url": "http://x/"}, `"url" applies to kind=http only`},
		{"body_regex remote", map[string]any{"kind": "http", "url": "http://x/", "mode": "remote", "body_regex": "ok"}, "mode=direct only"},
		{"script missing body", map[string]any{"kind": "script"}, `"script" is required`},
		{"bad kind", map[string]any{"kind": "icmp"}, "http|tcp|script"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			g := NewProbeGate("cfg", PhasePreApply, c.params)
			res, err := g.Check(context.Background(), GateInput{})
			require.Error(t, err)
			assert.False(t, res.Passed)
			assert.Contains(t, err.Error(), c.want)
		})
	}
}

// --- tests: http direct ----------------------------------------------------

func TestProbeGateHTTPDirectPass(t *testing.T) {
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("all good"))
	}))
	defer srv.Close()

	g := NewProbeGate("http-ok", PhasePostApply, map[string]any{
		"kind":          "http",
		"url":           srv.URL,
		"body_contains": "good",
	})
	res, err := g.Check(context.Background(), GateInput{RunID: "run-1"})
	require.NoError(t, err)
	assert.True(t, res.Passed, "message: %s", res.Message)
	assert.Equal(t, int64(1), hits.Load())
	assert.Positive(t, res.Latency)
}

func TestProbeGateHTTPStatusMismatch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	g := NewProbeGate("status", PhasePostApply, map[string]any{
		"kind":          "http",
		"url":           srv.URL,
		"expect_status": "404", // single code: server answers 200 => fail
	})
	res, err := g.Check(context.Background(), GateInput{})
	require.NoError(t, err)
	assert.False(t, res.Passed)
	assert.Contains(t, res.Message, "outside expected range")
}

func TestProbeGateHTTPBodyRegexMismatch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("version=1.2.3"))
	}))
	defer srv.Close()

	g := NewProbeGate("regex", PhasePostApply, map[string]any{
		"kind":       "http",
		"url":        srv.URL,
		"body_regex": `^version=2\.`, // does not match
	})
	res, err := g.Check(context.Background(), GateInput{})
	require.NoError(t, err)
	assert.False(t, res.Passed)
	assert.Contains(t, res.Message, "does not match regex")
}

func TestProbeGateHTTPMultiTargetExpansion(t *testing.T) {
	// The handler streams every requested path to the test so it can prove
	// that the "{target}" placeholder was expanded once per target. Target
	// "b" serves an error to prove that one failing target fails the whole
	// gate while naming it.
	pathCh := make(chan string, 4)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		pathCh <- r.URL.Path
		if strings.Contains(r.URL.Path, "/b") {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	g := NewProbeGate("multi", PhasePostBatch, map[string]any{
		"kind": "http",
		"url":  srv.URL + "/health/{target}",
	})
	res, err := g.Check(context.Background(), GateInput{RunID: "run-1", TargetIDs: []string{"a", "b"}})
	require.NoError(t, err)
	assert.False(t, res.Passed)
	assert.Contains(t, res.Message, `target "b"`)

	var got []string
	for i := 0; i < 2; i++ {
		select {
		case p := <-pathCh:
			got = append(got, p)
		case <-time.After(2 * time.Second):
			t.Fatal("timed out waiting for probed paths")
		}
	}
	assert.ElementsMatch(t, []string{"/health/a", "/health/b"}, got, "every target must be probed exactly once")
}

func TestProbeGateHTTPEmptyTargetsSingleRequest(t *testing.T) {
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	// expect_status as a bare integer must also parse.
	g := NewProbeGate("single", PhasePreApply, map[string]any{
		"kind":          "http",
		"url":           srv.URL + "/{target}",
		"expect_status": 204,
	})
	res, err := g.Check(context.Background(), GateInput{})
	require.NoError(t, err)
	assert.True(t, res.Passed, "message: %s", res.Message)
	assert.Equal(t, int64(1), hits.Load())
}

// --- tests: tcp ------------------------------------------------------------

func TestProbeGateTCPDirect(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer ln.Close()
	open := ln.Addr().String()
	closed := "127.0.0.1:1" // port 1 is not ours; nothing listens there

	pass := NewProbeGate("tcp-open", PhasePostBatch, map[string]any{
		"kind":      "tcp",
		"host_port": open,
	})
	res, err := pass.Check(context.Background(), GateInput{})
	require.NoError(t, err)
	assert.True(t, res.Passed, "message: %s", res.Message)

	failG := NewProbeGate("tcp-closed", PhasePostBatch, map[string]any{
		"kind":            "tcp",
		"host_port":       closed,
		"timeout_seconds": 1,
	})
	res, err = failG.Check(context.Background(), GateInput{})
	require.NoError(t, err)
	assert.False(t, res.Passed)
	assert.Contains(t, res.Message, closed)
}

func TestProbeGateTCPPortFromTarget(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer ln.Close()
	addr := ln.Addr().String() // "127.0.0.1:<port>"

	g := NewProbeGate("pft", PhasePostBatch, map[string]any{
		"kind":             "tcp",
		"port_from_target": true,
	})
	res, err := g.Check(context.Background(), GateInput{TargetIDs: []string{addr}})
	require.NoError(t, err)
	assert.True(t, res.Passed, "message: %s", res.Message)

	// A target without :port must fail the gate naming the offender.
	res, err = g.Check(context.Background(), GateInput{TargetIDs: []string{"host-without-port"}})
	require.NoError(t, err)
	assert.False(t, res.Passed)
	assert.Contains(t, res.Message, "host-without-port")
}

func TestProbeGateTCPRemote(t *testing.T) {
	t.Run("pass", func(t *testing.T) {
		ch := newFakeChannel(execResult(0, ""))
		g := NewProbeGate("tcp-remote", PhasePostBatch, map[string]any{
			"kind":      "tcp",
			"mode":      "remote",
			"host_port": "10.0.0.9:6379",
		})
		res, err := g.Check(context.Background(), GateInput{RunID: "run-1", Channel: ch})
		require.NoError(t, err)
		assert.True(t, res.Passed, "message: %s", res.Message)
		cmds := ch.commandsCopy()
		require.Len(t, cmds, 1)
		assert.Contains(t, cmds[0], "/dev/tcp/10.0.0.9:6379")
		assert.Contains(t, cmds[0], "bash -c")
	})

	t.Run("connect refused", func(t *testing.T) {
		ch := newFakeChannel(execResult(1, ""))
		g := NewProbeGate("tcp-remote-fail", PhasePostBatch, map[string]any{
			"kind":      "tcp",
			"mode":      "remote",
			"host_port": "10.0.0.9:6379",
		})
		res, err := g.Check(context.Background(), GateInput{Channel: ch})
		require.NoError(t, err)
		assert.False(t, res.Passed)
		assert.Contains(t, res.Message, "remote tcp connect")
	})

	t.Run("missing channel fails closed", func(t *testing.T) {
		g := NewProbeGate("tcp-nochan", PhasePostBatch, map[string]any{
			"kind":      "tcp",
			"mode":      "remote",
			"host_port": "10.0.0.9:6379",
		})
		res, err := g.Check(context.Background(), GateInput{})
		require.Error(t, err)
		assert.False(t, res.Passed)
		assert.Contains(t, res.Message, "has no channel")
	})
}

// --- tests: http remote ----------------------------------------------------

func TestProbeGateHTTPRemote(t *testing.T) {
	t.Run("2xx passes", func(t *testing.T) {
		ch := newFakeChannel(execResult(0, "200"))
		g := NewProbeGate("http-remote", PhasePostBatch, map[string]any{
			"kind": "http",
			"mode": "remote",
			"url":  "http://localhost/health",
		})
		res, err := g.Check(context.Background(), GateInput{Channel: ch})
		require.NoError(t, err)
		assert.True(t, res.Passed, "message: %s", res.Message)
		assert.Contains(t, ch.commandsCopy()[0], "curl -fsS -o /dev/null -w '%{http_code}' 'http://localhost/health'")
	})

	t.Run("non-2xx status fails", func(t *testing.T) {
		ch := newFakeChannel(execResult(0, "503"))
		g := NewProbeGate("http-remote-5xx", PhasePostBatch, map[string]any{
			"kind": "http",
			"mode": "remote",
			"url":  "http://localhost/health",
		})
		res, err := g.Check(context.Background(), GateInput{Channel: ch})
		require.NoError(t, err)
		assert.False(t, res.Passed)
		assert.Contains(t, res.Message, "non-2xx")
	})

	t.Run("curl exit non-zero fails", func(t *testing.T) {
		ch := newFakeChannel(execResult(7, ""))
		g := NewProbeGate("http-remote-refused", PhasePostBatch, map[string]any{
			"kind": "http",
			"mode": "remote",
			"url":  "http://localhost/health",
		})
		res, err := g.Check(context.Background(), GateInput{Channel: ch})
		require.NoError(t, err)
		assert.False(t, res.Passed)
		assert.Contains(t, res.Message, "curl failed")
	})
}

// --- tests: script ---------------------------------------------------------

func TestProbeGateScript(t *testing.T) {
	t.Run("exit zero passes by default", func(t *testing.T) {
		ch := newFakeChannel(execResult(0, "ready\n"))
		g := NewProbeGate("script-ok", PhasePostApply, map[string]any{
			"kind":   "script",
			"script": "echo ready\n",
		})
		res, err := g.Check(context.Background(), GateInput{RunID: "run-1", Channel: ch})
		require.NoError(t, err)
		assert.True(t, res.Passed, "message: %s", res.Message)

		cmd := ch.commandsCopy()[0]
		assert.True(t, strings.HasPrefix(cmd, "sh /tmp/.levee-probe-"), "cmd: %s", cmd)
		assert.Contains(t, cmd, "; rc=$?; rm -f ")
		assert.True(t, strings.HasSuffix(cmd, "; exit $rc"), "cleanup+rc propagation expected, cmd: %s", cmd)
	})

	t.Run("expected exit honoured", func(t *testing.T) {
		ch := newFakeChannel(execResult(3, ""))
		g := NewProbeGate("script-3", PhasePostApply, map[string]any{
			"kind":        "script",
			"script":      "exit 3\n",
			"expect_exit": 3,
		})
		res, err := g.Check(context.Background(), GateInput{Channel: ch})
		require.NoError(t, err)
		assert.True(t, res.Passed, "message: %s", res.Message)
	})

	t.Run("exit mismatch fails", func(t *testing.T) {
		ch := newFakeChannel(execResult(1, ""))
		g := NewProbeGate("script-bad", PhasePostApply, map[string]any{
			"kind":   "script",
			"script": "false\n",
		})
		res, err := g.Check(context.Background(), GateInput{Channel: ch})
		require.NoError(t, err)
		assert.False(t, res.Passed)
		assert.Contains(t, res.Message, "exited 1, want 0")
	})

	t.Run("missing channel fails closed", func(t *testing.T) {
		g := NewProbeGate("script-nochan", PhasePostApply, map[string]any{
			"kind":   "script",
			"script": "true\n",
		})
		res, err := g.Check(context.Background(), GateInput{})
		require.Error(t, err)
		assert.False(t, res.Passed)
		assert.Contains(t, res.Message, "has no channel")
	})
}

// commandsCopy returns a snapshot of the recorded command lines.
func (c *fakeChannel) commandsCopy() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]string, len(c.commands))
	copy(out, c.commands)
	return out
}

// --- tests: manager integration ---------------------------------------------

func TestProbeGateRegisteredWithManager(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintln(w, "ok")
	}))
	defer srv.Close()

	gm := NewGateManager()
	gm.Register(NewProbeGate("web-health", PhasePostBatch, map[string]any{
		"kind": "http",
		"url":  srv.URL,
	}))

	results := gm.RunPhase(context.Background(), PhasePostBatch, GateInput{BatchID: "b1"})
	require.Len(t, results, 1)
	assert.True(t, results[0].Passed)
}
