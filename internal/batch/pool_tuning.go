// pool_tuning.go defines tunable parameters for connection pool sizing and
// concurrency control in LEVEE's batch execution engine (design doc section
// 4.4, MVP task W11-T090).
//
// The PoolTuningConfig struct exposes the key knobs that operators can adjust
// to trade off throughput, resource usage, and latency. Two factory
// constructors are provided:
//
//   - DefaultPoolTuningConfig: safe defaults for small-to-medium deployments
//     (up to ~50 targets).
//   - OptimizedPoolTuningConfig: aggressive settings for 100+ target
//     deployments with higher parallelism and larger batch sizes.
//
// Both configurations are validated by Validate, which enforces sane minimum
// and maximum bounds for every field.
package batch

import (
	"fmt"
	"time"
)

// PoolTuningConfig holds tunable parameters for connection pool and
// concurrency. It is consumed by the Controller and ConcurrencyManager to
// size internal pools and gates.
//
// All fields have sensible defaults provided by DefaultPoolTuningConfig.
// Operators may override individual fields and then call Validate to ensure
// the resulting config is within acceptable bounds.
type PoolTuningConfig struct {
	// MaxConnectionsPerTarget is the maximum number of simultaneous
	// connections LEVEE will open to a single target host. This caps
	// per-target resource usage and prevents a single slow host from
	// consuming all available connections.
	//
	// Default: 5. Range: [1, 50].
	MaxConnectionsPerTarget int

	// MaxConcurrentTargets is the maximum number of distinct targets that
	// may be processed concurrently across all batches. This is the global
	// concurrency cap enforced by the channel.Limiter.
	//
	// Default: 20. Range: [1, 500].
	MaxConcurrentTargets int

	// ConnectionTimeout is the maximum time to wait for a connection to
	// be established before failing the attempt. Shorter timeouts fail
	// fast on unreachable hosts; longer timeouts tolerate slow networks.
	//
	// Default: 30s. Range: [1s, 5min].
	ConnectionTimeout time.Duration

	// IdleTimeout is how long an idle connection remains in the pool
	// before being closed. Shorter idle timeouts free resources faster;
	// longer timeouts reduce reconnection overhead for bursty workloads.
	//
	// Default: 5min. Range: [10s, 30min].
	IdleTimeout time.Duration

	// BatchConcurrency is the upper bound on the number of goroutines
	// processing work items within a single batch. This is separate from
	// MaxConcurrentTargets: the latter caps the number of targets, while
	// BatchConcurrency caps the number of concurrent work items (steps)
	// per batch.
	//
	// Default: 10. Range: [1, 100].
	BatchConcurrency int

	// TraceBatchSize is the number of trace records accumulated before
	// flushing them to storage in a single write. Larger batches reduce
	// write amplification; smaller batches reduce latency to visibility.
	//
	// Default: 100. Range: [10, 10000].
	TraceBatchSize int
}

// DefaultPoolTuningConfig returns the default tuning configuration suitable
// for small-to-medium deployments (up to ~50 targets). The defaults are
// conservative to avoid overwhelming either the LEVEE host or the target
// hosts.
func DefaultPoolTuningConfig() *PoolTuningConfig {
	return &PoolTuningConfig{
		MaxConnectionsPerTarget: 5,
		MaxConcurrentTargets:    20,
		ConnectionTimeout:       30 * time.Second,
		IdleTimeout:             5 * time.Minute,
		BatchConcurrency:        10,
		TraceBatchSize:          100,
	}
}

// OptimizedPoolTuningConfig returns optimized settings for 100+ target
// deployments. It raises concurrency caps and batch sizes to maximise
// throughput when the LEVEE host has sufficient resources (CPU, memory,
// network bandwidth) and the target fleet is large.
func OptimizedPoolTuningConfig() *PoolTuningConfig {
	return &PoolTuningConfig{
		MaxConnectionsPerTarget: 10,
		MaxConcurrentTargets:    100,
		ConnectionTimeout:       15 * time.Second,
		IdleTimeout:             10 * time.Minute,
		BatchConcurrency:        50,
		TraceBatchSize:          1000,
	}
}

// Validate checks the tuning config for valid ranges. It returns an error
// describing the first invalid field, or nil if all fields are within bounds.
//
// The validation bounds are:
//
//	MaxConnectionsPerTarget: [1, 50]
//	MaxConcurrentTargets:    [1, 500]
//	ConnectionTimeout:       [1s, 5min]
//	IdleTimeout:             [10s, 30min]
//	BatchConcurrency:        [1, 100]
//	TraceBatchSize:          [10, 10000]
func (c *PoolTuningConfig) Validate() error {
	if c.MaxConnectionsPerTarget < 1 || c.MaxConnectionsPerTarget > 50 {
		return fmt.Errorf(
			"batch: MaxConnectionsPerTarget must be in [1, 50], got %d",
			c.MaxConnectionsPerTarget,
		)
	}
	if c.MaxConcurrentTargets < 1 || c.MaxConcurrentTargets > 500 {
		return fmt.Errorf(
			"batch: MaxConcurrentTargets must be in [1, 500], got %d",
			c.MaxConcurrentTargets,
		)
	}
	if c.ConnectionTimeout < 1*time.Second || c.ConnectionTimeout > 5*time.Minute {
		return fmt.Errorf(
			"batch: ConnectionTimeout must be in [1s, 5min], got %v",
			c.ConnectionTimeout,
		)
	}
	if c.IdleTimeout < 10*time.Second || c.IdleTimeout > 30*time.Minute {
		return fmt.Errorf(
			"batch: IdleTimeout must be in [10s, 30min], got %v",
			c.IdleTimeout,
		)
	}
	if c.BatchConcurrency < 1 || c.BatchConcurrency > 100 {
		return fmt.Errorf(
			"batch: BatchConcurrency must be in [1, 100], got %d",
			c.BatchConcurrency,
		)
	}
	if c.TraceBatchSize < 10 || c.TraceBatchSize > 10000 {
		return fmt.Errorf(
			"batch: TraceBatchSize must be in [10, 10000], got %d",
			c.TraceBatchSize,
		)
	}
	return nil
}

// ApplyOverrides returns a copy of the receiver with the given functional
// options applied. The receiver is not modified. Options are applied in
// order; later options override earlier ones. The result is validated before
// returning; an invalid combination produces an error.
func (c *PoolTuningConfig) ApplyOverrides(opts ...PoolTuningOption) (*PoolTuningConfig, error) {
	cp := *c // shallow copy
	for _, opt := range opts {
		opt(&cp)
	}
	if err := cp.Validate(); err != nil {
		return nil, err
	}
	return &cp, nil
}

// PoolTuningOption is a functional option for modifying a PoolTuningConfig.
type PoolTuningOption func(*PoolTuningConfig)

// WithMaxConnectionsPerTarget sets MaxConnectionsPerTarget.
func WithMaxConnectionsPerTarget(n int) PoolTuningOption {
	return func(c *PoolTuningConfig) { c.MaxConnectionsPerTarget = n }
}

// WithMaxConcurrentTargets sets MaxConcurrentTargets.
func WithMaxConcurrentTargets(n int) PoolTuningOption {
	return func(c *PoolTuningConfig) { c.MaxConcurrentTargets = n }
}

// WithConnectionTimeout sets ConnectionTimeout.
func WithConnectionTimeout(d time.Duration) PoolTuningOption {
	return func(c *PoolTuningConfig) { c.ConnectionTimeout = d }
}

// WithIdleTimeout sets IdleTimeout.
func WithIdleTimeout(d time.Duration) PoolTuningOption {
	return func(c *PoolTuningConfig) { c.IdleTimeout = d }
}

// WithBatchConcurrency sets BatchConcurrency.
func WithBatchConcurrency(n int) PoolTuningOption {
	return func(c *PoolTuningConfig) { c.BatchConcurrency = n }
}

// WithTraceBatchSize sets TraceBatchSize.
func WithTraceBatchSize(n int) PoolTuningOption {
	return func(c *PoolTuningConfig) { c.TraceBatchSize = n }
}
