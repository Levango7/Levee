// expiry_cas_test.go — what CheckExpiry may and may not do when the record it
// scanned stops being pending before its own write.
//
// The sweep used to write with the unconditional Update, which made two
// concurrent writers indistinguishable: a second replica's sweep reported the
// same expiry again, and — worse — a decision that landed between the scan and
// the write was overwritten back to expired. The CAS on "still pending" is the
// fix; these cases pin both halves of it, because a guard whose losing branch
// is never exercised is indistinguishable from no guard at all.
package approval

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// decisionFirstStore answers the first compare-and-set by recording a human
// approval and reporting the CAS as lost, which is exactly the interleaving
// "decision lands between CheckExpiry's scan and its write".
type decisionFirstStore struct {
	Store
	mu       sync.Mutex
	observed bool
}

func (s *decisionFirstStore) UpdateIfPending(ctx context.Context, a *Approval) (bool, error) {
	s.mu.Lock()
	if !s.observed {
		s.observed = true
		s.mu.Unlock()
		decided := *a
		decided.Status = StatusApproved
		if err := s.Update(ctx, &decided); err != nil {
			return false, err
		}
		return false, nil
	}
	s.mu.Unlock()
	return s.Store.UpdateIfPending(ctx, a)
}

func TestCheckExpiry_DoesNotOverwriteADecisionThatLandedFirst(t *testing.T) {
	inner := newMockStore()
	svc := NewService(&decisionFirstStore{Store: inner})
	ctx := bgCtx()

	created, err := svc.Create(ctx, CreateRequest{RunID: "run-1", Level: "high", ExpiresAt: pastTime()})
	require.NoError(t, err)

	expired, err := svc.CheckExpiry(ctx)
	require.NoError(t, err, "losing the CAS is not an error — the record was taken")
	assert.Empty(t, expired, "a record another writer settled must not be reported as expired")

	stored, err := inner.Get(ctx, created.ID)
	require.NoError(t, err)
	require.NotNil(t, stored)
	assert.Equal(t, StatusApproved, stored.Status, "the sweep must never undo a decision that landed first")
}

func TestCheckExpiry_LosingTheCASIsSilent(t *testing.T) {
	svc, store := newService(t)
	ctx := bgCtx()

	created, err := svc.Create(ctx, CreateRequest{RunID: "run-1", Level: "high", ExpiresAt: pastTime()})
	require.NoError(t, err)
	store.failOn["update-if-pending-lost"] = errors.New("forced CAS loss")

	expired, err := svc.CheckExpiry(ctx)
	require.NoError(t, err)
	assert.Empty(t, expired)

	stored, err := store.Get(ctx, created.ID)
	require.NoError(t, err)
	require.NotNil(t, stored)
	assert.Equal(t, StatusPending, stored.Status)
}

func TestCheckExpiry_CASErrorStillAbortsTheSweep(t *testing.T) {
	// The same failure path as TestCheckExpiry_StoreUpdateError, kept here so
	// the CAS suite states the whole contract in one place: losing the CAS is
	// silent, failing the write is not.
	svc, store := newService(t)
	ctx := bgCtx()

	_, err := svc.Create(ctx, CreateRequest{RunID: "run-1", Level: "high", ExpiresAt: pastTime()})
	require.NoError(t, err)
	store.failOn["update-if-pending"] = errors.New("db down")

	_, err = svc.CheckExpiry(ctx)
	require.Error(t, err, "a store failure is still a failure, not a quiet skip")
	assert.Contains(t, err.Error(), "db down")
}

// barrierStore forces the interleaving the duplicate-report bug needs: every
// sweep finishes its scan before any sweep is allowed to write. Without this
// the goroutines can serialise scan→write→scan, and the second sweep finds
// nothing pending — a green result that says nothing about the race.
type barrierStore struct {
	*mockStore
	arrived chan struct{}
	release chan struct{}
}

func newBarrierStore(inner *mockStore, parties int) *barrierStore {
	s := &barrierStore{
		mockStore: inner,
		arrived:   make(chan struct{}, parties),
		release:   make(chan struct{}),
	}
	go func() {
		for i := 0; i < parties; i++ {
			<-s.arrived
		}
		close(s.release)
	}()
	return s
}

func (s *barrierStore) ListPending(ctx context.Context) ([]*Approval, error) {
	pending, err := s.mockStore.ListPending(ctx)
	s.arrived <- struct{}{}
	<-s.release
	return pending, err
}

// TestSweeper_TwoConcurrentSweepsReportEachRecordOnce is the duplicate-report
// half: two replicas scan the same record, only the winner of the CAS reports
// it.
func TestSweeper_TwoConcurrentSweepsReportEachRecordOnce(t *testing.T) {
	const parties = 2
	store := newBarrierStore(newMockStore(), parties)
	svc := NewService(store)
	ctx := bgCtx()

	_, err := svc.Create(ctx, CreateRequest{RunID: "run-1", Level: "high", ExpiresAt: pastTime()})
	require.NoError(t, err)

	var (
		mu       sync.Mutex
		reported []string
	)
	record := func(_ context.Context, expired []Approval) {
		mu.Lock()
		defer mu.Unlock()
		for _, a := range expired {
			reported = append(reported, a.ID)
		}
	}
	sweeper := NewExpirySweeper(svc, 0, record)

	var wg sync.WaitGroup
	for i := 0; i < parties; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = sweeper.SweepOnce(ctx)
		}()
	}
	wg.Wait()

	mu.Lock()
	defer mu.Unlock()
	assert.Len(t, reported, 1, "one expired record must produce exactly one report, not one per sweeper")
}
