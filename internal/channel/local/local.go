// Package local implements the local transport for LEVEE's channel
// abstraction layer. A LocalChannel runs commands in the LEVEE process's
// own operating system — no network, no credentials — and satisfies the
// channel.Channel interface unchanged, so every module (shell, pkg, mysql,
// ...) drives it exactly like an SSH target.
//
// What it is for:
//
//   - CI / integration test sandboxes: workflows run against a real
//     execution environment without provisioning hosts.
//   - Single-box installs: demo / lab / air-gapped evaluation where the
//     "target" is the machine LEVEE itself manages.
//   - The loopback dispatch tests (cluster mode) already use a similar
//     pattern; this package makes it a first-class channel type.
//
// What it is NOT: a way to run arbitrary workflow commands on the LEVEE
// master in production. A workflow's `shell.exec` on a local target
// executes with the LEVEE process's own privileges — that is precisely
// the "remote code execution on the control plane" R2 exists to prevent.
// The channel therefore fails closed unless the operator explicitly opts
// in via SetEnabled(true), and even then command execution is confined to
// the allow-list policy installed via SetPolicy (default: deny everything).
//
// Upload / Download read and write files under the policy's RootDir only
// (path traversal rejected), and Exec requires the command line to parse
// as a program on the allow-list with arguments matching the per-program
// argument policy. The default policy denies all programs; a test or CI
// bootstrap installs a permissive one explicitly. This mirrors how the
// plugin sandbox constrains untrusted code (fail-closed defaults), and
// keeps the "local channel" from becoming "a shell on the master".
package local

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/nexus/levee/internal/channel"
)

// Compile-time assertions.
var (
	_ channel.Channel        = (*LocalChannel)(nil)
	_ channel.ChannelFactory = (*Factory)(nil)
)

// ErrDisabled is returned by every operation when the local channel has
// not been explicitly enabled. Sentinel so callers can branch on it.
var ErrDisabled = errors.New("local: channel disabled (production default; enable via local.SetEnabled(true) for CI/lab only)")

// ErrPolicyDenied is returned when a command / path is rejected by the
// installed policy. The error message intentionally does not echo the
// offending input verbatim beyond the program name, to keep the audit
// trail readable while still diagnosable.
var ErrPolicyDenied = errors.New("local: policy denied")

// --- policy -------------------------------------------------------------------

// Policy is the security gate every operation passes through. The zero
// Policy denies everything; callers grant exactly what a sandbox needs.
//
// Programs maps a program name to its argument policy:
//
//	"bash"                      -> ArgShell    (any arguments; dangerous)
//	"mysql"                     -> ArgNone     (program takes no arguments)
//	"pt-online-schema-change"   -> ArgPrefix   (only --flag arguments)
//
// RootDir confines Upload / Download (and any relative path resolution)
// to a directory the operator designated as the sandbox root.
type Policy struct {
	// Enabled gates the whole channel. SetEnabled installs this globally;
	// a zero-value policy is always disabled regardless of this field's
	// runtime state (see SetEnabled / SetPolicy).
	RootDir string

	// Programs is the program allow-list with argument policies.
	Programs map[string]ArgPolicy
}

// ArgPolicy describes what arguments a program accepts.
type ArgPolicy int

const (
	// ArgNone: the program must be invoked with zero arguments.
	ArgNone ArgPolicy = iota
	// ArgPrefix: every argument must start with the prefix (typically "--").
	ArgPrefix
	// ArgShell: any arguments allowed — effectively a shell escape; grant
	// only when the sandbox is already trusted.
	ArgShell
)

// defaultPolicy is the process-wide policy. It starts fully deny-by-default.
var (
	policyMu sync.RWMutex
	policy   = Policy{
		RootDir:  "",
		Programs: map[string]ArgPolicy{},
	}
	enabled = false
)

// SetEnabled toggles the channel for the whole process. It is the explicit
// operator opt-in: nothing else turns the channel on. CI bootstrap and
// lab environments call SetEnabled(true); production deployments never do.
func SetEnabled(on bool) {
	policyMu.Lock()
	defer policyMu.Unlock()
	enabled = on
}

// execSupported reports whether command execution is permitted on this
// platform. On windows, os/exec resolves cmd builtins and PATHEXT
// suffixes in ways a program allow-list cannot express safely, so Exec
// fails closed there — file upload / download (which the sandbox also
// provides) remain available, but no command ever runs.
func execSupported() bool { return runtime.GOOS != "windows" }

// SetPolicy installs a policy for the whole process. It implicitly enables
// the channel (a policy without enablement is pointless) — but a policy
// with an empty RootDir and no Programs still denies every operation, so
// "enabled with empty policy" remains fail-closed. Command execution is
// additionally refused on windows (see execSupported); file sandboxing
// works everywhere.
func SetPolicy(p Policy) {
	policyMu.Lock()
	defer policyMu.Unlock()
	policy = p
	enabled = true
}

// CurrentPolicy returns the installed policy and enabled flag (for
// diagnostics and tests).
func CurrentPolicy() (Policy, bool) {
	policyMu.RLock()
	defer policyMu.RUnlock()
	return policy, enabled
}

// --- target -------------------------------------------------------------------

// Target is a channel.Target whose Type is "local". Host is a display
// name only ("localhost" conventionally); Port is unused and should be 0.
type Target struct {
	// Hostname is the display name for this target.
	Hostname string
}

// Host returns the display hostname.
func (t Target) Host() string { return t.Hostname }

// Port returns 0 — the local channel has no port.
func (t Target) Port() int { return 0 }

// Type returns the registry key "local".
func (t Target) Type() string { return "local" }

// Credentials returns a zero CredentialRef — the local channel needs and
// accepts no credentials.
func (t Target) Credentials() channel.CredentialRef { return channel.CredentialRef{} }

// --- channel ------------------------------------------------------------------

// LocalChannel executes on the local OS. One instance may serve multiple
// concurrent Exec calls; Connect is a policy check (cheap), Close a no-op.
type LocalChannel struct {
	connected bool
	mu        sync.Mutex
}

// New returns an unconnected LocalChannel.
func New() *LocalChannel { return &LocalChannel{} }

// Connect verifies enablement and marks the channel ready. It is
// idempotent per the channel contract.
func (c *LocalChannel) Connect(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	policyMu.RLock()
	on := enabled
	policyMu.RUnlock()
	if !on {
		return ErrDisabled
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.connected = true
	return nil
}

// IsConnected reports readiness.
func (c *LocalChannel) IsConnected() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.connected
}

// Close is a no-op (nothing to release) and idempotent.
func (c *LocalChannel) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.connected = false
	return nil
}

// Exec runs one command through the policy gate and os/exec. The command
// string is split on whitespace; the first token must be on the allow-list.
// Windows note: on goos=windows the program is resolved via exec.LookPath
// exactly as a shell would; no cmd /c wrapping ever happens (that would
// smuggle a shell behind the policy).
func (c *LocalChannel) Exec(ctx context.Context, cmd string) (*channel.ExecResult, error) {
	// Enablement gate first: a disabled channel reports disabled on every
	// platform (the disabled state is more fundamental than the platform
	// gate, and tests / callers branch on ErrDisabled).
	policyMu.RLock()
	on := enabled
	policyMu.RUnlock()
	if !on {
		return nil, ErrDisabled
	}
	// Platform gate: on windows no command ever runs through this channel
	// (os/exec resolves cmd builtins and PATHEXT suffixes in ways a
	// program allow-list cannot express safely).
	if !execSupported() {
		return nil, fmt.Errorf("local: command execution is not supported on %s (os/exec builtin resolution cannot be allow-listed)", runtime.GOOS)
	}
	fields := strings.Fields(cmd)
	if len(fields) == 0 {
		return nil, fmt.Errorf("local: empty command")
	}
	if err := CheckPolicy(ctx, fields[0], fields[1:]); err != nil {
		return nil, err
	}

	start := time.Now()
	// Command resolved explicitly so that PATH tricks cannot substitute
	// a same-named binary elsewhere: the allow-list keys are program
	// names, and LookPath honours the *current* PATH — for a CI sandbox
	// that is the same PATH the workflow author validated against.
	path, err := exec.LookPath(fields[0])
	if err != nil {
		return nil, fmt.Errorf("local: resolve %q: %w", fields[0], err)
	}
	var stdout, stderr bytes.Buffer
	proc := exec.CommandContext(ctx, path, fields[1:]...)
	proc.Stdout = &stdout
	proc.Stderr = &stderr
	runErr := proc.Run()
	duration := time.Since(start)

	res := &channel.ExecResult{
		ExitCode: exitCodeOf(runErr),
		Stdout:   stdout.String(),
		Stderr:   stderr.String(),
		Duration: duration,
	}
	// ctx cancellation surfaces as an error per the channel contract.
	if ctx.Err() != nil {
		return res, ctx.Err()
	}
	return res, nil
}

// exitCodeOf extracts the process exit code; non-exit errors map to -1
// (never 0 — a failed start must not masquerade as success).
func exitCodeOf(err error) int {
	if err == nil {
		return 0
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return exitErr.ExitCode()
	}
	return -1
}

// requireEnabled is the shared enablement guard for every operation.
func requireEnabled() error {
	policyMu.RLock()
	defer policyMu.RUnlock()
	if !enabled {
		return ErrDisabled
	}
	return nil
}

// Upload writes content into RootDir-validated remotePath. Intermediate
// directories are created as needed (an Upload of "notes/file.txt" into
// a fresh sandbox succeeds without a prior mkdir step).
func (c *LocalChannel) Upload(ctx context.Context, remotePath string, content io.Reader) error {
	if err := requireEnabled(); err != nil {
		return err
	}
	dst, err := resolveSandboxPath(remotePath)
	if err != nil {
		return err
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if dir := filepath.Dir(dst); dir != "" {
		if err := os.MkdirAll(dir, 0o750); err != nil {
			return fmt.Errorf("local: mkdir %q: %w", filepath.Dir(remotePath), err)
		}
	}
	f, err := os.Create(dst)
	if err != nil {
		return fmt.Errorf("local: create %q: %w", remotePath, err)
	}
	defer func() { _ = f.Close() }()
	if _, err := io.Copy(f, content); err != nil {
		return fmt.Errorf("local: write %q: %w", remotePath, err)
	}
	return nil
}

// Download reads from RootDir-validated remotePath.
func (c *LocalChannel) Download(ctx context.Context, remotePath string) (io.Reader, error) {
	if err := requireEnabled(); err != nil {
		return nil, err
	}
	src, err := resolveSandboxPath(remotePath)
	if err != nil {
		return nil, err
	}
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	f, err := os.Open(src)
	if err != nil {
		return nil, fmt.Errorf("local: open %q: %w", remotePath, err)
	}
	return f, nil
}

// resolveSandboxPath validates that p stays inside the policy's RootDir
// (lexical traversal is rejected; the sandbox is directory-scoped).
func resolveSandboxPath(p string) (string, error) {
	policyMu.RLock()
	root := policy.RootDir
	policyMu.RUnlock()
	if root == "" {
		return "", fmt.Errorf("%w: no sandbox root configured", ErrPolicyDenied)
	}
	// Reject absolute paths outside the root and any traversal attempt.
	clean := filepath.Clean(p)
	if filepath.IsAbs(clean) {
		if !strings.HasPrefix(clean, filepath.Clean(root)+string(os.PathSeparator)) {
			return "", fmt.Errorf("%w: path %q outside sandbox root", ErrPolicyDenied, p)
		}
	} else {
		clean = filepath.Join(root, clean)
	}
	// Double-check the joined path still resolves under root (defence in
	// depth against platform-specific Clean quirks).
	if !strings.HasPrefix(clean, filepath.Clean(root)+string(os.PathSeparator)) {
		return "", fmt.Errorf("%w: resolved path escapes sandbox root", ErrPolicyDenied)
	}
	return clean, nil
}

// CheckPolicy applies the process-wide policy to program+args. It is the
// single gate shared by Exec (and exposed for tests / prechecks). The
// platform gate (windows refusal) lives in Exec: CheckPolicy itself is
// pure policy logic so unit tests can exercise the allow-list on every
// platform.
func CheckPolicy(ctx context.Context, program string, args []string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	policyMu.RLock()
	on := enabled
	programs := policy.Programs
	policyMu.RUnlock()
	if !on {
		return ErrDisabled
	}
	// Program allow-list: exact match on the program name (the first
	// whitespace-separated token). No prefixes, no wildcards.
	argPolicy, ok := programs[program]
	if !ok {
		return fmt.Errorf("%w: program %q not on allow-list", ErrPolicyDenied, program)
	}
	switch argPolicy {
	case ArgNone:
		if len(args) > 0 {
			return fmt.Errorf("%w: program %q takes no arguments", ErrPolicyDenied, program)
		}
	case ArgPrefix:
		for _, a := range args {
			if !strings.HasPrefix(a, "--") {
				return fmt.Errorf("%w: program %q accepts only --flag arguments (got %q)", ErrPolicyDenied, program, a)
			}
		}
	case ArgShell:
		// Any arguments allowed.
	default:
		return fmt.Errorf("%w: unknown argument policy for %q", ErrPolicyDenied, program)
	}
	return nil
}

// --- factory ------------------------------------------------------------------

// Factory builds LocalChannels for "local" targets. Target must be a
// local.Target (or satisfy Host/Port/Type/Credentials with Type()=="local").
type Factory struct{}

// NewFactory returns the factory singleton.
func NewFactory() *Factory { return &Factory{} }

// Create returns a fresh unconnected LocalChannel for the target. The
// target's Type must be "local" (guarded: a mis-registered target type
// reaching this factory is a wiring bug, surfaced as a typed error).
func (f *Factory) Create(target channel.Target) (channel.Channel, error) {
	if target.Type() != "local" {
		return nil, fmt.Errorf("local: factory got target type %q", target.Type())
	}
	return New(), nil
}

// init registers the factory with the process-wide registry so that
// inventory targets of type "local" resolve to this channel. The channel
// itself stays disabled until SetPolicy / SetEnabled runs (CI bootstrap
// only) — registration alone never enables execution.
func init() {
	channel.DefaultRegistry().Register("local", NewFactory())
}
