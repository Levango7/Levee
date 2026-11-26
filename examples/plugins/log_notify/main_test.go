package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestServeMetaInitNotifyClose(t *testing.T) {
	cfg := defaultConfig()
	in := strings.Join([]string{
		`{"action":"meta"}`,
		`{"action":"init","config":{"path":"/tmp/lv-test-notify.log","format":"json"}}`,
		`{"action":"notify","params":{"event":"run_completed","run_id":"run-1","level":"info","title":"t","body":"b","recipients":["ops"]}}`,
		`{"action":"close"}`,
	}, "\n") + "\n"
	var out bytes.Buffer
	if err := serve(strings.NewReader(in), &out, &cfg); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 4 {
		t.Fatalf("want 4 responses, got %d: %q", len(lines), out.String())
	}
	if !strings.Contains(lines[0], `"type":"notify"`) {
		t.Errorf("meta must report type notify: %s", lines[0])
	}
	if !strings.Contains(lines[1], `"ok":true`) {
		t.Errorf("init ok: %s", lines[1])
	}
	if !strings.Contains(lines[2], `"ok":true`) {
		t.Errorf("notify ok: %s", lines[2])
	}
	if cfg.Path != "/tmp/lv-test-notify.log" || cfg.Format != "json" {
		t.Errorf("init did not apply config: %+v", cfg)
	}
}

func TestServeRefusesUnknownAction(t *testing.T) {
	var out bytes.Buffer
	in := "{\"action\":\"bogus\"}\n{\"action\":\"close\"}\n"
	err := serve(strings.NewReader(in), &out, ptr(defaultConfig()))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), `"ok":false`) {
		t.Errorf("unknown action must be refused: %s", out.String())
	}
}

func ptr(c config) *config { return &c }
