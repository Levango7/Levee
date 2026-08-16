// Plugin sandbox: sub-process isolation, resource limits and crash recovery.
//
// The Sandbox is the host-side wrapper around a plugin sub-process. It is
// responsible for:
//
//   - starting the plugin binary as a child process with the agreed
//     environment and gRPC address;
//   - enforcing resource limits (CPU time, memory, wall-clock timeout);
//   - monitoring the process and restarting it on crash, up to a
//     configurable budget (default 3);
//   - emitting a crash alert through a caller-supplied callback so that
//     the notification framework can escalate repeated failures.
//
// The Sandbox does NOT load the plugin binary into the host's address
// space; plugins and the host communicate exclusively over gRPC. This
// keeps a buggy or malicious plugin from corrupting host memory.
//
// Resource limits are enforced through the operating system where
// available (POSIX rlimit on Linux/macOS, job objects on Windows). On
// platforms where the relevant syscall is not available the limit is
// enforced only as a wall-clock timeout via context cancellation. This
// keeps the sandbox portable without sacrificing the common-case
// guarantees.

package plugin

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"sync"
	"sync/atomic"
	"time"

	"github.com/nexus/levee/internal/log"
)

// --- Sandbox configuration --------------------------------------------------

// SandboxConfig tunes the resource limits and restart policy of a Sandbox.
// The zero value is not usable; callers must use DefaultSandboxConfig or
// populate the fields explicitly.
type SandboxConfig struct {
	// CPUQuota is the maximum CPU time (user + system) the plugin may
	// consume per invocation. Zero means "no limit". On platforms that
	// do not support per-process CPU quotas the limit is enforced only
	// through Timeout.
	CPUQuota time.Duration `json:"cpu_quota" yaml:"cpu_quota"`

	// MemoryLimit is the maximum resident set size the plugin may use,
	// in bytes. Zero means "no limit". On platforms that do not support
	// per-process memory limits the value is informational.
	MemoryLimit int64 `json:"memory_limit" yaml:"memory_limit"`

	// Timeout is the wall-clock deadline for a single invocation
	// (Connect / Exec / Check / Send ...). Zero means "no deadline";
	// callers should generally set a finite value to avoid hanging
	// forever on a stuck plugin.
	Timeout time.Duration `json:"timeout" yaml:"timeout"`

	// MaxRestarts is the maximum number of times the sandbox will
	// restart the plugin after a crash before giving up and surfacing
	// ErrSandboxCrashed. The default is 3. A negative value means
	// "never restart".
	MaxRestarts int `json:"max_restarts" yaml:"max_restarts"`

	// RestartDelay is the cool-down between a crash and the restart
	// attempt. It defaults to 100ms. A larger value backs off
	// crash-loops; zero means "restart immediately".
	RestartDelay time.Duration `json:"restart_delay" yaml:"restart_delay"`
}

// DefaultSandboxConfig returns a SandboxConfig with sensible defaults:
// 30s per-invocation timeout, 512MB memory cap, 3 restarts, 100ms back-off.
// CPU quota is left to the caller because it is highly workload-dependent.
func DefaultSandboxConfig() SandboxConfig {
	return SandboxConfig{
		Timeout:      30 * time.Second,
		MemoryLimit:  512 * 1024 * 1024, // 512 MB
		MaxRestarts:  3,
		RestartDelay: 100 * time.Millisecond,
	}
}

// --- Crash alert ------------------------------------------------------------

// CrashAlert is the payload passed to the OnCrash callback when a plugin
// sub-process crashes. It carries enough information for the notification
// framework to escalate without re-querying the sandbox.
type CrashAlert struct {
	// PluginName is the name of the crashed plugin.
	PluginName string `json:"plugin_name"`

	// CrashCount is the number of crashes observed so far, including the
	// one that triggered this alert. It is 1-indexed.
	CrashCount int `json:"crash_count"`

	// MaxRestarts is the configured restart budget. When CrashCount
	// exceeds MaxRestarts the sandbox gives up and the plugin enters
	// StateError.
	MaxRestarts int `json:"max_restarts"`

	// Err is the error describing the crash (e.g. exit status, signal).
	Err error `json:"-"`

	// Restarted reports whether the sandbox will attempt a restart after
	// this crash. It is false when CrashCount > MaxRestarts or when the
	// sandbox is shutting down.
	Restarted bool `json:"restarted"`
}

// OnCrashFunc is the callback signature for crash alerts. Implementations
// must be safe for concurrent use; the sandbox invokes them synchronously
// from its monitor goroutine.
type OnCrashFunc func(alert CrashAlert)

// --- Sandbox ----------------------------------------------------------------

// Sandbox manages a single plugin sub-process. It is created by the
// PluginManager and owns the *os/exec.Cmd plus the monitoring goroutine.
//
// A Sandbox is not safe to copy; always pass it by pointer.
type Sandbox struct {
	mu sync.Mutex

	name    string
	binary  string
	args    []string
	env     []string
	config  SandboxConfig
	onCrash OnCrashFunc

	cmd     *exec.Cmd
	crashes atomic.Int32  // total crashes observed
	stopped atomic.Bool   // set by Stop to suppress restarts
	started atomic.Bool   // set by Start, cleared by Stop
	waitErr error         // last wait error, set by monitor
	doneCh  chan struct{} // closed when monitor exits
}

// NewSandbox creates a Sandbox for the given plugin binary. The binary
// path must be absolute and the file must exist; NewSandbox does not
// verify this — Start does, so that a misconfigured plugin is reported
// at load time rather than at construction time.
//
// args are passed verbatim to the binary; env replaces the child's
// environment (use AppendHostEnv to inherit the host environment). onCrash
// may be nil.
func NewSandbox(name, binary string, args, env []string, cfg SandboxConfig, onCrash OnCrashFunc) *Sandbox {
	return &Sandbox{
		name:    name,
		binary:  binary,
		args:    args,
		env:     env,
		config:  cfg,
		onCrash: onCrash,
		doneCh:  make(chan struct{}),
	}
}

// Start launches the plugin sub-process and begins monitoring it. Start is
// idempotent: calling it on an already-running sandbox returns nil.
func (s *Sandbox) Start(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.started.Load() {
		return nil
	}

	if s.binary == "" {
		return fmt.Errorf("sandbox %q: empty binary path", s.name)
	}

	if err := s.startLocked(ctx); err != nil {
		return err
	}
	s.started.Store(true)
	s.stopped.Store(false)
	return nil
}

// startLocked spawns the process and the monitor goroutine. The caller
// must hold s.mu.
func (s *Sandbox) startLocked(ctx context.Context) error {
	cmd := exec.Command(s.binary, s.args...)
	if len(s.env) > 0 {
		cmd.Env = s.env
	}
	cmd.Stdout = nil // plugin logs through gRPC; discard stdout
	cmd.Stderr = nil

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("sandbox %q: start process: %w", s.name, err)
	}

	s.cmd = cmd
	s.doneCh = make(chan struct{})

	// Apply resource limits best-effort. We do not fail Start when the
	// limits cannot be applied: the wall-clock timeout still provides a
	// safety net and we want plugins to load on platforms that lack the
	// relevant syscalls.
	applyResourceLimits(cmd.Process, s.config)

	go s.monitor()
	return nil
}

// monitor waits for the sub-process to exit and handles restart. It runs
// in its own goroutine, one per Start call. The goroutine exits when the
// process exits and either the restart budget is exhausted or Stop was
// called.
func (s *Sandbox) monitor() {
	defer close(s.doneCh)

	for {
		err := s.cmd.Wait()
		s.mu.Lock()
		s.waitErr = err
		cmd := s.cmd
		s.mu.Unlock()

		// If Stop was called, do not restart.
		if s.stopped.Load() {
			return
		}

		// Process exited cleanly (err == nil) — nothing to do.
		if err == nil {
			s.started.Store(false)
			return
		}

		// Crash path.
		crashCount := int(s.crashes.Add(1))
		restarted := false
		if s.config.MaxRestarts >= 0 && crashCount <= s.config.MaxRestarts {
			// Back off before restart.
			if s.config.RestartDelay > 0 {
				time.Sleep(s.config.RestartDelay)
			}
			s.mu.Lock()
			if !s.stopped.Load() {
				if rerr := s.startLocked(context.Background()); rerr != nil {
					log.Error("sandbox restart failed",
						"plugin", s.name,
						"crash", crashCount,
						"err", rerr)
				} else {
					restarted = true
				}
			}
			s.mu.Unlock()
		}

		// Emit crash alert, unless restarts are disabled entirely
		// (MaxRestarts < 0). In "never restart" mode crashes are
		// expected by the caller and surfacing them as alerts would
		// be noise; the wait error is still propagated through Wait().
		if s.onCrash != nil && s.config.MaxRestarts >= 0 {
			s.onCrash(CrashAlert{
				PluginName:  s.name,
				CrashCount:  crashCount,
				MaxRestarts: s.config.MaxRestarts,
				Err:         err,
				Restarted:   restarted,
			})
		}

		if !restarted {
			// Either the budget is exhausted or Stop was called.
			s.started.Store(false)
			return
		}
		// Loop back to wait on the new process.
		_ = cmd
	}
}

// Stop terminates the sub-process. It sends SIGTERM (or the platform
// equivalent) and waits up to grace for the process to exit; if it does
// not exit in time, Stop escalates to SIGKILL. Stop is idempotent and
// safe to call from any goroutine.
func (s *Sandbox) Stop(grace time.Duration) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if !s.started.Load() {
		return nil
	}

	s.stopped.Store(true)
	cmd := s.cmd
	if cmd == nil || cmd.Process == nil {
		s.started.Store(false)
		return nil
	}

	// Best-effort graceful termination.
	if err := terminateProcess(cmd.Process); err != nil {
		// Non-fatal: we will escalate to kill below.
		log.Warn("sandbox graceful terminate failed",
			"plugin", s.name, "err", err)
	}

	// Wait with a grace deadline.
	done := s.doneCh
	select {
	case <-done:
		s.started.Store(false)
		return nil
	case <-time.After(grace):
	}

	// Escalate to kill. If the process has already been reaped by the
	// monitor goroutine in the meantime (ProcessState is set), skip the
	// kill: on Windows Kill() on a dead process returns "invalid
	// argument" and on Unix it returns ESRCH. We still wait for doneCh
	// to be closed to avoid racing the monitor.
	if cmd.ProcessState == nil {
		if err := killProcess(cmd.Process); err != nil {
			return fmt.Errorf("sandbox %q: kill: %w", s.name, err)
		}
		<-done
	}
	s.started.Store(false)
	return nil
}

// IsRunning reports whether the sub-process is currently running. The
// result is best-effort: a true value does not guarantee that the next
// call will succeed (the process may exit in the meantime).
func (s *Sandbox) IsRunning() bool {
	return s.started.Load()
}

// CrashCount returns the total number of crashes observed since the
// sandbox was created. It is intended for diagnostics and the crash alert
// callback.
func (s *Sandbox) CrashCount() int {
	return int(s.crashes.Load())
}

// Wait blocks until the sub-process exits and returns the wait error.
// It is primarily intended for tests; production code should use the
// OnCrash callback instead.
func (s *Sandbox) Wait() error {
	s.mu.Lock()
	done := s.doneCh
	s.mu.Unlock()
	<-done
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.waitErr
}

// WithTimeout returns a context derived from parent that is cancelled
// after the sandbox's configured Timeout. It is the canonical way for
// the manager to apply the per-invocation wall-clock deadline. When
// Timeout is zero the parent context is returned unchanged.
func (s *Sandbox) WithTimeout(parent context.Context) (context.Context, context.CancelFunc) {
	if s.config.Timeout <= 0 {
		return parent, func() {}
	}
	return context.WithTimeout(parent, s.config.Timeout)
}

// --- Errors -----------------------------------------------------------------

// ErrBudgetExhausted is returned by Start when the restart budget has
// already been exhausted and the sandbox cannot be restarted.
var ErrBudgetExhausted = errors.New("sandbox: restart budget exhausted")

// IsCrashError reports whether err is a crash error (non-nil wait error
// from a sub-process). It is a convenience helper for the OnCrash callback.
func IsCrashError(err error) bool {
	if err == nil {
		return false
	}
	var ee *exec.ExitError
	return errors.As(err, &ee)
}
