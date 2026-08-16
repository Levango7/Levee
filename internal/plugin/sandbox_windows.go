//go:build windows

package plugin

import (
	"os"
	"syscall"
)

// applyResourceLimits applies the sandbox resource limits on Windows.
// Windows does not have per-process CPU/memory rlimits in the POSIX
// sense; the proper mechanism would be Job Objects. For portability and
// to keep the implementation dependency-free we skip hard enforcement
// here and rely on the wall-clock timeout enforced via context
// cancellation. Callers who need hard limits on Windows can wrap the
// plugin binary in a Job Object externally.
func applyResourceLimits(p *os.Process, cfg SandboxConfig) {
	// No-op on Windows in this build.
}

// doTerminate asks the process to exit on Windows. Windows has no
// SIGTERM equivalent; we use TerminateProcess with exit code 0 to
// signal a graceful request (the process cannot distinguish this from
// a hard kill, but the caller's grace period provides the back-off).
func doTerminate(p *os.Process) error {
	if p == nil {
		return nil
	}
	dll := syscall.NewLazyDLL("kernel32.dll")
	proc := dll.NewProc("TerminateProcess")
	handle, err := syscall.OpenProcess(syscall.PROCESS_TERMINATE, false, uint32(p.Pid))
	if err != nil {
		return err
	}
	defer syscall.CloseHandle(handle)
	r1, _, e := proc.Call(uintptr(handle), 0)
	if r1 == 0 {
		return e
	}
	return nil
}

// doKill forcefully kills the process on Windows.
func doKill(p *os.Process) error {
	if p == nil {
		return nil
	}
	return p.Kill()
}