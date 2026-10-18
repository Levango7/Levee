// expiry.go is the missing driver behind approval.CheckExpiry. The expiry
// logic itself (scan pending records, settle those past ExpiresAt) shipped
// long before this file, but it had zero production callers: timeouts were
// declared in config, printed in docs, and never enforced — a timed-out
// approval read as pending forever. The sweeper closes that loop: serve
// starts one, and every tick settles whatever aged out since the last.
//
// The sweep is detection, not a gate: an approval that expires between two
// ticks stays pending for at most one interval. The apply-side gate still
// refuses to run without a live approving record, so a late sweep never
// lets an unapproved change through — it only delays the visible truth.

package approval

import (
	"context"
	"time"

	"github.com/nexus/levee/internal/log"
)

// ExpirySweeper periodically settles pending approvals whose ExpiresAt has
// passed, handing each settled batch to onExpired (nil skips the callback).
// A nil or zero-interval sweeper is disabled: Run returns immediately.
type ExpirySweeper struct {
	svc       *Service
	interval  time.Duration
	onExpired func(ctx context.Context, expired []Approval)
}

// NewExpirySweeper wires the sweep. interval <= 0 disables it (Run exits at
// once) — a deployment that wants no automatic expiry sets the interval to
// zero rather than hacking timeouts out of the config. onExpired may be nil
// (the settlement is still durable; only the notification is skipped).
func NewExpirySweeper(svc *Service, interval time.Duration, onExpired func(ctx context.Context, expired []Approval)) *ExpirySweeper {
	return &ExpirySweeper{svc: svc, interval: interval, onExpired: onExpired}
}

// SweepOnce performs a single pass: settle every timed-out pending approval
// and return the full records that were expired by this call (not just their
// IDs — every consumer needs at least the run id and the level to say
// anything useful about what happened). The callback fires here, once per
// batch, after the durable settlement: observers see post-state, never a
// preview.
func (s *ExpirySweeper) SweepOnce(ctx context.Context) ([]Approval, error) {
	ids, err := s.svc.CheckExpiry(ctx)
	if err != nil {
		return nil, err
	}
	if len(ids) == 0 {
		return nil, nil
	}
	expired := make([]Approval, 0, len(ids))
	for _, id := range ids {
		a, err := s.svc.Get(ctx, id)
		if err != nil {
			// The settlement is durable either way; a read failure must
			// not un-expire anything or abort the remaining records.
			log.Warn("approval expiry: read settled record failed", "id", id, "error", err)
			continue
		}
		if a == nil {
			continue
		}
		expired = append(expired, *a)
	}
	if s.onExpired != nil && len(expired) > 0 {
		s.onExpired(ctx, expired)
	}
	return expired, nil
}

// Run sweeps every interval until ctx is cancelled. Interval <= 0 disables
// the sweep entirely: Run logs and returns (serve wires the configured
// interval; a deployment opting out sets it to zero).
func (s *ExpirySweeper) Run(ctx context.Context) {
	if s == nil || s.interval <= 0 {
		log.Info("approval expiry sweep disabled (interval <= 0); timeouts will not fire")
		return
	}
	ticker := time.NewTicker(s.interval)
	defer ticker.Stop()
	log.Info("approval expiry sweep started", "interval", s.interval)
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if _, err := s.SweepOnce(ctx); err != nil {
				// One failed pass must not stop the next: a store hiccup
				// during a sweep would otherwise silence expiry forever.
				log.Warn("approval expiry sweep failed", "error", err)
			}
		}
	}
}
