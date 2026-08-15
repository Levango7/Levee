// shell_run.go implements the "levee run --shell <cmd>" bare-shell direct
// execution path (MVP task T058). Unlike the shell Module in
// internal/executor/modules/shell, which drives a remote target through a
// channel.Channel, ShellRunner runs a single command line on the local
// machine (or, in future, on a remote target via RunOnTarget) without going
// through the workflow / plan / batch pipeline. It is the minimal "just run
// this" escape hatch for ad-hoc operations and quick smoke tests.
//
// The runner is intentionally small: it wraps os/exec, applies a timeout,
// captures stdout / stderr / exit-code / duration and returns a structured
// ShellRunResult. Cross-platform behaviour is handled by selecting the
// appropriate shell wrapper at runtime (cmd /c on Windows, sh -c elsewhere).

package executor

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"runtime"
	"time"
)

// DefaultShellTimeout is the wall-clock budget applied to a ShellRunner when
// the caller does not override it via WithTimeout. 30 seconds is long enough
// for ad-hoc diagnostics (ps, df, uname, ...) but short enough that a hung
// command does not stall a `levee run --shell` invocation indefinitely.
const DefaultShellTimeout = 30 * time.Second

// Sentinel errors returned by ShellRunner. They are wrapped with %w by the
// implementation so that callers can use errors.Is to classify failures
// (e.g. distinguish a timeout from a non-zero exit code).
var (
	// ErrEmptyCommand is returned when the command passed to Run is blank
	// after trimming surrounding whitespace.
	ErrEmptyCommand = errors.New("executor: empty command")

	// ErrCommandTimeout is returned when the command did not finish before
	// the configured timeout elapsed. The underlying context.DeadlineExceeded
	// is wrapped inside so callers can also match that.
	ErrCommandTimeout = errors.New("executor: command timeout")

	// ErrNotImplemented is returned by API surface that exists for forward
	// compatibility but has no MVP implementation (currently RunOnTarget).
	ErrNotImplemented = errors.New("executor: not implemented")
)

// ShellRunner provides "levee run --shell <cmd>" single-command direct
// execution. It bypasses the workflow / plan / batch pipeline and runs one
// shell command line, capturing the full result. The zero value is not
// usable; callers must use NewShellRunner.
//
// A ShellRunner is safe for concurrent use: Run does not mutate the receiver
// except for reading the immutable timeout field.
type ShellRunner struct {
	timeout time.Duration
}

// ShellRunResult is the outcome of a single ShellRunner.Run invocation. It
// mirrors channel.ExecResult but adds the command string and the Go-level
// error (if any) so that callers logging a result have everything in one
// place.
type ShellRunResult struct {
	Command  string        // the command line that was executed
	ExitCode int           // process exit code (0 = success); -1 when the process was killed before exiting
	Stdout   string        // captured standard output
	Stderr   string        // captured standard error
	Duration time.Duration // wall-clock time from process start to termination
	Err      error         // Go-level error (timeout, cancellation, spawn failure); nil on clean exit
}

// NewShellRunner returns a ShellRunner configured with DefaultShellTimeout.
func NewShellRunner() *ShellRunner {
	return &ShellRunner{timeout: DefaultShellTimeout}
}

// WithTimeout sets the per-command timeout and returns the receiver so that
// the call can be chained: NewShellRunner().WithTimeout(5*time.Second).
// A non-positive duration resets to DefaultShellTimeout.
func (r *ShellRunner) WithTimeout(d time.Duration) *ShellRunner {
	if d <= 0 {
		r.timeout = DefaultShellTimeout
	} else {
		r.timeout = d
	}
	return r
}

// Run executes command on the local machine. The command is run through the
// platform shell wrapper (cmd /c on Windows, sh -c elsewhere) so that shell
// features such as pipes, redirects and environment expansion are available.
//
// Behaviour:
//   - empty command -> returns (nil, ErrEmptyCommand)
//   - timeout elapsed -> returns (result, ErrCommandTimeout) with result
//     containing whatever was captured before the kill
//   - ctx cancelled -> returns (result, ErrCommandTimeout) wrapping ctx.Err()
//   - non-zero exit -> returns (result, nil); ExitCode carries the code
//   - success -> returns (result, nil) with ExitCode == 0
//
// The returned ShellRunResult is non-nil whenever a process was started, even
// on error paths, so that callers can inspect partial output.
//
// Implementation note: we use Start + a goroutine feeding cmd.Wait into a
// channel and race it against runCtx.Done() rather than exec.CommandContext's
// built-in cancellation. On Windows, killing cmd.exe while it is waiting on a
// child (e.g. ping.exe spawned by `cmd /c`) does not unblock cmd.Wait until
// the orphaned grandchild exits, which would make a 100ms timeout wait the
// full command duration. By selecting on runCtx.Done we guarantee that Run
// returns promptly on timeout; the killed process is reaped in a background
// goroutine to avoid leaking OS handles.
func (r *ShellRunner) Run(ctx context.Context, command string) (*ShellRunResult, error) {
	if isBlank(command) {
		return nil, ErrEmptyCommand
	}

	// Derive a child context with our own timeout. We keep the cancel func so
	// we can release the timer even when the parent context fires first.
	runCtx, cancel := context.WithTimeout(ctx, r.timeout)
	defer cancel()

	cmd := buildShellCommand(command)

	// Capture stdout and stderr into separate buffers so that ShellRunResult
	// can report them independently. We do not stream because the bare-shell
	// path is meant for short commands; long-running streaming output should
	// go through the workflow pipeline instead.
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	start := time.Now()

	// Start the process explicitly so we can race Wait against the context.
	if err := cmd.Start(); err != nil {
		duration := time.Since(start)
		return &ShellRunResult{
			Command:  command,
			ExitCode: -1,
			Duration: duration,
			Err:      err,
		}, fmt.Errorf("executor: start command: %w", err)
	}

	// waitCh receives the result of cmd.Wait. Buffer of 1 so the goroutine
	// never blocks even if we stop listening after a timeout.
	waitCh := make(chan error, 1)
	go func() { waitCh <- cmd.Wait() }()

	var waitErr error
	timedOut := false

	select {
	case waitErr = <-waitCh:
		// Process exited on its own before the context expired.
	case <-runCtx.Done():
		// Context expired (our timeout or parent cancellation). Kill the
		// process and reap it in the background so Run returns promptly.
		timedOut = true
		_ = cmd.Process.Kill()
		// Drain waitCh in a goroutine. On Windows cmd.Wait may block until
		// orphaned grandchildren exit; we must not wait for that here. The
		// goroutine ensures the process handle is eventually released.
		go func() { <-waitCh }()
		waitErr = runCtx.Err()
	}

	duration := time.Since(start)
	result := &ShellRunResult{
		Command:  command,
		Stdout:   stdout.String(),
		Stderr:   stderr.String(),
		Duration: duration,
	}

	if waitErr == nil && !timedOut {
		result.ExitCode = 0
		return result, nil
	}

	// Timeout / cancellation takes precedence over ExitError classification,
	// because on Windows a killed process surfaces as *exec.ExitError rather
	// than a context error.
	if timedOut {
		result.ExitCode = -1
		result.Err = waitErr
		return result, fmt.Errorf("%w: %v", ErrCommandTimeout, waitErr)
	}

	// Non-nil err from Wait without timeout: classify.
	var exitErr *exec.ExitError
	if errors.As(waitErr, &exitErr) {
		// Process ran and exited with a non-zero status. This is a "clean"
		// failure: the command did what it did, we just report the code.
		result.ExitCode = exitErr.ExitCode()
		return result, nil
	}

	// Other spawn / wait failures.
	result.ExitCode = -1
	result.Err = waitErr
	return result, fmt.Errorf("executor: run command: %w", waitErr)
}

// RunOnTarget executes command on a remote target identified by target. The
// MVP implementation does not wire up remote execution (that requires a live
// channel.Channel and target resolution); it returns ErrNotImplemented so
// that the CLI can surface a clear message instead of a silent no-op.
//
// Future versions will accept a channel.Channel (or a target descriptor that
// the runner resolves through the channel registry) and delegate to
// channel.Exec, reusing the same timeout / result-shaping logic as Run.
func (r *ShellRunner) RunOnTarget(ctx context.Context, command, target string) (*ShellRunResult, error) {
	_ = ctx
	_ = command
	_ = target
	return nil, ErrNotImplemented
}

// buildShellCommand constructs an *exec.Cmd that runs command through the
// platform shell wrapper. On Windows we use cmd /c so that built-ins like
// echo, exit and dir work; on every other platform we use sh -c. The chosen
// wrapper is the smallest one that gives us shell semantics (pipes, redirects,
// env expansion) without forcing a specific user shell.
//
// We use exec.Command (not exec.CommandContext) because Run manages the
// context itself via a select on runCtx.Done and an explicit Process.Kill.
// Relying on CommandContext's built-in watcher would race with our own Kill
// and, on Windows, would still block cmd.Wait on orphaned grandchildren.
func buildShellCommand(command string) *exec.Cmd {
	if runtime.GOOS == "windows" {
		// /c tells cmd to execute the string and then terminate. Without it
		// cmd would drop into an interactive prompt and hang forever.
		return exec.Command("cmd", "/c", command)
	}
	return exec.Command("sh", "-c", command)
}

// isBlank reports whether s contains only whitespace. It is a tiny helper so
// that Run does not need to import strings just for one call.
func isBlank(s string) bool {
	for _, c := range s {
		switch c {
		case ' ', '\t', '\n', '\r', '\v', '\f':
			continue
		default:
			return false
		}
	}
	return true
}
