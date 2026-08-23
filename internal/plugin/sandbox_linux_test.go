//go:build linux

package plugin

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

const testCGDir = "levee-plugin-test"

// TestCgroupApplyLimits verifies applyResourceLimits actually creates the
// cgroup directory and writes memory.max / cpu.max / cgroup.procs. It runs
// the sandbox on a no-op sleep so the PID is stable enough to move into
// the test cgroup. Skipped when /sys/fs/cgroup is not writable (non-root,
// unprivileged container, or cgroup v1-only system).
func TestCgroupApplyLimits(t *testing.T) {
	if !cgroupV2Available() {
		t.Skip("cgroup v2 unavailable — skip cgroup sandbox tests")
	}

	// We need write access to the cgroup base to create our test group.
	// If we can't write, skip rather than fail — CI often runs as non-root.
	base := filepath.Join(cgroupBase, testCGDir)
	if err := os.Mkdir(base, 0o755); err != nil {
		t.Skipf("cannot create test cgroup at %s: %v — need cgroup-writable privileges", base, err)
	}
	defer func() { _ = os.RemoveAll(base) }()

	// Spin up a trivial child that sleeps long enough for us to attach it.
	cmd := execCommandContext(globalCtx(), "sleep", "60")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start sleep: %v", err)
	}
	defer func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	}()

	p := cmd.Process

	// Apply both memory and CPU limits — memory is 1 MiB (trivially
	// exceeded by Go runtime, but the point here is that the file is
	// written correctly, not that the process OOMs).
	cfg := SandboxConfig{
		MemoryLimit: 1024 * 1024, // 1 MiB
		CPUQuota:    50 * time.Millisecond,
	}
	applyResourceLimits(p, cfg)

	// Verify memory.max
	memMax, err := os.ReadFile(filepath.Join(base, "memory.max"))
	if err != nil {
		t.Errorf("memory.max not found after applyResourceLimits: %v", err)
	} else if got := string(memMax); got != "1048576\n" {
		t.Errorf("memory.max = %q, want 1048576\\n", got)
	}

	// Verify cpu.max
	cpuMax, err := os.ReadFile(filepath.Join(base, "cpu.max"))
	if err != nil {
		t.Errorf("cpu.max not found after applyResourceLimits: %v", err)
	} else if got := string(cpuMax); got != "50000 100000\n" {
		t.Errorf("cpu.max = %q, want 50000 100000\\n", got)
	}

	// Verify the PID is in cgroup.procs
	procs, err := os.ReadFile(filepath.Join(base, "cgroup.procs"))
	if err != nil {
		t.Errorf("cgroup.procs not found: %v", err)
	} else if got := string(procs); got != fmt.Sprintf("%d\n", p.Pid) {
		t.Errorf("cgroup.procs = %q, want %q", got, fmt.Sprintf("%d\n", p.Pid))
	}
}

// TestCgroupApplyLimitsNoMemory verifies that when MemoryLimit == 0 only
// cpu.max is written (memory.max is left alone / not created by us).
func TestCgroupApplyLimitsNoMemory(t *testing.T) {
	if !cgroupV2Available() {
		t.Skip("cgroup v2 unavailable")
	}

	base := filepath.Join(cgroupBase, testCGDir+"-no-mem")
	if err := os.Mkdir(base, 0o755); err != nil {
		t.Skipf("cannot create test cgroup: %v", err)
	}
	defer func() { _ = os.RemoveAll(base) }()

	cmd := execCommandContext(globalCtx(), "sleep", "60")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start sleep: %v", err)
	}
	defer func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	}()

	cfg := SandboxConfig{
		CPUQuota: 250 * time.Millisecond, // 2.5 cores
	}
	applyResourceLimits(cmd.Process, cfg)

	cpuMax, err := os.ReadFile(filepath.Join(base, "cpu.max"))
	if err != nil {
		t.Fatalf("cpu.max not found: %v", err)
	}
	if got := string(cpuMax); got != "250000 100000\n" {
		t.Errorf("cpu.max = %q, want 250000 100000\\n", got)
	}

	// memory.max should NOT exist in our test group (we never wrote it).
	_, err = os.ReadFile(filepath.Join(base, "memory.max"))
	if err == nil {
		t.Error("memory.max should not have been created when MemoryLimit == 0")
	}
}

// TestCgroupApplyLimitsNoQuota verifies that when both MemoryLimit and
// CPUQuota are <= 0, no files are written and no cgroup side-effects occur.
func TestCgroupApplyLimitsNoQuota(t *testing.T) {
	if !cgroupV2Available() {
		t.Skip("cgroup v2 unavailable")
	}

	base := filepath.Join(cgroupBase, testCGDir+"-no-quota")
	if err := os.Mkdir(base, 0o755); err != nil {
		t.Skipf("cannot create test cgroup: %v", err)
	}
	defer func() { _ = os.RemoveAll(base) }()

	cmd := execCommandContext(globalCtx(), "sleep", "60")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start sleep: %v", err)
	}
	defer func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	}()

	// Empty config — nothing should be written.
	applyResourceLimits(cmd.Process, SandboxConfig{})

	// Verify the process was NOT moved into our (empty) cgroup dir.
	procs, err := os.ReadFile(filepath.Join(base, "cgroup.procs"))
	if err == nil {
		t.Errorf("cgroup.procs should be empty after no-quota apply; got %q", string(procs))
	}
}

// TestCgroupApplyLimitsNilProcess verifies the nil guard.
func TestCgroupApplyLimitsNilProcess(t *testing.T) {
	// Should not panic.
	applyResourceLimits(nil, SandboxConfig{MemoryLimit: 1024})
}

// TestFormatCpuMaxLinux directly exercises the Linux-specific helper.
func TestFormatCpuMaxLinux(t *testing.T) {
	cases := []struct {
		quota time.Duration
		want  string
	}{
		{quota: 50 * time.Millisecond, want: "50000 100000"},
		{quota: 100 * time.Millisecond, want: "100000 100000"},
		{quota: 250 * time.Millisecond, want: "250000 100000"},
		{quota: 0, want: "100000 100000"},
	}
	for _, tc := range cases {
		got := formatCpuMax(tc.quota)
		if got != tc.want {
			t.Errorf("formatCpuMax(%v) = %q, want %q", tc.quota, got, tc.want)
		}
	}
}
