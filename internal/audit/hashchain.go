package audit

import (
	"context"
	"errors"
	"fmt"

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
	// ErrChainAlreadyBuilt is returned by Build when the hash chain for a run
	// already exists and is intact. Rebuilding would destroy tampering evidence;
	// use BuildForce for administrative recovery.
	ErrChainAlreadyBuilt = errors.New("audit: hash chain already built for run; use BuildForce to override")
	// ErrChainBroken is returned by Build when the hash chain for a run exists
	// but is broken (tampered). Rebuilding would silently cover up the tampering;
	// manual intervention is required.
	ErrChainBroken = errors.New("audit: hash chain exists but is broken; manual intervention required")
)

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

// chainExists checks whether a run's trace records already have a hash chain.
// It returns three values:
//   - exists: true when the run has at least one trace record.
//   - hasHashes: true when at least one trace record has a non-empty CurrHash,
//     meaning the chain was previously built.
//   - err: non-nil when the store query fails.
func (b *HashChainBuilder) chainExists(ctx context.Context, runID string) (exists bool, hasHashes bool, err error) {
	traces, err := b.store.ListTraces(ctx, state.TraceFilter{RunID: runID})
	if err != nil {
		return false, false, err
	}
	if len(traces) == 0 {
		return false, false, nil
	}
	for _, t := range traces {
		if t.CurrHash != "" {
			return true, true, nil
		}
	}
	return true, false, nil
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

	// SA-002 fix: Check if chain already exists before building.
	exists, hasHashes, err := b.chainExists(ctx, runID)
	if err != nil {
		return 0, "", fmt.Errorf("audit: check chain existence for run %q: %w", runID, err)
	}
	if !exists {
		return 0, "", ErrNoTraces
	}
	if hasHashes {
		// Chain already exists — verify it before deciding.
		_, verifyErr := b.Verify(ctx, runID)
		if verifyErr == nil {
			// Chain is intact — refuse to rebuild.
			return 0, "", fmt.Errorf("audit: build chain for run %q: %w", runID, ErrChainAlreadyBuilt)
		}
		// Chain is broken — refuse to rebuild silently.
		return 0, "", fmt.Errorf("audit: build chain for run %q: %w", runID, ErrChainBroken)
	}
	// No hashes yet — safe to build.

	traces, err := b.store.ListTraces(ctx, state.TraceFilter{RunID: runID})
	if err != nil {
		return 0, "", fmt.Errorf("audit: list traces for run %q: %w", runID, err)
	}
	if len(traces) == 0 {
		return 0, "", ErrNoTraces
	}

	return b.buildChain(ctx, traces)
}

// SealResult reports what one Seal call did.
type SealResult struct {
	// Count is the number of trace records the walk examined.
	Count int
	// Sealed is how many records had their chain columns written — the newly
	// appended ones, plus any hole left by an earlier failed seal. Records
	// already carrying the right hash are not counted.
	Sealed int
	// TailHash is the CurrHash of the last record, i.e. the value the next
	// appended record chains onto.
	TailHash string
}

// Seal extends the run's chain over its unsealed tail and reports how many
// records it had to write. It is the production seal, and the counterpart of
// the audit chain's Seal of the same name: both walk the stored order, keep a
// running predecessor hash, and write only the records whose stored hashes
// differ from the recomputed ones.
//
// What makes it safe to call on every append is what it refuses to do. A record
// whose stored chain does not verify is NOT rewritten — the walk stops and
// returns ErrChainBroken. So:
//
//   - A record written after the chain was closed (CurrHash empty) is sealed and
//     the chain grows. This is the case that made a settle-time-only seal
//     untenable: a late trace arriving after a Build left Build refusing
//     forever, so /audit/verify reported the run as tampered for good.
//   - A record that IS sealed but does not verify is evidence — its content was
//     altered, or a row was inserted or removed with the WORM triggers dropped.
//     Recomputing would relink the chain and launder that evidence, so Seal
//     leaves it alone and reports it. BuildForce remains the deliberate
//     administrative override.
//
// Those two rules together mean a seal can never turn a broken chain back into
// a passing one, which is what makes it safe to run unattended on the write
// path — the audit chain's Seal can, since it rewrites any row that differs.
//
// Concurrent seals are safe the way the audit chain's are: hashes are derived
// from stored content, so two overlapping walks compute identical values for the
// records they share and at worst write the same hash twice.
//
// The cost is a full walk of the run's traces per call, so sealing on every
// append is O(n²) in a run's trace count. That is affordable at MVP volumes —
// a run carries roughly one record per step, gate and approval decision — and it
// is written down here because the audit chain's equivalent walk is global and
// needed paging for the same reason (see auditchain.go).
func (b *HashChainBuilder) Seal(ctx context.Context, runID string) (*SealResult, error) {
	if runID == "" {
		return nil, ErrEmptyRunID
	}

	// ListTraces orders by (timestamp, id) ascending — the same total order the
	// hashes are computed over. No Go-side re-sort: that would be a second place
	// where ties could break differently from the seal that wrote the hashes.
	traces, err := b.store.ListTraces(ctx, state.TraceFilter{RunID: runID})
	if err != nil {
		return nil, fmt.Errorf("audit: list traces for run %q: %w", runID, err)
	}
	if len(traces) == 0 {
		return nil, ErrNoTraces
	}

	result := &SealResult{Count: len(traces)}
	prev := ""
	for _, t := range traces {
		want := ComputeHash(t, prev)
		switch {
		case t.PrevHash == prev && t.CurrHash == want:
			// Already exactly what this walk would write: the steady state, and
			// the reason the comparison comes before the write.
		case t.CurrHash == "":
			// Unsealed — newly appended, or a hole left by an earlier failure.
			if err := b.store.UpdateTraceChain(ctx, t.ID, prev, want); err != nil {
				return result, fmt.Errorf("audit: seal trace %q: %w", t.ID, err)
			}
			result.Sealed++
		default:
			// Sealed, but not with the hash its position and content imply.
			// Report it; do not rewrite it.
			return result, fmt.Errorf("audit: seal run %q: record %q is sealed but does not verify: %w",
				runID, t.ID, ErrChainBroken)
		}
		prev = want
	}
	result.TailHash = prev
	return result, nil
}

// BuildForce forcefully rebuilds the hash chain even if one already exists.
// This should only be used for administrative recovery after a confirmed
// tampering incident. The caller is responsible for logging and auditing
// the force rebuild.
func (b *HashChainBuilder) BuildForce(ctx context.Context, runID string) (count int, tailHash string, err error) {
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

	// SA-002 fix: Check if chain already exists before building.
	exists, hasHashes, err := b.chainExists(ctx, runID)
	if err != nil {
		return 0, "", fmt.Errorf("audit: check chain existence for run %q: %w", runID, err)
	}
	if !exists {
		return 0, "", ErrNoTraces
	}
	if hasHashes {
		_, verifyErr := b.Verify(ctx, runID)
		if verifyErr == nil {
			return 0, "", fmt.Errorf("audit: build batch chain for run %q: %w", runID, ErrChainAlreadyBuilt)
		}
		return 0, "", fmt.Errorf("audit: build batch chain for run %q: %w", runID, ErrChainBroken)
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

// ComputeHash computes the canonical (V2) hash of a trace record chained to
// the given prevHash: a SHA-256 digest — or HMAC-SHA256 when
// LEVEE_AUDIT_HMAC_KEY is configured — over the length-prefixed concatenation of:
//
//	prevHash, trace.ID, trace.RunID, trace.Event, trace.Actor,
//	trace.Detail, trace.Timestamp.UnixNano()
//
// Each field is encoded as "<len>:<field>" and concatenated without any
// separator, removing the ambiguity of the legacy pipe-delimited scheme.
// The result is returned as a lower-case hex-encoded string. ComputeHash is
// deterministic: the same inputs always produce the same output. V2 is the
// default for all newly built chains; pre-V2 chains are still recognised by
// Verify via the private legacy fallback (canonicalV1).
func ComputeHash(trace *state.Trace, prevHash string) string {
	return ComputeHashV2(trace, prevHash)
}

// Verify checks the integrity of the hash chain for the given run. It recomputes
// the chain from the stored trace records and compares each recomputed CurrHash
// against the stored CurrHash. The first mismatch yields ErrHashMismatch wrapped
// with the trace id. An empty run id yields ErrEmptyRunID; a run with no trace
// records yields ErrNoTraces.
//
// Legacy chains: records hashed with the pre-V2 pipe-delimited canonical
// encoding are accepted — when the V2 digest mismatches, the legacy V1 digest
// is tried before reporting a mismatch, so chains built before the V2
// upgrade do not suddenly fail verification. New and rebuilt chains always
// use V2. Use ChainVerifier when per-record legacy accounting is required.
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
		if t.CurrHash != want && t.CurrHash != legacyHash(t, prevHash) {
			return i, fmt.Errorf("audit: trace %q curr hash mismatch: stored %q want %q: %w",
				t.ID, t.CurrHash, want, ErrHashMismatch)
		}
		prevHash = t.CurrHash
	}
	return len(traces), nil
}
