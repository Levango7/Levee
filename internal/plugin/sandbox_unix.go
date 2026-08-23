//go:build unix && !linux

package plugin

import (
	"os"
	"time"

	"github.com/nexus/levee/internal/log"
)

// applyResourceLimits is a no-op on POSIX platforms.
//
// syscall.Setrlimit(2) only affects the calling process (the host), not
// child processes. Applying it after cmd.Start() would restrict the
// host's own resource limits without constraining the plugin subprocess,
// which is both ineffective and potentially harmful (it would degrade
// the host's ability to manage the child). On Windows, job-object–based
// limits are handled in sandbox_windows.go.
//
// The wall-clock timeout (SandboxConfig.Timeout) remains the universal
// safety net across all platforms.
func applyResourceLimits(_ *os.Process, cfg SandboxConfig) {
	if cfg.MemoryLimit > 0 {
		log.Warn("sandbox: memory limit is informational on this platform; child processes are not constrained by rlimit",
			"plugin", cfg, "memory_limit", cfg.MemoryLimit)
	}
}

// doTerminate sends SIGTERM to the process on Unix.
func doTerminate(p *os.Process) error {
	if p == nil {
		return nil
	}
	return p.Signal(syscall.SIGTERM)
}

// doKill sends SIGKILL to the process on Unix.
func doKill(p *os.Process) error {
	if p == nil {
		return nil
	}
	return p.Signal(syscall.SIGKILL)
}
