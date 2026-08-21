// Package batch implements LEVEE's batch execution controller (design doc
// section 4.4, MVP task T025). Given an executable *plan.Plan and a
// caller-supplied ExecuteFunc, the Controller drives the plan's batches
// in order with explicit batch boundaries:
//
//   - Batches run sequentially: batch N+1 does not start until batch N
//     has fully completed (all its targets have returned).
//   - Targets within a batch run concurrently, capped by
//     batch.MaxConcurrency. A MaxConcurrency of zero or negative means
//     unlimited (every target in the batch launches at once).
//   - An optional inter-batch delay (InterBatchDelay) pauses the
//     controller between consecutive batches. The delay is observed
//     even when the previous batch produced no error.
//   - Error handling is configurable at two levels: the in-batch policy
//     (WithTargetErrorPolicy) decides whether a failing target aborts
//     the rest of its batch, and the inter-batch policy
//     (WithBatchErrorPolicy) decides whether a failing batch aborts the
//     remaining batches.
//
// The Controller is safe for concurrent use only when each goroutine
// drives its own Plan; a single Execute call is itself sequential across
// batches and must not be shared concurrently.
package batch

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/nexus/levee/internal/channel"
	"github.com/nexus/levee/internal/errors"
	"github.com/nexus/levee/internal/plan"
)

// --- Error policy ----------------------------------------------------------

// ErrorPolicy controls how the controller reacts to a failure.
type ErrorPolicy int

const (
	// PolicyContinue means the controller keeps executing the remaining
	// work after an error. For in-batch policy this lets the other
	// targets in the same batch finish; for inter-batch policy this
	// proceeds to the next batch.
	PolicyContinue ErrorPolicy = iota

	// PolicyAbort means the controller stops as soon as an error is
	// observed. For in-batch policy the failing target cancels the
	// remaining targets in its batch; for inter-batch policy the
	// failing batch prevents any later batch from starting.
	PolicyAbort
)

// String returns the policy name ("continue" or "abort").
func (p ErrorPolicy) String() string {
	switch p {
	case PolicyContinue:
		return "continue"
	case PolicyAbort:
		return "abort"
	default:
		return fmt.Sprintf("unknown(%d)", int(p))
	}
}

// --- Result types ----------------------------------------------------------

// StepResult is the outcome of executing a single PlanStep on a single
// target. Duration covers only the ExecuteFunc call.
type StepResult struct {
	// StepName is the name of the step that was executed.
	StepName string

	// Error is nil on success, or the error returned by ExecuteFunc.
	Error error

	// Duration is how long ExecuteFunc ran for this step.
	Duration time.Duration
}

// TargetResult is the outcome of executing every step of a batch on a
// single target. Steps run sequentially in plan order; when the in-batch
// error policy is PolicyAbort, execution stops at the first failing
// step and the remaining steps are skipped.
type TargetResult struct {
	// Target is the host name this result applies to.
	Target string

	// StepResults holds one entry per executed step, in plan order.
	StepResults []StepResult

	// Error is nil when every step succeeded, or the first step error
	// (wrapped with target and step context) when a step failed.
	Error error
}

// BatchResult is the outcome of executing one batch. Targets within the
// batch run concurrently; TargetResults preserves the plan's target
// order regardless of completion order.
type BatchResult struct {
	// BatchIndex is the 0-based batch ordinal, matching plan.Batch.Index.
	BatchIndex int

	// TargetResults holds one entry per target in the batch, in plan
	// order.
	TargetResults []TargetResult

	// Duration covers the whole batch: from first target start to last
	// target finish, including any inter-target serialisation.
	Duration time.Duration

	// Error is nil when every target succeeded, or the first target
	// error when at least one target failed.
	Error error
}

// --- Controller ------------------------------------------------------------

// ExecuteFunc is the caller-supplied work function. The Controller
// invokes it once per (batch, target, step) triple, in the order
// dictated by the plan. Implementations must respect ctx for
// cancellation and timeouts.
type ExecuteFunc func(ctx context.Context, batch plan.Batch, target string, step plan.PlanStep) error

// Controller orchestrates batch execution of a *plan.Plan. The zero
// value is not ready — use NewController.
//
// A Controller is configured once at construction time and may then
// drive any number of plans sequentially. Driving plans concurrently
// from the same Controller is not supported: each Execute call mutates
// no Controller state, but the configured *channel.Limiter (if any) is
// shared and must itself be concurrency-safe (it is).
type Controller struct {
	interBatchDelay   time.Duration
	batchErrorPolicy  ErrorPolicy // between batches
	targetErrorPolicy ErrorPolicy // within a batch (between targets / steps)
	limiter           *channel.Limiter
	manager           *ConcurrencyManager
}

// ControllerOption configures a Controller at construction time.
type ControllerOption func(*Controller)

// WithInterBatchDelay sets the delay applied between consecutive
// batches. A zero or negative duration means no delay. The delay is
// observed after every batch except the last one.
func WithInterBatchDelay(d time.Duration) ControllerOption {
	return func(c *Controller) { c.interBatchDelay = d }
}

// WithBatchErrorPolicy sets the inter-batch error policy: whether a
// failing batch aborts the remaining batches (PolicyAbort, the default)
// or lets them proceed (PolicyContinue).
func WithBatchErrorPolicy(p ErrorPolicy) ControllerOption {
	return func(c *Controller) { c.batchErrorPolicy = p }
}

// WithTargetErrorPolicy sets the in-batch error policy: whether a
// failing target aborts the remaining targets in its batch
// (PolicyAbort, the default) or lets them finish (PolicyContinue). The
// same policy also governs step-level behaviour inside a target: under
// PolicyAbort the first failing step stops the target's later steps,
// under PolicyContinue every step runs regardless.
func WithTargetErrorPolicy(p ErrorPolicy) ControllerOption {
	return func(c *Controller) { c.targetErrorPolicy = p }
}

// WithConcurrencyLimiter attaches an optional *channel.Limiter. When
// set, the controller calls Acquire("batch", target) before running
// each target and Release("batch", target) afterwards, giving the
// limiter a chance to enforce global / per-target caps on top of the
// batch-local MaxConcurrency. A nil limiter (the default) disables
// this extra tier.
//
// WithConcurrencyLimiter is the low-level integration point. For
// first-class context support, wait timeouts, and statistics prefer
// WithConcurrencyManager instead. When both are configured the
// manager takes precedence and the raw limiter is ignored.
func WithConcurrencyLimiter(l *channel.Limiter) ControllerOption {
	return func(c *Controller) { c.limiter = l }
}

// WithConcurrencyManager attaches an optional *ConcurrencyManager that
// wraps a *channel.Limiter with first-class context support, a
// configurable wait timeout, and statistics. When set, the controller
// calls Acquire(batchCtx, target) before running each target and the
// returned ReleaseFunc afterwards. A nil manager (the default) disables
// this tier.
//
// When both WithConcurrencyManager and WithConcurrencyLimiter are
// configured, the manager takes precedence and the raw limiter is
// ignored. This option is the recommended way to apply T015 limits to
// batch execution (MVP task W5-T026).
func WithConcurrencyManager(m *ConcurrencyManager) ControllerOption {
	return func(c *Controller) { c.manager = m }
}

// NewController returns a Controller configured with the given options.
// Defaults: no inter-batch delay, PolicyAbort for both error policies,
// no concurrency limiter.
func NewController(opts ...ControllerOption) *Controller {
	c := &Controller{
		batchErrorPolicy:  PolicyAbort,
		targetErrorPolicy: PolicyAbort,
	}
	for _, opt := range opts {
		opt(c)
	}
	return c
}

// Execute drives the plan's batches in order and returns one *BatchResult
// per executed batch. Batches are serial; targets within a batch are
// concurrent up to batch.MaxConcurrency. An optional inter-batch delay
// is observed between consecutive batches.
//
// Execution stops early when:
//   - ctx is cancelled: the in-flight batch is cancelled and the
//     partially-filled results up to that point are returned;
//   - a batch returns an error and the inter-batch policy is PolicyAbort:
//     no further batch is started.
//
// A nil plan or nil execFn produces a single BatchResult with BatchIndex
// -1 and a Fatal LE002 error, and no execution is attempted.
func (c *Controller) Execute(ctx context.Context, p *plan.Plan, execFn ExecuteFunc) []*BatchResult {
	if p == nil {
		return []*BatchResult{{
			BatchIndex: -1,
			Error:      errors.New(errors.LE002, "plan is nil", errors.Fatal),
		}}
	}
	if execFn == nil {
		return []*BatchResult{{
			BatchIndex: -1,
			Error:      errors.New(errors.LE002, "execFn is nil", errors.Fatal),
		}}
	}

	results := make([]*BatchResult, 0, len(p.Batches))
	for i, b := range p.Batches {
		// Refuse to start a new batch when the context is already gone.
		if err := ctx.Err(); err != nil {
			results = append(results, &BatchResult{
				BatchIndex: b.Index,
				Error:      fmt.Errorf("batch %d skipped before start: %w", b.Index, err),
			})
			return results
		}

		br := c.executeBatch(ctx, b, execFn)
		results = append(results, br)

		// Inter-batch error policy.
		if br.Error != nil && c.batchErrorPolicy == PolicyAbort {
			return results
		}

		// Inter-batch delay, except after the final batch.
		if i < len(p.Batches)-1 && c.interBatchDelay > 0 {
			select {
			case <-time.After(c.interBatchDelay):
			case <-ctx.Done():
				return results
			}
		}
	}
	return results
}

// executeBatch runs a single batch: targets concurrent up to
// MaxConcurrency, steps sequential per target. Returns a *BatchResult
// with TargetResults in plan target order.
func (c *Controller) executeBatch(ctx context.Context, b plan.Batch, execFn ExecuteFunc) *BatchResult {
	start := time.Now()
	br := &BatchResult{BatchIndex: b.Index}

	if len(b.Targets) == 0 {
		br.Duration = time.Since(start)
		return br
	}

	// In-batch cancellation: under PolicyAbort a failing target cancels
	// the rest of the batch. Under PolicyContinue the batch runs to
	// completion regardless.
	batchCtx, batchCancel := context.WithCancel(ctx)
	defer batchCancel()

	// Concurrency cap. Zero or negative means unlimited.
	maxConc := b.MaxConcurrency
	if maxConc <= 0 || maxConc > len(b.Targets) {
		maxConc = len(b.Targets)
	}
	sem := make(chan struct{}, maxConc)

	targetResults := make([]TargetResult, len(b.Targets))
	var wg sync.WaitGroup
	var failed int32 // atomic flag: at least one target failed

	for i, target := range b.Targets {
		wg.Add(1)
		go func(idx int, tgt string) {
			// Panic recovery: ensure wg.Done is always called and the
			// batch is marked as failed on panic.
			defer func() {
				if r := recover(); r != nil {
					atomic.StoreInt32(&failed, 1)
					targetResults[idx] = TargetResult{
						Target: tgt,
						Error:  fmt.Errorf("target %s panicked: %v", tgt, r),
					}
					if c.targetErrorPolicy == PolicyAbort {
						batchCancel()
					}
				}
				wg.Done()
			}()

			// Acquire the in-batch semaphore, respecting cancellation.
			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
			case <-batchCtx.Done():
				targetResults[idx] = TargetResult{
					Target: tgt,
					Error:  fmt.Errorf("target %s skipped: %w", tgt, batchCtx.Err()),
				}
				atomic.StoreInt32(&failed, 1)
				return
			}

			// Optional concurrency manager tier (preferred over the
			// raw limiter because it supports ctx and wait timeouts).
			if c.manager != nil {
				release, err := c.manager.Acquire(batchCtx, tgt)
				if err != nil {
					targetResults[idx] = TargetResult{
						Target: tgt,
						Error:  fmt.Errorf("concurrency acquire for target %s: %w", tgt, err),
					}
					atomic.StoreInt32(&failed, 1)
					if c.targetErrorPolicy == PolicyAbort {
						batchCancel()
					}
					return
				}
				defer release()
			} else if c.limiter != nil {
				// Fallback: raw limiter without ctx support.
				if err := c.limiter.Acquire("batch", tgt); err != nil {
					targetResults[idx] = TargetResult{
						Target: tgt,
						Error:  fmt.Errorf("limiter acquire for target %s: %w", tgt, err),
					}
					atomic.StoreInt32(&failed, 1)
					if c.targetErrorPolicy == PolicyAbort {
						batchCancel()
					}
					return
				}
				defer c.limiter.Release("batch", tgt)
			}

			tr := c.executeTarget(batchCtx, b, tgt, execFn)
			targetResults[idx] = tr

			if tr.Error != nil {
				atomic.StoreInt32(&failed, 1)
				if c.targetErrorPolicy == PolicyAbort {
					batchCancel()
				}
			}
		}(i, target)
	}
	wg.Wait()

	br.TargetResults = targetResults
	if atomic.LoadInt32(&failed) != 0 {
		// Report the first target error in plan order as the batch error.
		for _, tr := range targetResults {
			if tr.Error != nil {
				br.Error = tr.Error
				break
			}
		}
	}
	br.Duration = time.Since(start)
	return br
}

// executeTarget runs every step of the batch on a single target in plan
// order. Under PolicyAbort the first failing step stops the target's
// later steps; under PolicyContinue every step runs regardless and the
// first step error (if any) is still surfaced on the returned TargetResult.
func (c *Controller) executeTarget(ctx context.Context, b plan.Batch, target string, execFn ExecuteFunc) TargetResult {
	tr := TargetResult{Target: target}
	stepResults := make([]StepResult, 0, len(b.Steps))

	for _, step := range b.Steps {
		// Refuse to start a step when the context is already gone.
		if err := ctx.Err(); err != nil {
			tr.Error = fmt.Errorf("target %s step %q skipped: %w", target, step.Name, err)
			tr.StepResults = stepResults
			return tr
		}

		stepStart := time.Now()
		err := execFn(ctx, b, target, step)
		stepResults = append(stepResults, StepResult{
			StepName: step.Name,
			Error:    err,
			Duration: time.Since(stepStart),
		})

		if err != nil {
			// Record the first step error on the target result so the
			// batch-level aggregation can see it. Under PolicyAbort we
			// also stop the target's later steps; under PolicyContinue
			// every step still runs.
			if tr.Error == nil {
				tr.Error = fmt.Errorf("target %s step %q failed: %w", target, step.Name, err)
			}
			if c.targetErrorPolicy == PolicyAbort {
				tr.StepResults = stepResults
				return tr
			}
		}
	}

	tr.StepResults = stepResults
	return tr
}
