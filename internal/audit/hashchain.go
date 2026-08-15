package audit

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"

	"github.com/nexus/levee/internal/state"
)

// Sentinel errors for the hash-chain builder. Callers can use errors.Is to
// distinguish failure modes.
var (
	// ErrNoTraces is returned when Build is called for a run that has no trace
	// records. An empty chain cannot be built.
	ErrNoTraces = errors.New("audit: no traces found for run")
	// ErrHashMismatch is returned by Verify when the recomputed hash of a trace
	// record does not match the stored CurrHash, indicating tampering.
	ErrHashMismatch = errors.New("audit: hash mismatch")
	// ErrInvalidBatchSize is returned when BuildBatch is called with a
	// non-positive batch size.
	ErrInvalidBatchSize = errors.New("audit: invalid batch size")
)

// hashChainSeparator is the delimiter used when concatenating trace fields into
// the hash input. A pipe is chosen because it does not appear in any individual
// field by construction (ids are hex, events/actors are identifiers).
const hashChainSeparator = "|"

// HashChainBuilder builds a hash chain for the trace records of a run. It reads
// the trace records from state.Store, sorts them by timestamp, computes the
// CurrHash for each record (chained to the previous record's CurrHash via
// PrevHash) and persists the updated hashes back to the store.
//
// The resulting chain has the property that tampering with any record (changing
// its detail, event, actor, ...) changes its CurrHash, which in turn changes
// every subsequent CurrHash. Verification recomputes the chain and compares it
// against the stored hashes to detect tampering.
type HashChainBuilder struct {
	store state.Store
}

// NewHashChainBuilder creates a HashChainBuilder backed by the given store. The
// store must be non-nil.
func NewHashChainBuilder(store state.Store) (*HashChainBuilder, error) {
	if store == nil {
		return nil, ErrNilStore
	}
	return &HashChainBuilder{store: store}, nil
}

// Build constructs the hash chain for all trace records of the given run. The
// records are sorted by timestamp ascending; for each record the PrevHash is
// set to the previous record's CurrHash (empty for the first record) and the
// CurrHash is computed from the record's content plus PrevHash. The updated
// records are persisted back to the store.
//
// Build returns the number of records in the chain and the CurrHash of the last
// record (the chain tail). An empty run id yields ErrEmptyRunID; a run with no
// trace records yields ErrNoTraces.
func (b *HashChainBuilder) Build(ctx context.Context, runID string) (count int, tailHash string, err error) {
	if runID == "" {
		return 0, "", ErrEmptyRunID
	}

	traces, err := b.store.ListTraces(ctx, state.TraceFilter{RunID: runID})
	if err != nil {
		return 0, "", fmt.Errorf("audit: list traces for run %q: %w", runID, err)
	}
	if len(traces) == 0 {
		return 0, "", ErrNoTraces
	}

	return b.buildChain(ctx, traces)
}

// BuildBatch constructs the hash chain in batches. It is intended for runs with
// a large number of trace records where loading all records at once would be
// prohibitive. The records are sorted by timestamp ascending and processed
// batchSize at a time; the tail hash of each batch becomes the prev hash of the
// first record in the next batch, so the cross-batch chain stays continuous.
//
// batchSize must be positive; otherwise ErrInvalidBatchSize is returned. The
// returned count and tailHash have the same meaning as for Build.
func (b *HashChainBuilder) BuildBatch(ctx context.Context, runID string, batchSize int) (count int, tailHash string, err error) {
	if runID == "" {
		return 0, "", ErrEmptyRunID
	}
	if batchSize <= 0 {
		return 0, "", ErrInvalidBatchSize
	}

	traces, err := b.store.ListTraces(ctx, state.TraceFilter{RunID: runID})
	if err != nil {
		return 0, "", fmt.Errorf("audit: list traces for run %q: %w", runID, err)
	}
	if len(traces) == 0 {
		return 0, "", ErrNoTraces
	}

	total := 0
	prevHash := ""
	for start := 0; start < len(traces); start += batchSize {
		end := start + batchSize
		if end > len(traces) {
			end = len(traces)
		}
		batch := traces[start:end]

		n, tail, err := b.buildChainWithPrev(ctx, batch, prevHash)
		if err != nil {
			return total, "", fmt.Errorf("audit: build batch [%d:%d]: %w", start, end, err)
		}
		total += n
		prevHash = tail
	}

	return total, prevHash, nil
}

// buildChain builds a hash chain over the given traces assuming the first
// record has an empty PrevHash. It mutates each trace's PrevHash/CurrHash in
// place and persists the updates to the store.
func (b *HashChainBuilder) buildChain(ctx context.Context, traces []*state.Trace) (int, string, error) {
	return b.buildChainWithPrev(ctx, traces, "")
}

// buildChainWithPrev builds a hash chain over the given traces using prevHash
// as the PrevHash of the first record. It mutates each trace's PrevHash/CurrHash
// in place and persists the updates to the store. Returns the number of records
// processed and the CurrHash of the last record.
func (b *HashChainBuilder) buildChainWithPrev(ctx context.Context, traces []*state.Trace, prevHash string) (int, string, error) {
	current := prevHash
	for i, t := range traces {
		t.PrevHash = current
		t.CurrHash = ComputeHash(t, current)
		if err := b.store.UpdateTrace(ctx, t); err != nil {
			return i, "", fmt.Errorf("audit: update trace %q: %w", t.ID, err)
		}
		current = t.CurrHash
	}
	return len(traces), current, nil
}

// ComputeHash computes the SHA-256 hash of a trace record chained to the given
// prevHash. The hash input is the pipe-delimited concatenation of:
//
//	prevHash | trace.ID | trace.RunID | trace.Event | trace.Actor |
//	trace.Detail | trace.Timestamp.UnixNano()
//
// The result is returned as a lower-case hex-encoded string. ComputeHash is
// deterministic: the same inputs always produce the same output.
func ComputeHash(trace *state.Trace, prevHash string) string {
	if trace == nil {
		// Hashing a nil trace is a programming error; return the hash of an
		// empty payload so the chain stays well-defined rather than panicking.
		trace = &state.Trace{}
	}
	payload := prevHash +
		hashChainSeparator + trace.ID +
		hashChainSeparator + trace.RunID +
		hashChainSeparator + trace.Event +
		hashChainSeparator + trace.Actor +
		hashChainSeparator + trace.Detail +
		hashChainSeparator + strconv.FormatInt(trace.Timestamp.UnixNano(), 10)
	sum := sha256.Sum256([]byte(payload))
	return hex.EncodeToString(sum[:])
}

// Verify checks the integrity of the hash chain for the given run. It recomputes
// the chain from the stored trace records and compares each recomputed CurrHash
// against the stored CurrHash. The first mismatch yields ErrHashMismatch wrapped
// with the trace id. An empty run id yields ErrEmptyRunID; a run with no trace
// records yields ErrNoTraces.
//
// Verify does not modify the store; it only reads. Use Build to repair a broken
// chain (e.g. after a partial tamper that should actually be detected rather
// than silently fixed).
func (b *HashChainBuilder) Verify(ctx context.Context, runID string) (count int, err error) {
	if runID == "" {
		return 0, ErrEmptyRunID
	}

	traces, err := b.store.ListTraces(ctx, state.TraceFilter{RunID: runID})
	if err != nil {
		return 0, fmt.Errorf("audit: list traces for run %q: %w", runID, err)
	}
	if len(traces) == 0 {
		return 0, ErrNoTraces
	}

	prevHash := ""
	for i, t := range traces {
		if t.PrevHash != prevHash {
			return i, fmt.Errorf("audit: trace %q prev hash mismatch: stored %q want %q: %w",
				t.ID, t.PrevHash, prevHash, ErrHashMismatch)
		}
		want := ComputeHash(t, prevHash)
		if t.CurrHash != want {
			return i, fmt.Errorf("audit: trace %q curr hash mismatch: stored %q want %q: %w",
				t.ID, t.CurrHash, want, ErrHashMismatch)
		}
		prevHash = t.CurrHash
	}
	return len(traces), nil
}
