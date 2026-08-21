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
//
// SECURITY NOTE: On Windows this function does not enforce any resource
// limits, which means plugins can consume arbitrary CPU and memory.
// Production deployments on Windows should use external mechanisms
// (Job Objects, container isolation, or run plugins on Linux workers).
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

// doKill forcefully kills the process on Windows. If the process has
// already exited, p.Kill() returns "invalid argument" (or a similar
// os.SyscallError wrapping ERROR_INVALID_PARAMETER); we treat that as
// success so that Stop remains idempotent when the child exits on its
// own before Stop escalates to the hard kill.
func doKill(p *os.Process) error {
	if p == nil {
		return nil
	}
	err := p.Kill()
	if err == nil {
		return nil
	}
	// Windows returns "invalid argument" (ERROR_INVALID_PARAMETER) or
	// "The operation could not be completed" when the process has
	// already exited. Probe the process state and, if it is gone,
	// swallow the error so that callers see Stop as idempotent.
	if isProcessAlive(p.Pid) {
		return err
	}
	return nil
}

// stillActive is the exit code Windows reports for a process that has
// not yet exited. It is documented as STILL_ACTIVE (259) in the Win32
// API.
const stillActive = 259

// processQueryLimitedInformation is the Win32 access mask for querying
// a process's exit code without requiring full query access. It is
// defined here because the syscall package does not expose it on all
// supported Go versions. The value 0x1000 is
// PROCESS_QUERY_LIMITED_INFORMATION (available on Windows Vista+).
const processQueryLimitedInformation = 0x1000

// isProcessAlive reports whether the process with the given pid is still
// running. It opens the process for limited query access and reads its
// exit code: when the code equals STILL_ACTIVE the process is running.
// Any failure to open or query the process is treated as "not alive"
// so that callers fall through to the idempotent success path.
func isProcessAlive(pid int) bool {
	handle, err := syscall.OpenProcess(processQueryLimitedInformation, false, uint32(pid))
	if err != nil {
		return false
	}
	defer syscall.CloseHandle(handle)
	var exitCode uint32
	if err := syscall.GetExitCodeProcess(handle, &exitCode); err != nil {
		return false
	}
	return exitCode == stillActive
}
