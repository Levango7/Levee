// exec.go is the execution adapter: it turns the transport-agnostic
// rollback.ExecuteFunc callback (one step, one target) into a real remote
// dispatch — inventory lookup, credential expansion, channel dial (cached
// for the whole run), executor module invocation — and captures per-step
// evidence for the state layer.

package wiring

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"github.com/nexus/levee/internal/channel"
	"github.com/nexus/levee/internal/dsl"
	"github.com/nexus/levee/internal/executor"
	"github.com/nexus/levee/internal/rollback"
	"github.com/nexus/levee/internal/state"

	// Register the built-in executor modules on executor.DefaultExecutor().
	_ "github.com/nexus/levee/internal/executor/modules/file"
	_ "github.com/nexus/levee/internal/executor/modules/pkg"
	_ "github.com/nexus/levee/internal/executor/modules/shell"
	_ "github.com/nexus/levee/internal/executor/modules/svc"
	_ "github.com/nexus/levee/internal/executor/modules/user"
)

// maxErrOutputLen bounds how much stderr is echoed into the step error.
const maxErrOutputLen = 400

// stepOutput is the per-step evidence captured for persistence (exit code +
// streams). Keyed by host+step name, last call wins: a step re-executed
// within the same run (e.g. by rollback) overwrites its earlier capture.
type stepOutput struct {
	ExitCode int
	Stdout   string
	Stderr   string
}

// runExec is the run-scoped execution context: an inventory snapshot, the
// lazily dialled channels (shared across steps of the same target, closed
// together at the end of the run), the step output sink and — in cluster
// mode — the execution lease every step re-validates before dispatch. It
// is created per apply and is safe for the concurrent step dispatch the
// batch controller performs within a batch.
type runExec struct {
	e       *Engine
	targets map[string]*state.Target
	lease   ExecutionLease // nil in single-node mode (fencing disabled)

	mu      sync.Mutex
	chans   map[string]channel.Channel
	outputs map[string]stepOutput
}

// newRunExec snapshots the inventory for one execution run. lease may be
// nil (fencing disabled).
func newRunExec(ctx context.Context, e *Engine, lease ExecutionLease) (*runExec, error) {
	all, err := e.store.ListTargets(ctx, state.TargetFilter{})
	if err != nil {
		return nil, fmt.Errorf("wiring: inventory snapshot: %w", err)
	}
	byHost := make(map[string]*state.Target, len(all))
	for _, t := range all {
		if _, dup := byHost[t.Hostname]; !dup {
			byHost[t.Hostname] = t
		}
	}
	return &runExec{
		e:       e,
		targets: byHost,
		lease:   lease,
		chans:   make(map[string]channel.Channel),
		outputs: make(map[string]stepOutput),
	}, nil
}

// executeFunc returns the rollback.ExecuteFunc the ClosureRunner invokes
// for every (target, step) pair — both forward execution and rollback.
func (r *runExec) executeFunc() rollback.ExecuteFunc {
	return r.exec
}

// exec dispatches a single workflow step on a single target.
//
// Cluster-mode fencing gate: before any dispatch (forward or rollback
// undo), the execution lease is re-validated. Losing ownership returns
// an error chain wrapping engine.ErrFencedOut — the closure's batch
// controller propagates it as the step error, and closure.Run recognises
// the sentinel and SKIPS the automatic rollback (a fenced-out executor
// dispatching undo commands is the exact double-write fencing exists to
// prevent).
func (r *runExec) exec(ctx context.Context, host string, step dsl.Step) error {
	if r.lease != nil {
		if err := r.lease.Owns(ctx); err != nil {
			return fmt.Errorf("wiring: step %q on %q: %w", step.Name, host, err)
		}
	}
	ch, ct, err := r.channelFor(ctx, host)
	if err != nil {
		return fmt.Errorf("wiring: target %q: %w", host, err)
	}

	module := step.Module
	if module == "" {
		return fmt.Errorf("wiring: step %q on %q declares no module", step.Name, host)
	}
	out, err := executor.DefaultExecutor().Execute(ctx, module, step.Action, executor.ModuleInput{
		Action:  step.Action,
		Args:    step.Args,
		Target:  ct,
		Channel: ch,
	})
	if r.outputs != nil {
		r.mu.Lock()
		if out != nil {
			r.outputs[outputKey(host, step.Name)] = stepOutput{
				ExitCode: out.ExitCode, Stdout: out.Stdout, Stderr: out.Stderr,
			}
		} else {
			r.outputs[outputKey(host, step.Name)] = stepOutput{ExitCode: -1, Stderr: errString(err)}
		}
		r.mu.Unlock()
	}
	if err != nil {
		return fmt.Errorf("wiring: step %q on %q: %w", step.Name, host, err)
	}
	if out.ExitCode != 0 {
		return fmt.Errorf("wiring: step %q on %q exited with code %d: %s",
			step.Name, host, out.ExitCode, tail(out.Stderr, maxErrOutputLen))
	}
	return nil
}

// channelFor returns a connected channel for host, dialling on first use
// and re-dialling when a cached channel has lost its session. Channels are
// cached for the whole run so a multi-step workflow reuses one transport
// session per target.
func (r *runExec) channelFor(ctx context.Context, host string) (channel.Channel, channel.Target, error) {
	r.mu.Lock()
	if ch, ok := r.chans[host]; ok && ch.IsConnected() {
		r.mu.Unlock()
		ct, err := r.channelTarget(ctx, host)
		if err != nil {
			return nil, nil, err
		}
		return ch, ct, nil
	}
	r.mu.Unlock()

	// Dial outside the lock (connect may block on the network). A concurrent
	// dial of the same host is possible in this window; the loser's channel
	// is closed below so exactly one session survives per host.
	ct, err := r.channelTarget(ctx, host)
	if err != nil {
		return nil, nil, err
	}
	fresh, err := r.e.registry.Create(ct)
	if err != nil {
		return nil, nil, fmt.Errorf("dial: %w", err)
	}
	if err := fresh.Connect(ctx); err != nil {
		_ = fresh.Close()
		return nil, nil, fmt.Errorf("connect: %w", err)
	}

	r.mu.Lock()
	if ch, ok := r.chans[host]; ok && ch.IsConnected() {
		// Winner re-established the session while we were dialling.
		_ = fresh.Close()
		r.mu.Unlock()
		return ch, ct, nil
	}
	if stale, ok := r.chans[host]; ok {
		_ = stale.Close()
	}
	r.chans[host] = fresh
	r.mu.Unlock()
	return fresh, ct, nil
}

// channelTarget builds the transport descriptor for host, expanding the
// stored credential reference when a resolver is attached. A target with a
// credential ref but no resolver dials unauthenticated (same convention as
// CheckTarget probing) — surfaced in logs at engine construction.
func (r *runExec) channelTarget(ctx context.Context, host string) (channel.Target, error) {
	t, ok := r.targets[host]
	if !ok {
		return nil, fmt.Errorf("target %q is not registered in the inventory", host)
	}
	cred := channel.CredentialRef{}
	if t.CredentialRef != "" {
		if r.e.resolver == nil {
			// No resolver: dial unauthenticated, matching the probe path.
		} else {
			resolved, err := r.e.resolver.ResolveTargetCredential(ctx, t.CredentialRef)
			if err != nil {
				return nil, fmt.Errorf("resolve credential %q for %q: %w", t.CredentialRef, host, err)
			}
			if resolved != nil {
				cred = *resolved
			}
		}
	}
	return &liveTarget{t: t, cred: cred}, nil
}

// close releases every channel dialled during the run (idempotent per
// transport contract; errors are ignored — teardown only).
func (r *runExec) close() {
	r.mu.Lock()
	defer r.mu.Unlock()
	for host, ch := range r.chans {
		_ = ch.Close()
		delete(r.chans, host)
	}
}

// snapshotOutputs returns a copy of the captured step evidence.
func (r *runExec) snapshotOutputs() map[string]stepOutput {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make(map[string]stepOutput, len(r.outputs))
	for k, v := range r.outputs {
		out[k] = v
	}
	return out
}

// outputKey builds the sink key for one (host, step) capture point.
func outputKey(host, stepName string) string {
	return host + "\x00" + stepName
}

// liveTarget adapts an inventory record plus resolved credentials to the
// channel.Target interface.
type liveTarget struct {
	t    *state.Target
	cred channel.CredentialRef
}

func (l *liveTarget) Host() string { return l.t.Hostname }

func (l *liveTarget) Port() int { return l.t.Port }

func (l *liveTarget) Type() string {
	if l.t.ChannelType == "" {
		return "ssh"
	}
	return l.t.ChannelType
}

func (l *liveTarget) Credentials() channel.CredentialRef { return l.cred }

// errString renders err for evidence capture ("" when nil).
func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// tail returns at most the last max bytes of s, prefixing an ellipsis when
// truncation happened.
func tail(s string, max int) string {
	s = strings.TrimSpace(s)
	if len(s) <= max {
		return s
	}
	return "..." + s[len(s)-max:]
}
