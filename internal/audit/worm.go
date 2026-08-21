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

// Sentinel errors for the WORM store. Callers can use errors.Is to distinguish
// failure modes.
var (
	// ErrAlreadyExists is returned by Append when a trace with the given id is
	// already present. WORM semantics forbid overwriting an existing record.
	ErrAlreadyExists = errors.New("audit: trace already exists")
	// ErrTampered is returned by Read/ReadByRun when the stored checksum does not
	// match the recomputed checksum of the record content, indicating that the
	// record was modified after it was originally appended.
	ErrTampered = errors.New("audit: trace tampered: checksum mismatch")
	// ErrWORMNotFound is returned by Read when no trace with the given id exists.
	ErrWORMNotFound = errors.New("audit: trace not found")
)

// checksumSeparator is the delimiter used when concatenating trace fields into
// the WORM checksum input. A pipe is chosen for consistency with the hash-chain
// builder; it does not appear in any individual field by construction (ids are
// hex, events/actors are identifiers).
const checksumSeparator = "|"

// WORMStore simulates Write-Once-Read-Many semantics on top of a regular
// state.WORMStore. It enforces append-only writes for trace records and detects
// tampering by verifying a content checksum on every read.
//
// The store wraps a state.WORMStore and exposes only the operations that are
// consistent with WORM semantics:
//   - Append: insert a new trace record (no update/delete).
//   - Read / ReadByRun: read records and verify their checksums.
//   - Count: count records for a run.
//
// Update and Delete operations are intentionally not exposed. The underlying
// state.WORMStore interface omits UpdateTrace/DeleteTrace, so callers using
// WORMStore cannot bypass the append-only contract through this API. The SQLite
// triggers (worm_prevent_trace_update / worm_prevent_trace_delete) provide an
// additional defence-in-depth layer at the database level.
//
// The checksum is stored in the Trace.CurrHash field and covers the record
// content (id, run_id, event, actor, detail, timestamp). On read, the checksum
// is recomputed from the stored content and compared against the stored value;
// a mismatch indicates that the record was tampered with after it was appended.
type WORMStore struct {
	store state.WORMStore
}

// NewWORMStore creates a WORMStore backed by the given store. The store must be
// non-nil; otherwise ErrNilStore is returned.
func NewWORMStore(store state.WORMStore) (*WORMStore, error) {
	if store == nil {
		return nil, ErrNilStore
	}
	return &WORMStore{store: store}, nil
}

// Append inserts a new trace record. It computes a SHA-256 checksum of the
// record content and stores it in the CurrHash field before delegating to the
// underlying store's CreateTrace.
//
// Append is write-once: if a trace with the same id already exists, it returns
// an error wrapping ErrAlreadyExists. The input trace must be non-nil with a
// non-empty id. The CurrHash field of the input trace is overwritten with the
// computed checksum; callers may inspect it after a successful call.
//
// SECURITY: The checksum is computed before the write. If CreateTrace fails,
// the error is returned to the caller. This ensures that WORM integrity is
// not silently bypassed — callers must handle the error and retry or abort.
func (w *WORMStore) Append(ctx context.Context, trace *state.Trace) error {
	if trace == nil {
		return fmt.Errorf("audit: append trace: nil trace")
	}
	if trace.ID == "" {
		return fmt.Errorf("audit: append trace: empty id")
	}

	// Reject overwrite: WORM allows only the first write for a given id.
	existing, err := w.store.GetTrace(ctx, trace.ID)
	if err != nil {
		return fmt.Errorf("audit: append trace %q: %w", trace.ID, err)
	}
	if existing != nil {
		return fmt.Errorf("audit: append trace %q: %w", trace.ID, ErrAlreadyExists)
	}

	// Compute and attach the content checksum. CurrHash is the canonical
	// location for the checksum; PrevHash is left untouched so that the
	// hash-chain builder (T044) can still be run separately if desired.
	trace.CurrHash = computeChecksum(trace)

	if err := w.store.CreateTrace(ctx, trace); err != nil {
		return fmt.Errorf("audit: append trace %q: %w", trace.ID, err)
	}
	return nil
}

// Read returns the trace record with the given id and verifies its checksum.
// If no record exists, it returns ErrWORMNotFound. If the stored checksum does
// not match the recomputed checksum of the stored content, it returns an error
// wrapping ErrTampered.
func (w *WORMStore) Read(ctx context.Context, id string) (*state.Trace, error) {
	trace, err := w.store.GetTrace(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("audit: read trace %q: %w", id, err)
	}
	if trace == nil {
		return nil, ErrWORMNotFound
	}
	if !verifyChecksum(trace) {
		return nil, fmt.Errorf("audit: read trace %q: %w", id, ErrTampered)
	}
	return trace, nil
}

// ReadByRun returns all trace records for the given run, ordered by timestamp
// ascending. Each record's checksum is verified; if any record fails
// verification, the function returns an error wrapping ErrTampered and no
// records. An empty slice is returned when the run has no traces.
func (w *WORMStore) ReadByRun(ctx context.Context, runID string) ([]*state.Trace, error) {
	traces, err := w.store.ListTraces(ctx, state.TraceFilter{RunID: runID})
	if err != nil {
		return nil, fmt.Errorf("audit: read by run %q: %w", runID, err)
	}
	for _, t := range traces {
		if !verifyChecksum(t) {
			return nil, fmt.Errorf("audit: read by run %q: trace %q: %w", runID, t.ID, ErrTampered)
		}
	}
	return traces, nil
}

// Count returns the number of trace records for the given run. It does not
// verify checksums; use ReadByRun when integrity verification is required.
func (w *WORMStore) Count(ctx context.Context, runID string) (int, error) {
	traces, err := w.store.ListTraces(ctx, state.TraceFilter{RunID: runID})
	if err != nil {
		return 0, fmt.Errorf("audit: count run %q: %w", runID, err)
	}
	return len(traces), nil
}

// computeChecksum returns the SHA-256 checksum of a trace record's content.
// The checksum input is the pipe-delimited concatenation of:
//
//	trace.ID | trace.RunID | trace.Event | trace.Actor | trace.Detail | trace.Timestamp.UnixNano()
//
// The result is a lower-case hex-encoded string. computeChecksum is
// deterministic: identical inputs always produce identical outputs. A nil
// trace is treated as an empty record so the function never panics.
func computeChecksum(trace *state.Trace) string {
	if trace == nil {
		trace = &state.Trace{}
	}
	payload := trace.ID +
		checksumSeparator + trace.RunID +
		checksumSeparator + trace.Event +
		checksumSeparator + trace.Actor +
		checksumSeparator + trace.Detail +
		checksumSeparator + strconv.FormatInt(trace.Timestamp.UnixNano(), 10)
	sum := sha256.Sum256([]byte(payload))
	return hex.EncodeToString(sum[:])
}

// verifyChecksum reports whether the stored CurrHash matches the checksum
// recomputed from the record's content. A mismatch indicates tampering.
func verifyChecksum(trace *state.Trace) bool {
	if trace == nil {
		return false
	}
	return trace.CurrHash == computeChecksum(trace)
}
