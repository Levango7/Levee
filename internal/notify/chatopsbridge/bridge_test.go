package chatopsbridge

import (
	"context"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nexus/levee/internal/approval"
	"github.com/nexus/levee/internal/chatops"
)

// fakeBridgeBot is a minimal chatops.Bot capturing published events.
type fakeBridgeBot struct {
	name    string
	eventCh chan chatops.Event
}

func (b *fakeBridgeBot) Name() string                                                 { return b.name }
func (b *fakeBridgeBot) Platform() chatops.Platform                                   { return chatops.PlatformSlack }
func (b *fakeBridgeBot) Start(context.Context) error                                  { return nil }
func (b *fakeBridgeBot) Stop() error                                                  { return nil }
func (b *fakeBridgeBot) SubscribeEvents() <-chan chatops.Event                        { return b.eventCh }
func (b *fakeBridgeBot) PublishEvent(evt chatops.Event) error                         { b.eventCh <- evt; return nil }
func (b *fakeBridgeBot) SendMessage(context.Context, string, string) error            { return nil }
func (b *fakeBridgeBot) SendCard(context.Context, string, *chatops.Card) error        { return nil }
func (b *fakeBridgeBot) HandleMessage(context.Context, chatops.IncomingMessage) error { return nil }

func newBridge() (*ApprovalBridge, *fakeBridgeBot) {
	mgr := chatops.NewBotManager()
	bot := &fakeBridgeBot{name: "test-slack", eventCh: make(chan chatops.Event, 16)}
	if err := mgr.Register(bot); err != nil {
		panic(err)
	}
	return NewApprovalBridge(mgr), bot
}

func drainEvents(t *testing.T, ch <-chan chatops.Event, n int) []chatops.Event {
	t.Helper()
	out := make([]chatops.Event, 0, n)
	for i := 0; i < n; i++ {
		select {
		case evt := <-ch:
			out = append(out, evt)
		case <-time.After(2 * time.Second):
			t.Fatalf("timed out waiting for event %d of %d", i+1, n)
		}
	}
	return out
}

func TestBridgeOnApprovalCreatedBroadcasts(t *testing.T) {
	bridge, bot := newBridge()

	bridge.OnApprovalCreated(&approval.Approval{
		ID: "apr-1", RunID: "run-1", Level: "high",
		MinApprovers: 2, Status: approval.StatusPending,
	})

	events := drainEvents(t, bot.eventCh, 1)
	assert.Equal(t, chatops.EventApprovalRequested, events[0].Type)
	assert.Equal(t, "run-1", events[0].RunID)
	assert.Equal(t, "apr-1", events[0].ChangeID)
	assert.Equal(t, "high", events[0].Level)
	assert.Contains(t, events[0].Title, "Approval required")
	assert.Contains(t, events[0].Summary, "2 approver(s)")
}

func TestBridgeOnDecisionApprovedFinal(t *testing.T) {
	bridge, bot := newBridge()

	bridge.OnDecision(&approval.Approval{
		ID: "apr-2", RunID: "run-2", Level: "standard", Status: approval.StatusApproved,
		MinApprovers: 1,
		Decisions: []approval.Decision{
			{Approver: "alice", Action: approval.ActionApprove, At: time.Now()},
		},
	}, approval.ActionApprove)

	events := drainEvents(t, bot.eventCh, 1)
	assert.Equal(t, chatops.EventApprovalDecision, events[0].Type)
	assert.Contains(t, events[0].Title, "approved")
	assert.Contains(t, events[0].Summary, "approved by alice (1/1)")
	assert.Contains(t, events[0].Summary, "may proceed")
}

func TestBridgeOnDecisionPartialProgress(t *testing.T) {
	bridge, bot := newBridge()

	// First of two approvals: record stays pending.
	bridge.OnDecision(&approval.Approval{
		ID: "apr-3", RunID: "run-3", Level: "high", Status: approval.StatusPending,
		MinApprovers: 2,
		Decisions: []approval.Decision{
			{Approver: "bob", Action: approval.ActionApprove, At: time.Now()},
		},
	}, approval.ActionApprove)

	events := drainEvents(t, bot.eventCh, 1)
	assert.Equal(t, chatops.EventApprovalDecision, events[0].Type)
	assert.Contains(t, events[0].Summary, "1/2")
	assert.Contains(t, events[0].Summary, "waiting for more approvers")
}

func TestBridgeOnDecisionRejected(t *testing.T) {
	bridge, bot := newBridge()

	bridge.OnDecision(&approval.Approval{
		ID: "apr-4", RunID: "run-4", Level: "standard", Status: approval.StatusRejected,
		MinApprovers: 1,
		Decisions: []approval.Decision{
			{Approver: "carol", Action: approval.ActionReject, Reason: "not enough capacity", At: time.Now()},
		},
	}, approval.ActionReject)

	events := drainEvents(t, bot.eventCh, 1)
	assert.Contains(t, events[0].Title, "rejected")
	assert.Contains(t, events[0].Summary, "one-vote veto")
}

func TestBridgeNilManagerIsNoOp(t *testing.T) {
	// A nil BotManager must not panic — the bridge is wired in serve where
	// ChatOps may be disabled entirely.
	bridge := NewApprovalBridge(nil)
	bridge.OnApprovalCreated(&approval.Approval{ID: "x", RunID: "r"})
	bridge.OnDecision(&approval.Approval{ID: "x", RunID: "r"}, approval.ActionApprove)

	// Nil bridge receiver must also be safe.
	var nilBridge *ApprovalBridge
	nilBridge.OnApprovalCreated(nil)
	nilBridge.OnDecision(nil, approval.ActionApprove)
}

func TestBridgeNilApprovalIsNoOp(t *testing.T) {
	bridge, bot := newBridge()
	bridge.OnApprovalCreated(nil)
	bridge.OnDecision(nil, approval.ActionApprove)

	select {
	case evt := <-bot.eventCh:
		t.Fatalf("unexpected event: %+v", evt)
	default:
		// clean
	}
}

// memApprovalStore is a tiny in-memory approval.Store for the wiring pin
// test (the approval package's own mock stays private to its tests).
type memApprovalStore struct {
	mu   sync.Mutex
	recs map[string]*approval.Approval
}

func newMemApprovalStore() *memApprovalStore {
	return &memApprovalStore{recs: make(map[string]*approval.Approval)}
}

func (s *memApprovalStore) Create(_ context.Context, a *approval.Approval) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	cp := *a
	s.recs[a.ID] = &cp
	return nil
}

func (s *memApprovalStore) Get(_ context.Context, id string) (*approval.Approval, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if a, ok := s.recs[id]; ok {
		cp := *a
		return &cp, nil
	}
	return nil, approval.ErrNotFound
}

func (s *memApprovalStore) Update(_ context.Context, a *approval.Approval) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	cp := *a
	s.recs[a.ID] = &cp
	return nil
}

func (s *memApprovalStore) UpdateIfPending(ctx context.Context, a *approval.Approval) (bool, error) {
	cur, err := s.Get(ctx, a.ID)
	if err != nil {
		return false, err
	}
	if cur.Status != approval.StatusPending {
		return false, nil
	}
	return true, s.Update(ctx, a)
}

func (s *memApprovalStore) ListPending(context.Context) ([]*approval.Approval, error) {
	return nil, nil
}

// ListByRun returns the run's records newest first, matching the ordering
// contract the real stores are adapted into.
func (s *memApprovalStore) ListByRun(_ context.Context, runID string) ([]*approval.Approval, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []*approval.Approval
	for _, a := range s.recs {
		if a.RunID == runID {
			cp := *a
			out = append(out, &cp)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].CreatedAt.After(out[j].CreatedAt)
		}
		return out[i].ID > out[j].ID
	})
	return out, nil
}

// TestObserverWiringPinsServiceHook pins the intended composition: the
// bridge installs cleanly as the service's DecisionObserver and fires on
// a real Approve call over an in-memory store.
func TestObserverWiringPinsServiceHook(t *testing.T) {
	bridge, bot := newBridge()
	svc := approval.NewService(newMemApprovalStore()).WithDecisionObserver(bridge.OnDecision)

	created, err := svc.Create(context.Background(), approval.CreateRequest{
		RunID: "run-9", Level: "standard",
	})
	require.NoError(t, err)
	bridge.OnApprovalCreated(created)

	require.NoError(t, svc.Approve(context.Background(), created.ID, "dave"))

	events := drainEvents(t, bot.eventCh, 2)
	assert.Equal(t, chatops.EventApprovalRequested, events[0].Type)
	assert.Equal(t, chatops.EventApprovalDecision, events[1].Type)
	assert.Contains(t, events[1].Summary, "approved by dave (1/1)")
}

// =========================================================================
// OnApprovalExpired — the expiry sweeper's broadcast
// =========================================================================

func TestBridgeOnApprovalExpiredBroadcasts(t *testing.T) {
	bridge, bot := newBridge()

	expires := time.Now().UTC().Add(-time.Hour)
	bridge.OnApprovalExpired(&approval.Approval{
		ID: "apr-7", RunID: "run-7", Level: "high", Status: approval.StatusExpired,
		ExpiresAt: expires,
	})

	events := drainEvents(t, bot.eventCh, 1)
	assert.Equal(t, chatops.EventApprovalExpired, events[0].Type)
	assert.Equal(t, "apr-7", events[0].ChangeID)
	assert.Equal(t, "run-7", events[0].RunID)
	assert.Equal(t, "high", events[0].Level)
	assert.Contains(t, events[0].Title, "Approval expired")
	assert.Contains(t, events[0].Summary, "Re-initiate approval")
	// The card the room renders must carry the message too, not just the
	// struct — render it and look for the headline.
	card := chatops.BuildCardForEvent(events[0])
	require.NotNil(t, card)
	assert.Contains(t, card.Title, "expired")
}

func TestBridgeOnApprovalExpiredZeroExpiryStillSaysSomething(t *testing.T) {
	bridge, bot := newBridge()

	// A record with a zero ExpiresAt can reach the sweeper only if the
	// deployment built it that way; the card must not render a zero-time.
	bridge.OnApprovalExpired(&approval.Approval{ID: "apr-8", RunID: "run-8", Level: "standard"})

	events := drainEvents(t, bot.eventCh, 1)
	assert.NotContains(t, events[0].Summary, "0001-01-01")
}

// =========================================================================
// OnRunTransition — the run event funnel's chatops half
// =========================================================================

func TestBridgeOnRunTransitionMapsLifecycle(t *testing.T) {
	bridge, bot := newBridge()
	at := time.Now().UTC()

	// Start, completion, and every non-completion terminal status map to
	// their own event; the card carries the real status either way.
	bridge.OnRunTransition("chg-1", "approved", "running", at)
	bridge.OnRunTransition("chg-1", "running", "completed", at)
	bridge.OnRunTransition("chg-2", "running", "failed", at)
	bridge.OnRunTransition("chg-3", "running", "rolled_back", at)
	bridge.OnRunTransition("chg-4", "running", "interrupted", at)

	events := drainEvents(t, bot.eventCh, 5)
	assert.Equal(t, chatops.EventRunStarted, events[0].Type)
	assert.Equal(t, "chg-1", events[0].ChangeID)

	assert.Equal(t, chatops.EventRunCompleted, events[1].Type)
	assert.Equal(t, "chg-1", events[1].ChangeID)

	assert.Equal(t, chatops.EventRunFailed, events[2].Type)
	assert.Equal(t, "failed", events[2].Status)

	assert.Equal(t, chatops.EventRunFailed, events[3].Type)
	assert.Equal(t, "rolled_back", events[3].Status, "a rolled-back run must not read as a clean completion")

	assert.Equal(t, chatops.EventRunFailed, events[4].Type)
	assert.Equal(t, "interrupted", events[4].Status)

	// The status card renders the transition, not a hardcoded verdict.
	card := chatops.BuildCardForEvent(events[3])
	require.NotNil(t, card)
}

func TestBridgeOnRunTransitionSkipsIntermediateHops(t *testing.T) {
	bridge, bot := newBridge()

	// The product decision: intermediate hops (paused, pending_approval,
	// back to running after a pause) stay off chat — a card per hop is
	// noise, and the actionable moments already have their own cards.
	bridge.OnRunTransition("chg-1", "completed", "paused", time.Now().UTC())
	bridge.OnRunTransition("chg-1", "paused", "pending_approval", time.Now().UTC())

	select {
	case evt := <-bot.eventCh:
		t.Fatalf("intermediate transition must not broadcast, got %v", evt.Type)
	case <-time.After(200 * time.Millisecond):
	}
}

func TestBridgeOnRunTransitionSkipsNoopAndNil(t *testing.T) {
	bridge, bot := newBridge()

	// from == to is not a transition.
	bridge.OnRunTransition("chg-1", "running", "running", time.Now().UTC())
	// Empty change id has nothing to name in a card.
	bridge.OnRunTransition("", "running", "failed", time.Now().UTC())

	select {
	case evt := <-bot.eventCh:
		t.Fatalf("no-op transition must not broadcast, got %v", evt.Type)
	case <-time.After(200 * time.Millisecond):
	}

	// Nil bridge and nil bot map must be no-ops, not panics.
	var nilBridge *ApprovalBridge
	assert.NotPanics(t, func() {
		nilBridge.OnRunTransition("chg-1", "running", "failed", time.Now().UTC())
		nilBridge.OnApprovalExpired(nil)
		bridge.OnApprovalExpired(nil)
	})
}
