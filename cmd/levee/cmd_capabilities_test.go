package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nexus/levee/internal/channel"
	"github.com/nexus/levee/internal/targetcap"
)

type capabilitiesChannel struct {
	responses map[string]channel.ExecResult
	closed    bool
}

func (c *capabilitiesChannel) Connect(context.Context) error                   { return nil }
func (c *capabilitiesChannel) IsConnected() bool                               { return !c.closed }
func (c *capabilitiesChannel) Close() error                                    { c.closed = true; return nil }
func (c *capabilitiesChannel) Upload(context.Context, string, io.Reader) error { return nil }
func (c *capabilitiesChannel) Download(context.Context, string) (io.Reader, error) {
	return nil, io.EOF
}
func (c *capabilitiesChannel) Exec(_ context.Context, command string) (*channel.ExecResult, error) {
	if response, ok := c.responses[command]; ok {
		result := response
		return &result, nil
	}
	return &channel.ExecResult{ExitCode: 1}, nil
}

type fakeCapabilityDialer struct {
	channel *capabilitiesChannel
	host    string
	err     error
}

func (d *fakeCapabilityDialer) Dial(_ context.Context, host string) (channel.Channel, error) {
	d.host = host
	if d.err != nil {
		return nil, d.err
	}
	return d.channel, nil
}

func TestRunCapabilitiesWithEngineEmitsVersionedJSON(t *testing.T) {
	ch := &capabilitiesChannel{responses: map[string]channel.ExecResult{
		"uname -s":            {ExitCode: 0, Stdout: "Linux\n"},
		"cat /etc/os-release": {ExitCode: 0, Stdout: "ID=debian\nVERSION_ID=13\n"},
		"which apt":           {ExitCode: 0},
		"which systemctl":     {ExitCode: 0},
	}}
	dialer := &fakeCapabilityDialer{channel: ch}
	oldJSON, oldQuiet := optJSON, optQuiet
	optJSON, optQuiet = true, false
	defer func() { optJSON, optQuiet = oldJSON, oldQuiet }()

	var out strings.Builder
	require.NoError(t, runCapabilitiesWithEngine(context.Background(), dialer, "debian-1", &out))
	assert.Equal(t, "debian-1", dialer.host)
	assert.True(t, ch.closed)
	var envelope struct {
		Data targetcap.Report `json:"data"`
	}
	require.NoError(t, json.Unmarshal([]byte(out.String()), &envelope))
	assert.Equal(t, targetcap.SchemaVersion, envelope.Data.SchemaVersion)
	assert.Equal(t, "apt", envelope.Data.PackageManager)
	assert.Equal(t, "systemd", envelope.Data.InitSystem)
	assert.Equal(t, 9, envelope.Data.Summary.Supported)
	assert.Equal(t, 1, envelope.Data.Summary.Unknown)
	assert.Len(t, envelope.Data.Actions, 29)
}

func TestRunCapabilitiesHumanAndQuietOutput(t *testing.T) {
	ch := &capabilitiesChannel{responses: map[string]channel.ExecResult{
		"which apt":       {ExitCode: 0},
		"which systemctl": {ExitCode: 0},
	}}
	dialer := &fakeCapabilityDialer{channel: ch}
	oldJSON, oldQuiet := optJSON, optQuiet
	optJSON, optQuiet = false, false
	defer func() { optJSON, optQuiet = oldJSON, oldQuiet }()
	var human strings.Builder
	require.NoError(t, runCapabilitiesWithEngine(context.Background(), dialer, "debian-1", &human))
	assert.Contains(t, human.String(), "apt")
	assert.Contains(t, human.String(), "systemd")
	assert.Contains(t, human.String(), "not an acceptance verdict")

	optQuiet = true
	var quiet strings.Builder
	require.NoError(t, runCapabilitiesWithEngine(context.Background(), dialer, "debian-1", &quiet))
	assert.Contains(t, quiet.String(), "9/29 supported; 1 unknown")
}

func TestRunCapabilitiesRejectsEmptyTargetBeforeDial(t *testing.T) {
	dialer := &fakeCapabilityDialer{channel: &capabilitiesChannel{}}
	err := runCapabilitiesWithEngine(context.Background(), dialer, "", io.Discard)
	require.Error(t, err)
	assert.Empty(t, dialer.host)
}

func TestCapabilitiesCommandShape(t *testing.T) {
	cmd := newCapabilitiesCmd()
	assert.Equal(t, "capabilities [host]", cmd.Use)
	assert.NotNil(t, cmd.Flags().Lookup("target"))
	assert.NoError(t, cmd.Args(cmd, []string{"one"}))
	assert.Error(t, cmd.Args(cmd, []string{"one", "two"}))
}

func TestTargetCheckUsesRealChannel(t *testing.T) {
	ch := &capabilitiesChannel{responses: map[string]channel.ExecResult{
		"echo ok": {ExitCode: 0, Stdout: "ok\n"},
	}}
	dialer := &fakeCapabilityDialer{channel: ch}
	oldJSON, oldQuiet := optJSON, optQuiet
	optJSON, optQuiet = true, false
	defer func() { optJSON, optQuiet = oldJSON, oldQuiet }()
	var out strings.Builder
	require.NoError(t, runTargetCheckWithEngine(context.Background(), dialer, "web-1", &out))
	assert.True(t, ch.closed)
	var envelope struct {
		Data struct {
			Reachable bool `json:"reachable"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal([]byte(out.String()), &envelope))
	assert.True(t, envelope.Data.Reachable)
}

func TestTargetCheckReportsDialFailure(t *testing.T) {
	dialer := &fakeCapabilityDialer{channel: &capabilitiesChannel{}, err: errors.New("connection refused")}
	oldJSON, oldQuiet := optJSON, optQuiet
	optJSON, optQuiet = true, false
	defer func() { optJSON, optQuiet = oldJSON, oldQuiet }()
	var out strings.Builder
	require.NoError(t, runTargetCheckWithEngine(context.Background(), dialer, "web-1", &out))
	var envelope struct {
		Data struct {
			Reachable bool   `json:"reachable"`
			Error     string `json:"error"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal([]byte(out.String()), &envelope))
	assert.False(t, envelope.Data.Reachable)
	assert.Contains(t, envelope.Data.Error, "connection refused")
}
