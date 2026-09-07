// engine_cli.go wires the execution engine (internal/wiring, the same
// assembly factory serve uses) into local CLI commands. Both files here
// reuse serve's credential-resolver type so the CLI and the server expand
// LEVEE_MASTER_PASSWORD-backed credentials identically.

package main

import (
	"os"

	"github.com/nexus/levee/internal/credential"
	"github.com/nexus/levee/internal/grpc"
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
// ApplyChange executes them through the closure machinery. Command code
// must inject the actor via grpc.ContextWithActor so audits attribute to
// the CLI user rather than the "grpc-user" fallback.
func newCLIChangeService(store state.Store) *grpc.ChangeService {
	engine := wiring.NewEngine(store,
		wiring.WithCredentialResolver(cliCredentialResolver(store)),
	).Adapter()
	return grpc.NewChangeService(store, engine, nil, nil)
}
