// Package wiring assembles LEVEE's execution engine: it binds the
// plan/batch/lock/verify/rollback subsystems behind the gRPC EngineAdapter
// seam so production deployments (serve, CLI local mode) can execute real
// changes instead of tracking status only.
//
// Design (docs/design-engine-wiring.md):
//
//   - Plans are generated from the run's workflow source (the rendered YAML
//     stored in run.WorkflowName by InstantiateTemplate, or a workflow file
//     path set by CreateChange) and persisted as the canonical plan_json
//     artifact — Apply then executes exactly what was approved.
//   - Every execution builds a fresh per-run set of subsystem instances
//     (lock manager, gate manager, rollback manager, batch controller):
//     ClosureRunner is single-flight and must not be shared across runs.
//   - Step dispatch resolves each target through the inventory store,
//     expands its credential reference through an optional resolver and
//     dials a channel from an injectable registry (channel.DefaultRegistry
//     in production; a loopback registry in tests).
package wiring

import (
	"context"
	stderrors "errors"
	"fmt"
	"strings"
	"time"

	"github.com/nexus/levee/internal/channel"
	"github.com/nexus/levee/internal/engine"
	"github.com/nexus/levee/internal/state"

	// Register the built-in transports on channel.DefaultRegistry().
	_ "github.com/nexus/levee/internal/channel/ssh"
	_ "github.com/nexus/levee/internal/channel/winrm"
)

// CredentialResolver expands a target's stored credential reference (the
// credential name kept in state.Target.CredentialRef) into transport
// credentials. Backed by the encrypted credential store; nil disables
// resolution and channels are then dialled unauthenticated.
type CredentialResolver interface {
	ResolveTargetCredential(ctx context.Context, ref string) (*channel.CredentialRef, error)
}

// ExecutionLease is the fencing handle an executor holds for the
// duration of one run. It is the cluster-mode counterpart of "the
// process is still executing this run": the lease is renewed
// independently of step progress, and a failover takeover that observes
// it expired settles the run and invalidates every subsequent write by
// the old owner.
type ExecutionLease interface {
	// Owns re-validates AND renews the lease. Call it before every
	// structural write (step dispatch, evidence persistence); it returns
	// engine.ErrFencedOut (or an error wrapping it) once ownership is
	// lost — the caller must stop immediately without rolling back.
	Owns(ctx context.Context) error
	// Heartbeat extends the lease from the background renewal loop.
	Heartbeat(ctx context.Context) error
	// End releases the lease when the execution settles normally; a
	// no-op when ownership was already lost (zombie End is harmless).
	End(ctx context.Context) error
}

// ExecutionGuard issues execution leases. A nil guard (the default, and
// every single-node deployment) means fencing is disabled: executions
// proceed unregistered, exactly as before the cluster-failover work. A
// non-nil guard makes unleased execution IMPOSSIBLE — Begin failure
// refuses the run outright (a cluster-mode run without a lease would be
// an invisible takeover candidate).
type ExecutionGuard interface {
	// Begin claims the execution lease for runID; a non-nil error
	// (wrapping engine.ErrFencedOut when another live owner holds the
	// run) means the execution must not start.
	Begin(ctx context.Context, runID string) (ExecutionLease, error)
}

// Default tuning constants for the assembled engine.
const (
	// DefaultLockTTL bounds a held target lock; the per-run runner renews
	// nothing, so it must comfortably exceed a normal apply.
	DefaultLockTTL = 5 * time.Minute
	// DefaultMaxParallelRuns caps concurrently executing runs inside one
	// process; additional applies fast-fail rather than queue.
	DefaultMaxParallelRuns = 4
	// DefaultRollbackConcurrency bounds per-target rollback parallelism.
	DefaultRollbackConcurrency = 2
	// DefaultExecLeaseTTL is the execution-lease TTL used when a guard
	// is attached without an explicit TTL. Mirrors
	// cluster.DefaultExecLeaseTTL; the constant lives here too so the
	// single-node package does not import cluster just for a default.
	DefaultExecLeaseTTL = 30 * time.Second
)

// Engine is the assembled execution engine. It holds only shared,
// goroutine-safe dependencies (store, resolver, registry); per-run
// subsystems are built fresh for every apply. Construct with NewEngine.
type Engine struct {
	store    state.Store
	resolver CredentialResolver
	registry *channel.ChannelRegistry

	lockTTL             time.Duration
	maxParallelRuns     int
	rollbackConcurrency int
	gatePrometheusURL   string

	// guard issues execution leases in cluster mode; nil = fencing
	// disabled (single-node). execLeaseTTL is the lease lifetime the
	// guard issues/renews with; heartbeats run at TTL/3.
	guard        ExecutionGuard
	execLeaseTTL time.Duration
	execNodeID   string

	// sem is the process-wide parallel-run semaphore (capacity =
	// maxParallelRuns). Acquire is non-blocking: beyond the cap a run
	// fast-fails instead of queueing. Initialised in NewEngine after
	// options so WithMaxParallelRuns is honoured.
	sem chan struct{}
}

// Option customises an Engine (see With* helpers).
type Option func(*Engine)

// WithCredentialResolver attaches the credential resolver used to expand
// target credential references before dialling. Nil is valid (and the
// default): channels are then dialled unauthenticated.
func WithCredentialResolver(r CredentialResolver) Option {
	return func(e *Engine) { e.resolver = r }
}

// WithChannelRegistry overrides the channel registry (default:
// channel.DefaultRegistry(), which carries the ssh and winrm factories).
// Tests inject a loopback registry here.
func WithChannelRegistry(r *channel.ChannelRegistry) Option {
	return func(e *Engine) { e.registry = r }
}

// WithLockTTL overrides the per-run target-lock TTL.
func WithLockTTL(d time.Duration) Option {
	return func(e *Engine) { e.lockTTL = d }
}

// WithMaxParallelRuns overrides the concurrent-run cap (fast-fail beyond
// the cap). Must be > 0.
func WithMaxParallelRuns(n int) Option {
	return func(e *Engine) { e.maxParallelRuns = n }
}

// WithGatePrometheusURL supplies the Prometheus HTTP base URL used by
// declared slo verification gates. Empty (the default) keeps the existing
// fail-closed semantics: an slo gate without a Prometheus runtime fails
// materialisation instead of silently passing.
func WithGatePrometheusURL(url string) Option {
	return func(e *Engine) { e.gatePrometheusURL = url }
}

// WithExecutionGuard attaches the cluster-mode execution lease guard
// (fencing). Once attached, every execution must claim a lease before it
// dispatches a single step; Begin failure refuses the run outright.
// The node ID identifies this executor in the run_execution rows.
//
// The guard is automatically wrapped by SentinelAdapter: lease-loss
// errors from ANY guard implementation surface with engine.ErrFencedOut
// in their chain, because the closure's skip-rollback branch matches
// that sentinel. Forgetting the adapter at a composition root would
// silently degrade fencing into plain step failures (with rollback!) —
// the worst possible failure mode — so the wiring layer refuses to
// depend on the caller remembering it.
func WithExecutionGuard(guard ExecutionGuard, nodeID string) Option {
	return func(e *Engine) {
		e.guard = SentinelAdapter(guard)
		e.execNodeID = nodeID
	}
}

// WithExecLeaseTTL overrides the execution-lease TTL (default 30s).
// Renewal runs at TTL/3; the TTL bounds post-crash detection latency,
// NOT execution duration — slow steps are covered by renewals.
func WithExecLeaseTTL(d time.Duration) Option {
	return func(e *Engine) { e.execLeaseTTL = d }
}

// SentinelAdapter wraps a cluster-layer ExecutionGuard so every lease
// error it produces carries the ENGINE fencing sentinel
// (engine.ErrFencedOut) in its chain. The closure's skip-rollback
// branch matches that sentinel, so the wiring layer must guarantee it
// appears regardless of which guard implementation sits underneath —
// the cluster package defines its own cluster.ErrFencedOut for its SQL
// layer and the engine package its engine.ErrFencedOut for closure
// semantics; this adapter is the only place the two vocabularies meet.
func SentinelAdapter(guard ExecutionGuard) ExecutionGuard {
	if guard == nil {
		return nil
	}
	return sentinelGuard{guard}
}

type sentinelGuard struct{ inner ExecutionGuard }

func (g sentinelGuard) Begin(ctx context.Context, runID string) (ExecutionLease, error) {
	lease, err := g.inner.Begin(ctx, runID)
	if err != nil {
		return nil, wrapFence(err)
	}
	if lease == nil {
		return nil, nil
	}
	return sentinelLease{lease}, nil
}

type sentinelLease struct{ inner ExecutionLease }

func (l sentinelLease) Owns(ctx context.Context) error {
	if err := l.inner.Owns(ctx); err != nil {
		return wrapFence(err)
	}
	return nil
}

func (l sentinelLease) Heartbeat(ctx context.Context) error { return l.inner.Heartbeat(ctx) }

func (l sentinelLease) End(ctx context.Context) error { return l.inner.End(ctx) }

// wrapFence attaches engine.ErrFencedOut to err when it smells like a
// lease loss. The wiring layer cannot import the cluster package (it
// sits BELOW the composition root), so the cluster guard's own
// ErrFencedOut sentinel is matched by its stable message fragment
// "fenced out" — every cluster guard error path embeds it. Non-fencing
// errors (infrastructure failures) pass through untouched: they are not
// ownership verdicts and must not masquerade as one.
func wrapFence(err error) error {
	if err == nil {
		return nil
	}
	if stderrors.Is(err, engine.ErrFencedOut) {
		return err
	}
	if strings.Contains(err.Error(), "fenced out") {
		return fmt.Errorf("%w: %v", engine.ErrFencedOut, err)
	}
	return err
}

// NewEngine returns an Engine with the given store and options applied.
// The store must be non-nil.
func NewEngine(store state.Store, opts ...Option) *Engine {
	e := &Engine{
		store:               store,
		registry:            channel.DefaultRegistry(),
		lockTTL:             DefaultLockTTL,
		maxParallelRuns:     DefaultMaxParallelRuns,
		rollbackConcurrency: DefaultRollbackConcurrency,
		execLeaseTTL:        DefaultExecLeaseTTL,
	}
	for _, opt := range opts {
		opt(e)
	}
	if e.maxParallelRuns < 1 {
		e.maxParallelRuns = DefaultMaxParallelRuns
	}
	if e.execLeaseTTL <= 0 {
		e.execLeaseTTL = DefaultExecLeaseTTL
	}
	e.sem = make(chan struct{}, e.maxParallelRuns)
	return e
}
