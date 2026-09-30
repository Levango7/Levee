// cmd_approve_concurrent_test.go verifies the lost-update fix end-to-end over
// the PRODUCTION approval→state adapter and a real SQLite store (D-1 v2).
//
// Concurrent approvers deciding on the same quorum-N approval must all have
// their votes durably recorded: the revision-based compare-and-set detects the
// concurrent partial vote (revision mismatch) and retries from a fresh read
// instead of silently overwriting. Before the fix, both writers could succeed
// yet only one vote survived and the quorum stalled.
package main

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nexus/levee/internal/approval"
)

func TestApproveConcurrent_Quorum2NoLostVotes(t *testing.T) {
	defer resetRootFlags()
	e := newCLIEnv(t)
	store := e.open(t)
	defer func() { _ = store.Close() }()

	adapter := newApprovalStoreAdapter(store)
	svc := approval.NewService(adapter)
	ctx := context.Background()

	for i := 0; i < 20; i++ {
		changeID := fmt.Sprintf("chg-concurrent-2of2-%d", i)
		e.seedRun(t, changeID, "draft")
		ap, err := svc.Create(ctx, approval.CreateRequest{
			RunID:        changeID,
			Level:        approval.LevelStandard,
			Approvers:    []string{"alice", "bob"},
			MinApprovers: 2,
			ExpiresAt:    time.Now().UTC().Add(time.Hour),
		})
		require.NoError(t, err)

		start := make(chan struct{})
		var wg sync.WaitGroup
		errs := make([]error, 2)
		for _, approver := range []string{"alice", "bob"} {
			wg.Add(1)
			go func(apID, who string, slot int) {
				defer wg.Done()
				<-start
				errs[slot] = svc.Approve(ctx, apID, who)
			}(ap.ID, approver, map[string]int{"alice": 0, "bob": 1}[approver])
		}
		close(start)
		wg.Wait()

		require.NoError(t, errs[0], "alice's concurrent vote must not be lost")
		require.NoError(t, errs[1], "bob's concurrent vote must not be lost")

		got, err := adapter.Get(ctx, ap.ID)
		require.NoError(t, err)
		assert.True(t, got.Status == approval.StatusApproved,
			"quorum 2/2 must approve (status=%s)", got.Status)
		assert.Len(t, got.Decisions, 2, "both concurrent decisions must be durably recorded")
	}
}

// TestApproveConcurrent_Quorum3NoLostVotes widens the quorum to 3 so three
// simultaneous partial votes must all survive: each conflicts on the revision
// CAS and retries, and the chain must still reach 3/3. This is the evidence
// behind raising the retry budget (casMaxAttempts) alongside the revision guard.
func TestApproveConcurrent_Quorum3NoLostVotes(t *testing.T) {
	defer resetRootFlags()
	e := newCLIEnv(t)
	store := e.open(t)
	defer func() { _ = store.Close() }()

	adapter := newApprovalStoreAdapter(store)
	svc := approval.NewService(adapter)
	ctx := context.Background()

	for i := 0; i < 10; i++ {
		changeID := fmt.Sprintf("chg-concurrent-3of3-%d", i)
		e.seedRun(t, changeID, "draft")
		ap, err := svc.Create(ctx, approval.CreateRequest{
			RunID:        changeID,
			Level:        approval.LevelHigh,
			Approvers:    []string{"alice", "bob", "carol"},
			MinApprovers: 3,
			ExpiresAt:    time.Now().UTC().Add(time.Hour),
		})
		require.NoError(t, err)

		approvers := []string{"alice", "bob", "carol"}
		start := make(chan struct{})
		errs := make([]error, len(approvers))
		var wg sync.WaitGroup
		for slot, who := range approvers {
			wg.Add(1)
			go func(slot int, who string) {
				defer wg.Done()
				<-start
				errs[slot] = svc.Approve(ctx, ap.ID, who)
			}(slot, who)
		}
		close(start)
		wg.Wait()

		for slot := range approvers {
			require.NoError(t, errs[slot], "vote %d must not be lost", slot)
		}

		got, err := adapter.Get(ctx, ap.ID)
		require.NoError(t, err)
		assert.Len(t, got.Decisions, 3, "all three concurrent decisions must be durable")
		assert.Equal(t, approval.StatusApproved, got.Status, "3/3 votes must reach quorum")
	}
}
