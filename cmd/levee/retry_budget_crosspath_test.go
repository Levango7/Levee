// retry_budget_crosspath_test.go — the per-host retry cap is a documented
// product promise ("重试次数有上限（默认 3）", docs/levee-api.md) and it is
// enforced by counting audit rows. That makes the row SHAPE a contract between
// two entry points, and it used to be broken between them:
//
//   - cmd/levee wrote Action "retry_host" + Target "<run>/<host>";
//   - the gRPC RetryHost wrote Action "retry-host" + Target "h1,h2".
//
// cmd/levee's countHostRetries filters on the first pair, so a retry triggered
// through the API was invisible to the CLI's budget: the cap could be exceeded
// by alternating entry points, and nothing failed. Both sides now name
// state.AuditActionRetryHost and build the target with state.AuditTargetHost,
// and this test seeds rows in the gRPC shape to prove the CLI's counter sees
// them — the assertion is the cross-path one, not each side's idea of itself.
package main

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nexus/levee/internal/state"
)

// seedRetryHostRow writes one retry-host audit row exactly the way the gRPC
// service writes it (same constants, same target helper). If that shape ever
// changes on one side only, this stops compiling or the count below stops
// matching — either way the budget cannot silently lose sight of API retries.
func seedRetryHostRow(t *testing.T, store state.Store, id, runID, host string) {
	t.Helper()
	require.NoError(t, store.CreateAudit(context.Background(), &state.Audit{
		ID:        id,
		RunID:     runID,
		Action:    state.AuditActionRetryHost,
		Actor:     "console-user",
		Target:    state.AuditTargetHost(runID, host),
		Result:    state.AuditResultSuccess,
		Timestamp: time.Now().UTC(),
	}))
}

func TestHostRetryBudgetCountsRowsWrittenByTheOtherEntryPoint(t *testing.T) {
	e := newCLIEnv(t)
	store := e.open(t)
	defer func() { _ = store.Close() }()
	ctx := context.Background()

	// Two retries of web-01 that the CLI never performed: they came in through
	// the API. The budget must see both.
	seedRetryHostRow(t, store, "audit-api-1", "run-x", "web-01")
	seedRetryHostRow(t, store, "audit-api-2", "run-x", "web-01")

	count, err := countHostRetries(ctx, store, "run-x", "web-01")
	require.NoError(t, err)
	assert.Equal(t, 2, count,
		"the budget counts rows by (action, target); a row written by the gRPC path must be counted or the cap is exceedable from the console")

	// A retry of a DIFFERENT host must not consume web-01's budget: the cap is
	// per host, which is why the target carries the host at all.
	seedRetryHostRow(t, store, "audit-api-3", "run-x", "web-02")
	count, err = countHostRetries(ctx, store, "run-x", "web-01")
	require.NoError(t, err)
	assert.Equal(t, 2, count, "another host's retry must not count against web-01")

	// And the last slot is genuinely the last: with the budget at 2 of 3, one
	// more seeded row must push the counter to the limit the caller refuses at.
	seedRetryHostRow(t, store, "audit-api-4", "run-x", "web-01")
	count, err = countHostRetries(ctx, store, "run-x", "web-01")
	require.NoError(t, err)
	assert.Equal(t, maxRetryAttempts, count,
		"three rows must reach the documented cap, so the next CLI retry refuses")
}
