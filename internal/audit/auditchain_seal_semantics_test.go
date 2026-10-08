package audit

// Tests for the discriminator Seal uses to tell a legal write apart from a
// tamper: a row whose stored pair still satisfies H(content, prev) == curr is
// relocated (relinked at its current position); a row whose content no longer
// yields its own stored hash is reported and never rewritten.
//
// The distinction is load-bearing in both directions:
//
//   - Without the relocation branch, seal-on-every-write turns two concurrent
//     writers whose timestamps order opposite to their inserts into a
//     permanently red chain — legal traffic reported as tampering, with no
//     operator-facing way to clear it (BuildForce/Rebuild have no production
//     caller and no CLI). That failure mode is worse than the hole it closes.
//   - Without the report branch, "edit a row, then let any audit write happen"
//     erases the edit: the next Seal relinks the altered row and Verify passes.
//
// These tests pin both halves.

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nexus/levee/internal/state"
)

// TestAuditChain_SealRelinksSelfConsistentRelocation covers the legal case: a
// row sealed earlier is pushed off its predecessor by a row that arrives with
// an EARLIER timestamp (concurrent writers, or a backdated write). Its content
// still matches its own stored hash, so the seal relinks it instead of failing.
func TestAuditChain_SealRelinksSelfConsistentRelocation(t *testing.T) {
	_, sqlite := newAuditChainBuilder(t)
	ctx := context.Background()
	base := time.Now().UTC()

	// Two rows sealed in insert order.
	writeAudits(t, sqlite, "rel", 2, base.Add(time.Hour))

	// A third row arrives whose timestamp sorts it BEFORE both of them — the
	// shape a concurrent writer with an earlier clock produces.
	late := &state.Audit{
		ID: "rel-early", RunID: "run-early", Action: "login", Actor: "zoe",
		Result: "success", Timestamp: base,
	}
	require.NoError(t, sqlite.CreateAudit(ctx, late))

	b, err := NewAuditChainBuilder(sqlite)
	require.NoError(t, err)

	// The newcomer is unsealed and the two rows behind it are self-consistent
	// but mis-linked. Seal must extend over the newcomer and relink the rest —
	// not report a tamper, which would leave a legal write permanently red.
	sealed, err := b.Seal(ctx)
	require.NoError(t, err, "a self-consistent relocation must not be reported as tampering")
	assert.Equal(t, 3, sealed, "the newcomer plus the two rows it displaced")

	result, err := b.Verify(ctx)
	require.NoError(t, err)
	assert.True(t, result.Valid, "failures: %+v", result.Failures)
	assert.Equal(t, 3, result.Count)
	assert.Equal(t, 0, result.Unsealed)
}

// TestAuditChain_SealReportsContentTamper is the case the discriminator exists
// for: a row whose content was altered (with the WORM triggers dropped) no
// longer satisfies H(content, prev) == curr. Seal must report it and must NOT
// rewrite it — before this rule, the next seal relinked the row and the only
// evidence of the edit disappeared.
func TestAuditChain_SealReportsContentTamper(t *testing.T) {
	_, sqlite := newAuditChainBuilder(t)
	ctx := context.Background()
	writeAudits(t, sqlite, "ct", 3, time.Now().UTC())

	before, err := sqlite.GetAudit(ctx, "ct-1")
	require.NoError(t, err)
	stolenCurr := before.CurrHash

	// An attacker with direct database access edits a row's content. The stored
	// hashes are left as they were — which is what makes the row self-inconsistent.
	dropWORMTriggers(t, sqlite)
	_, err = sqlite.DB().ExecContext(ctx,
		`UPDATE audit SET result = 'rolled_back', actor = 'mallory' WHERE id = 'ct-1'`)
	require.NoError(t, err)

	b, err := NewAuditChainBuilder(sqlite)
	require.NoError(t, err)

	_, err = b.Seal(ctx)
	require.Error(t, err, "altered content must be reported, not relinked")
	assert.ErrorIs(t, err, ErrChainBroken)

	// The row still carries the hash it had before the edit: the seal wrote
	// nothing to it, so the evidence is intact.
	after, err := sqlite.GetAudit(ctx, "ct-1")
	require.NoError(t, err)
	assert.Equal(t, stolenCurr, after.CurrHash, "the seal must not have rewritten the row")
	assert.Equal(t, "mallory", after.Actor)

	// Rebuild is the deliberate override, and it is what launders the edit —
	// which is why it is a separate method nothing on the write path calls.
	written, err := b.Rebuild(ctx)
	require.NoError(t, err)
	assert.GreaterOrEqual(t, written, 1)
	result, err := b.Verify(ctx)
	require.NoError(t, err)
	assert.True(t, result.Valid, "Rebuild relinks the chain over whatever is stored")
}

// TestAuditChain_SealIsIdempotentAfterRelocation checks the relocation branch
// writes nothing on a second pass: the steady state must stay a no-op, or every
// audit write would rewrite the table.
func TestAuditChain_SealIsIdempotentAfterRelocation(t *testing.T) {
	_, sqlite := newAuditChainBuilder(t)
	ctx := context.Background()
	base := time.Now().UTC()
	writeAudits(t, sqlite, "idem", 2, base.Add(time.Hour))
	require.NoError(t, sqlite.CreateAudit(ctx, &state.Audit{
		ID: "idem-early", RunID: "run-early", Action: "login", Actor: "zoe",
		Result: "success", Timestamp: base,
	}))

	b, err := NewAuditChainBuilder(sqlite)
	require.NoError(t, err)

	first, err := b.Seal(ctx)
	require.NoError(t, err)
	require.Equal(t, 3, first)

	second, err := b.Seal(ctx)
	require.NoError(t, err)
	assert.Equal(t, 0, second, "a seal over a relinked chain must write nothing")
}
