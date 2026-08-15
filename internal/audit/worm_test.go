package audit

import (
	"context"
	"testing"
	"time"

	"github.com/nexus/levee/internal/state"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newWORMStore builds a WORMStore on top of a fresh temp-file store and
// returns both so the test can insert runs and simulate tampering.
func newWORMStore(t *testing.T) (*WORMStore, *state.SQLiteStore) {
	t.Helper()
	store := newTestStore(t)
	w, err := NewWORMStore(store)
	require.NoError(t, err)
	return w, store
}

// newWORMTrace builds a minimal trace with the given id and timestamp. The
// content is fixed so that checksums are deterministic across tests.
func newWORMTrace(runID, id string, ts time.Time) *state.Trace {
	return &state.Trace{
		ID:        id,
		RunID:     runID,
		Event:     EventStepExecute,
		Actor:     "system",
		Detail:    `{"target":"host:1"}`,
		Timestamp: ts,
	}
}

func TestNewWORMStore_NilStore(t *testing.T) {
	w, err := NewWORMStore(nil)
	require.ErrorIs(t, err, ErrNilStore)
	assert.Nil(t, w)
}

func TestAppend_Success_SetsChecksum(t *testing.T) {
	w, store := newWORMStore(t)
	ctx := context.Background()
	createRun(t, store, "run-1")
	now := time.Now().UTC()
	trace := newWORMTrace("run-1", "trace-1", now)

	require.NoError(t, w.Append(ctx, trace))

	// CurrHash must be populated with the SHA-256 hex checksum.
	assert.NotEmpty(t, trace.CurrHash, "CurrHash should be set to checksum")
	assert.Len(t, trace.CurrHash, 64, "SHA-256 hex is 64 chars")

	// The persisted row must carry the same checksum.
	got, err := store.GetTrace(ctx, "trace-1")
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, trace.CurrHash, got.CurrHash)
	assert.Equal(t, computeChecksum(trace), got.CurrHash)
}

func TestAppend_DuplicateID_ReturnsErrAlreadyExists(t *testing.T) {
	w, store := newWORMStore(t)
	ctx := context.Background()
	createRun(t, store, "run-1")
	now := time.Now().UTC()
	require.NoError(t, w.Append(ctx, newWORMTrace("run-1", "trace-1", now)))

	// Second append with the same id must fail.
	err := w.Append(ctx, newWORMTrace("run-1", "trace-1", now.Add(time.Second)))
	require.ErrorIs(t, err, ErrAlreadyExists)
}

func TestAppend_NilTrace(t *testing.T) {
	w, _ := newWORMStore(t)
	err := w.Append(context.Background(), nil)
	require.Error(t, err)
}

func TestAppend_EmptyID(t *testing.T) {
	w, store := newWORMStore(t)
	createRun(t, store, "run-1")
	err := w.Append(context.Background(), &state.Trace{
		RunID:     "run-1",
		Event:     EventStepExecute,
		Timestamp: time.Now().UTC(),
	})
	require.Error(t, err)
}

func TestRead_Success_VerifiesChecksum(t *testing.T) {
	w, store := newWORMStore(t)
	ctx := context.Background()
	createRun(t, store, "run-1")
	now := time.Now().UTC()
	require.NoError(t, w.Append(ctx, newWORMTrace("run-1", "trace-1", now)))

	got, err := w.Read(ctx, "trace-1")
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, "trace-1", got.ID)
	assert.Equal(t, "run-1", got.RunID)
}

func TestRead_NotFound_ReturnsErrWORMNotFound(t *testing.T) {
	w, _ := newWORMStore(t)
	_, err := w.Read(context.Background(), "no-such-trace")
	require.ErrorIs(t, err, ErrWORMNotFound)
}

func TestRead_TamperedDetail_ReturnsErrTampered(t *testing.T) {
	w, store := newWORMStore(t)
	ctx := context.Background()
	createRun(t, store, "run-1")
	now := time.Now().UTC()
	require.NoError(t, w.Append(ctx, newWORMTrace("run-1", "trace-1", now)))

	// Bypass WORM and tamper with the Detail column directly.
	trace, err := store.GetTrace(ctx, "trace-1")
	require.NoError(t, err)
	trace.Detail = `{"tampered":true}`
	require.NoError(t, store.UpdateTrace(ctx, trace))

	_, err = w.Read(ctx, "trace-1")
	require.ErrorIs(t, err, ErrTampered)
}

func TestRead_TamperedCurrHash_ReturnsErrTampered(t *testing.T) {
	// If an attacker overwrites CurrHash itself, verification recomputes from
	// the content and still detects the mismatch.
	w, store := newWORMStore(t)
	ctx := context.Background()
	createRun(t, store, "run-1")
	now := time.Now().UTC()
	require.NoError(t, w.Append(ctx, newWORMTrace("run-1", "trace-1", now)))

	trace, err := store.GetTrace(ctx, "trace-1")
	require.NoError(t, err)
	trace.CurrHash = "fake-hash-value"
	require.NoError(t, store.UpdateTrace(ctx, trace))

	_, err = w.Read(ctx, "trace-1")
	require.ErrorIs(t, err, ErrTampered)
}

func TestReadByRun_OrderedByTimestamp(t *testing.T) {
	w, store := newWORMStore(t)
	ctx := context.Background()
	createRun(t, store, "run-1")
	base := time.Now().UTC()

	// Append three traces with strictly increasing timestamps.
	require.NoError(t, w.Append(ctx, newWORMTrace("run-1", "t1", base)))
	time.Sleep(2 * time.Millisecond)
	require.NoError(t, w.Append(ctx, newWORMTrace("run-1", "t2", base.Add(time.Millisecond))))
	time.Sleep(2 * time.Millisecond)
	require.NoError(t, w.Append(ctx, newWORMTrace("run-1", "t3", base.Add(2*time.Millisecond))))

	got, err := w.ReadByRun(ctx, "run-1")
	require.NoError(t, err)
	require.Len(t, got, 3)
	// ListTraces orders by timestamp ASC.
	assert.Equal(t, "t1", got[0].ID)
	assert.Equal(t, "t2", got[1].ID)
	assert.Equal(t, "t3", got[2].ID)
}

func TestReadByRun_TamperDetected(t *testing.T) {
	w, store := newWORMStore(t)
	ctx := context.Background()
	createRun(t, store, "run-1")
	base := time.Now().UTC()
	require.NoError(t, w.Append(ctx, newWORMTrace("run-1", "t1", base)))
	require.NoError(t, w.Append(ctx, newWORMTrace("run-1", "t2", base.Add(time.Millisecond))))

	// Tamper with one record's Detail via the underlying store.
	traces, err := store.ListTraces(ctx, state.TraceFilter{RunID: "run-1"})
	require.NoError(t, err)
	require.Len(t, traces, 2)
	traces[0].Detail = `{"tampered":true}`
	require.NoError(t, store.UpdateTrace(ctx, traces[0]))

	_, err = w.ReadByRun(ctx, "run-1")
	require.ErrorIs(t, err, ErrTampered)
}

func TestReadByRun_Empty(t *testing.T) {
	w, store := newWORMStore(t)
	ctx := context.Background()
	createRun(t, store, "run-empty")

	got, err := w.ReadByRun(ctx, "run-empty")
	require.NoError(t, err)
	assert.Empty(t, got)
}

func TestCount_ReturnsCorrectNumber(t *testing.T) {
	w, store := newWORMStore(t)
	ctx := context.Background()
	createRun(t, store, "run-1")
	base := time.Now().UTC()

	require.NoError(t, w.Append(ctx, newWORMTrace("run-1", "t1", base)))
	require.NoError(t, w.Append(ctx, newWORMTrace("run-1", "t2", base.Add(time.Millisecond))))
	require.NoError(t, w.Append(ctx, newWORMTrace("run-1", "t3", base.Add(2*time.Millisecond))))

	n, err := w.Count(ctx, "run-1")
	require.NoError(t, err)
	assert.Equal(t, 3, n)
}

func TestCount_Zero(t *testing.T) {
	w, store := newWORMStore(t)
	ctx := context.Background()
	createRun(t, store, "run-empty")

	n, err := w.Count(ctx, "run-empty")
	require.NoError(t, err)
	assert.Equal(t, 0, n)
}

func TestMultipleRuns_Independent(t *testing.T) {
	w, store := newWORMStore(t)
	ctx := context.Background()
	createRun(t, store, "run-a")
	createRun(t, store, "run-b")
	base := time.Now().UTC()

	require.NoError(t, w.Append(ctx, newWORMTrace("run-a", "a1", base)))
	require.NoError(t, w.Append(ctx, newWORMTrace("run-a", "a2", base.Add(time.Millisecond))))
	require.NoError(t, w.Append(ctx, newWORMTrace("run-b", "b1", base)))
	require.NoError(t, w.Append(ctx, newWORMTrace("run-b", "b2", base.Add(time.Millisecond))))

	// run-a only contains its own traces.
	aTraces, err := w.ReadByRun(ctx, "run-a")
	require.NoError(t, err)
	require.Len(t, aTraces, 2)
	for _, tr := range aTraces {
		assert.Equal(t, "run-a", tr.RunID)
	}

	// run-b only contains its own traces.
	bTraces, err := w.ReadByRun(ctx, "run-b")
	require.NoError(t, err)
	require.Len(t, bTraces, 2)
	for _, tr := range bTraces {
		assert.Equal(t, "run-b", tr.RunID)
	}

	aCount, err := w.Count(ctx, "run-a")
	require.NoError(t, err)
	bCount, err := w.Count(ctx, "run-b")
	require.NoError(t, err)
	assert.Equal(t, 2, aCount)
	assert.Equal(t, 2, bCount)
}

func TestChecksum_Deterministic(t *testing.T) {
	now := time.Date(2026, 8, 15, 12, 0, 0, 0, time.UTC)
	t1 := newWORMTrace("run-1", "trace-1", now)
	t2 := newWORMTrace("run-1", "trace-1", now)
	assert.Equal(t, computeChecksum(t1), computeChecksum(t2), "identical content must produce identical checksum")
}

func TestChecksum_DifferentInputs(t *testing.T) {
	now := time.Date(2026, 8, 15, 12, 0, 0, 0, time.UTC)
	base := newWORMTrace("run-1", "trace-1", now)

	// Different ID.
	other := *base
	other.ID = "trace-2"
	assert.NotEqual(t, computeChecksum(base), computeChecksum(&other), "different id must produce different checksum")

	// Different Detail.
	other2 := *base
	other2.Detail = `{"target":"host:2"}`
	assert.NotEqual(t, computeChecksum(base), computeChecksum(&other2), "different detail must produce different checksum")

	// Different Timestamp.
	other3 := *base
	other3.Timestamp = now.Add(time.Second)
	assert.NotEqual(t, computeChecksum(base), computeChecksum(&other3), "different timestamp must produce different checksum")

	// Different RunID.
	other4 := *base
	other4.RunID = "run-2"
	assert.NotEqual(t, computeChecksum(base), computeChecksum(&other4), "different run id must produce different checksum")
}

func TestAppendOnly_OriginalUnchangedAfterDuplicate(t *testing.T) {
	// Append the same id twice; the second call must fail and the original
	// record must be byte-for-byte unchanged.
	w, store := newWORMStore(t)
	ctx := context.Background()
	createRun(t, store, "run-1")
	now := time.Now().UTC()

	original := newWORMTrace("run-1", "trace-1", now)
	original.Detail = `{"original":true}`
	require.NoError(t, w.Append(ctx, original))
	origHash := original.CurrHash

	// Second append with the same id but different content.
	second := newWORMTrace("run-1", "trace-1", now.Add(time.Second))
	second.Detail = `{"modified":true}`
	err := w.Append(ctx, second)
	require.ErrorIs(t, err, ErrAlreadyExists)

	// The original record must be unchanged.
	got, err := w.Read(ctx, "trace-1")
	require.NoError(t, err)
	assert.Equal(t, origHash, got.CurrHash, "checksum must be unchanged after rejected duplicate")
	assert.Equal(t, `{"original":true}`, got.Detail, "detail must be unchanged after rejected duplicate")
}

func TestVerifyChecksum_NilTrace(t *testing.T) {
	assert.False(t, verifyChecksum(nil))
}
