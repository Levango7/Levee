// Platform helpers for the plugin sandbox.
//
// This file contains the platform-specific implementations of resource
// limit application and process termination. The Unix variant lives in
// sandbox_unix.go and the Windows variant in sandbox_windows.go. This
// file holds the shared, platform-independent helpers.

package plugin

import (
	"os"
	"os/exec"
	"runtime"
)

// applyResourceLimits is a no-op stub on platforms that do not implement
// per-process resource limits. The concrete implementation is in
// sandbox_unix.go (POSIX rlimit) and sandbox_windows.go (job objects).
//
// The function is best-effort: it must never return an error and must
// never kill the process. When a limit cannot be applied the caller
// continues with only the wall-clock timeout as a safety net.

// terminateProcess asks the process to exit gracefully. On Unix it sends
// SIGTERM; on Windows it calls TerminateProcess (the platform has no
// equivalent of SIGTERM). It is best-effort.
func terminateProcess(p *os.Process) error {
	if p == nil {
		return nil
	}
	return doTerminate(p)
}

// killProcess forcefully kills the process. On Unix it sends SIGKILL; on
// Windows it calls TerminateProcess with exit code 1.
func killProcess(p *os.Process) error {
	if p == nil {
		return nil
	}
	return doKill(p)
}

// AppendHostEnv returns env with the host's environment appended. This is
// the default for plugin sub-processes so that they can find their shared
// libraries, TLS roots, etc. Callers who want a fully hermetic
// environment should pass env directly to NewSandbox without calling this
// helper.
func AppendHostEnv(env []string) []string {
	host := os.Environ()
	out := make([]string, 0, len(env)+len(host))
	out = append(out, env...)
	out = append(out, host...)
	return out
}

// runtimeOS is a tiny indirection so that tests can stub runtime.GOOS
// when exercising the platform dispatch logic. It is read once at
// package init time.
var runtimeOS = runtime.GOOS

// _ ensures exec is referenced even when the platform files do not use
// it directly, keeping the import stable across build tags.
var _ = exec.LookPath
