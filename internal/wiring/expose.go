// expose.go — the Engine's public surface for consumers outside the
// apply path. The GateService (grpc package) needs to dial a target
// channel on demand for ad-hoc cmd gate verification; the dial logic
// (inventory lookup + credential expansion + registry create + connect)
// already exists inside runExec, so this method exposes the same
// sequence as a one-shot dial without dragging a whole run context
// along. The grpc package cannot import wiring (cycle), so serve mode
// passes a closure over this method into NewGateService.

package wiring

import (
	"context"
	"fmt"

	"github.com/nexus/levee/internal/channel"
	"github.com/nexus/levee/internal/state"
)

// Dial opens a connected channel to the named inventory host, applying
// the same credential-expansion conventions as the apply path: a target
// with a credential ref and no resolver dials unauthenticated. The
// caller owns the channel and must Close it. Unknown and retired hosts
// are refused up front.
func (e *Engine) Dial(ctx context.Context, host string) (channel.Channel, error) {
	if e.store == nil {
		return nil, fmt.Errorf("wiring: dial: no store")
	}
	rows, err := e.store.ListTargets(ctx, state.TargetFilter{})
	if err != nil {
		return nil, fmt.Errorf("wiring: dial: list targets: %w", err)
	}
	var t *state.Target
	for i := range rows {
		if rows[i].Hostname == host {
			t = rows[i]
			break
		}
	}
	if t == nil {
		return nil, fmt.Errorf("wiring: dial: target %q is not registered in the inventory", host)
	}
	if t.Status == "retired" {
		return nil, fmt.Errorf("wiring: dial: target %q is retired", host)
	}

	cred := channel.CredentialRef{}
	if ref := t.CredentialRef; ref != "" && e.resolver != nil {
		resolved, err := e.resolver.ResolveTargetCredential(ctx, ref)
		if err != nil {
			return nil, fmt.Errorf("wiring: dial: resolve credential %q for %q: %w", ref, host, err)
		}
		if resolved != nil {
			cred = *resolved
		}
	}

	ch, err := e.registry.Create(&liveTarget{t: t, cred: cred})
	if err != nil {
		return nil, fmt.Errorf("wiring: dial: create channel for %q: %w", host, err)
	}
	if err := ch.Connect(ctx); err != nil {
		_ = ch.Close()
		return nil, fmt.Errorf("wiring: dial: connect %q: %w", host, err)
	}
	return ch, nil
}
