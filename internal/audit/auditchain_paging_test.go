package audit

// Paging tests for the audit chain walk. The walk reads the chain one bounded
// page at a time (see walkChain), so what these pin down is the property that
// paging could plausibly break and that a single-shot read got for free: the
// hashes must still be computed over ONE order, the true chain order, across
// every page boundary — and a walk in flight must survive rows being written
// underneath it, which is the normal case because Record seals on every write.

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nexus/levee/internal/state"
)

// appendingStore wraps a Store and appends one audit row after the Nth page of
// a chain walk, simulating the ordinary production case: a walk started by one
// audit write while another audit write is landing.
type appendingStore struct {
	state.Store
	insertAfterPage int
	pages           int
	appended        bool
	newRow          *state.Audit
}

func (s *appendingStore) ListAuditChainPage(ctx context.Context, after *state.AuditCursor, limit int, tenantID string) ([]*state.Audit, error) {
	page, err := s.Store.ListAuditChainPage(ctx, after, limit, tenantID)
	if err != nil {
		return nil, err
	}
	s.pages++
	if !s.appended && s.pages >= s.insertAfterPage {
		s.appended = true
		if err := s.CreateAudit(ctx, s.newRow); err != nil {
			return nil, err
		}
	}
	return page, nil
}

// pageSizeBuilder returns a builder whose walk reads pageSize rows at a time.
func pageSizeBuilder(t *testing.T, store state.Store, pageSize int) *AuditChainBuilder {
	t.Helper()
	b, err := NewAuditChainBuilder(store)
	require.NoError(t, err)
	b.pageSize = pageSize
	return b
}

// insertAudits writes n audit rows directly, bypassing Record's seal-on-write,
// so a test can watch a paged walk do the sealing itself.
func insertAudits(t *testing.T, store state.Store, prefix string, n int, base time.Time) {
	t.Helper()
	ctx := context.Background()
	for i := 0; i < n; i++ {
		require.NoError(t, store.CreateAudit(ctx, &state.Audit{
			ID:        fmt.Sprintf("%s-%d", prefix, i),
			RunID:     fmt.Sprintf("run-%d", i),
			Action:    "apply",
			Actor:     "alice",
			Target:    fmt.Sprintf("host-%d", i),
			Result:    "success",
			Timestamp: base.Add(time.Duration(i) * time.Millisecond),
		}))
	}
}

// TestAuditChain_Paging_ChainSpansPageBoundaries seals a chain with a page size
// that forces several boundaries, then verifies it. If the pages were walked in
// the wrong direction, or a boundary split the chain's order, every row after
// the first boundary would hash against the wrong predecessor and Verify would
// report them all.
func TestAuditChain_Paging_ChainSpansPageBoundaries(t *testing.T) {
	_, sqlite := newAuditChainBuilder(t)
	ctx := context.Background()
	insertAudits(t, sqlite, "pg", 7, time.Now().UTC())

	b := pageSizeBuilder(t, sqlite, 2) // 7 rows over pages of 2,2,2,1
	sealed, err := b.Seal(ctx)
	require.NoError(t, err)
	assert.Equal(t, 7, sealed, "every row starts unsealed")

	result, err := b.Verify(ctx)
	require.NoError(t, err)
	assert.True(t, result.Valid, "failures: %+v", result.Failures)
	assert.Equal(t, 7, result.Count)
	assert.Equal(t, 0, result.Unsealed)
	assert.Empty(t, result.Failures)

	// A second pass changes nothing: the paged walk reads the same order twice
	// and derives the same hashes.
	sealed, err = b.Seal(ctx)
	require.NoError(t, err)
	assert.Equal(t, 0, sealed, "an already-sealed chain must not be rewritten")
}

// TestAuditChain_Paging_DetectsTamperAcrossBoundary tampers a row that falls in
// a later page than the first. The failure must name that row and only that
// row — a paging walk that restarted its hash cursor per page, or resolved ties
// differently between pages, would either miss the break or report a cascade.
func TestAuditChain_Paging_DetectsTamperAcrossBoundary(t *testing.T) {
	_, sqlite := newAuditChainBuilder(t)
	ctx := context.Background()
	writeAudits(t, sqlite, "pt", 6, time.Now().UTC())

	b := pageSizeBuilder(t, sqlite, 2)
	_, err := b.Seal(ctx)
	require.NoError(t, err)
	before, err := b.Verify(ctx)
	require.NoError(t, err)
	require.True(t, before.Valid)

	// Row 3 of 6 lives in the second page.
	dropWORMTriggers(t, sqlite)
	_, err = sqlite.DB().ExecContext(ctx, `UPDATE audit SET result = 'rolled_back' WHERE id = 'pt-3'`)
	require.NoError(t, err)

	result, err := b.Verify(ctx)
	require.NoError(t, err)
	require.False(t, result.Valid)
	require.Len(t, result.Failures, 1, "exactly the edited row: %+v", result.Failures)
	assert.Equal(t, "pt-3", result.Failures[0].AuditID)
	assert.Equal(t, 3, result.Failures[0].Index)

	// Seal must NOT repair it. Rewriting the row the tamper lives in would relink
	// the chain and make Verify pass, erasing the only evidence of the edit — the
	// exact hole SA-002 recorded. Seal stops and reports instead.
	_, err = b.Seal(ctx)
	require.Error(t, err, "a sealed-but-wrong row must be reported, never rewritten")
	assert.ErrorIs(t, err, ErrChainBroken)

	after, err := b.Verify(ctx)
	require.NoError(t, err)
	assert.False(t, after.Valid, "the evidence must survive a seal attempt")

	// Rebuild is the deliberate administrative override: with the break
	// investigated, wiping the stored hashes is an explicit choice rather than
	// something every seal does silently. Three of six rows are rewritten — the
	// edited row and the two behind it whose prev_hash covered it.
	written, err := b.Rebuild(ctx)
	require.NoError(t, err)
	assert.Equal(t, 3, written)
	result, err = b.Verify(ctx)
	require.NoError(t, err)
	assert.True(t, result.Valid, "failures: %+v", result.Failures)
}

// TestAuditChain_Paging_SameTimestampAcrossBoundary pins the tie-breaker under
// paging. Rows written within the same instant are the case the (timestamp, id)
// order exists for, and they are exactly the case where two pages could be
// resolved inconsistently with each other.
func TestAuditChain_Paging_SameTimestampAcrossBoundary(t *testing.T) {
	_, sqlite := newAuditChainBuilder(t)
	ctx := context.Background()
	same := time.Now().UTC().Truncate(time.Second)

	for i := 0; i < 6; i++ {
		require.NoError(t, sqlite.CreateAudit(ctx, &state.Audit{
			ID: fmt.Sprintf("tie-%d", i), RunID: "run-t", Action: "apply",
			Actor: "alice", Result: "success", Timestamp: same,
		}))
	}

	b := pageSizeBuilder(t, sqlite, 2)
	_, err := b.Seal(ctx)
	require.NoError(t, err)
	result, err := b.Verify(ctx)
	require.NoError(t, err)
	assert.True(t, result.Valid, "failures: %+v", result.Failures)
	assert.Equal(t, 6, result.Count)

	// The order really is the id order within the shared timestamp, so the
	// chain walks tie-0 first — assert it rather than trusting the hashes.
	order := chainOrder(t, sqlite)
	assert.Equal(t, []string{"tie-0", "tie-1", "tie-2", "tie-3", "tie-4", "tie-5"}, order)
}

// TestAuditChain_Paging_SurvivesConcurrentAppend is the reason the walk pages by
// cursor instead of by offset. A row lands while the walk is in flight; the
// walk must still end up with one consistent chain.
func TestAuditChain_Paging_SurvivesConcurrentAppend(t *testing.T) {
	_, sqlite := newAuditChainBuilder(t)
	ctx := context.Background()
	base := time.Now().UTC()
	writeAudits(t, sqlite, "ca", 6, base)

	// Newest row, written after the first page has been served: it sorts after
	// everything the walk has read, so this walk picks it up.
	wrapped := &appendingStore{
		Store:           sqlite,
		insertAfterPage: 1,
		newRow: &state.Audit{
			ID: "ca-live", RunID: "run-live", Action: "login", Actor: "carol",
			Result: "success", Timestamp: base.Add(time.Hour),
		},
	}
	b := pageSizeBuilder(t, wrapped, 2)
	_, err := b.Seal(ctx)
	require.NoError(t, err)
	require.True(t, wrapped.appended, "the append must actually have happened mid-walk")

	verify := pageSizeBuilder(t, sqlite, 2)
	result, err := verify.Verify(ctx)
	require.NoError(t, err)
	assert.True(t, result.Valid, "the concurrent row must not corrupt the chain: %+v", result.Failures)
	assert.Equal(t, 7, result.Count, "the concurrent row is part of the chain")
}

// TestAuditChain_Paging_BackdatedRowIsDeferredNotCorrupted covers the other
// direction: a row that lands BEHIND the walk's cursor. This walk cannot see it
// — its position is already past — and the correct outcome is that it shows up
// as unsealed and is closed by the next seal, not that the chain forks.
func TestAuditChain_Paging_BackdatedRowIsDeferredNotCorrupted(t *testing.T) {
	_, sqlite := newAuditChainBuilder(t)
	ctx := context.Background()
	base := time.Now().UTC()
	writeAudits(t, sqlite, "bd", 6, base)

	// Older than everything, so it sorts before the first page — but it is
	// written after the walk has already passed the head.
	wrapped := &appendingStore{
		Store:           sqlite,
		insertAfterPage: 1,
		newRow: &state.Audit{
			ID: "bd-back", RunID: "run-back", Action: "login", Actor: "dave",
			Result: "success", Timestamp: base.Add(-time.Hour),
		},
	}
	b := pageSizeBuilder(t, wrapped, 2)
	_, err := b.Seal(ctx)
	require.NoError(t, err)

	verify := pageSizeBuilder(t, sqlite, 2)
	result, err := verify.Verify(ctx)
	require.NoError(t, err)
	assert.False(t, result.Valid, "a row the walk could not see must be visible as a gap")
	require.Len(t, result.Failures, 1)
	assert.Equal(t, "bd-back", result.Failures[0].AuditID)
	assert.Equal(t, FailureEmptyHash, result.Failures[0].Type)
	assert.Equal(t, 1, result.Unsealed)

	// The next seal walks from the head again and closes it.
	sealed, err := verify.Seal(ctx)
	require.NoError(t, err)
	assert.Greater(t, sealed, 0)
	result, err = verify.Verify(ctx)
	require.NoError(t, err)
	assert.True(t, result.Valid, "failures: %+v", result.Failures)
	assert.Equal(t, 7, result.Count)
}

// chainOrder returns the audit ids in chain order, read one page at a time.
func chainOrder(t *testing.T, store state.Store) []string {
	t.Helper()
	var out []string
	var cursor *state.AuditCursor
	for {
		page, err := store.ListAuditChainPage(context.Background(), cursor, 2, "")
		require.NoError(t, err)
		for _, a := range page {
			out = append(out, a.ID)
		}
		if len(page) < 2 {
			return out
		}
		last := page[len(page)-1]
		cursor = &state.AuditCursor{Timestamp: last.Timestamp, ID: last.ID}
	}
}
