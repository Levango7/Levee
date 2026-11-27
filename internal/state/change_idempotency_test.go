package state

import (
	"context"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestChangeIdempotencyConcurrentInsertAndReopen(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "idempotency.db")
	store, err := NewSQLiteStore(ctx, path)
	require.NoError(t, err)
	for _, id := range []string{"run-1", "run-2"} {
		now := time.Now().UTC()
		require.NoError(t, store.CreateRun(ctx, &Run{ID: id, Status: "completed", CreatedAt: now, UpdatedAt: now}))
	}

	const callers = 20
	var accepted, duplicates atomic.Int32
	var wg sync.WaitGroup
	wg.Add(callers)
	for i := 0; i < callers; i++ {
		go func() {
			defer wg.Done()
			already, err := store.PutChangeIdempotency(ctx, &ChangeIdempotency{ChangeID: "run-1", IdempotencyKey: "request-1"})
			require.NoError(t, err)
			if already {
				duplicates.Add(1)
			} else {
				accepted.Add(1)
			}
		}()
	}
	wg.Wait()
	assert.Equal(t, int32(1), accepted.Load(), "exactly one caller may win the durable insert")
	assert.Equal(t, int32(callers-1), duplicates.Load())

	already, err := store.PutChangeIdempotency(ctx, &ChangeIdempotency{ChangeID: "run-1", IdempotencyKey: "request-2"})
	require.NoError(t, err)
	assert.False(t, already, "a different key is a different request")
	already, err = store.PutChangeIdempotency(ctx, &ChangeIdempotency{ChangeID: "run-2", IdempotencyKey: "request-1"})
	require.NoError(t, err)
	assert.False(t, already, "the same key on another change is a different request")
	require.NoError(t, store.Close())

	reopened, err := NewSQLiteStore(ctx, path)
	require.NoError(t, err)
	defer func() { _ = reopened.Close() }()
	record, err := reopened.GetChangeIdempotency(ctx, "run-1", "request-1")
	require.NoError(t, err)
	require.NotNil(t, record, "the accepted key must survive a process restart")
	assert.Equal(t, "run-1", record.ChangeID)
	assert.Equal(t, "request-1", record.IdempotencyKey)
}

func TestChangeIdempotencyMigrationFromV11(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	_, err := store.DB().ExecContext(ctx, "DROP TABLE change_idempotency")
	require.NoError(t, err)
	_, err = store.DB().ExecContext(ctx, "DELETE FROM schema_version")
	require.NoError(t, err)
	_, err = store.DB().ExecContext(ctx, "INSERT INTO schema_version(version) VALUES (11)")
	require.NoError(t, err)

	require.NoError(t, Migrate(ctx, store.DB()))
	var table string
	require.NoError(t, store.DB().QueryRowContext(ctx,
		"SELECT name FROM sqlite_master WHERE type='table' AND name='change_idempotency'").Scan(&table))
	assert.Equal(t, "change_idempotency", table)
	var version int
	require.NoError(t, store.DB().QueryRowContext(ctx, "SELECT MAX(version) FROM schema_version").Scan(&version))
	assert.Equal(t, currentSchemaVersion, version)
}

func TestClaimChangeExecutionIsAtomic(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	run := &Run{ID: "run-claim", Status: "approved", CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC()}
	require.NoError(t, store.CreateRun(ctx, run))

	already, transitioned, err := store.ClaimChangeExecution(ctx, &ChangeIdempotency{
		ChangeID: run.ID, IdempotencyKey: "request-claim", RequestFingerprint: "fingerprint-1",
	}, "approved", "running", time.Now().UTC())
	require.NoError(t, err)
	assert.False(t, already)
	assert.True(t, transitioned)

	already, transitioned, err = store.ClaimChangeExecution(ctx, &ChangeIdempotency{
		ChangeID: run.ID, IdempotencyKey: "request-claim", RequestFingerprint: "fingerprint-1",
	}, "approved", "running", time.Now().UTC())
	require.NoError(t, err)
	assert.True(t, already, "the existing key wins without requiring the old run status")
	assert.False(t, transitioned)

	already, transitioned, err = store.ClaimChangeExecution(ctx, &ChangeIdempotency{
		ChangeID: run.ID, IdempotencyKey: "different-request", RequestFingerprint: "fingerprint-2",
	}, "approved", "running", time.Now().UTC())
	require.NoError(t, err)
	assert.False(t, already)
	assert.False(t, transitioned, "a different key cannot bypass a lost status CAS")
	lost, err := store.GetChangeIdempotency(ctx, run.ID, "different-request")
	require.NoError(t, err)
	assert.Nil(t, lost, "a failed status CAS must roll back its newly inserted key")

	got, err := store.GetRun(ctx, run.ID)
	require.NoError(t, err)
	assert.Equal(t, "running", got.Status)
}

func TestClaimChangeExecutionOnPostgreSQL(t *testing.T) {
	store, cleanup := newPGTestStore(t)
	defer cleanup()
	ctx := context.Background()
	run := &Run{ID: "run-claim-pg", Status: "approved", CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC()}
	require.NoError(t, store.CreateRun(ctx, run))

	already, transitioned, err := store.ClaimChangeExecution(ctx, &ChangeIdempotency{
		ChangeID: run.ID, IdempotencyKey: "request-claim", RequestFingerprint: "fingerprint-1",
	}, "approved", "running", time.Now().UTC())
	require.NoError(t, err)
	assert.False(t, already)
	assert.True(t, transitioned)

	already, transitioned, err = store.ClaimChangeExecution(ctx, &ChangeIdempotency{
		ChangeID: run.ID, IdempotencyKey: "request-claim", RequestFingerprint: "fingerprint-1",
	}, "approved", "running", time.Now().UTC())
	require.NoError(t, err)
	assert.True(t, already)
	assert.False(t, transitioned)

	already, transitioned, err = store.ClaimChangeExecution(ctx, &ChangeIdempotency{
		ChangeID: run.ID, IdempotencyKey: "different-request", RequestFingerprint: "fingerprint-2",
	}, "approved", "running", time.Now().UTC())
	require.NoError(t, err)
	assert.False(t, already)
	assert.False(t, transitioned)
	lost, err := store.GetChangeIdempotency(ctx, run.ID, "different-request")
	require.NoError(t, err)
	assert.Nil(t, lost, "PostgreSQL must roll back a new key when the status CAS loses")
	got, err := store.GetRun(ctx, run.ID)
	require.NoError(t, err)
	assert.Equal(t, "running", got.Status)
}
