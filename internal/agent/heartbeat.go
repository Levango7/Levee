package agent


import (
	"context"
	"sync/atomic"
	"time"

	"github.com/nexus/levee/internal/log"
)

// DefaultHeartbeatInterval is the period between heartbeat sends when
// the caller does not override it. 10 seconds is a compromise between
// master-side staleness detection (3 missed beats = 30s) and the
// background traffic an agent generates.
const DefaultHeartbeatInterval = 10 * time.Second

// HeartbeatMissThreshold is the number of consecutive missed heartbeats
// after which the master marks the agent offline. With the default 10s
// interval this gives a 30s failure-detection window.
const HeartbeatMissThreshold = 3

// Heartbeat is the periodic liveness payload sent by an agent to the
// master. It carries enough load information for the master to make
// scheduling decisions without having to query the agent synchronously.
type Heartbeat struct {
	// AgentID is the sending agent's unique identifier.
	AgentID string `json:"agent_id"`

	// Timestamp is the wall-clock time at which the heartbeat was
	// generated. The master uses it to detect stale agents.
	Timestamp time.Time `json:"timestamp"`

	// ActiveTasks is the number of tasks currently executing on the
	// agent. It is the same value as ExecutorStats.Active.
	ActiveTasks int `json:"active_tasks"`

	// CompletedTasks is the cumulative number of tasks the agent has
	// finished successfully since it started.
	CompletedTasks int64 `json:"completed_tasks"`

	// FailedTasks is the cumulative number of tasks that have failed
	// (non-zero exit or executor error) since startup.
	FailedTasks int64 `json:"failed_tasks"`

	// MaxConcurrent is the agent's configured concurrency limit. The
	// master uses it to compute the agent's spare capacity.
	MaxConcurrent int `json:"max_concurrent"`
}

// HeartbeatSender is the interface used by the heartbeat loop to deliver
// heartbeats to the master. The real implementation is a gRPC stream
// client; tests substitute an in-memory recorder.
type HeartbeatSender interface {
	// SendHeartbeat delivers hb to the master. It must be safe to call
	// concurrently with other sender methods. A non-nil error causes
	// the heartbeat loop to log a warning but does not stop the loop;
	// the master will mark the agent offline after the miss threshold.
	SendHeartbeat(ctx context.Context, hb Heartbeat) error
}

// heartbeatLoop runs the periodic heartbeat sender until ctx is
// cancelled. It is started by Agent.startHeartbeat and is intended to
// run in its own goroutine.
//
// The loop is deliberately simple: it does not implement jitter or
// exponential backoff. If the master is unreachable the heartbeat is
// dropped and the master's staleness detector will eventually mark the
// agent offline; the agent itself keeps trying so that it recovers
// automatically once the master comes back.
//
// stats is read atomically; the loop does not take a lock on the
// executor.
func (a *Agent) heartbeatLoop(ctx context.Context, interval time.Duration, sender HeartbeatSender, stats func() Heartbeat) {
	if interval <= 0 {
		interval = DefaultHeartbeatInterval
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	// Track consecutive send failures for logging.
	var missCount atomic.Int32

	for {
		select {
		case <-ctx.Done():
			log.InfoCtx(ctx, "agent: heartbeat loop stopped",
				"agent_id", a.ID, "reason", ctx.Err())
			return
		case <-ticker.C:
			hb := stats()
			hb.AgentID = a.ID
			hb.Timestamp = time.Now()
			if err := sender.SendHeartbeat(ctx, hb); err != nil {
				n := missCount.Add(1)
				log.WarnCtx(ctx, "agent: heartbeat send failed",
					"agent_id", a.ID, "miss", n, "err", err)
				continue
			}
			if missCount.Load() > 0 {
				missCount.Store(0)
			}
		}
	}
}

// buildHeartbeatFromStats translates an ExecutorStats snapshot into a
// Heartbeat payload. It is a free function so that tests can exercise
// the translation without an Agent.
func buildHeartbeatFromStats(agentID string, st ExecutorStats, maxConcurrent int) Heartbeat {
	return Heartbeat{
		AgentID:       agentID,
		Timestamp:     time.Now(),
		ActiveTasks:   int(st.Active),
		CompletedTasks: st.Completed,
		FailedTasks:   st.Failed,
		MaxConcurrent: maxConcurrent,
	}
}