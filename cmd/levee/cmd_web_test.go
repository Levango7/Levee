package main

import (
	"bytes"
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/nexus/levee/internal/web"
)

// TestNewWebCmd verifies the command wiring.
func TestNewWebCmd(t *testing.T) {
	cmd := newWebCmd()
	if cmd == nil {
		t.Fatal("expected non-nil command")
	}
	if cmd.Use != "web" {
		t.Errorf("expected Use=web, got %q", cmd.Use)
	}
	if cmd.RunE == nil {
		t.Error("expected RunE to be set")
	}
	for _, flag := range []string{"port", "addr", "api", "dev", "dev-server"} {
		if cmd.Flag(flag) == nil {
			t.Errorf("missing --%s flag", flag)
		}
	}
}

// TestResolveWebAddr covers the --addr / --port merge logic.
func TestResolveWebAddr(t *testing.T) {
	cases := []struct {
		name string
		addr string
		port int
		want string
	}{
		{"addr wins", "0.0.0.0:9090", 8080, "0.0.0.0:9090"},
		{"port only", "", 8080, ":8080"},
		{"invalid port", "", 0, ""},
		{"negative port", "", -1, ""},
		{"too large port", "", 70000, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			webOptAddr = c.addr
			webOptPort = c.port
			if got := resolveWebAddr(); got != c.want {
				t.Errorf("resolveWebAddr() = %q, want %q", got, c.want)
			}
		})
	}
}

// TestWebCmd_HelpOutput verifies that the help text mentions the key flags.
func TestWebCmd_HelpOutput(t *testing.T) {
	cmd := newWebCmd()
	buf := &bytes.Buffer{}
	cmd.SetOut(buf)
	cmd.SetErr(buf)
	_ = cmd.Help()
	out := buf.String()
	for _, want := range []string{"--port", "--addr", "--api", "--dev", "--dev-server"} {
		if !strings.Contains(out, want) {
			t.Errorf("help output missing %q", want)
		}
	}
}

// TestWebCmd_ServesSpa is a smoke test that runs the WebUIServer on a free
// port and verifies the embedded placeholder shell is served. It exercises
// the same configuration path the cobra command would use.
func TestWebCmd_ServesSpa(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()

	cfg := web.ServerConfig{Addr: addr}
	srv, err := web.NewServer(cfg)
	if err != nil {
		t.Fatalf("new server: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- srv.Start(ctx) }()

	time.Sleep(100 * time.Millisecond)
	resp, err := http.Get("http://" + addr + "/")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("expected 200, got %d", resp.StatusCode)
	}
	cancel()
	<-done
}

// TestWebCmd_DevMode exercises the dev-mode proxy path through the command
// configuration.
func TestWebCmd_DevMode(t *testing.T) {
	dev := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("vite"))
	}))
	defer dev.Close()

	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	addr := ln.Addr().String()
	_ = ln.Close()

	cfg := web.ServerConfig{Addr: addr, DevMode: true, DevServerURL: dev.URL}
	srv, err := web.NewServer(cfg)
	if err != nil {
		t.Fatalf("new server: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- srv.Start(ctx) }()

	time.Sleep(100 * time.Millisecond)
	resp, err := http.Get("http://" + addr + "/")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()
	if !strings.Contains(resp.Status, "200") {
		t.Errorf("expected 200, got %s", resp.Status)
	}
	cancel()
	<-done
}
