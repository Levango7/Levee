// Package retrybudget owns the retry cap and the audit-row counting that
// enforces it.
//
// The cap is a product promise — docs/levee-api.md: "重试次数有上限（默认 3），
// 超限升级人工" — and it is enforced by COUNTING AUDIT ROWS rather than by a
// counter column: a retry attempt is an audit row, so the budget is derived
// from the trail instead of living beside it (there is no second copy of the
// number to drift). That only works if every entry point counts the same rows,
// which is why this lives here and not in either caller:
//
//   - cmd/levee (levee retry / levee retry-host)
//   - internal/grpc (RetryChange / RetryHost RPCs — what the console calls)
//
// Both used to be possible to satisfy separately, and the RPC side simply did
// not count at all: the cap was enforced by the CLI against rows the API could
// write in a shape the CLI could not see, so the documented limit could be
// exceeded from the console.
//
// Two budgets, one per action, matching what each row records:
//
//   - a RUN budget: audit.action=retry rows for the run (whole-run retries);
//   - a PER-HOST budget: audit.action=retry_host rows whose target is
//     state.AuditTargetHost(runID, host).
package retrybudget

import (
	"context"
	"fmt"

	"github.com/nexus/levee/internal/state"
)

// MaxAttempts is the retry cap, per run and per host. It is one number on
// purpose: the CLI messages, the JSON output and the RPC refusals all name it,
// and a second copy would be a second answer to "how many are allowed".
const MaxAttempts = 3

// RunUsage reports how many run-level retry attempts are recorded for runID.
//
// The Limit is MaxAttempts+1, not a count(*) equivalent: callers only need to
// know whether the cap is reached, and the bound keeps a run with a long retry
// history from loading it entirely.
func RunUsage(ctx context.Context, store state.Store, runID string) (int, error) {
	rows, err := store.ListAudits(ctx, state.AuditFilter{
		RunID:  runID,
		Action: state.AuditActionRetry,
		Limit:  MaxAttempts + 1,
	})
	if err != nil {
		return 0, fmt.Errorf("retrybudget: count run retries: %w", err)
	}
	return len(rows), nil
}

// HostUsage reports how many retry attempts are recorded for one host of a run.
//
// The Target filter is applied by the store, not here: filtering in Go after a
// LIMITed query under-counts as soon as a run is retried across several hosts
// (the newest MaxAttempts+1 rows belong to other hosts and this host's rows
// fall outside the window), which is a budget that silently stops enforcing.
func HostUsage(ctx context.Context, store state.Store, runID, host string) (int, error) {
	rows, err := store.ListAudits(ctx, state.AuditFilter{
		RunID:  runID,
		Action: state.AuditActionRetryHost,
		Target: state.AuditTargetHost(runID, host),
		Limit:  MaxAttempts + 1,
	})
	if err != nil {
		return 0, fmt.Errorf("retrybudget: count host retries: %w", err)
	}
	return len(rows), nil
}

// Exhausted reports whether a recorded attempt count has reached the cap. It is
// the one place the comparison lives, so "at the limit" cannot mean one thing
// in a CLI refusal and another in an RPC refusal.
func Exhausted(attempts int) bool {
	return attempts >= MaxAttempts
}
