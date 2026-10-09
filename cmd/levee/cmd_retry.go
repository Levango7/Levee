package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"time"

	"github.com/spf13/cobra"

	"github.com/nexus/levee/internal/audit"
	"github.com/nexus/levee/internal/retrybudget"
	"github.com/nexus/levee/internal/state"
)

// maxRetryAttempts is the cap, owned by internal/retrybudget: the RPCs the
// console calls enforce the same number, and a second copy here would be a
// second answer to "how many retries are allowed".
const maxRetryAttempts = retrybudget.MaxAttempts

// retryHostOptHost holds the value of the <host> positional arg for retry-host.
var retryHostOptHost string

func init() {
	RegisterCommand(newRetryCmd())
	RegisterCommand(newRetryHostCmd())
}

// newRetryCmd builds the `levee retry <run-id>` sub-command.
func newRetryCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "retry <run-id>",
		Short: "Retry a failed change run",
		Long: "Retry a failed change run. The run is re-queued for execution " +
			"from the point of failure. A run may be retried at most " +
			"3 times; exceeding this limit returns exit code 5.",
		Args: cobra.ExactArgs(1),
		RunE: runRetry,
	}
	return cmd
}

// newRetryHostCmd builds the `levee retry-host <run-id> <host>` sub-command.
func newRetryHostCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "retry-host <run-id> <host>",
		Short: "Retry a failed host within a change run",
		Long: "Retry execution of a specific failed host within a change run. " +
			"A host may be retried at most 3 times; exceeding this limit " +
			"returns exit code 5.",
		Args: cobra.ExactArgs(2),
		RunE: runRetryHost,
	}
	return cmd
}

// runRetry executes the `levee retry <run-id>` command.
func runRetry(cmd *cobra.Command, args []string) error {
	retryOptRunID := args[0]
	actor := currentActor()

	ctx := context.Background()

	// 1. Open the state store.
	store, err := openStore(ctx)
	if err != nil {
		return fmt.Errorf("open store: %w", err)
	}
	defer func() { _ = store.Close() }()

	// 2. Load the run record.
	run, err := store.GetRun(ctx, retryOptRunID)
	if err != nil {
		return fmt.Errorf("get run: %w", err)
	}
	if run == nil {
		return fmt.Errorf("run %q not found [exit=1]", retryOptRunID)
	}

	// 3. Check that the run is in a retryable state.
	if !isRetryableStatus(run.Status) {
		return fmt.Errorf("run %q is in %q state, cannot retry [exit=1]", retryOptRunID, run.Status)
	}

	// 4. Check retry count via audit trail.
	retryCount, err := countRetries(ctx, store, retryOptRunID)
	if err != nil {
		return fmt.Errorf("count retries: %w", err)
	}
	if retryCount >= maxRetryAttempts {
		return fmt.Errorf("run %q has reached retry limit (%d/%d) [exit=5]", retryOptRunID, retryCount, maxRetryAttempts)
	}

	// 5. Transition the run to "running" (retry). Mint the audit identity
	// BEFORE mutating anything: a CSPRNG failure must fail the command
	// outright, never degrade to a timestamp-based audit id.
	now := time.Now().UTC()
	auditID, err := newRetryAuditID()
	if err != nil {
		return err
	}
	run.Status = "running"
	run.UpdatedAt = now
	if err := store.UpdateRun(ctx, run); err != nil {
		return fmt.Errorf("update run: %w", err)
	}

	// 6. Record an audit entry.
	entry := &state.Audit{
		ID:        auditID,
		RunID:     retryOptRunID,
		Action:    state.AuditActionRetry,
		Actor:     actor,
		Target:    retryOptRunID,
		Result:    state.AuditResultSuccess,
		Timestamp: now,
	}
	if err := audit.Record(ctx, store, entry); err != nil {
		fmt.Fprintf(os.Stderr, "warning: failed to write retry audit: %v\n", err)
	}

	// 7. Output the result.
	output := map[string]any{
		"run_id":      retryOptRunID,
		"action":      state.AuditActionRetry,
		"actor":       actor,
		"retry_count": retryCount + 1,
		"max_retries": maxRetryAttempts,
		"retried_at":  now,
	}

	if optJSON {
		return PrintJSON(os.Stdout, map[string]any{
			"data":  output,
			"meta":  nil,
			"error": nil,
		})
	}

	if optQuiet {
		fmt.Fprintln(os.Stdout, retryOptRunID)
		return nil
	}

	fmt.Fprintf(os.Stdout, "Run %s retry #%d triggered by %s\n", retryOptRunID, retryCount+1, actor)
	return nil
}

// runRetryHost executes the `levee retry-host <run-id> <host>` command.
func runRetryHost(cmd *cobra.Command, args []string) error {
	retryHostOptRunID := args[0]
	retryHostOptHost = args[1]
	actor := currentActor()

	ctx := context.Background()

	// 1. Open the state store.
	store, err := openStore(ctx)
	if err != nil {
		return fmt.Errorf("open store: %w", err)
	}
	defer func() { _ = store.Close() }()

	// 2. Load the run record.
	run, err := store.GetRun(ctx, retryHostOptRunID)
	if err != nil {
		return fmt.Errorf("get run: %w", err)
	}
	if run == nil {
		return fmt.Errorf("run %q not found [exit=1]", retryHostOptRunID)
	}

	// 3. Check that the run is in a retryable state.
	if !isRetryableStatus(run.Status) {
		return fmt.Errorf("run %q is in %q state, cannot retry [exit=1]", retryHostOptRunID, run.Status)
	}

	// 4. Check host-level retry count.
	hostRetryCount, err := countHostRetries(ctx, store, retryHostOptRunID, retryHostOptHost)
	if err != nil {
		return fmt.Errorf("count host retries: %w", err)
	}
	if hostRetryCount >= maxRetryAttempts {
		return fmt.Errorf("host %q in run %q has reached retry limit (%d/%d) [exit=5]",
			retryHostOptHost, retryHostOptRunID, hostRetryCount, maxRetryAttempts)
	}

	// 5. Record an audit entry for the host retry.
	now := time.Now().UTC()
	auditID, err := newRetryAuditID()
	if err != nil {
		return err
	}
	entry := &state.Audit{
		ID:        auditID,
		RunID:     retryHostOptRunID,
		Action:    state.AuditActionRetryHost,
		Actor:     actor,
		Target:    state.AuditTargetHost(retryHostOptRunID, retryHostOptHost),
		Result:    state.AuditResultSuccess,
		Timestamp: now,
	}
	if err := audit.Record(ctx, store, entry); err != nil {
		fmt.Fprintf(os.Stderr, "warning: failed to write retry-host audit: %v\n", err)
	}

	// 6. Output the result.
	output := map[string]any{
		"run_id":      retryHostOptRunID,
		"host":        retryHostOptHost,
		"action":      state.AuditActionRetryHost,
		"actor":       actor,
		"retry_count": hostRetryCount + 1,
		"max_retries": maxRetryAttempts,
		"retried_at":  now,
	}

	if optJSON {
		return PrintJSON(os.Stdout, map[string]any{
			"data":  output,
			"meta":  nil,
			"error": nil,
		})
	}

	if optQuiet {
		fmt.Fprintf(os.Stdout, "%s/%s\n", retryHostOptRunID, retryHostOptHost)
		return nil
	}

	fmt.Fprintf(os.Stdout, "Run %s host %s retry #%d triggered by %s\n",
		retryHostOptRunID, retryHostOptHost, hostRetryCount+1, actor)
	return nil
}

// isRetryableStatus reports whether a run in the given status can be retried.
func isRetryableStatus(status string) bool {
	return status == "failed" || status == "paused"
}

// countRetries and countHostRetries delegate to internal/retrybudget, which
// the serving path's RetryChange/RetryHost also use. They are kept as names
// because the CLI's call sites and tests read better with them, but they add
// no logic — the shared package owns both the cap and the counting.
func countRetries(ctx context.Context, store state.Store, runID string) (int, error) {
	return retrybudget.RunUsage(ctx, store, runID)
}

func countHostRetries(ctx context.Context, store state.Store, runID, host string) (int, error) {
	return retrybudget.HostUsage(ctx, store, runID, host)
}

// newRetryAuditID generates a unique audit identifier for retry actions.
// A crypto/rand failure is returned as an error: audit identities must
// never fall back to predictable timestamp-based values.
func newRetryAuditID() (string, error) {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate retry audit id: %w", err)
	}
	return "audit-retry-" + hex.EncodeToString(b), nil
}
