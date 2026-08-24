// validate.go — inventory-side guards consumed by change execution paths.
package inventory

import (
	"context"
	"fmt"
	"strings"

	"github.com/nexus/levee/internal/state"
)

// FrozenError reports the hosts rejected because they are frozen.
type FrozenError struct {
	Hosts []string
}

func (e *FrozenError) Error() string {
	return fmt.Sprintf("frozen targets cannot receive changes: %s", strings.Join(e.Hosts, ", "))
}

// Store is the subset of state.Store needed for validation.
type ValidateStore interface {
	ListTargets(ctx context.Context, filter state.TargetFilter) ([]*state.Target, error)
}

// ValidateNotFrozen checks every host address against the inventory and
// returns a *FrozenError listing the hosts currently in frozen status.
// Hosts unknown to the inventory are allowed (execution is not restricted
// to imported targets). Callers should invoke this at BOTH guard points:
// when a change is planned AND again right before execution, since a host
// may be frozen between the two.
func ValidateNotFrozen(ctx context.Context, store ValidateStore, hosts []string) error {
	if len(hosts) == 0 {
		return nil
	}
	targets, err := store.ListTargets(ctx, state.TargetFilter{Status: state.StatusFrozen})
	if err != nil {
		return fmt.Errorf("inventory: list frozen targets: %w", err)
	}
	frozenSet := map[string]bool{}
	for _, t := range targets {
		frozenSet[t.Hostname] = true
	}
	var hit []string
	for _, h := range hosts {
		if frozenSet[h] {
			hit = append(hit, h)
		}
	}
	if len(hit) > 0 {
		return &FrozenError{Hosts: hit}
	}
	return nil
}
