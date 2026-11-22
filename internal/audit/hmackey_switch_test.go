package audit

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nexus/levee/internal/state"
)

// TestHMACKeying_KeySwitchInvalidatesHistoricalChain pins the operational
// hazard behind the delivery-checklist scoping of LEVEE_AUDIT_HMAC_KEY.
//
// The HMAC key is deployment-global (one env var, one funnel in canonical.go)
// and chain records carry NO key-epoch marker — the trace/audit tables have
// prev_hash/curr_hash and nothing else. (The 2026-11 SM3 migration added a
// per-row ALGORITHM marker as a hash prefix, but that is orthogonal to the key
// epoch and does not help here.) So a deployment that starts keyed can no
// longer recompute the digests of a chain that was built unkeyed: the verifier
// reports "hash mismatch" for every historical record.
//
// Two consequences this test makes explicit:
//
//  1. The delivery checklist must REQUIRE the key for new deployments and
//     must WARN against adding it to an existing unkeyed deployment at
//     handover time (docs/deployment.md §5.6 and §15).
//  2. The verifier cannot tell "the operator changed the key" apart from
//     "someone rewrote the chain" — both surface as a broken chain. That
//     gap is registered as docs/blocked-items.md D-8.
//
// This is a ratchet, not a wish: if someone later adds a per-record KEY-EPOCH
// marker (distinct from the algorithm marker already added), this test turns
// red and the change has to be made deliberately — with the checklist text
// updated in the same commit — rather than silently.
func TestHMACKeying_KeySwitchInvalidatesHistoricalChain(t *testing.T) {
	unsetHMACKey(t)
	b, store := newChainBuilder(t)
	ctx := context.Background()
	createRun(t, store, "run-key-switch")
	recordTraces(t, store, "run-key-switch", 4)

	_, _, err := b.Build(ctx, "run-key-switch")
	require.NoError(t, err)

	// Unkeyed: the chain verifies.
	count, err := b.Verify(ctx, "run-key-switch")
	require.NoError(t, err)
	assert.Equal(t, 4, count)
	require.False(t, Keyed())

	// The operator adds the key (or rotates it). The historical chain is
	// now unverifiable — not because it was tampered with, but because the
	// digest algorithm changed underneath it.
	setHMACKey(t, "0123456789abcdef0123456789abcdef")
	require.True(t, Keyed())

	count, err = b.Verify(ctx, "run-key-switch")
	require.Error(t, err, "enabling the key on an existing chain must not silently verify")
	assert.ErrorIs(t, err, ErrHashMismatch)
	assert.Equal(t, 0, count, "the first historical record already fails to recompute")

	// The read-only verifier used by the console endpoints says the same
	// thing (one verifier, one answer).
	traces, err := store.ListTraces(ctx, state.TraceFilter{RunID: "run-key-switch"})
	require.NoError(t, err)
	require.Len(t, traces, 4)
	result := VerifyTraces("run-key-switch", traces)
	assert.False(t, result.Valid)
	assert.Len(t, result.Failures, 4)
}
