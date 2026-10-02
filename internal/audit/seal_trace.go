package audit

import (
	"context"
	"errors"
	"fmt"

	"github.com/nexus/levee/internal/state"
)

// SealRunTraceChain seals the per-run trace hash chain after a run settles.
//
// Until this wiring existed no trace chain was ever built outside tests:
// HashChainBuilder's Build/BuildBatch/BuildForce had zero production callers,
// so /audit/verify's trace half had nothing to verify and the takeover path's
// "lands in the run's hash chain" comment described a mechanism that could not
// run. Every funnel that settles a run into a terminal status calls this after
// its last trace write, so a settled run carries a closed, verifiable chain.
//
// Semantics, deliberately narrow:
//   - A run with no traces (rejected / cancelled before apply) has nothing to
//     seal: ErrNoTraces maps to nil, not to an error every caller must dedupe.
//   - An already-sealed chain is left alone: Build refuses to rebuild an
//     intact chain, which makes a lost settle race (the loser also calls this
//     after the winner already sealed) a no-op rather than a conflict.
//   - ErrChainBroken is returned, never swallowed: a chain that exists but
//     does not verify is evidence, and must reach the operator's log.
//
// Callers treat a returned error as non-fatal — the run's verdict is already
// durably settled, the same reasoning as audit.Record — but log it loudly:
// unsealed rows surface as empty_hash at the next verification.
func SealRunTraceChain(ctx context.Context, store state.Store, runID string) error {
	if store == nil {
		return ErrNilStore
	}
	b, err := NewHashChainBuilder(store)
	if err != nil {
		return err
	}
	_, _, err = b.Build(ctx, runID)
	switch {
	case err == nil,
		errors.Is(err, ErrNoTraces),
		errors.Is(err, ErrChainAlreadyBuilt):
		return nil
	}
	return fmt.Errorf("audit: seal trace chain for run %q: %w", runID, err)
}
