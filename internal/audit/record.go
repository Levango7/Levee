package audit

import (
	"context"
	"fmt"

	"github.com/nexus/levee/internal/log"
	"github.com/nexus/levee/internal/state"
)

// Record persists an audit entry and then seals the hash chain over it.
//
// It exists because a hash chain that is never sealed is not evidence of
// anything: every row would carry an empty CurrHash and verification would
// either fail forever or — worse — be written to ignore empty hashes, which is
// exactly the blind spot a DBA needs. The trace chain has this shape of gap
// (its builder is wired to nothing but tests); the audit chain must not
// inherit it.
//
// Seal failures are logged, not returned. The row itself is already durably
// written at that point, and LEVEE treats an audit write as non-fatal for the
// operation that triggered it. A row left unsealed is not silently accepted
// though — Verify reports it as FailureEmptyHash, so the gap is visible at the
// next verification and the next successful Seal closes it.
func Record(ctx context.Context, store state.Store, a *state.Audit) error {
	if store == nil {
		return ErrNilStore
	}
	if a == nil {
		return fmt.Errorf("audit: record: nil audit entry")
	}
	if err := store.CreateAudit(ctx, a); err != nil {
		return err
	}
	b, err := NewAuditChainBuilder(store)
	if err != nil {
		return err
	}
	if _, err := b.Seal(ctx); err != nil {
		log.Warn("audit: hash chain seal failed",
			"audit_id", a.ID,
			"run_id", a.RunID,
			"action", a.Action,
			"error", err)
	}
	return nil
}
