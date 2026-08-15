package pkg

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
	mu sync.Mutex

	execs         []string
	execResponses []execResponse
	execResult    *channel.ExecResult
	execErr       error

	connected bool
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

// aptDetect is the standard sequence of `which` probes for an apt host.
// We return the responses to program: which apt -> 0, which dnf -> 1, which yum -> 1.
func aptDetect() []execResponse {
	return []execResponse{
		{result: &channel.ExecResult{ExitCode: 0, Stdout: "/usr/bin/apt"}}, // which apt
	}
}

func dnfDetect() []execResponse {
	return []execResponse{
		{result: &channel.ExecResult{ExitCode: 1}},                         // which apt
		{result: &channel.ExecResult{ExitCode: 0, Stdout: "/usr/bin/dnf"}}, // which dnf
	}
}

func yumDetect() []execResponse {
	return []execResponse{
		{result: &channel.ExecResult{ExitCode: 1}},                         // which apt
		{result: &channel.ExecResult{ExitCode: 1}},                         // which dnf
		{result: &channel.ExecResult{ExitCode: 0, Stdout: "/usr/bin/yum"}}, // which yum
	}
}

func noneDetect() []execResponse {
	return []execResponse{
		{result: &channel.ExecResult{ExitCode: 1}}, // which apt
		{result: &channel.ExecResult{ExitCode: 1}}, // which dnf
		{result: &channel.ExecResult{ExitCode: 1}}, // which yum
	}
}

// --- module metadata ------------------------------------------------------

func TestModuleName(t *testing.T) {
	assert.Equal(t, "pkg", New().Name())
}

func TestModuleActions(t *testing.T) {
	assert.Equal(t, []string{"install", "remove", "upgrade"}, New().Actions())
}

func TestModuleIdempotent(t *testing.T) {
	assert.True(t, New().Idempotent())
}

// --- detection ------------------------------------------------------------

func TestDetectApt(t *testing.T) {
	ch := &mockChannel{connected: true, execResponses: aptDetect()}
	pm, err := detectPackageManager(context.Background(), ch)
	require.NoError(t, err)
	assert.Equal(t, "apt", pm.name())
}

func TestDetectDnf(t *testing.T) {
	ch := &mockChannel{connected: true, execResponses: dnfDetect()}
	pm, err := detectPackageManager(context.Background(), ch)
	require.NoError(t, err)
	assert.Equal(t, "dnf", pm.name())
}

func TestDetectYum(t *testing.T) {
	ch := &mockChannel{connected: true, execResponses: yumDetect()}
	pm, err := detectPackageManager(context.Background(), ch)
	require.NoError(t, err)
	assert.Equal(t, "yum", pm.name())
}

func TestDetectNone(t *testing.T) {
	ch := &mockChannel{connected: true, execResponses: noneDetect()}
	_, err := detectPackageManager(context.Background(), ch)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no supported package manager")
}

// --- install --------------------------------------------------------------

func TestInstallAptWhenAbsent(t *testing.T) {
	ch := &mockChannel{connected: true}
	ch.execResponses = append(aptDetect(),
		// dpkg-query: package not installed (exit 1).
		execResponse{result: &channel.ExecResult{ExitCode: 1, Stderr: "no packages found matching"}},
		// apt-get install.
		execResponse{result: &channel.ExecResult{ExitCode: 0, Stdout: "Setting up nginx"}},
	)

	out, err := New().Execute(context.Background(), "install", executor.ModuleInput{
		Args:    map[string]any{"name": "nginx"},
		Channel: ch,
	})
	require.NoError(t, err)
	assert.Equal(t, 0, out.ExitCode)
	assert.True(t, out.Changed)
	assert.Contains(t, ch.execAt(ch.execCount()-1), "apt-get install -y nginx")
}

func TestInstallAptWhenPresent(t *testing.T) {
	ch := &mockChannel{connected: true}
	ch.execResponses = append(aptDetect(),
		// dpkg-query: installed.
		execResponse{result: &channel.ExecResult{ExitCode: 0, Stdout: "install ok installed 1.24.0-1"}},
	)

	out, err := New().Execute(context.Background(), "install", executor.ModuleInput{
		Args:    map[string]any{"name": "nginx"},
		Channel: ch,
	})
	require.NoError(t, err)
	assert.Equal(t, 0, out.ExitCode)
	assert.False(t, out.Changed, "already installed -> no change")
	// Only the detect + query commands should have run; no install command.
	assert.Equal(t, 2, ch.execCount())
	_ = out
}

func TestInstallAptVersionMismatchReinstalls(t *testing.T) {
	ch := &mockChannel{connected: true}
	ch.execResponses = append(aptDetect(),
		execResponse{result: &channel.ExecResult{ExitCode: 0, Stdout: "install ok installed 1.20.0-1"}},
		execResponse{result: &channel.ExecResult{ExitCode: 0, Stdout: "Setting up nginx"}},
	)

	out, err := New().Execute(context.Background(), "install", executor.ModuleInput{
		Args:    map[string]any{"name": "nginx", "version": "1.24.0-1"},
		Channel: ch,
	})
	require.NoError(t, err)
	assert.True(t, out.Changed)
	assert.Contains(t, ch.execAt(ch.execCount()-1), "nginx=1.24.0-1")
}

func TestInstallAptVersionMatchSkips(t *testing.T) {
	ch := &mockChannel{connected: true}
	ch.execResponses = append(aptDetect(),
		execResponse{result: &channel.ExecResult{ExitCode: 0, Stdout: "install ok installed 1.24.0-1"}},
	)

	out, err := New().Execute(context.Background(), "install", executor.ModuleInput{
		Args:    map[string]any{"name": "nginx", "version": "1.24.0-1"},
		Channel: ch,
	})
	require.NoError(t, err)
	assert.False(t, out.Changed)
}

func TestInstallDnfCommand(t *testing.T) {
	ch := &mockChannel{connected: true}
	ch.execResponses = append(dnfDetect(),
		execResponse{result: &channel.ExecResult{ExitCode: 1}}, // rpm -q: not installed
		execResponse{result: &channel.ExecResult{ExitCode: 0}}, // dnf install
	)

	_, err := New().Execute(context.Background(), "install", executor.ModuleInput{
		Args:    map[string]any{"name": "nginx", "version": "1.24.0"},
		Channel: ch,
	})
	require.NoError(t, err)
	assert.Contains(t, ch.execAt(ch.execCount()-1), "dnf install -y nginx-1.24.0")
}

func TestInstallYumCommand(t *testing.T) {
	ch := &mockChannel{connected: true}
	ch.execResponses = append(yumDetect(),
		execResponse{result: &channel.ExecResult{ExitCode: 1}}, // rpm -q: not installed
		execResponse{result: &channel.ExecResult{ExitCode: 0}}, // yum install
	)

	_, err := New().Execute(context.Background(), "install", executor.ModuleInput{
		Args:    map[string]any{"name": "nginx"},
		Channel: ch,
	})
	require.NoError(t, err)
	assert.Contains(t, ch.execAt(ch.execCount()-1), "yum install -y nginx")
}

func TestInstallMissingName(t *testing.T) {
	ch := &mockChannel{connected: true, execResponses: aptDetect()}
	_, err := New().Execute(context.Background(), "install", executor.ModuleInput{
		Args:    map[string]any{},
		Channel: ch,
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "missing argument")
	assert.Contains(t, err.Error(), "name")
}

func TestInstallNoPackageManager(t *testing.T) {
	ch := &mockChannel{connected: true, execResponses: noneDetect()}
	_, err := New().Execute(context.Background(), "install", executor.ModuleInput{
		Args:    map[string]any{"name": "nginx"},
		Channel: ch,
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no supported package manager")
}

// --- remove ---------------------------------------------------------------

func TestRemoveAptWhenPresent(t *testing.T) {
	ch := &mockChannel{connected: true}
	ch.execResponses = append(aptDetect(),
		execResponse{result: &channel.ExecResult{ExitCode: 0, Stdout: "install ok installed 1.24.0-1"}},
		execResponse{result: &channel.ExecResult{ExitCode: 0, Stdout: "Removing nginx"}},
	)

	out, err := New().Execute(context.Background(), "remove", executor.ModuleInput{
		Args:    map[string]any{"name": "nginx"},
		Channel: ch,
	})
	require.NoError(t, err)
	assert.True(t, out.Changed)
	assert.Contains(t, ch.execAt(ch.execCount()-1), "apt-get remove -y nginx")
}

func TestRemoveAptWhenAbsent(t *testing.T) {
	ch := &mockChannel{connected: true}
	ch.execResponses = append(aptDetect(),
		execResponse{result: &channel.ExecResult{ExitCode: 1}},
	)

	out, err := New().Execute(context.Background(), "remove", executor.ModuleInput{
		Args:    map[string]any{"name": "nginx"},
		Channel: ch,
	})
	require.NoError(t, err)
	assert.False(t, out.Changed, "already absent -> no change")
	assert.Equal(t, 2, ch.execCount())
}

func TestRemoveDnfCommand(t *testing.T) {
	ch := &mockChannel{connected: true}
	ch.execResponses = append(dnfDetect(),
		execResponse{result: &channel.ExecResult{ExitCode: 0, Stdout: "1.24.0"}}, // rpm -q
		execResponse{result: &channel.ExecResult{ExitCode: 0}},                   // dnf remove
	)

	_, err := New().Execute(context.Background(), "remove", executor.ModuleInput{
		Args:    map[string]any{"name": "nginx"},
		Channel: ch,
	})
	require.NoError(t, err)
	assert.Contains(t, ch.execAt(ch.execCount()-1), "dnf remove -y nginx")
}

func TestRemoveMissingName(t *testing.T) {
	ch := &mockChannel{connected: true, execResponses: aptDetect()}
	_, err := New().Execute(context.Background(), "remove", executor.ModuleInput{
		Args:    map[string]any{},
		Channel: ch,
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "missing argument")
}

// --- upgrade --------------------------------------------------------------

func TestUpgradeAptSpecificPackage(t *testing.T) {
	ch := &mockChannel{connected: true}
	ch.execResponses = append(aptDetect(),
		execResponse{result: &channel.ExecResult{ExitCode: 0, Stdout: "upgraded"}},
	)

	_, err := New().Execute(context.Background(), "upgrade", executor.ModuleInput{
		Args:    map[string]any{"name": "nginx"},
		Channel: ch,
	})
	require.NoError(t, err)
	assert.Contains(t, ch.execAt(ch.execCount()-1), "apt-get install -y --only-upgrade nginx")
}

func TestUpgradeAptAllPackages(t *testing.T) {
	ch := &mockChannel{connected: true}
	ch.execResponses = append(aptDetect(),
		execResponse{result: &channel.ExecResult{ExitCode: 0}},
	)

	_, err := New().Execute(context.Background(), "upgrade", executor.ModuleInput{
		Args:    map[string]any{},
		Channel: ch,
	})
	require.NoError(t, err)
	assert.Contains(t, ch.execAt(ch.execCount()-1), "apt-get upgrade -y")
}

func TestUpgradeDnfAll(t *testing.T) {
	ch := &mockChannel{connected: true}
	ch.execResponses = append(dnfDetect(),
		execResponse{result: &channel.ExecResult{ExitCode: 0}},
	)

	_, err := New().Execute(context.Background(), "upgrade", executor.ModuleInput{
		Args:    map[string]any{},
		Channel: ch,
	})
	require.NoError(t, err)
	assert.Contains(t, ch.execAt(ch.execCount()-1), "dnf upgrade -y")
}

func TestUpgradeYumSpecific(t *testing.T) {
	ch := &mockChannel{connected: true}
	ch.execResponses = append(yumDetect(),
		execResponse{result: &channel.ExecResult{ExitCode: 0}},
	)

	_, err := New().Execute(context.Background(), "upgrade", executor.ModuleInput{
		Args:    map[string]any{"name": "kernel"},
		Channel: ch,
	})
	require.NoError(t, err)
	assert.Contains(t, ch.execAt(ch.execCount()-1), "yum update -y kernel")
}

// --- dispatch / unknown action --------------------------------------------

func TestExecuteUnknownAction(t *testing.T) {
	ch := &mockChannel{connected: true, execResponses: aptDetect()}
	_, err := New().Execute(context.Background(), "bogus", executor.ModuleInput{
		Args:    map[string]any{},
		Channel: ch,
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unsupported action")
}

// --- registration ---------------------------------------------------------

func TestModuleRegistered(t *testing.T) {
	m, ok := executor.DefaultExecutor().Module("pkg")
	require.True(t, ok, "pkg module should self-register via init()")
	assert.Equal(t, "pkg", m.Name())
}

// --- command builders sanity ---------------------------------------------

func TestAptInstallCmd(t *testing.T) {
	pm := aptPM{}
	assert.Equal(t, "apt-get install -y nginx", pm.installCmd("nginx", ""))
	assert.Equal(t, "apt-get install -y nginx=1.24.0", pm.installCmd("nginx", "1.24.0"))
}

func TestDnfUpgradeCmd(t *testing.T) {
	pm := dnfPM{}
	assert.Equal(t, "dnf upgrade -y", pm.upgradeCmd("", ""))
	assert.Equal(t, "dnf upgrade -y nginx", pm.upgradeCmd("nginx", ""))
	assert.Equal(t, "dnf install -y nginx-1.24.0", pm.upgradeCmd("nginx", "1.24.0"))
}

func TestYumRemoveCmd(t *testing.T) {
	pm := yumPM{}
	assert.Equal(t, "yum remove -y nginx", pm.removeCmd("nginx"))
}
