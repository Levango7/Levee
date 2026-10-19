// run_lifecycle_test.go pins the outbound webhook mirror of the run lifecycle
// funnel: the full stream must reach the notification channel, including the
// intermediate hops the chat room deliberately stays silent on, and the exact
// terminal status must survive in the metadata rather than being flattened.
package notify

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newMirrorHarness(t *testing.T) (*RunLifecycleMirror, *mockNotifier) {
	t.Helper()
	mgr := NewNotificationManager()
	n := newMock("webhook")
	require.NoError(t, mgr.Register(n))
	return NewRunLifecycleMirror(mgr, "webhook"), n
}

// waitForMessages blocks until the async delivery has appended want messages.
func waitForMessages(t *testing.T, n *mockNotifier, want int) []Message {
	t.Helper()
	require.Eventually(t, func() bool { return int(n.callCount()) >= want },
		2*time.Second, 5*time.Millisecond, "expected %d mirrored message(s)", want)
	return n.messages()
}

func TestRunLifecycleMirror_NamedMoments(t *testing.T) {
	m, n := newMirrorHarness(t)
	at := time.Now().UTC()

	m.OnTransition("run-1", "approved", "running", at)
	msgs := waitForMessages(t, n, 1)
	assert.Equal(t, string(TriggerRunStarted), msgs[0].Event)
	assert.Equal(t, "run-1", msgs[0].RunID)
	assert.Equal(t, []Recipient{Channel("webhook", "webhook")}, msgs[0].Recipients)
	assert.Equal(t, "approved", msgs[0].Metadata["from"])
	assert.Equal(t, "running", msgs[0].Metadata["to"])
	assert.Equal(t, "run_lifecycle", msgs[0].Metadata["scope"])
	assert.True(t, msgs[0].Timestamp.Equal(at), "the store-stamped time is kept, not now()")

	m.OnTransition("run-2", "running", "completed", at)
	msgs = waitForMessages(t, n, 2)
	assert.Equal(t, string(TriggerRunCompleted), msgs[1].Event)
}

func TestRunLifecycleMirror_TerminalVerdictKeepsTheExactStatus(t *testing.T) {
	// All three rollback verdicts are terminal but distinct. The event is the
	// generic run_failed; the precise status must survive in the metadata, the
	// same rule the assignment-result column is guarded by.
	for _, status := range []string{"failed", "rolled_back", "rolled_back_partial", "rollback_incomplete", "cancelled", "rejected", "interrupted"} {
		status := status
		t.Run(status, func(t *testing.T) {
			m, n := newMirrorHarness(t)
			m.OnTransition("run-3", "running", status, time.Now())
			msgs := waitForMessages(t, n, 1)
			assert.Equal(t, string(TriggerRunFailed), msgs[0].Event)
			assert.Equal(t, LevelError, msgs[0].Level)
			assert.Equal(t, status, msgs[0].Metadata["to"])
		})
	}
}

func TestRunLifecycleMirror_MirrorsIntermediateHopsAndDistinguishesResume(t *testing.T) {
	m, n := newMirrorHarness(t)

	// One transition per wait: NotifyAsync delivers on its own goroutine, so
	// issuing three calls up front would make the slice order a race. The
	// assertions are about the mapping, not about fan-in ordering.
	m.OnTransition("run-4", "draft", "planned", time.Now()) // not a named moment
	msgs := waitForMessages(t, n, 1)
	assert.Equal(t, string(TriggerRunStatusChanged), msgs[0].Event)
	assert.Equal(t, LevelInfo, msgs[0].Level)

	m.OnTransition("run-5", "running", "paused", time.Now()) // warning
	msgs = waitForMessages(t, n, 2)
	assert.Equal(t, string(TriggerPaused), msgs[1].Event)
	assert.Equal(t, LevelWarning, msgs[1].Level)

	m.OnTransition("run-6", "paused", "running", time.Now()) // resume, not a start
	msgs = waitForMessages(t, n, 3)
	assert.Equal(t, string(TriggerResumed), msgs[2].Event,
		"paused → running is a resume; naming it run_started would report a new run that never started")
}

func TestRunLifecycleMirror_GuardsProduceNoMessage(t *testing.T) {
	m, n := newMirrorHarness(t)

	m.OnTransition("", "a", "b", time.Now())      // no run id
	m.OnTransition("run-7", "x", "x", time.Now()) // no transition
	var nilMirror *RunLifecycleMirror
	nilMirror.OnTransition("run-8", "a", "b", time.Now())                             // nil receiver
	NewRunLifecycleMirror(nil, "webhook").OnTransition("run-9", "a", "b", time.Now()) // no manager

	time.Sleep(50 * time.Millisecond)
	assert.Equal(t, int64(0), n.callCount(), "none of the guarded calls may deliver")
}
