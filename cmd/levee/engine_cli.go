// engine_cli.go wires the execution engine (internal/wiring, the same
// assembly factory serve uses) into local CLI commands. Both files here
// reuse serve's credential-resolver type so the CLI and the server expand
// LEVEE_MASTER_PASSWORD-backed credentials identically.

package main

import (
	"context"
	"fmt"
	"os"

	"github.com/nexus/levee/internal/approval"
	"github.com/nexus/levee/internal/credential"
	"github.com/nexus/levee/internal/grpc"
	"github.com/nexus/levee/internal/log"
	"github.com/nexus/levee/internal/state"
	"github.com/nexus/levee/internal/wiring"
)

// cliCredentialResolver expands LEVEE_MASTER_PASSWORD into the encrypted
// credential resolver. It returns nil (no error) when the password is
// unset or the store cannot be opened: channels then dial targets
// unauthenticated — the exact convention serve logs as a warning.
func cliCredentialResolver(store state.Store) wiring.CredentialResolver {
	mp := os.Getenv("LEVEE_MASTER_PASSWORD")
	if mp == "" {
		return nil
	}
	cs, err := credential.NewCredentialStore(store, mp)
	if err != nil {
		return nil
	}
	return &serveCredentialResolver{store: cs}
}

// newCLIChangeService builds the in-process ChangeService with a real
// engine adapter attached: PlanChange persists plan artifacts and
// ApplyChange executes them through the closure machinery. The approval
// service rides the same store so `levee plan` kicks off the approval
// chain (risk-tiered routing) exactly like serve does — otherwise the
// CLI would produce plans whose pending approvals never exist and
// `levee approve` would find nothing to decide on. Command code must
// inject the actor via grpc.ContextWithActor so audits attribute to the
// CLI user rather than the "grpc-user" fallback.
func newCLIChangeService(store state.Store) (*grpc.ChangeService, error) {
	opts := []wiring.Option{wiring.WithCredentialResolver(cliCredentialResolver(store))}
	// The plan-time irreversibility gate must see the same plugin-declared
	// destructive actions serve sees, or the verdict (and the plan hash that
	// carries it) would depend on whether the plan came from the CLI or the
	// server. Loading config mirrors openStore; a present-but-unreadable
	// registry fails the command rather than judging plugin steps reversible.
	cfg, err := loadConfigForCmd()
	if err != nil {
		return nil, fmt.Errorf("load config: %w", err)
	}
	pluginPolicy, policyErr := declaredPluginActionPolicy(context.Background(), cfg)
	if policyErr != nil {
		return nil, policyErr
	}
	if !pluginPolicy.IsEmpty() {
		opts = append(opts, wiring.WithPluginActionPolicy(pluginPolicy))
	}
	// Same rule as serve: the calendar gates must read the database the operator's
	// `levee calendar` commands write, and a failure to open it is reported rather
	// than turned into an inert gate.
	cal, calErr := calendarFor(context.Background(), store)
	if calErr != nil {
		log.Warn("change calendar UNAVAILABLE for this local command — freeze periods will not block plan or apply",
			"error", calErr)
	} else {
		opts = append(opts, wiring.WithChangeCalendar(cal))
	}
	engine := wiring.NewEngine(store, opts...).Adapter()
	// The fourth argument is the policy authorizer, and passing nil here is what
	// made local mode the one path that skipped every judgement the server makes.
	// localAuthorizer returns nil unless --as asked otherwise, so the default
	// behaves exactly as before.
	authzSvc, err := localAuthorizer()
	if err != nil {
		return nil, err
	}
	return grpc.NewChangeService(store, engine, approval.NewService(newApprovalStoreAdapter(store)), nil).WithAuthorizer(authzSvc), nil
}
