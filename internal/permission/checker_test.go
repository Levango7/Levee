package permission

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// checkerTestMatrix returns a PermissionMatrix populated with the rules
// used by the checker tests:
//   - sre: plan/apply/approve/rollback/view on dev and staging;
//     plan/approve/rollback/view on prod (no apply)
//   - dba: apply/rollback/view on prod only
//   - security: admin on every env (wildcard) — used for the admin
//     super-set test
//   - readonly: view on dev only — used to test denial of apply
func checkerTestMatrix() *PermissionMatrix {
	m := NewPermissionMatrix()
	m.Grant("sre", "dev", ActionPlan)
	m.Grant("sre", "dev", ActionApply)
	m.Grant("sre", "dev", ActionApprove)
	m.Grant("sre", "dev", ActionRollback)
	m.Grant("sre", "dev", ActionView)
	m.Grant("sre", "staging", ActionPlan)
	m.Grant("sre", "staging", ActionApply)
	m.Grant("sre", "staging", ActionApprove)
	m.Grant("sre", "staging", ActionRollback)
	m.Grant("sre", "staging", ActionView)
	m.Grant("sre", "prod", ActionPlan)
	m.Grant("sre", "prod", ActionApprove)
	m.Grant("sre", "prod", ActionRollback)
	m.Grant("sre", "prod", ActionView)

	m.Grant("dba", "prod", ActionApply)
	m.Grant("dba", "prod", ActionRollback)
	m.Grant("dba", "prod", ActionView)

	m.Grant("security", Wildcard, ActionAdmin)

	m.Grant("readonly", "dev", ActionView)
	return m
}

func TestNewPermissionChecker_NilMatrix(t *testing.T) {
	c, err := NewPermissionChecker(nil)
	assert.ErrorIs(t, err, ErrNilMatrix)
	assert.Nil(t, c)
}

func TestNewPermissionChecker_Valid(t *testing.T) {
	m := NewPermissionMatrix()
	c, err := NewPermissionChecker(m)
	require.NoError(t, err)
	assert.NotNil(t, c)
}

func TestCheck_Allowed(t *testing.T) {
	c := NewPermissionCheckerOrPanic(checkerTestMatrix())

	err := c.Check(context.Background(), OperationContext{
		Actor:  "alice",
		Team:   "sre",
		Env:    "dev",
		Action: ActionApply,
		RunID:  "run-1",
	})
	assert.NoError(t, err)
}

func TestCheck_Denied(t *testing.T) {
	c := NewPermissionCheckerOrPanic(checkerTestMatrix())

	err := c.Check(context.Background(), OperationContext{
		Actor:  "bob",
		Team:   "dba",
		Env:    "prod",
		Action: ActionPlan,
	})
	require.Error(t, err)

	// Should be a PermissionDeniedError.
	var pde *PermissionDeniedError
	require.ErrorAs(t, err, &pde)
	assert.Equal(t, "bob", pde.Actor)
	assert.Equal(t, "dba", pde.Team)
	assert.Equal(t, "prod", pde.Env)
	assert.Equal(t, ActionPlan, pde.Action)
	assert.NotEmpty(t, pde.Reason)

	// Should satisfy errors.Is(ErrPermissionDenied).
	assert.ErrorIs(t, err, ErrPermissionDenied)
}

func TestCheck_Denied_UnknownTeam(t *testing.T) {
	c := NewPermissionCheckerOrPanic(checkerTestMatrix())

	err := c.Check(context.Background(), OperationContext{
		Actor:  "eve",
		Team:   "ghost",
		Env:    "dev",
		Action: ActionView,
	})
	require.Error(t, err)
	var pde *PermissionDeniedError
	require.ErrorAs(t, err, &pde)
	assert.Equal(t, "ghost", pde.Team)
}

func TestCheckBatch_AllAllowed(t *testing.T) {
	c := NewPermissionCheckerOrPanic(checkerTestMatrix())

	err := c.CheckBatch(context.Background(),
		OperationContext{Actor: "alice", Team: "sre", Env: "dev", Action: ActionPlan},
		OperationContext{Actor: "alice", Team: "sre", Env: "dev", Action: ActionApply},
		OperationContext{Actor: "carol", Team: "dba", Env: "prod", Action: ActionRollback},
	)
	assert.NoError(t, err)
}

func TestCheckBatch_FirstFails(t *testing.T) {
	c := NewPermissionCheckerOrPanic(checkerTestMatrix())

	// First op denied (dba has no plan on prod), second op allowed.
	err := c.CheckBatch(context.Background(),
		OperationContext{Actor: "bob", Team: "dba", Env: "prod", Action: ActionPlan},
		OperationContext{Actor: "bob", Team: "dba", Env: "prod", Action: ActionApply},
	)
	require.Error(t, err)
	var pde *PermissionDeniedError
	require.ErrorAs(t, err, &pde)
	assert.Equal(t, ActionPlan, pde.Action)
}

func TestCheckBatch_SecondFails(t *testing.T) {
	c := NewPermissionCheckerOrPanic(checkerTestMatrix())

	// First allowed, second denied — should return the second error.
	err := c.CheckBatch(context.Background(),
		OperationContext{Actor: "alice", Team: "sre", Env: "dev", Action: ActionPlan},
		OperationContext{Actor: "alice", Team: "sre", Env: "prod", Action: ActionApply},
	)
	require.Error(t, err)
	var pde *PermissionDeniedError
	require.ErrorAs(t, err, &pde)
	assert.Equal(t, ActionApply, pde.Action)
	assert.Equal(t, "prod", pde.Env)
}

func TestCheckBatch_Empty(t *testing.T) {
	c := NewPermissionCheckerOrPanic(checkerTestMatrix())
	assert.NoError(t, c.CheckBatch(context.Background()))
}

func TestCheckPlan(t *testing.T) {
	c := NewPermissionCheckerOrPanic(checkerTestMatrix())

	// sre can plan on dev.
	assert.NoError(t, c.CheckPlan(context.Background(), "alice", "sre", "dev", "run-1"))
	// dba cannot plan on prod.
	err := c.CheckPlan(context.Background(), "bob", "dba", "prod", "run-2")
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrPermissionDenied)
}

func TestCheckApply(t *testing.T) {
	c := NewPermissionCheckerOrPanic(checkerTestMatrix())

	assert.NoError(t, c.CheckApply(context.Background(), "alice", "sre", "dev", "run-1"))
	err := c.CheckApply(context.Background(), "alice", "sre", "prod", "run-2")
	require.Error(t, err)
	var pde *PermissionDeniedError
	require.ErrorAs(t, err, &pde)
	assert.Equal(t, ActionApply, pde.Action)
}

func TestCheckApprove(t *testing.T) {
	c := NewPermissionCheckerOrPanic(checkerTestMatrix())

	assert.NoError(t, c.CheckApprove(context.Background(), "alice", "sre", "prod", "run-1"))
	err := c.CheckApprove(context.Background(), "bob", "dba", "prod", "run-2")
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrPermissionDenied)
}

func TestCheckRollback(t *testing.T) {
	c := NewPermissionCheckerOrPanic(checkerTestMatrix())

	assert.NoError(t, c.CheckRollback(context.Background(), "bob", "dba", "prod", "run-1"))
	err := c.CheckRollback(context.Background(), "bob", "dba", "dev", "run-2")
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrPermissionDenied)
}

func TestCheckPause(t *testing.T) {
	c := NewPermissionCheckerOrPanic(checkerTestMatrix())

	// security has admin everywhere → pause allowed on prod.
	assert.NoError(t, c.CheckPause(context.Background(), "sec", "security", "prod", "run-1"))
	// readonly has only view on dev → pause denied.
	err := c.CheckPause(context.Background(), "rod", "readonly", "dev", "run-2")
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrPermissionDenied)
}

func TestCheckResume(t *testing.T) {
	c := NewPermissionCheckerOrPanic(checkerTestMatrix())

	assert.NoError(t, c.CheckResume(context.Background(), "sec", "security", "prod", "run-1"))
	err := c.CheckResume(context.Background(), "rod", "readonly", "dev", "run-2")
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrPermissionDenied)
}

func TestCheckPauseAll(t *testing.T) {
	c := NewPermissionCheckerOrPanic(checkerTestMatrix())

	assert.NoError(t, c.CheckPauseAll(context.Background(), "sec", "security", "prod"))
	err := c.CheckPauseAll(context.Background(), "alice", "sre", "dev")
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrPermissionDenied)
}

func TestCheckResumeAll(t *testing.T) {
	c := NewPermissionCheckerOrPanic(checkerTestMatrix())

	assert.NoError(t, c.CheckResumeAll(context.Background(), "sec", "security", "prod"))
	err := c.CheckResumeAll(context.Background(), "alice", "sre", "dev")
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrPermissionDenied)
}

func TestPermissionDeniedError_Format(t *testing.T) {
	e := &PermissionDeniedError{
		Team:   "dba",
		Env:    "prod",
		Action: ActionPlan,
		Actor:  "bob",
		Reason: "no matching grant in permission matrix",
	}
	const want = `permission denied: actor "bob" (team "dba") cannot "plan" on env "prod": no matching grant in permission matrix`
	assert.Equal(t, want, e.Error())
}

func TestPermissionDeniedError_ErrorIs(t *testing.T) {
	e := &PermissionDeniedError{
		Team: "dba", Env: "prod", Action: ActionPlan, Actor: "bob", Reason: "x",
	}
	assert.True(t, errors.Is(e, ErrPermissionDenied))
	// Should not match unrelated sentinels.
	assert.False(t, errors.Is(e, ErrNilMatrix))
}

func TestPermissionDeniedError_ErrorAs(t *testing.T) {
	c := NewPermissionCheckerOrPanic(checkerTestMatrix())
	err := c.Check(context.Background(), OperationContext{
		Actor: "bob", Team: "dba", Env: "prod", Action: ActionPlan,
	})
	require.Error(t, err)

	var pde *PermissionDeniedError
	require.ErrorAs(t, err, &pde)
	assert.Equal(t, "bob", pde.Actor)
	assert.Equal(t, "dba", pde.Team)
	assert.Equal(t, "prod", pde.Env)
	assert.Equal(t, ActionPlan, pde.Action)
}

func TestCheck_EmptyActor(t *testing.T) {
	c := NewPermissionCheckerOrPanic(checkerTestMatrix())
	err := c.Check(context.Background(), OperationContext{
		Actor: "", Team: "sre", Env: "dev", Action: ActionPlan,
	})
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrEmptyActor)
	// Should NOT be a denial.
	assert.False(t, errors.Is(err, ErrPermissionDenied))
}

func TestCheck_EmptyTeam(t *testing.T) {
	c := NewPermissionCheckerOrPanic(checkerTestMatrix())
	err := c.Check(context.Background(), OperationContext{
		Actor: "alice", Team: "", Env: "dev", Action: ActionPlan,
	})
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrEmptyTeam)
	assert.False(t, errors.Is(err, ErrPermissionDenied))
}

func TestCheck_EmptyEnv(t *testing.T) {
	c := NewPermissionCheckerOrPanic(checkerTestMatrix())
	err := c.Check(context.Background(), OperationContext{
		Actor: "alice", Team: "sre", Env: "", Action: ActionPlan,
	})
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrEmptyEnv)
	assert.False(t, errors.Is(err, ErrPermissionDenied))
}

func TestCheck_EmptyAction(t *testing.T) {
	c := NewPermissionCheckerOrPanic(checkerTestMatrix())
	err := c.Check(context.Background(), OperationContext{
		Actor: "alice", Team: "sre", Env: "dev", Action: "",
	})
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrEmptyAction)
	assert.False(t, errors.Is(err, ErrPermissionDenied))
}

func TestCheck_NilChecker(t *testing.T) {
	var c *PermissionChecker
	err := c.Check(context.Background(), OperationContext{
		Actor: "alice", Team: "sre", Env: "dev", Action: ActionPlan,
	})
	assert.ErrorIs(t, err, ErrNilMatrix)
}

func TestCheck_AdminSuperSet(t *testing.T) {
	c := NewPermissionCheckerOrPanic(checkerTestMatrix())

	// security has admin on every env (wildcard). Every action in
	// AllActions should be allowed on prod, including actions that were
	// never explicitly granted.
	for _, action := range AllActions {
		err := c.Check(context.Background(), OperationContext{
			Actor: "sec", Team: "security", Env: "prod", Action: action,
		})
		assert.NoErrorf(t, err, "security should be allowed to %q on prod via admin super-set", action)
	}

	// Also works on an env that was never named explicitly.
	assert.NoError(t, c.Check(context.Background(), OperationContext{
		Actor: "sec", Team: "security", Env: "emergency", Action: ActionApply,
	}))
}

func TestCheck_AdminSuperSet_CanBeRevoked(t *testing.T) {
	m := checkerTestMatrix()
	// Even with admin, an explicit revoke on a specific action must
	// block it.
	m.Revoke("security", "prod", ActionApply)
	c := NewPermissionCheckerOrPanic(m)

	err := c.Check(context.Background(), OperationContext{
		Actor: "sec", Team: "security", Env: "prod", Action: ActionApply,
	})
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrPermissionDenied)

	// Other actions remain allowed via admin.
	assert.NoError(t, c.Check(context.Background(), OperationContext{
		Actor: "sec", Team: "security", Env: "prod", Action: ActionPlan,
	}))
}
