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
	"time"

	"github.com/nexus/levee/internal/channel"
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

// NewEngine returns an Engine with the given store and options applied.
// The store must be non-nil.
func NewEngine(store state.Store, opts ...Option) *Engine {
	e := &Engine{
		store:               store,
		registry:            channel.DefaultRegistry(),
		lockTTL:             DefaultLockTTL,
		maxParallelRuns:     DefaultMaxParallelRuns,
		rollbackConcurrency: DefaultRollbackConcurrency,
	}
	for _, opt := range opts {
		opt(e)
	}
	if e.maxParallelRuns < 1 {
		e.maxParallelRuns = DefaultMaxParallelRuns
	}
	e.sem = make(chan struct{}, e.maxParallelRuns)
	return e
}
