package retrybudget

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nexus/levee/internal/state"
)

func newStore(t *testing.T) state.Store {
	t.Helper()
	store, err := state.NewSQLiteStore(context.Background(), filepath.Join(t.TempDir(), "budget.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	return store
}

// seed writes one audit row the way the writers do (state's constants and the
// target helper), so the test measures the budget against the real row shape.
func seed(t *testing.T, store state.Store, id, runID, action, target string, at time.Time) {
	t.Helper()
	require.NoError(t, store.CreateAudit(context.Background(), &state.Audit{
		ID: id, RunID: runID, Action: action, Actor: "operator",
		Target: target, Result: state.AuditResultSuccess, Timestamp: at,
	}))
}

func TestRunUsageCountsOnlyThisRunAndAction(t *testing.T) {
	store := newStore(t)
	now := time.Now().UTC()

	seed(t, store, "r1", "run-a", state.AuditActionRetry, "run-a", now)
	seed(t, store, "r2", "run-a", state.AuditActionRetry, "run-a", now.Add(time.Second))
	// Rows the run budget must ignore: another run, another action.
	seed(t, store, "other-run", "run-b", state.AuditActionRetry, "run-b", now)
	seed(t, store, "other-action", "run-a", state.AuditActionRetryHost,
		state.AuditTargetHost("run-a", "h1"), now)

	got, err := RunUsage(context.Background(), store, "run-a")
	require.NoError(t, err)
	assert.Equal(t, 2, got)
}

// TestHostUsageIsUnaffectedByOtherHosts is the regression that made the Target
// filter server-side. HostUsage used to fetch the newest MaxAttempts+1
// retry_host rows for the run and filter them in Go: once the run was retried
// across enough hosts, this host's rows fell outside that window and its count
// read low — a budget that stops enforcing exactly when retries are being
// spread around, which is when it matters.
func TestHostUsageIsUnaffectedByOtherHosts(t *testing.T) {
	store := newStore(t)
	now := time.Now().UTC()

	// web-01 is retried up to the cap first...
	for i := range MaxAttempts {
		seed(t, store, fmt.Sprintf("h1-%d", i), "run-x", state.AuditActionRetryHost,
			state.AuditTargetHost("run-x", "web-01"), now.Add(time.Duration(i)*time.Second))
	}
	// ...then four OTHER hosts are retried, all of them NEWER. With a
	// Go-side filter over the newest MaxAttempts+1 rows, web-01's rows are
	// pushed out of the window and the count returns 0.
	for i := range MaxAttempts + 1 {
		seed(t, store, fmt.Sprintf("other-%d", i), "run-x", state.AuditActionRetryHost,
			state.AuditTargetHost("run-x", fmt.Sprintf("web-1%d", i)),
			now.Add(time.Duration(10+i)*time.Second))
	}

	got, err := HostUsage(context.Background(), store, "run-x", "web-01")
	require.NoError(t, err)
	assert.Equal(t, MaxAttempts, got,
		"a host's own retry count must not depend on how many other hosts were retried after it")
	assert.True(t, Exhausted(got), "and the cap must therefore still be enforced")

	// A host that was never retried reads zero, not "the newest rows".
	got, err = HostUsage(context.Background(), store, "run-x", "web-99")
	require.NoError(t, err)
	assert.Zero(t, got)
}

func TestExhaustedBoundary(t *testing.T) {
	for attempts, want := range map[int]bool{
		0: false, 1: false, MaxAttempts - 1: false,
		MaxAttempts: true, MaxAttempts + 1: true, 99: true,
	} {
		assert.Equal(t, want, Exhausted(attempts), "Exhausted(%d)", attempts)
	}
}
