//go:build linux

package plugin

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

// requireCgroupWritable skips the test unless cgroup v2 is available AND we
// can create subgroups under /sys/fs/cgroup (root or delegated container).
func requireCgroupWritable(t *testing.T) {
	t.Helper()
	if !cgroupV2Available() {
		t.Skip("cgroup v2 unavailable — skip cgroup sandbox tests")
	}
	probe := filepath.Join(cgroupBase, "levee-plugin-test-probe")
	if err := os.Mkdir(probe, 0o755); err != nil {
		t.Skipf("cannot create test cgroup at %s: %v — need cgroup-writable privileges", probe, err)
	}
	_ = os.Remove(probe)
}

// startSleep spawns a trivially quiet child whose PID is stable enough to
// move into a test cgroup.
func startSleep(t *testing.T) (pid int, kill func()) {
	t.Helper()
	cmd := exec.Command("sleep", "60")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start sleep: %v", err)
	}
	return cmd.Process.Pid, func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	}
}

// TestCgroupApplyLimits verifies applyResourceLimits creates the cgroup
// directory itself and writes memory.max / cpu.max / cgroup.procs correctly.
func TestCgroupApplyLimits(t *testing.T) {
	requireCgroupWritable(t)

	pid, kill := startSleep(t)
	defer kill()

	cfg := SandboxConfig{
		MemoryLimit: 1024 * 1024, // 1 MiB — content check only, no OOM expected
		CPUQuota:    50 * time.Millisecond,
	}
	applyResourceLimits(&os.Process{Pid: pid}, cfg)
	defer cleanupResources(&os.Process{Pid: pid})

	dir := cgroupDirFor(pid)

	memMax, err := os.ReadFile(filepath.Join(dir, "memory.max"))
	if err != nil {
		t.Errorf("memory.max not found after applyResourceLimits: %v", err)
	} else if got := string(memMax); got != "1048576\n" {
		t.Errorf("memory.max = %q, want %q", got, "1048576\n")
	}

	cpuMax, err := os.ReadFile(filepath.Join(dir, "cpu.max"))
	if err != nil {
		t.Errorf("cpu.max not found after applyResourceLimits: %v", err)
	} else if got := string(cpuMax); got != "50000 100000\n" {
		t.Errorf("cpu.max = %q, want %q", got, "50000 100000\n")
	}

	procs, err := os.ReadFile(filepath.Join(dir, "cgroup.procs"))
	if err != nil {
		t.Errorf("cgroup.procs not found: %v", err)
	} else if want := strconv.Itoa(pid) + "\n"; string(procs) != want {
		t.Errorf("cgroup.procs = %q, want %q", string(procs), want)
	}
}

// TestCgroupApplyLimitsNoMemory verifies CPU-only configuration writes
// cpu.max but leaves memory.max untouched.
func TestCgroupApplyLimitsNoMemory(t *testing.T) {
	requireCgroupWritable(t)

	pid, kill := startSleep(t)
	defer kill()

	applyResourceLimits(&os.Process{Pid: pid}, SandboxConfig{
		CPUQuota: 250 * time.Millisecond, // 2.5 cores
	})
	defer cleanupResources(&os.Process{Pid: pid})

	dir := cgroupDirFor(pid)
	cpuMax, err := os.ReadFile(filepath.Join(dir, "cpu.max"))
	if err != nil {
		t.Fatalf("cpu.max not found: %v", err)
	}
	if got := string(cpuMax); got != "250000 100000\n" {
		t.Errorf("cpu.max = %q, want %q", got, "250000 100000\n")
	}

	if _, err := os.ReadFile(filepath.Join(dir, "memory.max")); !os.IsNotExist(err) {
		t.Errorf("memory.max should not exist when MemoryLimit == 0 (err=%v)", err)
	}
}

// TestCgroupApplyLimitsNoQuota verifies an empty config has zero
// side-effects: no directory is created at all.
func TestCgroupApplyLimitsNoQuota(t *testing.T) {
	requireCgroupWritable(t)

	pid, kill := startSleep(t)
	defer kill()

	applyResourceLimits(&os.Process{Pid: pid}, SandboxConfig{})

	if _, err := os.Stat(cgroupDirFor(pid)); !os.IsNotExist(err) {
		t.Errorf("cgroup dir should not exist after empty-config apply (err=%v)", err)
	}
}

// TestCgroupCleanupResources verifies Stop's cleanup hook removes the
// group directory once the process is gone.
func TestCgroupCleanupResources(t *testing.T) {
	requireCgroupWritable(t)

	pid, kill := startSleep(t)
	proc := &os.Process{Pid: pid}

	applyResourceLimits(proc, SandboxConfig{CPUQuota: 50 * time.Millisecond})
	dir := cgroupDirFor(pid)
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("cgroup dir missing before cleanup: %v", err)
	}

	kill() // process must be gone for rmdir to succeed on a non-empty group
	// Wait briefly for the kernel to release the group.
	for i := 0; i < 50; i++ {
		cleanupResources(proc)
		if _, err := os.Stat(dir); os.IsNotExist(err) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Errorf("cgroup dir %s still present after cleanupResources loop", dir)
}

// TestCgroupApplyLimitsNilProcess verifies the nil guard.
func TestCgroupApplyLimitsNilProcess(t *testing.T) {
	applyResourceLimits(nil, SandboxConfig{MemoryLimit: 1024})
	cleanupResources(nil)
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
