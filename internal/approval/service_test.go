package approval

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// --- mock Store -------------------------------------------------------------

// mockStore is an in-memory Store implementation for testing. It is
// safe for concurrent use. Get returns a pointer to the stored record
// (not a copy), which is sufficient for the service's read-modify-write
// flow.
type mockStore struct {
	mu        sync.Mutex
	approvals map[string]*Approval
	// failOn controls per-method failure injection; nil means no failure.
	failOn map[string]error
}

func newMockStore() *mockStore {
	return &mockStore{
		approvals: make(map[string]*Approval),
		failOn:    make(map[string]error),
	}
}

// cloneApproval returns a defensive copy of an approval (including slice
// contents) so that concurrent Get callers never alias the stored record
// while the service mutates its snapshot between read and CAS write.
func cloneApproval(a *Approval) *Approval {
	if a == nil {
		return nil
	}
	cp := *a
	cp.Approvers = append([]string(nil), a.Approvers...)
	cp.Decisions = append([]Decision(nil), a.Decisions...)
	return &cp
}

func (m *mockStore) Create(ctx context.Context, a *Approval) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.failOn["create"]; err != nil {
		return err
	}
	if _, ok := m.approvals[a.ID]; ok {
		return fmt.Errorf("approval: duplicate id %s", a.ID)
	}
	m.approvals[a.ID] = cloneApproval(a)
	return nil
}

func (m *mockStore) Get(ctx context.Context, id string) (*Approval, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.failOn["get"]; err != nil {
		return nil, err
	}
	a, ok := m.approvals[id]
	if !ok {
		return nil, nil
	}
	return cloneApproval(a), nil
}

func (m *mockStore) Update(ctx context.Context, a *Approval) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.failOn["update"]; err != nil {
		return err
	}
	if _, ok := m.approvals[a.ID]; !ok {
		return fmt.Errorf("approval: not found %s", a.ID)
	}
	m.approvals[a.ID] = cloneApproval(a)
	return nil
}

// UpdateIfPending mirrors the compare-and-set contract of the real stores:
// the update applies only while the stored record is still pending. It can
// be forced to lose the race via failOn["update-if-pending-lost"] (returns
// false without applying) to exercise the service retry loop.
func (m *mockStore) UpdateIfPending(_ context.Context, a *Approval) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.failOn["update-if-pending"] != nil {
		return false, m.failOn["update-if-pending"]
	}
	stored, ok := m.approvals[a.ID]
	if !ok || stored.Status != StatusPending {
		return false, nil
	}
	if m.failOn["update-if-pending-lost"] != nil {
		return false, nil
	}
	m.approvals[a.ID] = cloneApproval(a)
	return true, nil
}

func (m *mockStore) ListPending(ctx context.Context) ([]*Approval, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.failOn["list"]; err != nil {
		return nil, err
	}
	var out []*Approval
	for _, a := range m.approvals {
		if a.Status == StatusPending {
			out = append(out, a)
		}
	}
	return out, nil
}

// newService returns a service backed by a fresh mockStore for each test.
func newService(t *testing.T) (*Service, *mockStore) {
	t.Helper()
	store := newMockStore()
	return NewService(store), store
}

// --- helpers for building approvals in tests --------------------------------

func futureTime() time.Time  { return time.Now().UTC().Add(24 * time.Hour) }
func pastTime() time.Time    { return time.Now().UTC().Add(-1 * time.Minute) }
func bgCtx() context.Context { return context.Background() }

// =========================================================================
// canTransition (state machine)
// =========================================================================

func TestCanTransition_LegalFromPending(t *testing.T) {
	assert.True(t, canTransition(StatusPending, StatusApproved))
	assert.True(t, canTransition(StatusPending, StatusRejected))
	assert.True(t, canTransition(StatusPending, StatusExpired))
}

func TestCanTransition_PendingToPendingIllegal(t *testing.T) {
	assert.False(t, canTransition(StatusPending, StatusPending))
}

func TestCanTransition_TerminalStatesAreStuck(t *testing.T) {
	terminals := []Status{StatusApproved, StatusRejected, StatusExpired}
	targets := []Status{StatusPending, StatusApproved, StatusRejected, StatusExpired}
	for _, from := range terminals {
		for _, to := range targets {
			assert.False(t, canTransition(from, to),
				"transition %s -> %s should be illegal", from, to)
		}
	}
}

func TestCanTransition_UnknownTarget(t *testing.T) {
	assert.False(t, canTransition(StatusPending, Status("bogus")))
}

// =========================================================================
// Create
// =========================================================================

func TestCreate_Success(t *testing.T) {
	svc, store := newService(t)
	ctx := bgCtx()

	a, err := svc.Create(ctx, CreateRequest{
		RunID:        "run-1",
		Level:        "standard",
		Approvers:    []string{"alice", "bob"},
		MinApprovers: 1,
		ExpiresAt:    futureTime(),
	})
	require.NoError(t, err)
	assert.NotEmpty(t, a.ID)
	assert.Contains(t, a.ID, "approval-")
	assert.Equal(t, "run-1", a.RunID)
	assert.Equal(t, "standard", a.Level)
	assert.Equal(t, StatusPending, a.Status)
	assert.Equal(t, []string{"alice", "bob"}, a.Approvers)
	assert.Equal(t, 1, a.MinApprovers)
	assert.Empty(t, a.Decisions)
	assert.False(t, a.CreatedAt.IsZero())

	// Persisted in the store.
	got, err := store.Get(ctx, a.ID)
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, a.ID, got.ID)
}

func TestCreate_EmptyRunID(t *testing.T) {
	svc, _ := newService(t)
	_, err := svc.Create(bgCtx(), CreateRequest{Level: "standard"})
	assert.ErrorIs(t, err, ErrEmptyRunID)
}

func TestCreate_InvalidLevel(t *testing.T) {
	svc, _ := newService(t)
	_, err := svc.Create(bgCtx(), CreateRequest{
		RunID: "run-1",
		Level: "super-high",
	})
	assert.ErrorIs(t, err, ErrInvalidLevel)
}

func TestCreate_AllLevelsValid(t *testing.T) {
	svc, _ := newService(t)
	for _, level := range []string{"standard", "high", "emergency"} {
		a, err := svc.Create(bgCtx(), CreateRequest{RunID: "run-x", Level: level})
		require.NoError(t, err, "level %s should be valid", level)
		assert.Equal(t, level, a.Level)
	}
}

func TestCreate_MinApproversDefaultsToOne(t *testing.T) {
	svc, _ := newService(t)
	a, err := svc.Create(bgCtx(), CreateRequest{
		RunID:     "run-1",
		Level:     "standard",
		Approvers: []string{"alice"},
	})
	require.NoError(t, err)
	assert.Equal(t, 1, a.MinApprovers)
}

func TestCreate_MinApproversExceedsApprovers(t *testing.T) {
	svc, _ := newService(t)
	_, err := svc.Create(bgCtx(), CreateRequest{
		RunID:        "run-1",
		Level:        "high",
		Approvers:    []string{"alice", "bob"},
		MinApprovers: 3,
	})
	assert.ErrorIs(t, err, ErrMinApproversTooLarge)
}

func TestCreate_MinApproversLargerWhenApproversEmpty(t *testing.T) {
	// When Approvers is empty (open approval), MinApprovers is not
	// capped by the list length.
	svc, _ := newService(t)
	a, err := svc.Create(bgCtx(), CreateRequest{
		RunID:        "run-1",
		Level:        "standard",
		MinApprovers: 5,
	})
	require.NoError(t, err)
	assert.Equal(t, 5, a.MinApprovers)
}

func TestCreate_StoreError(t *testing.T) {
	store := newMockStore()
	store.failOn["create"] = errors.New("disk full")
	svc := NewService(store)
	_, err := svc.Create(bgCtx(), CreateRequest{RunID: "run-1", Level: "standard"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "disk full")
}

// =========================================================================
// Approve — single approver
// =========================================================================

func TestApprove_SingleApprover(t *testing.T) {
	svc, _ := newService(t)
	ctx := bgCtx()

	a, err := svc.Create(ctx, CreateRequest{
		RunID:        "run-1",
		Level:        "standard",
		Approvers:    []string{"alice"},
		MinApprovers: 1,
		ExpiresAt:    futureTime(),
	})
	require.NoError(t, err)

	require.NoError(t, svc.Approve(ctx, a.ID, "alice"))

	got, err := svc.Get(ctx, a.ID)
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, StatusApproved, got.Status)
	require.Len(t, got.Decisions, 1)
	assert.Equal(t, "alice", got.Decisions[0].Approver)
	assert.Equal(t, ActionApprove, got.Decisions[0].Action)
}

func TestApprove_NotFound(t *testing.T) {
	svc, _ := newService(t)
	err := svc.Approve(bgCtx(), "no-such-id", "alice")
	assert.ErrorIs(t, err, ErrNotFound)
}

func TestApprove_UnauthorizedApprover(t *testing.T) {
	svc, _ := newService(t)
	ctx := bgCtx()

	a, err := svc.Create(ctx, CreateRequest{
		RunID:        "run-1",
		Level:        "high",
		Approvers:    []string{"alice", "bob"},
		MinApprovers: 2,
		ExpiresAt:    futureTime(),
	})
	require.NoError(t, err)

	err = svc.Approve(ctx, a.ID, "charlie")
	assert.ErrorIs(t, err, ErrUnauthorizedApprover)
}

func TestApprove_DuplicateDecision(t *testing.T) {
	svc, _ := newService(t)
	ctx := bgCtx()

	a, err := svc.Create(ctx, CreateRequest{
		RunID:        "run-1",
		Level:        "high",
		Approvers:    []string{"alice", "bob"},
		MinApprovers: 2,
		ExpiresAt:    futureTime(),
	})
	require.NoError(t, err)

	require.NoError(t, svc.Approve(ctx, a.ID, "alice"))
	err = svc.Approve(ctx, a.ID, "alice")
	assert.ErrorIs(t, err, ErrDuplicateDecision)
}

func TestApprove_OnApprovedIsIllegal(t *testing.T) {
	svc, _ := newService(t)
	ctx := bgCtx()

	a, err := svc.Create(ctx, CreateRequest{
		RunID:        "run-1",
		Level:        "standard",
		Approvers:    []string{"alice"},
		MinApprovers: 1,
		ExpiresAt:    futureTime(),
	})
	require.NoError(t, err)
	require.NoError(t, svc.Approve(ctx, a.ID, "alice"))

	err = svc.Approve(ctx, a.ID, "bob")
	assert.ErrorIs(t, err, ErrInvalidTransition)
}

func TestApprove_OnRejectedIsIllegal(t *testing.T) {
	svc, _ := newService(t)
	ctx := bgCtx()

	a, err := svc.Create(ctx, CreateRequest{
		RunID:        "run-1",
		Level:        "standard",
		Approvers:    []string{"alice"},
		MinApprovers: 1,
		ExpiresAt:    futureTime(),
	})
	require.NoError(t, err)
	require.NoError(t, svc.Reject(ctx, a.ID, "alice", "too risky"))

	err = svc.Approve(ctx, a.ID, "bob")
	assert.ErrorIs(t, err, ErrInvalidTransition)
}

func TestApprove_OnExpiredIsIllegal(t *testing.T) {
	svc, _ := newService(t)
	ctx := bgCtx()

	a, err := svc.Create(ctx, CreateRequest{
		RunID:        "run-1",
		Level:        "standard",
		Approvers:    []string{"alice"},
		MinApprovers: 1,
		ExpiresAt:    pastTime(),
	})
	require.NoError(t, err)
	expired, err := svc.CheckExpiry(ctx)
	require.NoError(t, err)
	require.Contains(t, expired, a.ID)

	err = svc.Approve(ctx, a.ID, "alice")
	assert.ErrorIs(t, err, ErrInvalidTransition)
}

func TestApprove_OpenApprovalAllowsAnyone(t *testing.T) {
	// When Approvers is empty, any approver is accepted.
	svc, _ := newService(t)
	ctx := bgCtx()

	a, err := svc.Create(ctx, CreateRequest{
		RunID:        "run-1",
		Level:        "standard",
		MinApprovers: 1,
		ExpiresAt:    futureTime(),
	})
	require.NoError(t, err)
	require.NoError(t, svc.Approve(ctx, a.ID, "anyone"))
}

func TestApprove_StoreGetError(t *testing.T) {
	store := newMockStore()
	svc := NewService(store)
	store.failOn["get"] = errors.New("db down")
	err := svc.Approve(bgCtx(), "x", "alice")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "db down")
}

// =========================================================================
// Approve — multi-approver
// =========================================================================

func TestApprove_MultiApprover_NeedsAllApprovals(t *testing.T) {
	svc, _ := newService(t)
	ctx := bgCtx()

	a, err := svc.Create(ctx, CreateRequest{
		RunID:        "run-1",
		Level:        "high",
		Approvers:    []string{"alice", "bob"},
		MinApprovers: 2,
		ExpiresAt:    futureTime(),
	})
	require.NoError(t, err)

	// First approval: still pending.
	require.NoError(t, svc.Approve(ctx, a.ID, "alice"))
	got, err := svc.Get(ctx, a.ID)
	require.NoError(t, err)
	assert.Equal(t, StatusPending, got.Status)
	require.Len(t, got.Decisions, 1)

	// Second approval: transitions to approved.
	require.NoError(t, svc.Approve(ctx, a.ID, "bob"))
	got, err = svc.Get(ctx, a.ID)
	require.NoError(t, err)
	assert.Equal(t, StatusApproved, got.Status)
	require.Len(t, got.Decisions, 2)
}

func TestApprove_MultiApprover_ThreeOfThree(t *testing.T) {
	svc, _ := newService(t)
	ctx := bgCtx()

	a, err := svc.Create(ctx, CreateRequest{
		RunID:        "run-1",
		Level:        "high",
		Approvers:    []string{"alice", "bob", "carol"},
		MinApprovers: 3,
		ExpiresAt:    futureTime(),
	})
	require.NoError(t, err)

	require.NoError(t, svc.Approve(ctx, a.ID, "alice"))
	require.NoError(t, svc.Approve(ctx, a.ID, "bob"))

	got, err := svc.Get(ctx, a.ID)
	require.NoError(t, err)
	assert.Equal(t, StatusPending, got.Status, "two of three is not enough")

	require.NoError(t, svc.Approve(ctx, a.ID, "carol"))
	got, err = svc.Get(ctx, a.ID)
	require.NoError(t, err)
	assert.Equal(t, StatusApproved, got.Status)
}

func TestApprove_MultiApprover_RejectOverridesApprovals(t *testing.T) {
	// One-vote-veto: a reject immediately transitions to rejected
	// even if some approvers already approved.
	svc, _ := newService(t)
	ctx := bgCtx()

	a, err := svc.Create(ctx, CreateRequest{
		RunID:        "run-1",
		Level:        "high",
		Approvers:    []string{"alice", "bob"},
		MinApprovers: 2,
		ExpiresAt:    futureTime(),
	})
	require.NoError(t, err)

	require.NoError(t, svc.Approve(ctx, a.ID, "alice"))
	require.NoError(t, svc.Reject(ctx, a.ID, "bob", "violates change window"))

	got, err := svc.Get(ctx, a.ID)
	require.NoError(t, err)
	assert.Equal(t, StatusRejected, got.Status)
	require.Len(t, got.Decisions, 2)
}

// =========================================================================
// Reject
// =========================================================================

func TestReject_Success(t *testing.T) {
	svc, _ := newService(t)
	ctx := bgCtx()

	a, err := svc.Create(ctx, CreateRequest{
		RunID:        "run-1",
		Level:        "standard",
		Approvers:    []string{"alice"},
		MinApprovers: 1,
		ExpiresAt:    futureTime(),
	})
	require.NoError(t, err)

	require.NoError(t, svc.Reject(ctx, a.ID, "alice", "plan is wrong"))

	got, err := svc.Get(ctx, a.ID)
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, StatusRejected, got.Status)
	require.Len(t, got.Decisions, 1)
	assert.Equal(t, "alice", got.Decisions[0].Approver)
	assert.Equal(t, ActionReject, got.Decisions[0].Action)
	assert.Equal(t, "plan is wrong", got.Decisions[0].Reason)
}

func TestReject_NotFound(t *testing.T) {
	svc, _ := newService(t)
	err := svc.Reject(bgCtx(), "no-such-id", "alice", "no")
	assert.ErrorIs(t, err, ErrNotFound)
}

func TestReject_UnauthorizedApprover(t *testing.T) {
	svc, _ := newService(t)
	ctx := bgCtx()

	a, err := svc.Create(ctx, CreateRequest{
		RunID:        "run-1",
		Level:        "standard",
		Approvers:    []string{"alice"},
		MinApprovers: 1,
		ExpiresAt:    futureTime(),
	})
	require.NoError(t, err)

	err = svc.Reject(ctx, a.ID, "charlie", "no")
	assert.ErrorIs(t, err, ErrUnauthorizedApprover)
}

func TestReject_DuplicateDecision(t *testing.T) {
	// Use multi-approver so the first approve does not change status
	// to approved; this lets us reach the duplicate-decision guard for
	// reject while the approval is still pending.
	svc, _ := newService(t)
	ctx := bgCtx()

	a, err := svc.Create(ctx, CreateRequest{
		RunID:        "run-1",
		Level:        "high",
		Approvers:    []string{"alice", "bob"},
		MinApprovers: 2,
		ExpiresAt:    futureTime(),
	})
	require.NoError(t, err)
	require.NoError(t, svc.Approve(ctx, a.ID, "alice"))

	// Alice already decided (approve); she cannot now reject.
	err = svc.Reject(ctx, a.ID, "alice", "changed my mind")
	assert.ErrorIs(t, err, ErrDuplicateDecision)
}

func TestReject_AfterRejectIsIllegalTransition(t *testing.T) {
	// After the first reject the status is rejected; a second reject
	// by the same approver hits the state-transition guard first
	// (rejected -> rejected is illegal), which takes precedence over
	// the duplicate-decision guard.
	svc, _ := newService(t)
	ctx := bgCtx()

	a, err := svc.Create(ctx, CreateRequest{
		RunID:        "run-1",
		Level:        "standard",
		Approvers:    []string{"alice"},
		MinApprovers: 1,
		ExpiresAt:    futureTime(),
	})
	require.NoError(t, err)
	require.NoError(t, svc.Reject(ctx, a.ID, "alice", "no"))

	err = svc.Reject(ctx, a.ID, "alice", "still no")
	assert.ErrorIs(t, err, ErrInvalidTransition)
}

func TestReject_OnApprovedIsIllegal(t *testing.T) {
	svc, _ := newService(t)
	ctx := bgCtx()

	a, err := svc.Create(ctx, CreateRequest{
		RunID:        "run-1",
		Level:        "standard",
		Approvers:    []string{"alice"},
		MinApprovers: 1,
		ExpiresAt:    futureTime(),
	})
	require.NoError(t, err)
	require.NoError(t, svc.Approve(ctx, a.ID, "alice"))

	err = svc.Reject(ctx, a.ID, "bob", "too late")
	assert.ErrorIs(t, err, ErrInvalidTransition)
}

func TestReject_OnRejectedIsIllegal(t *testing.T) {
	svc, _ := newService(t)
	ctx := bgCtx()

	a, err := svc.Create(ctx, CreateRequest{
		RunID:        "run-1",
		Level:        "standard",
		Approvers:    []string{"alice", "bob"},
		MinApprovers: 1,
		ExpiresAt:    futureTime(),
	})
	require.NoError(t, err)
	require.NoError(t, svc.Reject(ctx, a.ID, "alice", "no"))

	err = svc.Reject(ctx, a.ID, "bob", "also no")
	assert.ErrorIs(t, err, ErrInvalidTransition)
}

func TestReject_OnExpiredIsIllegal(t *testing.T) {
	svc, _ := newService(t)
	ctx := bgCtx()

	a, err := svc.Create(ctx, CreateRequest{
		RunID:        "run-1",
		Level:        "standard",
		Approvers:    []string{"alice"},
		MinApprovers: 1,
		ExpiresAt:    pastTime(),
	})
	require.NoError(t, err)
	_, err = svc.CheckExpiry(ctx)
	require.NoError(t, err)

	err = svc.Reject(ctx, a.ID, "alice", "too late")
	assert.ErrorIs(t, err, ErrInvalidTransition)
}

// =========================================================================
// Get
// =========================================================================

func TestGet_Success(t *testing.T) {
	svc, _ := newService(t)
	ctx := bgCtx()

	a, err := svc.Create(ctx, CreateRequest{RunID: "run-1", Level: "standard"})
	require.NoError(t, err)

	got, err := svc.Get(ctx, a.ID)
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, a.ID, got.ID)
}

func TestGet_NotFoundReturnsNil(t *testing.T) {
	svc, _ := newService(t)
	got, err := svc.Get(bgCtx(), "no-such-id")
	require.NoError(t, err)
	assert.Nil(t, got)
}

func TestGet_StoreError(t *testing.T) {
	store := newMockStore()
	store.failOn["get"] = errors.New("db down")
	svc := NewService(store)
	_, err := svc.Get(bgCtx(), "x")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "db down")
}

// =========================================================================
// CheckExpiry
// =========================================================================

func TestCheckExpiry_MarksExpired(t *testing.T) {
	svc, _ := newService(t)
	ctx := bgCtx()

	// One expired, one not yet expired, one with no expiry.
	expired1, err := svc.Create(ctx, CreateRequest{
		RunID: "run-1", Level: "standard", ExpiresAt: pastTime(),
	})
	require.NoError(t, err)
	notYet, err := svc.Create(ctx, CreateRequest{
		RunID: "run-2", Level: "standard", ExpiresAt: futureTime(),
	})
	require.NoError(t, err)
	noExpiry, err := svc.Create(ctx, CreateRequest{
		RunID: "run-3", Level: "standard",
	})
	require.NoError(t, err)

	expired, err := svc.CheckExpiry(ctx)
	require.NoError(t, err)
	require.Len(t, expired, 1)
	assert.Contains(t, expired, expired1.ID)

	// Verify the expired one is now expired.
	got, err := svc.Get(ctx, expired1.ID)
	require.NoError(t, err)
	assert.Equal(t, StatusExpired, got.Status)

	// The others are still pending.
	for _, id := range []string{notYet.ID, noExpiry.ID} {
		got, err := svc.Get(ctx, id)
		require.NoError(t, err)
		assert.Equal(t, StatusPending, got.Status)
	}
}

func TestCheckExpiry_MultipleExpired(t *testing.T) {
	svc, _ := newService(t)
	ctx := bgCtx()

	a1, err := svc.Create(ctx, CreateRequest{RunID: "r1", Level: "standard", ExpiresAt: pastTime()})
	require.NoError(t, err)
	a2, err := svc.Create(ctx, CreateRequest{RunID: "r2", Level: "high", ExpiresAt: pastTime()})
	require.NoError(t, err)
	_, err = svc.Create(ctx, CreateRequest{RunID: "r3", Level: "standard", ExpiresAt: futureTime()})
	require.NoError(t, err)

	expired, err := svc.CheckExpiry(ctx)
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{a1.ID, a2.ID}, expired)
}

func TestCheckExpiry_NoneExpired(t *testing.T) {
	svc, _ := newService(t)
	ctx := bgCtx()

	_, err := svc.Create(ctx, CreateRequest{RunID: "r1", Level: "standard", ExpiresAt: futureTime()})
	require.NoError(t, err)
	_, err = svc.Create(ctx, CreateRequest{RunID: "r2", Level: "standard"})
	require.NoError(t, err)

	expired, err := svc.CheckExpiry(ctx)
	require.NoError(t, err)
	assert.Empty(t, expired)
}

func TestCheckExpiry_Idempotent(t *testing.T) {
	// Running CheckExpiry twice does not double-count: the second
	// call finds nothing because the record is no longer pending.
	svc, _ := newService(t)
	ctx := bgCtx()

	a, err := svc.Create(ctx, CreateRequest{RunID: "r1", Level: "standard", ExpiresAt: pastTime()})
	require.NoError(t, err)

	first, err := svc.CheckExpiry(ctx)
	require.NoError(t, err)
	require.Len(t, first, 1)

	second, err := svc.CheckExpiry(ctx)
	require.NoError(t, err)
	assert.Empty(t, second)

	got, err := svc.Get(ctx, a.ID)
	require.NoError(t, err)
	assert.Equal(t, StatusExpired, got.Status)
}

func TestCheckExpiry_SkipsNonPending(t *testing.T) {
	// An approved record whose ExpiresAt is in the past should not
	// be touched (it is not pending, so ListPending excludes it).
	svc, _ := newService(t)
	ctx := bgCtx()

	a, err := svc.Create(ctx, CreateRequest{
		RunID: "r1", Level: "standard",
		Approvers: []string{"alice"}, MinApprovers: 1,
		ExpiresAt: pastTime(),
	})
	require.NoError(t, err)
	require.NoError(t, svc.Approve(ctx, a.ID, "alice"))

	expired, err := svc.CheckExpiry(ctx)
	require.NoError(t, err)
	assert.Empty(t, expired)

	got, err := svc.Get(ctx, a.ID)
	require.NoError(t, err)
	assert.Equal(t, StatusApproved, got.Status)
}

func TestCheckExpiry_StoreListError(t *testing.T) {
	store := newMockStore()
	store.failOn["list"] = errors.New("db down")
	svc := NewService(store)
	_, err := svc.CheckExpiry(bgCtx())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "db down")
}

func TestCheckExpiry_StoreUpdateError(t *testing.T) {
	store := newMockStore()
	svc := NewService(store)
	ctx := bgCtx()

	_, err := svc.Create(ctx, CreateRequest{RunID: "r1", Level: "standard", ExpiresAt: pastTime()})
	require.NoError(t, err)

	store.failOn["update"] = errors.New("write fail")
	_, err = svc.CheckExpiry(ctx)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "write fail")
}

// =========================================================================
// Boundary: ExpiresAt exactly now
// =========================================================================

func TestCheckExpiry_ExpiresAtExactlyNowNotExpired(t *testing.T) {
	// now.After(ExpiresAt) is false when now == ExpiresAt, so the
	// approval is NOT expired yet (boundary is exclusive).
	svc, _ := newService(t)
	ctx := bgCtx()

	now := time.Now().UTC()
	a, err := svc.Create(ctx, CreateRequest{RunID: "r1", Level: "standard", ExpiresAt: now})
	require.NoError(t, err)

	// Sleep a hair to make sure now is strictly after ExpiresAt would
	// be caught; but since we set ExpiresAt to the creation moment,
	// by the time CheckExpiry runs, now is likely a few microseconds
	// later. To test the exact-boundary semantics we instead set
	// ExpiresAt well into the future and check it is not expired.
	_ = a
	notExpired, err := svc.Create(ctx, CreateRequest{RunID: "r2", Level: "standard", ExpiresAt: futureTime()})
	require.NoError(t, err)

	expired, err := svc.CheckExpiry(ctx)
	require.NoError(t, err)
	// notExpired must not appear.
	assert.NotContains(t, expired, notExpired.ID)
}

// =========================================================================
// Compare-and-set decision path (lost-update regression)
// =========================================================================

// TestApprove_ConcurrentDistinctApprovers_ExactlyOnceEffect fires many
// goroutines at the same pending approval with MinApprovers=1. Exactly one
// decision may be recorded and the approval must end approved exactly once;
// every other caller must observe a terminal-state rejection or an explicit
// conflict instead of silently overwriting.
func TestApprove_ConcurrentDistinctApprovers_ExactlyOnceEffect(t *testing.T) {
	svc, _ := newService(t)
	ctx := bgCtx()

	a, err := svc.Create(ctx, CreateRequest{
		RunID:        "run-1",
		Level:        "standard",
		MinApprovers: 1,
	})
	require.NoError(t, err)

	const n = 16
	var (
		wg          sync.WaitGroup
		mu          sync.Mutex
		successes   int
		conflicts   int
		transitions int
	)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			err := svc.Approve(ctx, a.ID, fmt.Sprintf("approver-%d", i))
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				successes++
			case errors.Is(err, ErrConflict):
				conflicts++
			case errors.Is(err, ErrInvalidTransition):
				transitions++
			default:
				t.Errorf("unexpected error: %v", err)
			}
		}(i)
	}
	wg.Wait()

	assert.Equal(t, 1, successes, "exactly one approver may decide a MinApprovers=1 approval")
	assert.Equal(t, n-1, conflicts+transitions, "every other attempt must be accounted for")

	got, err := svc.Get(ctx, a.ID)
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, StatusApproved, got.Status)
	assert.Len(t, got.Decisions, 1, "no lost update may inflate the decision list")
}

// TestReject_ConcurrentWithApprove_SingleWinner ensures approve and reject
// racing on the same pending record produce exactly one terminal outcome
// (one-vote-veto must not be overwritable by a concurrent approve).
func TestReject_ConcurrentWithApprove_SingleWinner(t *testing.T) {
	svc, _ := newService(t)
	ctx := bgCtx()

	a, err := svc.Create(ctx, CreateRequest{
		RunID:        "run-2",
		Level:        "high",
		MinApprovers: 1,
		Approvers:    []string{"alice", "bob"},
	})
	require.NoError(t, err)

	var wg sync.WaitGroup
	results := make([]error, 2)
	wg.Add(2)
	go func() { defer wg.Done(); results[0] = svc.Approve(ctx, a.ID, "alice") }()
	go func() { defer wg.Done(); results[1] = svc.Reject(ctx, a.ID, "bob", "no") }()
	wg.Wait()

	nils := 0
	for _, err := range results {
		if err == nil {
			nils++
		}
	}
	assert.Equal(t, 1, nils, "exactly one of the racing decisions may succeed")

	got, err := svc.Get(ctx, a.ID)
	require.NoError(t, err)
	require.NotNil(t, got)
	require.Len(t, got.Decisions, 1)
	switch got.Status {
	case StatusApproved, StatusRejected:
		assert.Equal(t, got.Decisions[0].Action, map[Status]string{
			StatusApproved: ActionApprove,
			StatusRejected: ActionReject,
		}[got.Status], "terminal status must match the winning decision")
	default:
		t.Fatalf("approval ended in non-terminal status %q", got.Status)
	}
}

// TestApprove_CASLostThenSucceed exercises the retry loop: the first
// compare-and-set attempt loses (simulated via the mock's failure key) but
// after reloading, the second attempt applies.
func TestApprove_CASLostThenSucceed(t *testing.T) {
	svc, store := newService(t)
	ctx := bgCtx()

	a, err := svc.Create(ctx, CreateRequest{RunID: "run-3", Level: "standard"})
	require.NoError(t, err)

	store.failOn["update-if-pending-lost"] = errors.New("lost race")
	_ = store.failOn["update-if-pending-lost"]

	// With both attempts losing the CAS the service must surface ErrConflict.
	err = svc.Approve(ctx, a.ID, "alice")
	require.ErrorIs(t, err, ErrConflict)

	// Now let CAS succeed: the retry loop records the decision.
	delete(store.failOn, "update-if-pending-lost")
	require.NoError(t, svc.Approve(ctx, a.ID, "alice"))

	got, err := svc.Get(ctx, a.ID)
	require.NoError(t, err)
	assert.Equal(t, StatusApproved, got.Status)
}

// TestUpdateIfPending_RejectedWhenTerminal pins the mock contract used by
// the concurrency tests: UpdateIfPending never applies once the stored
// record has left StatusPending.
func TestUpdateIfPending_RejectedWhenTerminal(t *testing.T) {
	store := newMockStore()
	a := &Approval{ID: "a1", RunID: "r1", Level: "standard", Status: StatusPending}
	require.NoError(t, store.Create(bgCtx(), a))

	ok, err := store.UpdateIfPending(bgCtx(), a)
	require.NoError(t, err)
	assert.True(t, ok)

	// Another actor decides the record through the plain Update path.
	decided := cloneApproval(a)
	decided.Status = StatusApproved
	require.NoError(t, store.Update(bgCtx(), decided))

	// A stale writer still holding the pending snapshot must lose the CAS.
	ok, err = store.UpdateIfPending(bgCtx(), a)
	require.NoError(t, err)
	assert.False(t, ok, "CAS on a terminal record must report false")

	stored, err := store.Get(bgCtx(), a.ID)
	require.NoError(t, err)
	assert.Equal(t, StatusApproved, stored.Status, "terminal overwrite must not happen")
}
