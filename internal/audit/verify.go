package audit

import (
	"context"
	"errors"
	"fmt"

	"github.com/nexus/levee/internal/state"
)

// Sentinel errors for chain verification.
var (
	// ErrVerifyFailed is returned by VerifyStrict when chain verification
	// detects one or more tampered records. The wrapped VerifyResult is not
	// included; callers should use Verify when they need the failure details.
	ErrVerifyFailed = errors.New("audit: chain verification failed")
)

// FailureType identifies the kind of tampering detected by ChainVerifier.
type FailureType int

const (
	// FailureHashMismatch indicates the stored CurrHash does not match the
	// hash recomputed from the trace record's content. This is the strongest
	// signal of tampering: someone modified the record's payload (event,
	// actor, detail, timestamp, ...) without rebuilding the chain.
	FailureHashMismatch FailureType = iota
	// FailurePrevHashMismatch indicates the stored PrevHash does not equal
	// the previous record's stored CurrHash, breaking the chain continuity.
	// This happens when a record is inserted or removed without rebuilding,
	// or when a CurrHash is directly overwritten.
	FailurePrevHashMismatch
	// FailureEmptyHash indicates the stored CurrHash is empty, meaning the
	// hash chain has not been built for this record (Build was never run or
	// the record was added after Build).
	FailureEmptyHash
)

// String returns a human-readable description of the failure type.
func (ft FailureType) String() string {
	switch ft {
	case FailureHashMismatch:
		return "hash_mismatch"
	case FailurePrevHashMismatch:
		return "prev_hash_mismatch"
	case FailureEmptyHash:
		return "empty_hash"
	default:
		return "unknown"
	}
}

// ChainFailure describes a single tampered record detected during
// verification. The fields capture both the expected and actual values so
// that callers can build a detailed tamper report without re-running the
// verification.
type ChainFailure struct {
	TraceID      string      // id of the tampered trace record
	Index        int         // 0-based position of the record in the chain
	Expected     string      // expected CurrHash (recomputed from content)
	Actual       string      // stored CurrHash
	PrevExpected string      // expected PrevHash (previous record's CurrHash)
	PrevActual   string      // stored PrevHash
	Type         FailureType // kind of failure
}

// VerifyResult is the outcome of a hash-chain verification. Valid is true when
// every record's stored CurrHash matches the recomputed hash and every
// PrevHash equals the previous record's CurrHash. When Valid is false,
// Failures contains one entry per tampered record.
type VerifyResult struct {
	RunID    string         // run id that was verified
	Valid    bool           // true when the chain is intact
	Count    int            // number of trace records checked
	Failures []ChainFailure // tamper details (empty when Valid)
}

// ChainVerifier verifies the hash-chain integrity of a run's trace records.
// It recomputes each trace's hash from its stored content and compares it
// against the stored CurrHash, and checks that each PrevHash equals the
// previous record's CurrHash. Any discrepancy is reported as a ChainFailure.
//
// ChainVerifier is read-only: it never modifies the store. Use
// HashChainBuilder.Build to repair a broken chain after deciding that the
// tampering should be resolved by rebuilding rather than rejected.
type ChainVerifier struct {
	store state.Store
}

// NewChainVerifier creates a ChainVerifier backed by the given store. The
// store must be non-nil; otherwise ErrNilStore is returned.
func NewChainVerifier(store state.Store) (*ChainVerifier, error) {
	if store == nil {
		return nil, ErrNilStore
	}
	return &ChainVerifier{store: store}, nil
}

// Verify checks the integrity of the hash chain for the given run. It returns
// a VerifyResult whose Valid field is true when the chain is intact and false
// when one or more records were tampered. In the latter case Failures holds
// one entry per tampered record, in chain order.
//
// Verify does not modify the store. An empty run id yields ErrEmptyRunID; a
// run with no trace records yields ErrNoTraces. A non-nil error means
// verification could not be performed; a nil error with Valid=false means
// verification completed and detected tampering.
func (v *ChainVerifier) Verify(ctx context.Context, runID string) (*VerifyResult, error) {
	if runID == "" {
		return nil, ErrEmptyRunID
	}

	traces, err := v.store.ListTraces(ctx, state.TraceFilter{RunID: runID})
	if err != nil {
		return nil, fmt.Errorf("audit: list traces for run %q: %w", runID, err)
	}
	if len(traces) == 0 {
		return nil, ErrNoTraces
	}

	result := &VerifyResult{
		RunID: runID,
		Count: len(traces),
		Valid: true,
	}

	// prevHash tracks the previous record's stored CurrHash. The first
	// record's expected PrevHash is the empty string.
	prevHash := ""
	for i, t := range traces {
		failure := checkTrace(t, i, prevHash)
		if failure != nil {
			result.Failures = append(result.Failures, *failure)
			result.Valid = false
		}
		// Advance prevHash to the stored CurrHash so the next record's
		// PrevHash is compared against this record's stored value (not the
		// recomputed one). This keeps the continuity check independent of
		// whether the current record's CurrHash was tampered.
		prevHash = t.CurrHash
	}

	return result, nil
}

// VerifyStrict is like Verify but returns ErrVerifyFailed when the chain is
// tampered, instead of a VerifyResult. It returns nil when the chain is
// intact. Callers that need the failure details should use Verify instead.
//
// The returned error wraps ErrVerifyFailed so that callers can use errors.Is
// to detect the tampering case distinctly from operational errors
// (ErrEmptyRunID, ErrNoTraces, store errors).
func (v *ChainVerifier) VerifyStrict(ctx context.Context, runID string) error {
	result, err := v.Verify(ctx, runID)
	if err != nil {
		return err
	}
	if !result.Valid {
		return fmt.Errorf("audit: chain verification failed for run %q: %d of %d records tampered: %w",
			result.RunID, len(result.Failures), result.Count, ErrVerifyFailed)
	}
	return nil
}

// checkTrace checks a single trace record against the expected prevHash. It
// returns a non-nil ChainFailure when the record is tampered, nil when it is
// intact. The checks are applied in priority order so that each record
// reports at most one failure:
//  1. Empty CurrHash (unbuilt chain) — most fundamental.
//  2. PrevHash continuity break.
//  3. CurrHash mismatch (content tampering).
func checkTrace(t *state.Trace, index int, prevHash string) *ChainFailure {
	expected := ComputeHash(t, prevHash)

	// 1. Empty CurrHash means the chain was never built for this record.
	if t.CurrHash == "" {
		return &ChainFailure{
			TraceID:      t.ID,
			Index:        index,
			Expected:     expected,
			Actual:       t.CurrHash,
			PrevExpected: prevHash,
			PrevActual:   t.PrevHash,
			Type:         FailureEmptyHash,
		}
	}

	// 2. PrevHash must equal the previous record's CurrHash.
	if t.PrevHash != prevHash {
		return &ChainFailure{
			TraceID:      t.ID,
			Index:        index,
			Expected:     expected,
			Actual:       t.CurrHash,
			PrevExpected: prevHash,
			PrevActual:   t.PrevHash,
			Type:         FailurePrevHashMismatch,
		}
	}

	// 3. CurrHash must match the recomputed hash.
	if t.CurrHash != expected {
		return &ChainFailure{
			TraceID:      t.ID,
			Index:        index,
			Expected:     expected,
			Actual:       t.CurrHash,
			PrevExpected: prevHash,
			PrevActual:   t.PrevHash,
			Type:         FailureHashMismatch,
		}
	}

	return nil
}
