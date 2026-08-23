//go:build linux

// sandbox_linux.go implements per-plugin resource limits via cgroup v2
// (unified hierarchy). setrlimit(2) cannot constrain an already-started
// child, but a cgroup can: the child's PID is written into a dedicated
// cgroup whose memory.max / cpu.max were configured beforehand.
//
// Requirements: cgroup v2 mounted at /sys/fs/cgroup, writable for the
// levee user. When cgroups are unavailable or read-only the functions
// degrade gracefully — applyResourceLimits logs and returns, leaving
// the wall-clock timeout as the only safety net.
package plugin

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/nexus/levee/internal/log"
)

// cgroupBase is the mount point of the unified cgroup v2 hierarchy.
const cgroupBase = "/sys/fs/cgroup"

// cgroupV2Available reports whether the unified hierarchy is present.
func cgroupV2Available() bool {
	_, err := os.Stat(filepath.Join(cgroupBase, "cgroup.controllers"))
	return err == nil
}

// applyResourceLimits places the plugin process under cgroup v2 memory
// and CPU limits. It never fails the caller: on any error it logs and
// leaves the wall-clock timeout as the remaining safety net.
func applyResourceLimits(p *os.Process, cfg SandboxConfig) {
	if p == nil {
		return
	}
	if cfg.MemoryLimit <= 0 && cfg.CPUQuota <= 0 {
		return
	}
	if !cgroupV2Available() {
		log.Warn("plugin sandbox: cgroup v2 unavailable, resource limits not enforced")
		return
	}

	dir := filepath.Join(cgroupBase, fmt.Sprintf("levee-plugin-%d", p.Pid))
	cleanup := func() { _ = os.Remove(dir) }

	if cfg.MemoryLimit > 0 {
		if err := writeCgroupFile(dir, "memory.max", fmt.Sprintf("%d", cfg.MemoryLimit)); err != nil {
			log.Warn("plugin sandbox: set memory.max failed", "pid", p.Pid, "error", err.Error())
			cleanup()
			return
		}
	}
	if cfg.CPUQuota > 0 {
		if err := writeCgroupFile(dir, "cpu.max", formatCpuMax(cfg.CPUQuota)); err != nil {
			log.Warn("plugin sandbox: set cpu.max failed", "pid", p.Pid, "error", err.Error())
			cleanup()
			return
		}
	}

	// Move the child into the cgroup. There is a small window between
	// Start and this write during which the child runs unconstrained;
	// closing it requires CLONE_INTO_CGROUP which os/exec does not yet
	// expose. The wall-clock timeout bounds that window's damage.
	if err := writeCgroupFile(dir, "cgroup.procs", fmt.Sprintf("%d", p.Pid)); err != nil {
		log.Warn("plugin sandbox: attach pid to cgroup failed", "pid", p.Pid, "error", err.Error())
		cleanup()
		return
	}
}

// formatCpuMax renders a cpu.max value ("$QUOTA $PERIOD") for the given
// CPU time budget. A 100ms period is used; quota above the period means
// multiple cores (e.g. 250ms/100ms = 2.5 cores) and is preserved.
func formatCpuMax(quota time.Duration) string {
	const periodUs = 100000 // 100ms
	quotaUs := quota.Microseconds()
	if quotaUs <= 0 {
		quotaUs = periodUs
	}
	return fmt.Sprintf("%d %d", quotaUs, periodUs)
}

// writeCgroupFile writes one line to a cgroup control file using a
// single write(2), as the kernel requires for internal files.
func writeCgroupFile(dir, file, content string) error {
	f, err := os.OpenFile(filepath.Join(dir, file), os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return fmt.Errorf("open %s: %w", file, err)
	}
	defer f.Close()
	if _, err := f.WriteString(content + "\n"); err != nil {
		return fmt.Errorf("write %s: %w", file, err)
	}
	return nil
}

// doTerminate sends SIGTERM to the plugin process (graceful request).
func doTerminate(p *os.Process) error {
	return p.Signal(syscall.SIGTERM)
}

// doKill sends SIGKILL to the plugin process.
func doKill(p *os.Process) error {
	return p.Kill()
}
