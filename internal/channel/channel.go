// Package channel defines the channel abstraction layer (CAL) for LEVEE.
//
// The CAL unifies the five transport classes that LEVEE must drive — SSH,
// WinRM, in-process agent, vendor REST API and interactive SSH — behind a
// single Channel interface. The upper orchestration layers (executor, batch,
// verify) program against the interface and remain oblivious to the concrete
// transport, while each transport implementation lives in its own sub-package
// (internal/channel/ssh, internal/channel/winrm, ...) and registers itself at
// init time through a ChannelFactory.
//
// The lifecycle of every channel follows the same four phases mandated by the
// design document (section 4.1.4):
//
//	建连 (Connect)  ->  执行 (Exec / Upload / Download)  ->  收证 (ExecResult)  ->  断开 (Close)
//
// All methods accept a context.Context so that callers can enforce timeouts and
// cancellation uniformly across transports. Implementations must respect ctx
// and return context.Canceled / context.DeadlineExceeded verbatim when the
// context expires before the operation completes.
package channel

import (
	"context"
	"fmt"
	"io"
	"sync"
	"time"
)

// --- Target ----------------------------------------------------------------

// Target describes a remote host that LEVEE needs to act on. It is the
// immutable addressing + credential portion of a channel; the channel itself
// is the live, stateful connection to a Target.
//
// The Type() string returned by implementations must match the key under which
// the corresponding ChannelFactory is registered in the ChannelRegistry (e.g.
// "ssh", "winrm", "agent", "api", "interactive").
type Target interface {
	// Host returns the target hostname or IP literal. It never includes a
	// port; use Port() for the port.
	Host() string

	// Port returns the TCP port to connect on. Zero means "use the transport
	// default" (22 for SSH, 5985/5986 for WinRM, ...).
	Port() int

	// Type returns the channel type identifier, e.g. "ssh" or "winrm". The
	// value is used to look up a ChannelFactory in the registry.
	Type() string

	// Credentials returns the credential reference to use when authenticating
	// against the target. Implementations should return a copy so that callers
	// cannot mutate the embedded credential.
	Credentials() CredentialRef
}

// CredentialRef is a transport-agnostic credential descriptor. Only the
// fields relevant to the chosen transport need to be populated:
//
//   - SSH with password auth:  Username + Password
//   - SSH with key auth:       Username + KeyPath (+ KeyPassphrase for encrypted keys)
//   - WinRM negotiate:         Username + Password
//   - Vendor API:              Username + Password (treated as token / secret)
//
// Plaintext credentials are kept here only in memory; they never enter the
// audit log, trace chain or persistent state. The state package stores them
// AES-GCM encrypted under the Credential record.
type CredentialRef struct {
	Username      string `json:"username"`
	Password      string `json:"password,omitempty"`
	KeyPath       string `json:"key_path,omitempty"`
	KeyPassphrase string `json:"key_passphrase,omitempty"`
}

// --- Channel ----------------------------------------------------------------

// Channel is the live connection to a Target. A Channel owns exactly one
// transport session; it is not safe to share a single Channel across goroutines
// unless the underlying transport documents otherwise (the SSH implementation
// does, via its connection pool; the WinRM implementation does not).
//
// Implementations must guarantee that Close is idempotent: calling it on an
// already-closed channel returns nil.
type Channel interface {
	// Connect establishes the transport session. It must be safe to call
	// Connect on an already-connected channel (the call is a no-op and returns
	// nil). When ctx is cancelled before the connection completes, Connect
	// must release any partial state and return ctx.Err().
	Connect(ctx context.Context) error

	// Exec runs a command on the target and returns its full result. The
	// command is executed non-interactively; interactive shells must be driven
	// through a dedicated interactive channel type. Exec blocks until the
	// command terminates or ctx is cancelled.
	Exec(ctx context.Context, cmd string) (*ExecResult, error)

	// Upload streams the content of r to remotePath on the target. The
	// implementation is responsible for choosing the underlying mechanism
	// (SCP / SFTP for SSH, WinRM put-file for WinRM, ...). Upload blocks until
	// the transfer completes or ctx is cancelled.
	Upload(ctx context.Context, remotePath string, content io.Reader) error

	// Download streams the content of remotePath back to the caller as an
	// io.Reader. The caller is responsible for closing the reader when done;
	// implementations should return a reader whose Close also releases any
	// transport resource held open during the transfer.
	Download(ctx context.Context, remotePath string) (io.Reader, error)

	// Close tears down the transport session and releases all underlying
	// resources. It is idempotent.
	Close() error

	// IsConnected reports whether the channel currently holds an active
	// transport session. The result is best-effort: a true value does not
	// guarantee that the next Exec will succeed (the peer may have dropped the
	// connection in the meantime), but a false value guarantees that Connect
	// must be called before any other method.
	IsConnected() bool
}

// ExecResult is the structured outcome of a single Exec call. It captures the
// three pieces of evidence that every LEVEE step must record for audit and
// verification: the process exit code, the combined stdout / stderr and the
// wall-clock duration.
type ExecResult struct {
	ExitCode int           `json:"exit_code"`
	Stdout   string        `json:"stdout"`
	Stderr   string        `json:"stderr"`
	Duration time.Duration `json:"duration_ms"`
}

// --- Factory & Registry -----------------------------------------------------

// ChannelFactory builds a Channel for a given Target. Each transport
// sub-package registers one factory under its type identifier (e.g. "ssh")
// so that the orchestrator can create channels without importing any concrete
// implementation.
type ChannelFactory interface {
	// Create returns a new, unconnected Channel for the given target. The
	// caller is responsible for invoking Connect before use.
	Create(target Target) (Channel, error)
}

// ChannelRegistry maps a channel type identifier (the string returned by
// Target.Type) to the ChannelFactory that builds channels of that type. It is
// the indirection that lets the upper layers program against the Channel
// interface while concrete transports plug in via init().
type ChannelRegistry struct {
	mu        sync.RWMutex
	factories map[string]ChannelFactory
}

// NewChannelRegistry returns an empty registry.
func NewChannelRegistry() *ChannelRegistry {
	return &ChannelRegistry{factories: make(map[string]ChannelFactory)}
}

// Register associates f with the given type identifier. Register is safe for
// concurrent use. Registering a second factory for the same type overwrites
// the previous registration; this is intentional so that tests can swap
// transports, but production code should register each type exactly once.
func (r *ChannelRegistry) Register(typ string, f ChannelFactory) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.factories[typ] = f
}

// Unregister removes the factory registered under typ, if any. It is primarily
// useful in tests to guarantee a clean slate between cases.
func (r *ChannelRegistry) Unregister(typ string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.factories, typ)
}

// Factory returns the factory registered under typ, or false when no factory
// is registered.
func (r *ChannelRegistry) Factory(typ string) (ChannelFactory, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	f, ok := r.factories[typ]
	return f, ok
}

// Create is a convenience helper that looks up the factory for target.Type()
// and delegates to it. It returns a typed error when no factory is registered
// so that callers can distinguish "unknown transport" from a factory-internal
// failure.
func (r *ChannelRegistry) Create(target Target) (Channel, error) {
	f, ok := r.Factory(target.Type())
	if !ok {
		return nil, fmt.Errorf("channel: no factory registered for type %q", target.Type())
	}
	return f.Create(target)
}

// Types returns the sorted list of registered type identifiers. It is intended
// for diagnostics (e.g. `levee version --verbose` listing available
// transports).
func (r *ChannelRegistry) Types() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]string, 0, len(r.factories))
	for k := range r.factories {
		out = append(out, k)
	}
	// Stable order for deterministic output.
	sortStrings(out)
	return out
}

// sortStrings is a tiny dependency-free insertion sort. The number of
// registered transports is tiny (≤ ~10) so we avoid pulling in sort.Slice and
// its reflect dependency.
func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j-1] > s[j]; j-- {
			s[j-1], s[j] = s[j], s[j-1]
		}
	}
}

// defaultRegistry is the process-wide registry used by Register / Factory /
// Create when the caller does not supply its own. It mirrors the singleton
// pattern used by the log package.
var defaultRegistry = NewChannelRegistry()

// DefaultRegistry returns the process-wide ChannelRegistry. Sub-packages call
// DefaultRegistry().Register("ssh", factory) from their init() to plug in.
func DefaultRegistry() *ChannelRegistry { return defaultRegistry }
