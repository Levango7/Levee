// host_guard_test.go — pins the apply-time frozen re-check seam: the
// WithHostGuard hook runs after target collection and before any lock is
// acquired; a rejection aborts the run with PhaseFailed and no side effects.
package engine

import (
	"context"
	"fmt"
	"testing"

	"github.com/nexus/levee/internal/batch"
	"github.com/nexus/levee/internal/plan"
	"github.com/nexus/levee/internal/rollback"
	"github.com/nexus/levee/internal/verify"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestClosureHostGuardBlocksRun(t *testing.T) {
	store := newTestStore(t)
	lockMgr := newTestLockManager(t, store)
	gm := verify.NewGateManager()
	rm := rollback.NewManager(rollback.WithWhitelistAll())
	bc := batch.NewController()

	guard := func(_ context.Context, hosts []string) error {
		for _, h := range hosts {
			if h == "frozen-host" {
				return fmt.Errorf("frozen targets cannot receive changes: %s", h)
			}
		}
		return nil
	}

	newRunner := func() *ClosureRunner {
		return NewClosureRunner(store, lockMgr, gm, rm, bc, nil, WithHostGuard(guard))
	}

	p := &plan.Plan{
		ID:           "pln-guard",
		WorkflowName: "wf",
		Batches: []plan.Batch{{
			Index:   0,
			Targets: []string{"ok-host", "frozen-host"},
			Steps:   []plan.PlanStep{{Name: "noop", Module: "shell", Action: "exec"}},
		}},
		TotalTargets: 2,
	}

	res, err := newRunner().Run(context.Background(), p, nil)
	// A guard rejection is a fatal precondition: Run surfaces it BOTH via
	// the returned error and via the result (Phase=Failed), matching the
	// documented contract for pre-apply gate failures.
	require.Error(t, err)
	assert.Contains(t, err.Error(), "host guard")
	assert.Equal(t, PhaseFailed, res.Phase)
	require.NotNil(t, res.Error)
	assert.Contains(t, res.Error.Error(), "host guard")

	// The guard fires before lock acquisition: no locks may be left behind.
	locks, lerr := store.ListLocks(context.Background())
	require.NoError(t, lerr)
	assert.Empty(t, locks)

	p2 := &plan.Plan{
		ID:           "pln-guard-ok",
		WorkflowName: "wf",
		Batches: []plan.Batch{{
			Index:   0,
			Targets: []string{"ok-host"},
			Steps:   []plan.PlanStep{{Name: "noop", Module: "shell", Action: "exec"}},
		}},
		TotalTargets: 1,
	}
	res2, err := newRunner().Run(context.Background(), p2, nil)
	require.NoError(t, err)
	assert.Equal(t, PhaseCompleted, res2.Phase)
}
