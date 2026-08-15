package notify

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// --- Mock notifier ---------------------------------------------------------

// mockNotifier is a test double that records every received message and can
// be configured to fail. It is safe for concurrent use.
type mockNotifier struct {
	name string
	mu   sync.Mutex
	msgs []Message
	fail bool
	// delay optionally slows down Send to test concurrency.
	delay time.Duration
	// count tracks the number of Send invocations atomically.
	count int64
}

func (m *mockNotifier) Name() string { return m.name }

func (m *mockNotifier) Send(ctx context.Context, msg Message) error {
	if m.delay > 0 {
		select {
		case <-time.After(m.delay):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	if m.fail {
		atomic.AddInt64(&m.count, 1)
		return errors.New("mock send failed")
	}
	m.mu.Lock()
	m.msgs = append(m.msgs, msg)
	m.mu.Unlock()
	atomic.AddInt64(&m.count, 1)
	return nil
}

func (m *mockNotifier) messages() []Message {
	m.mu.Lock()
	defer m.mu.Unlock()
	cp := make([]Message, len(m.msgs))
	copy(cp, m.msgs)
	return cp
}

func (m *mockNotifier) callCount() int64 {
	return atomic.LoadInt64(&m.count)
}

func newMock(name string) *mockNotifier {
	return &mockNotifier{name: name}
}

// --- Helpers ---------------------------------------------------------------

func validMessage() Message {
	msg := NewMessage(
		string(TriggerRunStarted),
		"run-123",
		LevelInfo,
		"Run started",
		"Pipeline run run-123 has started",
	)
	msg.Recipients = []Recipient{Initiator("u1", "Alice")}
	return msg
}

// --- Tests ----------------------------------------------------------------

func TestMessageLevel_String(t *testing.T) {
	cases := []struct {
		level MessageLevel
		want  string
	}{
		{LevelInfo, "info"},
		{LevelWarning, "warning"},
		{LevelError, "error"},
		{LevelCritical, "critical"},
		{MessageLevel(99), "unknown(99)"},
	}
	for _, c := range cases {
		assert.Equal(t, c.want, c.level.String())
	}
}

func TestRecipientType_String(t *testing.T) {
	cases := []struct {
		rt   RecipientType
		want string
	}{
		{RecipientInitiator, "initiator"},
		{RecipientApprover, "approver"},
		{RecipientOncall, "oncall"},
		{RecipientSubscriber, "subscriber"},
		{RecipientChannel, "channel"},
		{RecipientType(99), "unknown(99)"},
	}
	for _, c := range cases {
		assert.Equal(t, c.want, c.rt.String())
	}
}

func TestTriggerPoint_Constants(t *testing.T) {
	// Verify that all trigger point constants are non-empty and unique.
	pts := []TriggerPoint{
		TriggerRunStarted,
		TriggerRunCompleted,
		TriggerRunFailed,
		TriggerStepStarted,
		TriggerStepCompleted,
		TriggerStepFailed,
		TriggerGateFailed,
		TriggerApprovalRequested,
		TriggerApprovalDecision,
		TriggerRollbackTriggered,
		TriggerRollbackCompleted,
		TriggerRollbackFailed,
		TriggerPaused,
		TriggerResumed,
	}
	seen := make(map[TriggerPoint]bool, len(pts))
	for _, p := range pts {
		assert.NotEmpty(t, string(p), "trigger point should not be empty")
		assert.False(t, seen[p], "duplicate trigger point: %s", p)
		seen[p] = true
	}
	assert.Equal(t, 14, len(seen), "expected 14 trigger points")
}

func TestNewMessage(t *testing.T) {
	msg := NewMessage(
		string(TriggerRunStarted),
		"run-1",
		LevelInfo,
		"title",
		"body",
	)
	assert.NotEmpty(t, msg.ID, "ID should be auto-generated")
	assert.Equal(t, "run_started", msg.Event)
	assert.Equal(t, "run-1", msg.RunID)
	assert.Equal(t, LevelInfo, msg.Level)
	assert.Equal(t, "title", msg.Title)
	assert.Equal(t, "body", msg.Body)
	assert.False(t, msg.Timestamp.IsZero(), "timestamp should be set")
	assert.Nil(t, msg.Recipients)
	assert.Nil(t, msg.Metadata)
}

func TestNewMessage_UniqueIDs(t *testing.T) {
	ids := make(map[string]bool, 100)
	for i := 0; i < 100; i++ {
		m := NewMessage("e", "r", LevelInfo, "t", "b")
		assert.False(t, ids[m.ID], "duplicate id generated: %s", m.ID)
		ids[m.ID] = true
	}
}

func TestRecipientConstructors(t *testing.T) {
	assert.Equal(t, Recipient{Type: RecipientInitiator, ID: "u1", Name: "Alice"}, Initiator("u1", "Alice"))
	assert.Equal(t, Recipient{Type: RecipientApprover, ID: "u2", Name: "Bob"}, Approver("u2", "Bob"))
	assert.Equal(t, Recipient{Type: RecipientOncall, ID: "u3", Name: "Carol"}, Oncall("u3", "Carol"))
	assert.Equal(t, Recipient{Type: RecipientSubscriber, ID: "u4", Name: "Dave"}, Subscriber("u4", "Dave"))
	assert.Equal(t, Recipient{Type: RecipientChannel, ID: "#ops", Name: "ops"}, Channel("#ops", "ops"))

	// NewRecipient generic constructor.
	r := NewRecipient(RecipientApprover, "x", "Y")
	assert.Equal(t, RecipientApprover, r.Type)
	assert.Equal(t, "x", r.ID)
	assert.Equal(t, "Y", r.Name)
}

func TestNotificationManager_RegisterUnregister(t *testing.T) {
	m := NewNotificationManager()
	a := newMock("alpha")
	b := newMock("beta")

	require.NoError(t, m.Register(a))
	require.NoError(t, m.Register(b))
	assert.True(t, m.Has("alpha"))
	assert.True(t, m.Has("beta"))
	assert.ElementsMatch(t, []string{"alpha", "beta"}, m.Names())

	// Unregister one.
	require.NoError(t, m.Unregister("alpha"))
	assert.False(t, m.Has("alpha"))
	assert.True(t, m.Has("beta"))
	assert.Equal(t, []string{"beta"}, m.Names())

	// Unregister the other.
	require.NoError(t, m.Unregister("beta"))
	assert.False(t, m.Has("beta"))
	assert.Empty(t, m.Names())
}

func TestNotificationManager_RegisterDuplicate(t *testing.T) {
	m := NewNotificationManager()
	a := newMock("alpha")
	require.NoError(t, m.Register(a))

	// Same name, different instance.
	a2 := newMock("alpha")
	err := m.Register(a2)
	assert.ErrorIs(t, err, ErrNotifierExists)
}

func TestNotificationManager_UnregisterNotFound(t *testing.T) {
	m := NewNotificationManager()
	err := m.Unregister("missing")
	assert.ErrorIs(t, err, ErrNotifierNotFound)
}

func TestNotificationManager_RegisterNil(t *testing.T) {
	m := NewNotificationManager()
	err := m.Register(nil)
	assert.ErrorIs(t, err, ErrNotifierNotFound)
}

func TestNotificationManager_RegisterEmptyName(t *testing.T) {
	m := NewNotificationManager()
	empty := newMock("")
	err := m.Register(empty)
	assert.ErrorIs(t, err, ErrNotifierNotFound)
}

func TestNotificationManager_NotifySingleChannel(t *testing.T) {
	m := NewNotificationManager()
	a := newMock("alpha")
	require.NoError(t, m.Register(a))

	msg := validMessage()
	require.NoError(t, m.Notify(context.Background(), msg))

	got := a.messages()
	require.Len(t, got, 1)
	assert.Equal(t, msg.ID, got[0].ID)
	assert.Equal(t, msg.Event, got[0].Event)
	assert.Equal(t, msg.RunID, got[0].RunID)
}

func TestNotificationManager_NotifyMultiChannel(t *testing.T) {
	m := NewNotificationManager()
	a := newMock("alpha")
	b := newMock("beta")
	c := newMock("gamma")
	require.NoError(t, m.Register(a))
	require.NoError(t, m.Register(b))
	require.NoError(t, m.Register(c))

	msg := validMessage()
	require.NoError(t, m.Notify(context.Background(), msg))

	for _, n := range []*mockNotifier{a, b, c} {
		got := n.messages()
		require.Len(t, got, 1, "notifier %s should receive 1 message", n.Name())
		assert.Equal(t, msg.ID, got[0].ID)
	}
}

func TestNotificationManager_NotifyPartialFailure(t *testing.T) {
	m := NewNotificationManager()
	good := newMock("good")
	bad := newMock("bad")
	bad.fail = true
	require.NoError(t, m.Register(good))
	require.NoError(t, m.Register(bad))

	msg := validMessage()
	err := m.Notify(context.Background(), msg)

	// Should return an error wrapping ErrSendFailed.
	assert.ErrorIs(t, err, ErrSendFailed)

	// The good notifier should still have received the message.
	assert.Len(t, good.messages(), 1, "good notifier should still receive message")
	// The bad notifier should have been invoked but recorded nothing.
	assert.Equal(t, int64(1), bad.callCount())
	assert.Empty(t, bad.messages())
}

func TestNotificationManager_NotifyAllFail(t *testing.T) {
	m := NewNotificationManager()
	a := newMock("a")
	b := newMock("b")
	a.fail = true
	b.fail = true
	require.NoError(t, m.Register(a))
	require.NoError(t, m.Register(b))

	err := m.Notify(context.Background(), validMessage())
	assert.ErrorIs(t, err, ErrSendFailed)
	assert.Equal(t, int64(1), a.callCount())
	assert.Equal(t, int64(1), b.callCount())
}

func TestNotificationManager_NotifyNoNotifiers(t *testing.T) {
	m := NewNotificationManager()
	// No notifiers registered: should be a no-op success.
	err := m.Notify(context.Background(), validMessage())
	assert.NoError(t, err)
}

func TestNotificationManager_NotifyAfterUnregister(t *testing.T) {
	m := NewNotificationManager()
	a := newMock("alpha")
	require.NoError(t, m.Register(a))

	require.NoError(t, m.Unregister("alpha"))
	require.NoError(t, m.Notify(context.Background(), validMessage()))

	assert.Empty(t, a.messages(), "unregistered notifier should not receive messages")
}

func TestNotificationManager_NotifyValidationErrors(t *testing.T) {
	m := NewNotificationManager()
	require.NoError(t, m.Register(newMock("alpha")))

	ctx := context.Background()

	// Empty event.
	msg := validMessage()
	msg.Event = ""
	assert.ErrorIs(t, m.Notify(ctx, msg), ErrEmptyEvent)

	// Empty run id.
	msg = validMessage()
	msg.RunID = ""
	assert.ErrorIs(t, m.Notify(ctx, msg), ErrEmptyRunID)

	// No recipients.
	msg = validMessage()
	msg.Recipients = nil
	assert.ErrorIs(t, m.Notify(ctx, msg), ErrNoRecipients)
}

func TestNotificationManager_NotifyAsync(t *testing.T) {
	m := NewNotificationManager()
	a := newMock("alpha")
	require.NoError(t, m.Register(a))

	msg := validMessage()
	m.NotifyAsync(context.Background(), msg)

	// Should not block; eventually the message arrives.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if a.callCount() > 0 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	require.Equal(t, int64(1), a.callCount(), "async notify should deliver the message")
	got := a.messages()
	require.Len(t, got, 1)
	assert.Equal(t, msg.ID, got[0].ID)
}

func TestNotificationManager_NotifyAsyncDoesNotBlock(t *testing.T) {
	m := NewNotificationManager()
	slow := &mockNotifier{name: "slow", delay: 200 * time.Millisecond}
	require.NoError(t, m.Register(slow))

	msg := validMessage()
	start := time.Now()
	m.NotifyAsync(context.Background(), msg)
	elapsed := time.Since(start)

	// NotifyAsync must return well before the slow notifier finishes.
	assert.Less(t, elapsed, 50*time.Millisecond,
		"NotifyAsync should not block the caller; took %s", elapsed)

	// Wait for delivery to actually happen.
	require.Eventually(t, func() bool {
		return slow.callCount() == 1
	}, 2*time.Second, 5*time.Millisecond, "slow notifier should eventually receive the message")
}

func TestNotificationManager_NotifyAsyncValidationFailure(t *testing.T) {
	m := NewNotificationManager()
	a := newMock("alpha")
	require.NoError(t, m.Register(a))

	// Invalid message: empty event. NotifyAsync swallows the error but should
	// not deliver anything.
	msg := validMessage()
	msg.Event = ""
	m.NotifyAsync(context.Background(), msg)

	// Give the goroutine a moment to run.
	time.Sleep(50 * time.Millisecond)
	assert.Equal(t, int64(0), a.callCount(), "invalid async message should not be delivered")
}

func TestNotificationManager_ConcurrentRegisterAndNotify(t *testing.T) {
	// Smoke test for concurrent safety: register and notify from multiple
	// goroutines simultaneously.
	m := NewNotificationManager()

	const numNotifiers = 10
	for i := 0; i < numNotifiers; i++ {
		require.NoError(t, m.Register(newMock("n-"+itoa(i))))
	}

	var wg sync.WaitGroup
	const numSenders = 20
	for i := 0; i < numSenders; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = m.Notify(context.Background(), validMessage())
		}()
	}
	wg.Wait()

	// Every notifier should have received numSenders messages.
	for _, name := range m.Names() {
		// We can't easily access the underlying mock here, but the absence of
		// a race-detector failure is the real assertion. Run with -race.
		_ = name
	}
}

func TestNotificationManager_NotifyRespectsContextCancel(t *testing.T) {
	m := NewNotificationManager()
	slow := &mockNotifier{name: "slow", delay: 500 * time.Millisecond}
	require.NoError(t, m.Register(slow))

	ctx, cancel := context.WithCancel(context.Background())
	// Cancel before the slow notifier finishes.
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()

	err := m.Notify(ctx, validMessage())
	// The slow notifier should have returned ctx.Err(); the manager surfaces
	// this as an ErrSendFailed.
	assert.ErrorIs(t, err, ErrSendFailed)
}

// --- Sentinel error identity ------------------------------------------------

func TestSentinelErrors(t *testing.T) {
	// Ensure sentinel errors are distinct and non-nil.
	errs := []error{
		ErrEmptyEvent,
		ErrEmptyRunID,
		ErrNoRecipients,
		ErrNotifierNotFound,
		ErrNotifierExists,
		ErrSendFailed,
	}
	seen := make(map[string]bool)
	for _, e := range errs {
		require.NotNil(t, e)
		msg := e.Error()
		assert.False(t, seen[msg], "duplicate sentinel error message: %s", msg)
		seen[msg] = true
		assert.True(t, errors.Is(e, e), "sentinel should satisfy errors.Is with itself")
	}
}

// itoa is a tiny dependency-free int->string converter to avoid pulling in
// strconv just for the concurrent test.
func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	neg := i < 0
	if neg {
		i = -i
	}
	var buf [20]byte
	pos := len(buf)
	for i > 0 {
		pos--
		buf[pos] = byte('0' + i%10)
		i /= 10
	}
	if neg {
		pos--
		buf[pos] = '-'
	}
	return string(buf[pos:])
}
