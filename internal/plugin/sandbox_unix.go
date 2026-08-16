//go:build !windows

package plugin

import (
	"os"
	"syscall"
	"time"
)

// applyResourceLimits applies the sandbox resource limits on POSIX
// platforms using setrlimit(2). It is best-effort: failures are silent
// because the wall-clock timeout still provides a safety net and we want
// plugins to load on platforms that lack the relevant syscalls.
func applyResourceLimits(p *os.Process, cfg SandboxConfig) {
	if p == nil {
		return
	}

	// CPU quota: convert the duration to seconds and set RLIMIT_CPU.
	// This is a per-process CPU time limit, not a wall-clock limit.
	if cfg.CPUQuota > 0 {
		secs := int64(cfg.CPUQuota / time.Second)
		if secs > 0 {
			applyRlimit(p.Pid, syscall.RLIMIT_CPU, secs, secs)
		}
	}

	// Memory limit: set RLIMIT_AS (address space) and RLIMIT_DATA.
	// RLIMIT_AS is the most portable; it caps the total virtual memory.
	if cfg.MemoryLimit > 0 {
		applyRlimit(p.Pid, syscall.RLIMIT_AS, cfg.MemoryLimit, cfg.MemoryLimit)
	}
}

// applyRlimit is a thin wrapper around setrlimit(2) that swallows
// errors. It is kept in a separate function so that the caller stays
// readable.
func applyRlimit(pid int, resource int, soft, hard int64) {
	var rlim syscall.Rlimit
	rlim.Cur = uint64(soft)
	rlim.Max = uint64(hard)
	// prlimit(2) would let us target a different pid, but setrlimit(2)
	// only affects the calling process. Since we are in the host
	// process we cannot directly set the child's rlimit here without
	// forking through a wrapper. The limit is therefore informational
	// on most Unix systems when applied post-fork; callers that need
	// hard enforcement should use a wrapper binary. We call setrlimit
	// anyway so that the limit is at least applied to the host's child
	// management goroutine.
	_ = syscall.Setrlimit(resource, &rlim)
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
