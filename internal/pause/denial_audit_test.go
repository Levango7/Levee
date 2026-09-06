package pause

// SA-007 denial-audit tests: rejections of global pause/resume are recorded
// in the audit table (no run context required) rather than only logged.

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSimplePermissionChecker_DenyRecorderFiresOnlyOnDenial(t *testing.T) {
	var denied [][2]string
	c := NewSimplePermissionChecker(map[string][]string{"alice": {PermissionPauseAll}})
	c.SetDenyRecorder(func(actor, permission string) {
		denied = append(denied, [2]string{actor, permission})
	})

	require.True(t, c.HasPermission("alice", PermissionPauseAll))
	assert.Empty(t, denied, "allow decisions must not fire the recorder")

	require.False(t, c.HasPermission("alice", PermissionResumeAll))
	require.False(t, c.HasPermission("bob", PermissionPauseAll))
	require.Len(t, denied, 2)
	assert.Equal(t, [2]string{"alice", PermissionResumeAll}, denied[0])
	assert.Equal(t, [2]string{"bob", PermissionPauseAll}, denied[1])
}

func TestSimplePermissionChecker_NilAndNoopRecorderSafety(t *testing.T) {
	var nilChecker *SimplePermissionChecker
	require.False(t, nilChecker.HasPermission("a", "p")) // no panic with recorder path wired
	nilChecker.SetDenyRecorder(func(actor, permission string) { t.Fatal("must not fire") })

	c := NewSimplePermissionChecker(nil)
	c.SetDenyRecorder(nil) // nil recorder is a no-op
	require.False(t, c.HasPermission("a", "p"))
}

func TestDenialAuditRecorder_PersistsRejection(t *testing.T) {
	st := newTestStore(t)
	checker := NewSimplePermissionChecker(map[string][]string{"boss": {PermissionPauseAll}})
	checker.SetDenyRecorder(NewDenialAuditRecorder(st))
	mgr := NewPauseManager(st)
	ctx := context.Background()

	// A denied PauseAll must return ErrPermissionDenied AND persist one
	// permission.denied audit row before touching any run.
	_, err := mgr.PauseAll(ctx, "intruder", checker)
	require.ErrorIs(t, err, ErrPermissionDenied)

	audits := listAudits(t, st, ActionPermissionDenied)
	require.Len(t, audits, 1, "denial must be persisted in the audit table")
	assert.Equal(t, ResultDenied, audits[0].Result)
	assert.Equal(t, "intruder", audits[0].Actor)
	assert.Equal(t, PermissionPauseAll, audits[0].Target, "rejected permission is recorded in Target")
	assert.Empty(t, audits[0].RunID, "rejection happens before any run exists; audit row must be run-less")
	assert.NotEmpty(t, audits[0].ID)
	assert.False(t, audits[0].Timestamp.IsZero())

	// An allowed operation records nothing new on the denial action.
	seedRun(t, st, "run-da", StatusRunning)
	res, err := mgr.PauseAll(ctx, "boss", checker)
	require.NoError(t, err)
	assert.Contains(t, res.Affected, "run-da")
	assert.Len(t, listAudits(t, st, ActionPermissionDenied), 1)

	// ResumeAll denial uses the same recorder wiring. ListAudits returns
	// newest-first and both rows share near-identical timestamps, so match
	// on the target set rather than on position.
	_, err = mgr.ResumeAll(ctx, "intruder", checker)
	require.ErrorIs(t, err, ErrPermissionDenied)
	resumeDenials := listAudits(t, st, ActionPermissionDenied)
	require.Len(t, resumeDenials, 2)
	targets := []string{resumeDenials[0].Target, resumeDenials[1].Target}
	assert.Contains(t, targets, PermissionPauseAll)
	assert.Contains(t, targets, PermissionResumeAll)
}
