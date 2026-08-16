// log_collector_test.go exercises LogCollector with the mock CommandExecutor
// defined in health_probe_test.go. The mock returns canned stdout keyed by
// the command string, so tests can assert exactly which command the
// collector built for each source type and verify the parsing pipeline
// end-to-end without touching a real transport.

package diagnosis

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// --- Shared mock CommandExecutor -------------------------------------------
//
// mockExecutor / mockOutput / newMockExecutor are defined here and shared
// with health_probe_test.go (which reuses the same stub to exercise the
// health prober without a real remote target). The stub is thread-safe:
// commands are matched exactly and unmatched commands return exit code 1.

// mockOutput is the canned output for a single command.
type mockOutput struct {
	stdout   string
	stderr   string
	exitCode int
	err      error
}

// mockResult is an alias for mockOutput kept for compatibility with
// health_probe_test.go which uses the older name.
type mockResult = mockOutput

// mockExecutor is a thread-safe CommandExecutor stub. Commands are matched
// exactly; unmatched commands return exit code 1. The outputs map is
// exported so tests can register canned results with
// `exec.outputs["cmd"] = mockOutput{...}`.
type mockExecutor struct {
	mu      sync.Mutex
	outputs map[string]mockOutput
	calls   []string
}

// newMockExecutor returns an empty mock executor.
func newMockExecutor() *mockExecutor {
	return &mockExecutor{outputs: make(map[string]mockOutput)}
}

// set registers the canned result for cmd. It is a convenience wrapper
// around the outputs map for tests that prefer a method-style API.
func (m *mockExecutor) set(cmd string, r mockOutput) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.outputs[cmd] = r
}

// Execute implements CommandExecutor.
func (m *mockExecutor) Execute(_ context.Context, _, command string) (string, string, int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls = append(m.calls, command)
	if r, ok := m.outputs[command]; ok {
		return r.stdout, r.stderr, r.exitCode, r.err
	}
	return "", "command not found", 1, nil
}

// callCount returns the number of Execute calls recorded so far.
func (m *mockExecutor) callCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.calls)
}

// Compile-time guard: mockExecutor must satisfy CommandExecutor.
var _ CommandExecutor = (*mockExecutor)(nil)

// --- helpers ----------------------------------------------------------------

// mustCollector builds a LogCollector backed by exec, failing the test on
// construction error.
func mustCollector(t *testing.T, exec CommandExecutor) *LogCollector {
	t.Helper()
	c, err := NewLogCollector(exec)
	require.NoError(t, err)
	return c
}

// validWindow returns a 10-minute window ending at a fixed instant so
// tests are deterministic.
func validWindow() TimeWindow {
	end := time.Date(2024, 6, 1, 12, 10, 0, 0, time.UTC)
	return TimeWindow{Start: end.Add(-10 * time.Minute), End: end}
}

// --- NewLogCollector --------------------------------------------------------

func TestNewLogCollector_NilExecutor(t *testing.T) {
	c, err := NewLogCollector(nil)
	require.ErrorIs(t, err, ErrNilExecutor)
	assert.Nil(t, c)
}

func TestNewLogCollector_Valid(t *testing.T) {
	exec := newMockExecutor()
	c := mustCollector(t, exec)
	assert.NotNil(t, c)
}

// --- Collect: argument validation ------------------------------------------

func TestCollect_EmptyTarget(t *testing.T) {
	c := mustCollector(t, newMockExecutor())
	_, err := c.Collect(context.Background(), "", []LogSource{{Name: "x"}}, validWindow())
	require.ErrorIs(t, err, ErrEmptyTarget)
}

func TestCollect_EmptySources(t *testing.T) {
	c := mustCollector(t, newMockExecutor())
	_, err := c.Collect(context.Background(), "host", nil, validWindow())
	require.ErrorIs(t, err, ErrEmptySources)
}

func TestCollect_InvalidWindow(t *testing.T) {
	c := mustCollector(t, newMockExecutor())
	w := TimeWindow{Start: time.Now(), End: time.Now()}
	_, err := c.Collect(context.Background(), "host", []LogSource{{Name: "x"}}, w)
	require.ErrorIs(t, err, ErrZeroWindow)
}

func TestCollect_NegativeWindow(t *testing.T) {
	c := mustCollector(t, newMockExecutor())
	w := TimeWindow{Start: time.Now(), End: time.Now().Add(-time.Second)}
	_, err := c.Collect(context.Background(), "host", []LogSource{{Name: "x"}}, w)
	require.ErrorIs(t, err, ErrZeroWindow)
}

func TestCollect_CancelledContext(t *testing.T) {
	c := mustCollector(t, newMockExecutor())
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := c.Collect(ctx, "host", []LogSource{{Name: "x"}}, validWindow())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "cancelled")
}

// --- Collect: journald ------------------------------------------------------

func TestCollect_Journald(t *testing.T) {
	exec := newMockExecutor()
	window := validWindow()

	// Build the exact command the collector will issue so we can mock it.
	src := LogSource{Name: "journal", Type: SourceJournald, Format: "syslog"}
	cmd, err := buildCollectCommand(src, window)
	require.NoError(t, err)

	exec.set(cmd, mockResult{
		stdout: strings.Join([]string{
			"2024-06-01T12:01:00+00:00 host sshd[123]: ERROR failed to authenticate",
			"2024-06-01T12:02:00+00:00 host kernel[0]: WARN oom-kill",
			"", // blank line should be skipped
			"2024-06-01T12:03:00+00:00 host app[456]: INFO request completed",
		}, "\n"),
	})

	c := mustCollector(t, exec)
	batch, err := c.Collect(context.Background(), "node-1", []LogSource{src}, window)
	require.NoError(t, err)

	assert.Equal(t, "node-1", batch.Target)
	assert.Equal(t, window, batch.Window)
	assert.Empty(t, batch.Errors)
	require.Len(t, batch.Lines, 3)

	// First line: ERROR level, timestamp parsed.
	l0 := batch.Lines[0]
	assert.Equal(t, "journal", l0.Source)
	assert.Equal(t, "ERROR", l0.Level)
	assert.Contains(t, l0.Message, "failed to authenticate")
	assert.False(t, l0.Timestamp.IsZero())

	// Second line: WARN level.
	l1 := batch.Lines[1]
	assert.Equal(t, "WARN", l1.Level)
	assert.Contains(t, l1.Message, "oom-kill")

	// Third line: INFO level.
	l2 := batch.Lines[2]
	assert.Equal(t, "INFO", l2.Level)
	assert.Contains(t, l2.Message, "request completed")

	// Exactly one executor call.
	assert.Equal(t, 1, exec.callCount())
	assert.Equal(t, cmd, exec.calls[0])
}

// --- Collect: app source ---------------------------------------------------

func TestCollect_AppSource(t *testing.T) {
	exec := newMockExecutor()
	window := validWindow()

	src := LogSource{Name: "applog", Type: SourceApp, Path: "/var/log/app.log", Format: "plain"}
	cmd, err := buildCollectCommand(src, window)
	require.NoError(t, err)

	exec.set(cmd, mockResult{stdout: "line one\nline two\nline three\n"})

	c := mustCollector(t, exec)
	batch, err := c.Collect(context.Background(), "host", []LogSource{src}, window)
	require.NoError(t, err)
	require.Len(t, batch.Lines, 3)
	assert.Equal(t, "line one", batch.Lines[0].Message)
	assert.Equal(t, "line one", batch.Lines[0].Raw)
	assert.Equal(t, "applog", batch.Lines[0].Source)
}

func TestCollect_AppSourceEmptyPath(t *testing.T) {
	c := mustCollector(t, newMockExecutor())
	src := LogSource{Name: "applog", Type: SourceApp, Path: "", Format: "plain"}
	batch, err := c.Collect(context.Background(), "host", []LogSource{src}, validWindow())
	require.NoError(t, err) // top-level call succeeds
	require.Len(t, batch.Errors, 1)
	assert.Contains(t, batch.Errors[0].Error, "empty path")
	assert.Empty(t, batch.Lines)
}

// --- Collect: multiple sources run concurrently ----------------------------

func TestCollect_MultipleSources(t *testing.T) {
	exec := newMockExecutor()
	window := validWindow()

	srcs := []LogSource{
		{Name: "j", Type: SourceJournald, Format: "syslog"},
		{Name: "a", Type: SourceApp, Path: "/var/log/x.log", Format: "plain"},
	}
	for _, s := range srcs {
		cmd, err := buildCollectCommand(s, window)
		require.NoError(t, err)
		exec.set(cmd, mockResult{stdout: s.Name + "-line1\n" + s.Name + "-line2\n"})
	}

	c := mustCollector(t, exec)
	batch, err := c.Collect(context.Background(), "host", srcs, window)
	require.NoError(t, err)
	assert.Empty(t, batch.Errors)
	assert.Len(t, batch.Lines, 4)

	// Both sources must have been called.
	assert.Equal(t, 2, exec.callCount())
}

// --- Collect: per-source failures ------------------------------------------

func TestCollect_SourceExecFailure(t *testing.T) {
	exec := newMockExecutor()
	window := validWindow()

	src := LogSource{Name: "bad", Type: SourceJournald, Format: "syslog"}
	cmd, _ := buildCollectCommand(src, window)
	exec.set(cmd, mockResult{err: errors.New("connection refused")})

	c := mustCollector(t, exec)
	batch, err := c.Collect(context.Background(), "host", []LogSource{src}, window)
	require.NoError(t, err) // top-level succeeds
	require.Len(t, batch.Errors, 1)
	assert.Equal(t, "bad", batch.Errors[0].Source)
	assert.Contains(t, batch.Errors[0].Error, "connection refused")
	assert.Empty(t, batch.Lines)
}

func TestCollect_SourceNonZeroExit(t *testing.T) {
	exec := newMockExecutor()
	window := validWindow()

	src := LogSource{Name: "bad", Type: SourceJournald, Format: "syslog"}
	cmd, _ := buildCollectCommand(src, window)
	exec.set(cmd, mockResult{exitCode: 1, stderr: "permission denied"})

	c := mustCollector(t, exec)
	batch, err := c.Collect(context.Background(), "host", []LogSource{src}, window)
	require.NoError(t, err)
	require.Len(t, batch.Errors, 1)
	assert.Contains(t, batch.Errors[0].Error, "exit code 1")
	assert.Contains(t, batch.Errors[0].Error, "permission denied")
}

func TestCollect_PartialFailure(t *testing.T) {
	exec := newMockExecutor()
	window := validWindow()

	goodSrc := LogSource{Name: "good", Type: SourceJournald, Format: "syslog"}
	badSrc := LogSource{Name: "bad", Type: SourceApp, Path: "/x.log", Format: "plain"}

	goodCmd, _ := buildCollectCommand(goodSrc, window)
	badCmd, _ := buildCollectCommand(badSrc, window)

	exec.set(goodCmd, mockResult{stdout: "good-line\n"})
	exec.set(badCmd, mockResult{err: errors.New("timeout")})

	c := mustCollector(t, exec)
	batch, err := c.Collect(context.Background(), "host", []LogSource{goodSrc, badSrc}, window)
	require.NoError(t, err)
	require.Len(t, batch.Errors, 1)
	assert.Equal(t, "bad", batch.Errors[0].Source)
	require.Len(t, batch.Lines, 1)
	assert.Equal(t, "good-line", batch.Lines[0].Message)
}

// --- Collect: context cancellation mid-run --------------------------------

func TestCollect_ContextCancelMidRun(t *testing.T) {
	exec := newMockExecutor()
	window := validWindow()

	src := LogSource{Name: "j", Type: SourceJournald, Format: "syslog"}
	cmd, _ := buildCollectCommand(src, window)

	// Cancel the context before calling Collect: the collector must
	// refuse to start and return a top-level error.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	exec.set(cmd, mockResult{err: context.Canceled})

	c := mustCollector(t, exec)
	_, err := c.Collect(ctx, "host", []LogSource{src}, window)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "cancelled")
}

// --- DefaultSources --------------------------------------------------------

func TestDefaultSources_Linux(t *testing.T) {
	srcs := DefaultSources(RuntimeLinux)
	require.Len(t, srcs, 3)

	types := make(map[SourceType]bool, len(srcs))
	for _, s := range srcs {
		types[s.Type] = true
	}
	assert.True(t, types[SourceSyslog])
	assert.True(t, types[SourceJournald])
	assert.True(t, types[SourceApp])
}

func TestDefaultSources_Windows(t *testing.T) {
	srcs := DefaultSources(RuntimeWindows)
	require.Len(t, srcs, 2)

	types := make(map[SourceType]bool, len(srcs))
	for _, s := range srcs {
		types[s.Type] = true
	}
	assert.True(t, types[SourceEventLog])
	assert.True(t, types[SourceApp])
}

func TestDefaultSources_UnknownRuntime(t *testing.T) {
	assert.Nil(t, DefaultSources(Runtime("plan9")))
}

func TestDefaultSources_FreshCopy(t *testing.T) {
	a := DefaultSources(RuntimeLinux)
	a[0].Name = "mutated"
	b := DefaultSources(RuntimeLinux)
	assert.NotEqual(t, "mutated", b[0].Name, "DefaultSources must return a fresh slice")
}

// --- buildCollectCommand ---------------------------------------------------

func TestBuildCollectCommand_Journald(t *testing.T) {
	w := validWindow()
	src := LogSource{Name: "j", Type: SourceJournald}
	cmd, err := buildCollectCommand(src, w)
	require.NoError(t, err)
	assert.Contains(t, cmd, "journalctl")
	assert.Contains(t, cmd, "--since")
	assert.Contains(t, cmd, "--until")
}

func TestBuildCollectCommand_Syslog(t *testing.T) {
	w := validWindow()
	src := LogSource{Name: "s", Type: SourceSyslog}
	cmd, err := buildCollectCommand(src, w)
	require.NoError(t, err)
	assert.Contains(t, cmd, "journalctl")
}

func TestBuildCollectCommand_EventLog(t *testing.T) {
	w := validWindow()
	src := LogSource{Name: "e", Type: SourceEventLog, Path: "System"}
	cmd, err := buildCollectCommand(src, w)
	require.NoError(t, err)
	assert.Contains(t, cmd, "Get-WinEvent")
	assert.Contains(t, cmd, "System")
}

func TestBuildCollectCommand_EventLogDefaultName(t *testing.T) {
	w := validWindow()
	src := LogSource{Name: "e", Type: SourceEventLog, Path: ""}
	cmd, err := buildCollectCommand(src, w)
	require.NoError(t, err)
	assert.Contains(t, cmd, "Application")
}

func TestBuildCollectCommand_AppEmptyPath(t *testing.T) {
	w := validWindow()
	src := LogSource{Name: "a", Type: SourceApp, Path: ""}
	_, err := buildCollectCommand(src, w)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "empty path")
}

func TestBuildCollectCommand_UnknownType(t *testing.T) {
	w := validWindow()
	src := LogSource{Name: "x", Type: SourceType("magic")}
	_, err := buildCollectCommand(src, w)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unknown source type")
}

// --- TimeWindow.Validate ---------------------------------------------------

func TestTimeWindowValidate(t *testing.T) {
	assert.NoError(t, validWindow().Validate())

	now := time.Now()
	assert.ErrorIs(t, TimeWindow{Start: now, End: now}.Validate(), ErrZeroWindow)
	assert.ErrorIs(t, TimeWindow{Start: now, End: now.Add(-time.Second)}.Validate(), ErrZeroWindow)
}

// --- parseOutput / parseLine -----------------------------------------------

func TestParseOutput_PlainFormat(t *testing.T) {
	src := LogSource{Name: "x", Format: "plain"}
	lines := parseOutput(src, "a\nb\nc\n")
	require.Len(t, lines, 3)
	assert.Equal(t, "a", lines[0].Message)
	assert.Equal(t, "a", lines[0].Raw)
	assert.Equal(t, "x", lines[0].Source)
}

func TestParseOutput_SkipsBlankLines(t *testing.T) {
	src := LogSource{Name: "x", Format: "plain"}
	lines := parseOutput(src, "a\n\nb\n\n")
	require.Len(t, lines, 2)
}

func TestParseOutput_SyslogFormat(t *testing.T) {
	src := LogSource{Name: "j", Format: "syslog"}
	lines := parseOutput(src, "2024-06-01T12:00:00+00:00 host app[1]: ERROR boom\n")
	require.Len(t, lines, 1)
	assert.False(t, lines[0].Timestamp.IsZero())
	assert.Equal(t, "ERROR", lines[0].Level)
	assert.Contains(t, lines[0].Message, "boom")
}

func TestParseOutput_LongLine(t *testing.T) {
	src := LogSource{Name: "x", Format: "plain"}
	long := strings.Repeat("a", 200*1024) // 200 KiB, exceeds default scanner buffer
	lines := parseOutput(src, long+"\n")
	require.Len(t, lines, 1)
	assert.Equal(t, long, lines[0].Message)
}

// --- parseSyslogLine -------------------------------------------------------

func TestParseSyslogLine_RFC3339(t *testing.T) {
	var line LogLine
	parseSyslogLine(&line, "2024-06-01T12:00:00+00:00 host app[1]: ERROR boom")
	assert.False(t, line.Timestamp.IsZero())
	assert.Equal(t, "ERROR", line.Level)
	assert.Contains(t, line.Message, "boom")
}

func TestParseSyslogLine_RFC3164(t *testing.T) {
	var line LogLine
	parseSyslogLine(&line, "Jun  1 12:00:00 host app[1]: WARN wobble")
	assert.False(t, line.Timestamp.IsZero())
	assert.Equal(t, "WARN", line.Level)
	assert.Contains(t, line.Message, "wobble")
}

func TestParseSyslogLine_Unparseable(t *testing.T) {
	var line LogLine
	parseSyslogLine(&line, "this is not a syslog line at all")
	assert.True(t, line.Timestamp.IsZero())
	assert.Equal(t, "this is not a syslog line at all", line.Message)
}

// --- extractLevel ----------------------------------------------------------

func TestExtractLevel_KnownLevels(t *testing.T) {
	cases := []struct {
		in       string
		wantLvl  string
		wantRest string
	}{
		{"ERROR something", "ERROR", "something"},
		{"warn: foo", "WARN", ": foo"},
		{"INFO  spaced", "INFO", "spaced"},
		{"FATAL boom", "FATAL", "boom"},
		{"debug here", "DEBUG", "here"},
		{"nosynergy", "", "nosynergy"},
	}
	for _, tc := range cases {
		var line LogLine
		rest := extractLevel(tc.in, &line)
		assert.Equal(t, tc.wantLvl, line.Level, "input %q", tc.in)
		assert.Equal(t, tc.wantRest, rest, "input %q", tc.in)
	}
}

func TestExtractLevel_DoesNotMatchSubstring(t *testing.T) {
	var line LogLine
	rest := extractLevel("INFORMATION", &line)
	assert.Empty(t, line.Level)
	assert.Equal(t, "INFORMATION", rest)
}
