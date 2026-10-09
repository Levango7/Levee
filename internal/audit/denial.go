package audit

import (
	"context"
	"time"

	"github.com/nexus/levee/internal/log"
	"github.com/nexus/levee/internal/state"
)

// Audit vocabulary and the recorder for permission refusals (SA-007).
//
// This package owns the two spellings: internal/pause declared them itself
// until now, and the service-layer gate would have made a third copy, which is
// how the batch/step status vocabularies drifted before. pause aliases what is
// here instead, so the CLI pause path and the serving path cannot write two
// different rows for the same refusal.
const (
	// ActionPermissionDenied is the state.Audit.Action recorded when a
	// permission refusal is persisted. It is spelled the same as
	// permission.EventPermissionDenied (the trace-table event): both are read
	// by the same person looking for the same thing, and two spellings for it
	// would be a search that silently misses rows.
	ActionPermissionDenied = state.AuditActionPermissionDenied

	// ResultDenied is the state.Audit.Result for a refusal. It is distinct
	// from ResultFailed on purpose: "understood and refused" is a different
	// fact from "attempted and failed", and only one of them is a security
	// event.
	ResultDenied = state.AuditResultDenied
)

// DenialTarget renders the refused permission the way the audit Target column
// carries it: "<action>@<env>". internal/pause fills the same slot with a bare
// permission name and passes an empty environment, so its rows keep the exact
// shape they had.
//
// The environment is the one the decision actually judged rather than the one
// the caller passed: the default-env fallback happens inside the decision, and
// "refused in which environment" is the question an auditor is asking.
func DenialTarget(action, env string) string {
	if env == "" {
		return action
	}
	return action + "@" + env
}

// NewDenialRecorder returns a recorder that persists one refusal as a row in
// the audit chain: Action=ActionPermissionDenied, Result=ResultDenied,
// Actor=<refused subject>, Target=<refused permission>, RunID="".
//
// RunID is empty on purpose, and that is why this sink is the audit table and
// not trace: audit.run_id carries no foreign key (NOT NULL DEFAULT ”, as
// state.Audit documents: "a run-less entry is exactly the security-relevant
// kind"), so a refusal that happens before any run exists — or that is not
// about a run at all — still gets recorded. The trace table's run_id does
// reference runs(id), which is why the same idea cannot be recorded there.
//
// Write failures and ID-minting failures are logged and swallowed: the refusal
// is the security decision, and it must not depend on the observability path
// succeeding.
func NewDenialRecorder(store state.Store) func(ctx context.Context, actor, action, env string) {
	return func(ctx context.Context, actor, action, env string) {
		if store == nil {
			return
		}
		id, err := newID()
		if err != nil {
			log.Warn("audit: permission-denied audit id generation failed; entry skipped",
				"actor", actor, "action", action, "err", err)
			return
		}
		entry := &state.Audit{
			ID:        id,
			Action:    ActionPermissionDenied,
			Actor:     actor,
			Target:    DenialTarget(action, env),
			Result:    ResultDenied,
			Timestamp: time.Now().UTC(),
		}
		if err := Record(ctx, store, entry); err != nil {
			log.Warn("audit: permission-denied audit write failed",
				"actor", actor, "action", action, "err", err)
		}
	}
}
