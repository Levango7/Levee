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
// This ratchet pins the LIMIT of what key-epoch tagging can fix. The 2026-11-23
// D-8 work DID add a per-row key-epoch marker (a short key fingerprint embedded
// in NEW keyed digests as "k<8hex>:"), so a keyed chain built TODAY reports
// key_epoch_mismatch (distinct from tampering) when the key rotates — see
// key_epoch_test.go. But this test builds its chain UNKEYED (no epoch tag can
// exist for an unkeyed digest), then adds a key. Those un-tagged historical rows
// cannot be epoch-checked, so they still surface as a hash mismatch. That is
// the unavoidable boundary for pre-epoch data, and the reason the delivery
// checklist still REQUIRES the key for new deployments and WARNs against adding
// it to an existing unkeyed deployment (docs/deployment.md §5.6 and §15).
//
// If someone ever makes unkeyed→keyed historical rows verifiable (which would
// require recording an "unkeyed epoch" marker at write time), this test turns
// red and the change must be made deliberately — with the checklist text
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
