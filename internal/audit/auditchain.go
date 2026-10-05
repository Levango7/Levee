package audit

import (
	"context"
	"fmt"
	"strconv"

	"github.com/nexus/levee/internal/state"
)

// This file implements the hash chain over the `audit` table — the high-level
// "who did what to which target" log served by GET /audit/log. It is the
// sibling of the per-run `trace` chain in hashchain.go, and the two differ in
// two ways that matter:
//
//   - SCOPE. The trace chain is per-run. The audit chain is GLOBAL, because an
//     audit row with an empty run_id is exactly the security-relevant kind —
//     a login, a config change, a credential read — and a per-run chain
//     structurally cannot reach those rows. When multi-tenancy is enabled the
//     chain is scoped by whatever the Store exposes, so TenantStore yields one
//     chain per tenant rather than one shared chain; that is a scope
//     restriction, not a hole, since a tenant can neither see nor seal rows it
//     does not own.
//
//   - ORDER. The trace chain sorts by timestamp within a single run, whose
//     traces are written by one process in one order. The audit chain spans
//     the whole table, written by many callers, so its order must be a TOTAL
//     order or the chain would not be reproducible. See loadChainOrder.
//
// The chain is sealed by recomputing it over the stored order, not by
// appending to a tail at insert time. That choice is forced by the data: audit
// ids are 8 random bytes rendered as hex (grpc.newID), so two rows written in
// the same millisecond sort by id essentially at random. A tail-append scheme
// would chain the second-inserted row onto the first and then discover at
// verification time that the stored order is the other way round. Recomputing
// has the matching virtue that it is idempotent: two concurrent Seal calls
// compute byte-identical hashes for any row they both cover, so they cannot
// fork the chain, and neither can a retried write.

// AuditChainFailure describes one audit row that does not link up correctly.
type AuditChainFailure struct {
	AuditID      string      // id of the offending audit row
	Index        int         // 0-based position in the chain order
	Type         FailureType // reuse the trace chain's failure taxonomy
	Expected     string      // CurrHash recomputed from the row's content
	Actual       string      // CurrHash as stored
	PrevExpected string      // PrevHash the row should carry
	PrevActual   string      // PrevHash as stored
}

// AuditChainResult is the outcome of verifying the audit chain. Valid is true
// only when every visible row carries the hash its position and content imply.
type AuditChainResult struct {
	Count int // rows checked
	// Unsealed counts rows carrying no hash at all — the signature of a Seal
	// that did not run or did not complete, as opposed to a row whose content
	// was altered. The two need different responses, so they are counted
	// separately.
	Unsealed int
	Valid    bool                // true when the chain is intact
	Failures []AuditChainFailure // one entry per broken row, in chain order
}

// AuditChainBuilder seals and verifies the global audit hash chain.
type AuditChainBuilder struct {
	store state.Store
	// pageSize overrides how many rows one walk reads at a time; 0 means
	// state.DefaultAuditChainPageSize. Only tests set it — small enough to
	// exercise paging with a handful of rows rather than a thousand-row fixture.
	pageSize int
}

// NewAuditChainBuilder creates an AuditChainBuilder over the given store.
func NewAuditChainBuilder(store state.Store) (*AuditChainBuilder, error) {
	if store == nil {
		return nil, ErrNilStore
	}
	return &AuditChainBuilder{store: store}, nil
}

// auditContentFields is the canonical field set of an audit row. It mirrors
// contentFields for traces but is NOT the same set: audit has Target, Result
// and TenantID where trace has Event and Detail, and drops nothing. TenantID
// is in the hashed content deliberately — without it a row could be moved
// between tenants by rewriting one column, and the chain would not notice.
//
// Timestamp is hashed as UnixNano, matching the trace chain, so the value is
// independent of the location it was written in.
func auditContentFields(a *state.Audit) []string {
	if a == nil {
		a = &state.Audit{}
	}
	return []string{
		a.ID,
		a.RunID,
		a.Action,
		a.Actor,
		a.Target,
		a.Result,
		a.TenantID,
		strconv.FormatInt(a.Timestamp.UnixNano(), 10),
	}
}

// ComputeAuditHash returns the canonical (V2) hash of an audit row bound to
// prevHash: a SHA-256 — or HMAC-SHA256 when LEVEE_AUDIT_HMAC_KEY is set — over
// the length-prefixed concatenation of prevHash and the row's content fields
// (see auditContentFields), lower-case hex encoded.
//
// Unlike the trace chain there is no legacy V1 fallback: the audit chain is
// introduced together with the v7 schema, so every chained row was produced by
// this function and a V1-shaped digest is not evidence of an older record, it
// is evidence of a tampered one.
func ComputeAuditHash(a *state.Audit, prevHash string) string {
	fields := append([]string{prevHash}, auditContentFields(a)...)
	return digest(canonicalV2(fields...))
}

// walkChain calls fn once per audit row the store exposes, in chain order —
// ascending by (timestamp, id) — holding one page of rows at a time.
//
// The order is the chain's whole foundation, so it is read from the store as
// (timestamp, id) rather than derived here. ListAudits orders by timestamp DESC
// alone, which is not a total order: rows sharing a timestamp come back in
// whatever order the engine picks, and the engine is free to pick differently
// on the next read. With audit ids being 8 random bytes rendered as hex, that
// would make the order unstable between a Seal and a later Verify — which is
// indistinguishable from tampering. ListAuditChainPage pins the tie-breaker in
// SQL, where it is the same every time.
//
// Paging is by cursor rather than offset for a reason that only shows up in
// production: Record seals on every write, so a walk is normally running while
// rows are being appended. An offset walk would re-read rows it already
// processed and skip the oldest ones, and for a hash chain a re-read row is
// sealed against a predecessor that is no longer its own. Comparing against the
// last row read makes the walk move forward no matter what arrives at the head.
//
// The cost that paging does NOT remove is the read itself: sealing still walks
// every visible row, and Record calls it per write. A deployment with a very
// large audit log therefore still pays a full scan per audit write. Making that
// incremental means sealing only the rows after the sealed tail, which needs a
// way to tell a mid-chain hole from a merely new tail (an index over unsealed
// rows, say) — a change to what a broken chain means, so it is not folded in
// here.
func (b *AuditChainBuilder) walkChain(ctx context.Context, fn func(a *state.Audit) error) error {
	limit := b.pageSize
	if limit <= 0 {
		limit = state.DefaultAuditChainPageSize
	}
	var cursor *state.AuditCursor
	for {
		page, err := b.store.ListAuditChainPage(ctx, cursor, limit, "")
		if err != nil {
			return fmt.Errorf("audit: list audits for chain: %w", err)
		}
		for _, a := range page {
			if err := fn(a); err != nil {
				return err
			}
		}
		if len(page) < limit {
			return nil
		}
		last := page[len(page)-1]
		cursor = &state.AuditCursor{Timestamp: last.Timestamp, ID: last.ID}
	}
}

// Seal recomputes the chain over every visible audit row and persists any hash
// that is missing or wrong, returning how many rows it had to write.
//
// The walk writes only rows whose stored hash differs from the recomputed one,
// so the steady state — one new row per call — costs one read of the chain and
// a single-row update. It is safe to call concurrently: any two overlapping
// calls derive identical hashes for the rows they share, so at worst they
// write the same value twice.
//
// An empty audit log is not an error; it seals to zero rows. A failure to seal
// leaves the affected rows without hashes, which Verify then reports as
// FailureEmptyHash rather than silently passing.
func (b *AuditChainBuilder) Seal(ctx context.Context) (int, error) {
	prev := ""
	sealed := 0
	err := b.walkChain(ctx, func(a *state.Audit) error {
		want := ComputeAuditHash(a, prev)
		if a.PrevHash != prev || a.CurrHash != want {
			if err := b.store.UpdateAuditChain(ctx, a.ID, prev, want); err != nil {
				return fmt.Errorf("audit: seal audit %q: %w", a.ID, err)
			}
			sealed++
		}
		prev = want
		return nil
	})
	if err != nil {
		return sealed, err
	}
	return sealed, nil
}

// Verify walks the chain read-only and reports every row that does not link up.
// It never writes, so it cannot repair what it finds — a broken chain is
// evidence and must be investigated, not papered over. Use Seal afterwards
// only to re-establish the chain over rows that were legitimately left
// unsealed.
func (b *AuditChainBuilder) Verify(ctx context.Context) (*AuditChainResult, error) {
	result := &AuditChainResult{Valid: true}

	prev := ""
	i := 0
	err := b.walkChain(ctx, func(a *state.Audit) error {
		want := ComputeAuditHash(a, prev)
		switch {
		case a.CurrHash == "":
			result.Valid = false
			result.Unsealed++
			result.Failures = append(result.Failures, AuditChainFailure{
				AuditID: a.ID, Index: i, Type: FailureEmptyHash,
				Expected: want, Actual: a.CurrHash,
				PrevExpected: prev, PrevActual: a.PrevHash,
			})
		case a.PrevHash != prev:
			// Continuity break: this row's predecessor is not the row that
			// actually precedes it, so a row was inserted, removed or
			// relinked.
			result.Valid = false
			result.Failures = append(result.Failures, AuditChainFailure{
				AuditID: a.ID, Index: i, Type: FailurePrevHashMismatch,
				Expected: want, Actual: a.CurrHash,
				PrevExpected: prev, PrevActual: a.PrevHash,
			})
		case a.CurrHash != want:
			result.Valid = false
			result.Failures = append(result.Failures, AuditChainFailure{
				AuditID: a.ID, Index: i, Type: FailureHashMismatch,
				Expected: want, Actual: a.CurrHash,
				PrevExpected: prev, PrevActual: a.PrevHash,
			})
		}
		// Advance on the STORED CurrHash, matching ChainVerifier. A row whose
		// content was altered still carries the hash its predecessor was
		// actually built against, so continuing from the stored value keeps a
		// single tampered row from cascading into a failure report for every
		// row behind it. The real break stays the only one reported.
		prev = a.CurrHash
		i++
		return nil
	})
	if err != nil {
		return nil, err
	}
	result.Count = i
	return result, nil
}
