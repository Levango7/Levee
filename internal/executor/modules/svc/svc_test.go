package svc

import (
	"context"
	"errors"
	"io"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nexus/levee/internal/channel"
	"github.com/nexus/levee/internal/executor"
)

// --- mock channel ----------------------------------------------------------

type mockChannel struct {
	mu            sync.Mutex
	execs         []string
	execResponses []execResponse
	execResult    *channel.ExecResult
	execErr       error
	connected     bool
}

type execResponse struct {
	result *channel.ExecResult
	err    error
}

func (m *mockChannel) Connect(context.Context) error { m.connected = true; return nil }
func (m *mockChannel) IsConnected() bool             { return m.connected }
func (m *mockChannel) Close() error                  { m.connected = false; return nil }

func (m *mockChannel) Exec(_ context.Context, cmd string) (*channel.ExecResult, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.execs = append(m.execs, cmd)

	if len(m.execResponses) > 0 {
		r := m.execResponses[0]
		m.execResponses = m.execResponses[1:]
		return r.result, r.err
	}
	if m.execErr != nil {
		return nil, m.execErr
	}
	if m.execResult != nil {
		res := *m.execResult
		return &res, nil
	}
	return &channel.ExecResult{ExitCode: 0, Stdout: "", Stderr: ""}, nil
}

func (m *mockChannel) Upload(context.Context, string, io.Reader) error { return nil }
func (m *mockChannel) Download(context.Context, string) (io.Reader, error) {
	return nil, errors.New("not implemented")
}

func (m *mockChannel) execAt(i int) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	if i < 0 || i >= len(m.execs) {
		return ""
	}
	return m.execs[i]
}

func (m *mockChannel) execCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.execs)
}

func (m *mockChannel) lastExec() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.execs) == 0 {
		return ""
	}
	return m.execs[len(m.execs)-1]
}

// systemdDetect returns the response for `which systemctl` -> 0.
func systemdDetect() []execResponse {
	return []execResponse{
		{result: &channel.ExecResult{ExitCode: 0, Stdout: "/usr/bin/systemctl"}},
	}
}

// sysvinitDetect returns the response for `which systemctl` -> 1.
func sysvinitDetect() []execResponse {
	return []execResponse{
		{result: &channel.ExecResult{ExitCode: 1}},
	}
}

// --- module metadata ------------------------------------------------------

func TestModuleName(t *testing.T) {
	assert.Equal(t, "svc", New().Name())
}

func TestModuleActions(t *testing.T) {
	assert.Equal(t, []string{"start", "stop", "restart", "enable", "disable", "reload"}, New().Actions())
}

func TestModuleIdempotent(t *testing.T) {
	assert.True(t, New().Idempotent())
}

// --- detection ------------------------------------------------------------

func TestDetectSystemd(t *testing.T) {
	ch := &mockChannel{connected: true, execResponses: systemdDetect()}
	init, err := detectInitSystem(context.Background(), ch)
	require.NoError(t, err)
	assert.Equal(t, "systemd", init.name())
}

func TestDetectSysvinit(t *testing.T) {
	ch := &mockChannel{connected: true, execResponses: sysvinitDetect()}
	init, err := detectInitSystem(context.Background(), ch)
	require.NoError(t, err)
	assert.Equal(t, "sysvinit", init.name())
}

// --- start ----------------------------------------------------------------

func TestStartSystemdWhenInactive(t *testing.T) {
	ch := &mockChannel{connected: true}
	ch.execResponses = append(systemdDetect(),
		execResponse{result: &channel.ExecResult{ExitCode: 3}}, // is-active -> inactive (non-zero)
		execResponse{result: &channel.ExecResult{ExitCode: 0}}, // systemctl start
	)

	out, err := New().Execute(context.Background(), "start", executor.ModuleInput{
		Args:    map[string]any{"name": "nginx"},
		Channel: ch,
	})
	require.NoError(t, err)
	assert.True(t, out.Changed)
	assert.Contains(t, ch.lastExec(), "systemctl start 'nginx'")
}

func TestStartSystemdWhenActive(t *testing.T) {
	ch := &mockChannel{connected: true}
	ch.execResponses = append(systemdDetect(),
		execResponse{result: &channel.ExecResult{ExitCode: 0}}, // is-active -> active
	)

	out, err := New().Execute(context.Background(), "start", executor.ModuleInput{
		Args:    map[string]any{"name": "nginx"},
		Channel: ch,
	})
	require.NoError(t, err)
	assert.False(t, out.Changed, "already active -> no change")
	assert.Equal(t, 2, ch.execCount())
}

func TestStartSysvinitWhenInactive(t *testing.T) {
	ch := &mockChannel{connected: true}
	ch.execResponses = append(sysvinitDetect(),
		execResponse{result: &channel.ExecResult{ExitCode: 3}}, // service status -> inactive
		execResponse{result: &channel.ExecResult{ExitCode: 0}}, // service start
	)

	out, err := New().Execute(context.Background(), "start", executor.ModuleInput{
		Args:    map[string]any{"name": "nginx"},
		Channel: ch,
	})
	require.NoError(t, err)
	assert.True(t, out.Changed)
	assert.Contains(t, ch.lastExec(), "service 'nginx' start")
}

// --- stop -----------------------------------------------------------------

func TestStopSystemdWhenActive(t *testing.T) {
	ch := &mockChannel{connected: true}
	ch.execResponses = append(systemdDetect(),
		execResponse{result: &channel.ExecResult{ExitCode: 0}}, // is-active -> active
		execResponse{result: &channel.ExecResult{ExitCode: 0}}, // systemctl stop
	)

	out, err := New().Execute(context.Background(), "stop", executor.ModuleInput{
		Args:    map[string]any{"name": "nginx"},
		Channel: ch,
	})
	require.NoError(t, err)
	assert.True(t, out.Changed)
	assert.Contains(t, ch.lastExec(), "systemctl stop 'nginx'")
}

func TestStopSystemdWhenInactive(t *testing.T) {
	ch := &mockChannel{connected: true}
	ch.execResponses = append(systemdDetect(),
		execResponse{result: &channel.ExecResult{ExitCode: 3}}, // is-active -> inactive
	)

	out, err := New().Execute(context.Background(), "stop", executor.ModuleInput{
		Args:    map[string]any{"name": "nginx"},
		Channel: ch,
	})
	require.NoError(t, err)
	assert.False(t, out.Changed)
}

// --- restart --------------------------------------------------------------

func TestRestartSystemd(t *testing.T) {
	ch := &mockChannel{connected: true}
	ch.execResponses = append(systemdDetect(),
		execResponse{result: &channel.ExecResult{ExitCode: 0}},
	)

	_, err := New().Execute(context.Background(), "restart", executor.ModuleInput{
		Args:    map[string]any{"name": "nginx"},
		Channel: ch,
	})
	require.NoError(t, err)
	assert.Contains(t, ch.lastExec(), "systemctl restart 'nginx'")
}

func TestRestartSysvinit(t *testing.T) {
	ch := &mockChannel{connected: true}
	ch.execResponses = append(sysvinitDetect(),
		execResponse{result: &channel.ExecResult{ExitCode: 0}},
	)

	_, err := New().Execute(context.Background(), "restart", executor.ModuleInput{
		Args:    map[string]any{"name": "nginx"},
		Channel: ch,
	})
	require.NoError(t, err)
	assert.Contains(t, ch.lastExec(), "service 'nginx' restart")
}

// --- reload ---------------------------------------------------------------

func TestReloadSystemd(t *testing.T) {
	ch := &mockChannel{connected: true}
	ch.execResponses = append(systemdDetect(),
		execResponse{result: &channel.ExecResult{ExitCode: 0}},
	)

	_, err := New().Execute(context.Background(), "reload", executor.ModuleInput{
		Args:    map[string]any{"name": "nginx"},
		Channel: ch,
	})
	require.NoError(t, err)
	assert.Contains(t, ch.lastExec(), "systemctl reload 'nginx'")
}

func TestReloadSysvinitSuccess(t *testing.T) {
	ch := &mockChannel{connected: true}
	ch.execResponses = append(sysvinitDetect(),
		execResponse{result: &channel.ExecResult{ExitCode: 0, Stdout: "reloaded"}}, // service reload
	)

	out, err := New().Execute(context.Background(), "reload", executor.ModuleInput{
		Args:    map[string]any{"name": "nginx"},
		Channel: ch,
	})
	require.NoError(t, err)
	assert.Equal(t, 0, out.ExitCode)
	assert.True(t, out.Changed)
	assert.Contains(t, ch.execAt(1), "service 'nginx' reload")
}

func TestReloadSysvinitFallsBackToRestart(t *testing.T) {
	ch := &mockChannel{connected: true}
	ch.execResponses = append(sysvinitDetect(),
		execResponse{result: &channel.ExecResult{ExitCode: 1, Stderr: "reload not supported"}}, // reload fails
		execResponse{result: &channel.ExecResult{ExitCode: 0, Stdout: "restarted"}},            // restart
	)

	out, err := New().Execute(context.Background(), "reload", executor.ModuleInput{
		Args:    map[string]any{"name": "nginx"},
		Channel: ch,
	})
	require.NoError(t, err)
	assert.Equal(t, 0, out.ExitCode)
	assert.Contains(t, out.Stderr, "fell back to restart")
	assert.Contains(t, ch.lastExec(), "service 'nginx' restart")
}

// --- enable / disable -----------------------------------------------------

func TestEnableSystemdWhenDisabled(t *testing.T) {
	ch := &mockChannel{connected: true}
	ch.execResponses = append(systemdDetect(),
		execResponse{result: &channel.ExecResult{ExitCode: 1}}, // is-enabled -> disabled
		execResponse{result: &channel.ExecResult{ExitCode: 0}}, // systemctl enable
	)

	out, err := New().Execute(context.Background(), "enable", executor.ModuleInput{
		Args:    map[string]any{"name": "nginx"},
		Channel: ch,
	})
	require.NoError(t, err)
	assert.True(t, out.Changed)
	assert.Contains(t, ch.lastExec(), "systemctl enable 'nginx'")
}

func TestEnableSystemdWhenEnabled(t *testing.T) {
	ch := &mockChannel{connected: true}
	ch.execResponses = append(systemdDetect(),
		execResponse{result: &channel.ExecResult{ExitCode: 0}}, // is-enabled -> enabled
	)

	out, err := New().Execute(context.Background(), "enable", executor.ModuleInput{
		Args:    map[string]any{"name": "nginx"},
		Channel: ch,
	})
	require.NoError(t, err)
	assert.False(t, out.Changed)
}

func TestDisableSystemdWhenEnabled(t *testing.T) {
	ch := &mockChannel{connected: true}
	ch.execResponses = append(systemdDetect(),
		execResponse{result: &channel.ExecResult{ExitCode: 0}}, // is-enabled -> enabled
		execResponse{result: &channel.ExecResult{ExitCode: 0}}, // systemctl disable
	)

	out, err := New().Execute(context.Background(), "disable", executor.ModuleInput{
		Args:    map[string]any{"name": "nginx"},
		Channel: ch,
	})
	require.NoError(t, err)
	assert.True(t, out.Changed)
	assert.Contains(t, ch.lastExec(), "systemctl disable 'nginx'")
}

func TestDisableSystemdWhenDisabled(t *testing.T) {
	ch := &mockChannel{connected: true}
	ch.execResponses = append(systemdDetect(),
		execResponse{result: &channel.ExecResult{ExitCode: 1}}, // is-enabled -> disabled
	)

	out, err := New().Execute(context.Background(), "disable", executor.ModuleInput{
		Args:    map[string]any{"name": "nginx"},
		Channel: ch,
	})
	require.NoError(t, err)
	assert.False(t, out.Changed)
}

func TestEnableSysvinit(t *testing.T) {
	ch := &mockChannel{connected: true}
	ch.execResponses = append(sysvinitDetect(),
		execResponse{result: &channel.ExecResult{ExitCode: 1}}, // ls /etc/rc2.d/... -> not present
		execResponse{result: &channel.ExecResult{ExitCode: 0}}, // update-rc.d defaults
	)

	_, err := New().Execute(context.Background(), "enable", executor.ModuleInput{
		Args:    map[string]any{"name": "nginx"},
		Channel: ch,
	})
	require.NoError(t, err)
	assert.Contains(t, ch.lastExec(), "update-rc.d 'nginx' defaults")
}

// --- error paths ----------------------------------------------------------

func TestMissingName(t *testing.T) {
	ch := &mockChannel{connected: true, execResponses: systemdDetect()}
	_, err := New().Execute(context.Background(), "start", executor.ModuleInput{
		Args:    map[string]any{},
		Channel: ch,
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "missing argument")
	assert.Contains(t, err.Error(), "name")
}

func TestUnknownAction(t *testing.T) {
	ch := &mockChannel{connected: true, execResponses: systemdDetect()}
	_, err := New().Execute(context.Background(), "bogus", executor.ModuleInput{
		Args:    map[string]any{"name": "nginx"},
		Channel: ch,
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unsupported action")
}

func TestChannelErrorOnDetect(t *testing.T) {
	ch := &mockChannel{connected: true, execErr: errors.New("connection lost")}
	_, err := New().Execute(context.Background(), "start", executor.ModuleInput{
		Args:    map[string]any{"name": "nginx"},
		Channel: ch,
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "detect init system")
}

// --- registration ---------------------------------------------------------

func TestModuleRegistered(t *testing.T) {
	m, ok := executor.DefaultExecutor().Module("svc")
	require.True(t, ok, "svc module should self-register via init()")
	assert.Equal(t, "svc", m.Name())
}

// --- command builders sanity ---------------------------------------------

func TestSystemdCmds(t *testing.T) {
	init := systemdInit{}
	assert.Equal(t, "systemctl start 'nginx'", init.startCmd("nginx"))
	assert.Equal(t, "systemctl stop 'nginx'", init.stopCmd("nginx"))
	assert.Equal(t, "systemctl restart 'nginx'", init.restartCmd("nginx"))
	assert.Equal(t, "systemctl reload 'nginx'", init.reloadCmd("nginx"))
	assert.Equal(t, "systemctl enable 'nginx'", init.enableCmd("nginx"))
	assert.Equal(t, "systemctl disable 'nginx'", init.disableCmd("nginx"))
}

func TestSysvinitCmds(t *testing.T) {
	init := sysvinitInit{}
	assert.Equal(t, "service 'nginx' start", init.startCmd("nginx"))
	assert.Equal(t, "service 'nginx' stop", init.stopCmd("nginx"))
	assert.Equal(t, "service 'nginx' restart", init.restartCmd("nginx"))
	assert.Equal(t, "service 'nginx' reload", init.reloadCmd("nginx"))
	assert.Equal(t, "update-rc.d 'nginx' defaults", init.enableCmd("nginx"))
	assert.Equal(t, "update-rc.d -f 'nginx' remove", init.disableCmd("nginx"))
}
