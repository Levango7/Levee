package audit

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nexus/levee/internal/state"
)

func TestDenialTarget(t *testing.T) {
	assert.Equal(t, "apply@prod", DenialTarget("apply", "prod"))
	// internal/pause passes no environment (its permission names carry no
	// environment), so its rows keep the bare-permission Target they had.
	assert.Equal(t, "pause:all", DenialTarget("pause:all", ""))
	assert.Equal(t, "", DenialTarget("", ""))
}

func TestNewDenialRecorder_WritesTheRefusalIntoTheAuditChain(t *testing.T) {
	store := newTestStore(t)
	rec := NewDenialRecorder(store)

	rec(context.Background(), "carol", "apply", "prod")

	rows, err := store.ListAudits(context.Background(), state.AuditFilter{Action: ActionPermissionDenied})
	require.NoError(t, err)
	require.Len(t, rows, 1)
	row := rows[0]
	assert.Equal(t, ResultDenied, row.Result, "a refusal is a row of its own, not a failure")
	assert.Equal(t, "carol", row.Actor)
	assert.Equal(t, "apply@prod", row.Target)
	// RunID empty is the whole reason this sink is audit and not trace:
	// audit.run_id carries no foreign key, so a refusal that happens before any
	// run is picked can still be recorded (the trace table's run_id references
	// runs(id) and would reject the row).
	assert.Empty(t, row.RunID)
	// Record() seals the row into the hash chain; an unsealed row would mean
	// the refusal is in the table but not in the tamper-evident log.
	assert.NotEmpty(t, row.CurrHash, "the refusal must be sealed into the audit chain")
}

func TestNewDenialRecorder_NilStoreIsInert(t *testing.T) {
	rec := NewDenialRecorder(nil)
	require.NotPanics(t, func() {
		rec(context.Background(), "carol", "apply", "prod")
	})
}
