// run_lifecycle.go is the outbound webhook half of the run lifecycle funnel.
//
// The funnel (wiring.RunEventStore) sees every successful run-status
// transition. The chat room projection is deliberately partial — start and the
// terminal statuses only, because a card per intermediate hop is noise — but
// that split was never meant to hide transitions from machines: a webhook
// consumer (change-management system, audit sink, status board) wants the full
// stream, including the hops the room stays silent on. This mirror delivers
// exactly that.
//
// It is a Notifier consumer, not a second funnel: it does not read the store
// and cannot fail a write. A missing notification transport means the mirror
// never exists (serve builds it only when notify.webhook is enabled); the run
// transition itself is unaffected either way.
package notify

import (
	"context"
	"fmt"
	"time"

	"github.com/nexus/levee/internal/log"
	"github.com/nexus/levee/internal/runstatus"
)

// RunLifecycleMirror projects every run-status transition onto the configured
// notification channels. It is safe for concurrent use (it holds no mutable
// state) and matches wiring.RunEventSink structurally, so serve can hand
// OnTransition to the run-event store directly without this package importing
// the wiring layer.
type RunLifecycleMirror struct {
	manager *NotificationManager
	channel string
}

// NewRunLifecycleMirror returns a mirror that delivers through manager. channel
// is the recipient channel named in the message (the webhook channel's
// registered name); it is metadata for consumers, not a routing key — the
// webhook POSTs to its configured URL regardless.
func NewRunLifecycleMirror(manager *NotificationManager, channel string) *RunLifecycleMirror {
	return &RunLifecycleMirror{manager: manager, channel: channel}
}

// OnTransition records one transition. It mirrors the whole stream: the three
// named lifecycle moments get their own event, a resume is distinguished from a
// start, pause gets a warning level, and every other hop is reported as
// run_status_changed with the exact from/to pair in the metadata.
//
// Delivery is asynchronous: the run-event sink contract forbids blocking the
// store write on external I/O, and a webhook POST with retries is exactly that.
// A failed delivery is logged by the manager; it never reaches the run.
func (m *RunLifecycleMirror) OnTransition(changeID, from, to string, at time.Time) {
	if m == nil || m.manager == nil || changeID == "" || from == to {
		return
	}
	event, level := runTransitionEvent(from, to)
	msg, err := NewMessage(string(event), changeID, level,
		fmt.Sprintf("Run %s: %s → %s", changeID, from, to),
		fmt.Sprintf("run %s transitioned from %s to %s", changeID, from, to))
	if err != nil {
		log.Warn("notify: build run lifecycle message failed",
			"change_id", changeID, "from", from, "to", to, "error", err)
		return
	}
	if !at.IsZero() {
		msg.Timestamp = at
	}
	msg.Recipients = []Recipient{Channel(m.channel, m.channel)}
	msg.Metadata = map[string]string{
		"run_id":    changeID,
		"from":      from,
		"to":        to,
		"event":     string(event),
		"scope":     "run_lifecycle",
		"timestamp": msg.Timestamp.UTC().Format(time.RFC3339),
	}
	m.manager.NotifyAsync(context.Background(), msg)
}

// runTransitionEvent maps a from/to pair onto the trigger point and severity a
// full-stream consumer sees. The order matters: a resume (paused → running)
// must be named before the generic running → run_started case, or it would read
// as a fresh start.
func runTransitionEvent(from, to string) (TriggerPoint, MessageLevel) {
	switch {
	case to == runstatus.StatusRunning && from == runstatus.StatusPaused:
		return TriggerResumed, LevelInfo
	case to == runstatus.StatusRunning:
		return TriggerRunStarted, LevelInfo
	case to == runstatus.StatusCompleted:
		return TriggerRunCompleted, LevelInfo
	case runstatus.IsTerminal(to):
		// Failure, rejected, cancelled, archived, and all three rollback
		// verdicts. The exact status stays in the message metadata; flattening
		// it to "failed" here would repeat the mistake the assignment-result
		// column is guarded against.
		return TriggerRunFailed, LevelError
	case to == runstatus.StatusPaused:
		return TriggerPaused, LevelWarning
	default:
		return TriggerRunStatusChanged, LevelInfo
	}
}
