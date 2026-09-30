package chatopsbridge

import (
	"context"
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
