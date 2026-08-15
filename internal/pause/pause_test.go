package pause

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nexus/levee/internal/state"
)

// --- test helpers -----------------------------------------------------------

// newTestStore returns a fresh SQLite-backed state.Store for each test.
// The database file lives in a per-test temp dir and is closed via
// t.Cleanup.
func newTestStore(t *testing.T) *state.SQLiteStore {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	path := filepath.Join(dir, "levee-pause-test.db")
	store, err := state.NewSQLiteStore(ctx, path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	return store
}

// newTestManager returns a PauseManager backed by a fresh SQLite store.
func newTestManager(t *testing.T) *PauseManager {
	t.Helper()
	return NewPauseManager(newTestStore(t))
}

// seedRun creates a run row with the given id and status. Returns the
// created run for convenience.
func seedRun(t *testing.T, st state.Store, id, status string) *state.Run {
	t.Helper()
	ctx := context.Background()
	now := time.Now().UTC()
	r := &state.Run{
		ID:             id,
		WorkflowName:   "w",
		TemplateName:   "t",
		PlanHash:       "h",
		Status:         status,
		ApprovalStatus: "approved",
		ApprovalLevel:  "low",
		CreatedAt:      now,
		UpdatedAt:      now,
		Creator:        "u",
	}
	require.NoError(t, st.CreateRun(ctx, r))
	return r
}

// countAudits returns the total number of audit entries for the given
// action. Used to verify the audit trail after pause/resume operations.
func countAudits(t *testing.T, st state.Store, action string) int {
	t.Helper()
	ctx := context.Background()
	audits, err := st.ListAudits(ctx, state.AuditFilter{Action: action})
	require.NoError(t, err)
	return len(audits)
}

// listAudits returns all audit entries for the given action, sorted as
// returned by the store.
func listAudits(t *testing.T, st state.Store, action string) []*state.Audit {
	t.Helper()
	ctx := context.Background()
	audits, err := st.ListAudits(ctx, state.AuditFilter{Action: action})
	require.NoError(t, err)
	return audits
}

// bgCtx is a short alias for a background context.
func bgCtx() context.Context { return context.Background() }

// allPerm grants every known pause permission to the actor. Useful for
// the happy-path global tests.
func allPerm(actor string) *SimplePermissionChecker {
	return NewSimplePermissionChecker(map[string][]string{
		actor: {PermissionPauseAll, PermissionResumeAll},
	})
}

// noPerm grants the actor no permissions.
func noPerm() *SimplePermissionChecker {
	return NewSimplePermissionChecker(map[string][]string{})
}

// =========================================================================
// SimplePermissionChecker
// =========================================================================

func TestSimplePermissionChecker_HasPermission(t *testing.T) {
	t.Run("granted permission returns true", func(t *testing.T) {
		c := NewSimplePermissionChecker(map[string][]string{
			"alice": {PermissionPauseAll, PermissionResumeAll},
		})
		assert.True(t, c.HasPermission("alice", PermissionPauseAll))
		assert.True(t, c.HasPermission("alice", PermissionResumeAll))
	})

	t.Run("missing permission returns false", func(t *testing.T) {
		c := NewSimplePermissionChecker(map[string][]string{
			"alice": {PermissionPauseAll},
		})
		assert.False(t, c.HasPermission("alice", PermissionResumeAll))
	})

	t.Run("unknown actor returns false", func(t *testing.T) {
		c := NewSimplePermissionChecker(map[string][]string{
			"alice": {PermissionPauseAll},
		})
		assert.False(t, c.HasPermission("bob", PermissionPauseAll))
	})

	t.Run("empty checker returns false", func(t *testing.T) {
		c := NewSimplePermissionChecker(map[string][]string{})
		assert.False(t, c.HasPermission("alice", PermissionPauseAll))
	})

	t.Run("nil receiver returns false", func(t *testing.T) {
		var c *SimplePermissionChecker
		assert.False(t, c.HasPermission("alice", PermissionPauseAll))
	})

	t.Run("caller map mutation does not affect checker", func(t *testing.T) {
		perms := map[string][]string{
			"alice": {PermissionPauseAll},
		}
		c := NewSimplePermissionChecker(perms)
		// Mutate the caller's map after construction.
		perms["alice"] = nil
		perms["bob"] = []string{PermissionPauseAll}
		assert.True(t, c.HasPermission("alice", PermissionPauseAll))
		assert.False(t, c.HasPermission("bob", PermissionPauseAll))
	})
}

// =========================================================================
// PauseRun
// =========================================================================

func TestPauseRun_Success_FromRunning(t *testing.T) {
	st := newTestStore(t)
	m := NewPauseManager(st)
	seedRun(t, st, "r1", StatusRunning)

	require.NoError(t, m.PauseRun(bgCtx(), "r1", "alice"))

	run, err := st.GetRun(bgCtx(), "r1")
	require.NoError(t, err)
	assert.Equal(t, StatusPaused, run.Status)

	audits := listAudits(t, st, ActionPause)
	require.Len(t, audits, 1)
	assert.Equal(t, "alice", audits[0].Actor)
	assert.Equal(t, "r1", audits[0].Target)
	assert.Equal(t, ResultSuccess, audits[0].Result)
	assert.Equal(t, "r1", audits[0].RunID)
}

func TestPauseRun_Success_FromPending(t *testing.T) {
	st := newTestStore(t)
	m := NewPauseManager(st)
	seedRun(t, st, "r1", StatusPending)

	require.NoError(t, m.PauseRun(bgCtx(), "r1", "alice"))

	run, err := st.GetRun(bgCtx(), "r1")
	require.NoError(t, err)
	assert.Equal(t, StatusPaused, run.Status)
	assert.Equal(t, 1, countAudits(t, st, ActionPause))
}

func TestPauseRun_Fail_NotFound(t *testing.T) {
	m := newTestManager(t)

	err := m.PauseRun(bgCtx(), "nonexistent", "alice")
	assert.ErrorIs(t, err, ErrRunNotFound)
}

func TestPauseRun_Fail_NotPausable(t *testing.T) {
	st := newTestStore(t)
	m := NewPauseManager(st)

	for _, status := range []string{StatusPaused, StatusCompleted, StatusFailed, StatusCancelled} {
		t.Run(status, func(t *testing.T) {
			id := "r-" + status
			seedRun(t, st, id, status)
			err := m.PauseRun(bgCtx(), id, "alice")
			assert.ErrorIs(t, err, ErrNotPausable)
		})
	}
}

func TestPauseRun_Fail_EmptyArgs(t *testing.T) {
	m := newTestManager(t)

	t.Run("empty run id", func(t *testing.T) {
		err := m.PauseRun(bgCtx(), "", "alice")
		assert.ErrorIs(t, err, ErrEmptyRunID)
	})

	t.Run("empty actor", func(t *testing.T) {
		err := m.PauseRun(bgCtx(), "r1", "")
		assert.ErrorIs(t, err, ErrEmptyActor)
	})
}

// =========================================================================
// ResumeRun
// =========================================================================

func TestResumeRun_Success(t *testing.T) {
	st := newTestStore(t)
	m := NewPauseManager(st)
	seedRun(t, st, "r1", StatusPaused)

	require.NoError(t, m.ResumeRun(bgCtx(), "r1", "alice"))

	run, err := st.GetRun(bgCtx(), "r1")
	require.NoError(t, err)
	assert.Equal(t, StatusRunning, run.Status)

	audits := listAudits(t, st, ActionResume)
	require.Len(t, audits, 1)
	assert.Equal(t, "alice", audits[0].Actor)
	assert.Equal(t, "r1", audits[0].Target)
	assert.Equal(t, ResultSuccess, audits[0].Result)
	assert.Equal(t, "r1", audits[0].RunID)
}

func TestResumeRun_Fail_NotFound(t *testing.T) {
	m := newTestManager(t)

	err := m.ResumeRun(bgCtx(), "nonexistent", "alice")
	assert.ErrorIs(t, err, ErrRunNotFound)
}

func TestResumeRun_Fail_NotResumable(t *testing.T) {
	st := newTestStore(t)
	m := NewPauseManager(st)

	for _, status := range []string{StatusRunning, StatusPending, StatusCompleted, StatusFailed, StatusCancelled} {
		t.Run(status, func(t *testing.T) {
			id := "r-" + status
			seedRun(t, st, id, status)
			err := m.ResumeRun(bgCtx(), id, "alice")
			assert.ErrorIs(t, err, ErrNotResumable)
		})
	}
}

func TestResumeRun_Fail_EmptyArgs(t *testing.T) {
	m := newTestManager(t)

	t.Run("empty run id", func(t *testing.T) {
		err := m.ResumeRun(bgCtx(), "", "alice")
		assert.ErrorIs(t, err, ErrEmptyRunID)
	})

	t.Run("empty actor", func(t *testing.T) {
		err := m.ResumeRun(bgCtx(), "r1", "")
		assert.ErrorIs(t, err, ErrEmptyActor)
	})
}

// =========================================================================
// PauseAll
// =========================================================================

func TestPauseAll_Success(t *testing.T) {
	st := newTestStore(t)
	m := NewPauseManager(st)

	// Three running, one pending, one already paused (should be ignored),
	// one completed (should be ignored).
	seedRun(t, st, "r1", StatusRunning)
	seedRun(t, st, "r2", StatusRunning)
	seedRun(t, st, "r3", StatusRunning)
	seedRun(t, st, "r4", StatusPending)
	seedRun(t, st, "r5", StatusPaused)
	seedRun(t, st, "r6", StatusCompleted)

	result, err := m.PauseAll(bgCtx(), "alice", allPerm("alice"))
	require.NoError(t, err)
	assert.Equal(t, ActionPauseAll, result.Action)
	assert.Empty(t, result.Failed)
	// r1..r4 should be paused; r5/r6 untouched.
	require.Len(t, result.Affected, 4)
	assert.ElementsMatch(t, []string{"r1", "r2", "r3", "r4"}, result.Affected)

	for _, id := range []string{"r1", "r2", "r3", "r4"} {
		run, err := st.GetRun(bgCtx(), id)
		require.NoError(t, err)
		assert.Equalf(t, StatusPaused, run.Status, "run %s should be paused", id)
	}
	r5, err := st.GetRun(bgCtx(), "r5")
	require.NoError(t, err)
	assert.Equal(t, StatusPaused, r5.Status, "r5 was already paused, should remain paused")
	r6, err := st.GetRun(bgCtx(), "r6")
	require.NoError(t, err)
	assert.Equal(t, StatusCompleted, r6.Status, "r6 completed, should remain completed")

	// Per-run audit entries (4) plus one global summary entry (Target="*").
	audits := listAudits(t, st, ActionPauseAll)
	require.Len(t, audits, 5)
	var globalEntry *state.Audit
	perRunTargets := make(map[string]struct{})
	for _, a := range audits {
		assert.Equal(t, "alice", a.Actor)
		if a.Target == TargetAll {
			globalEntry = a
			continue
		}
		perRunTargets[a.Target] = struct{}{}
		assert.Equal(t, ResultSuccess, a.Result)
	}
	require.NotNil(t, globalEntry, "global summary audit entry should exist")
	assert.Equal(t, ResultSuccess, globalEntry.Result)
	assert.Equal(t, TargetAll, globalEntry.Target)
	assert.ElementsMatch(t, []string{"r1", "r2", "r3", "r4"}, keys(perRunTargets))
}

func TestPauseAll_EmptyStore(t *testing.T) {
	m := newTestManager(t)

	result, err := m.PauseAll(bgCtx(), "alice", allPerm("alice"))
	require.NoError(t, err)
	assert.Empty(t, result.Affected)
	assert.Empty(t, result.Failed)
}

func TestPauseAll_Fail_PermissionDenied(t *testing.T) {
	st := newTestStore(t)
	m := NewPauseManager(st)
	seedRun(t, st, "r1", StatusRunning)

	t.Run("no permission", func(t *testing.T) {
		result, err := m.PauseAll(bgCtx(), "alice", noPerm())
		assert.ErrorIs(t, err, ErrPermissionDenied)
		assert.Nil(t, result)
		// Run must be untouched.
		run, err := st.GetRun(bgCtx(), "r1")
		require.NoError(t, err)
		assert.Equal(t, StatusRunning, run.Status)
	})

	t.Run("nil permission checker", func(t *testing.T) {
		result, err := m.PauseAll(bgCtx(), "alice", nil)
		assert.ErrorIs(t, err, ErrPermissionDenied)
		assert.Nil(t, result)
	})

	t.Run("empty actor", func(t *testing.T) {
		result, err := m.PauseAll(bgCtx(), "", allPerm(""))
		assert.ErrorIs(t, err, ErrEmptyActor)
		assert.Nil(t, result)
	})

	// No audit entries should have been written for the rejected attempts.
	assert.Equal(t, 0, countAudits(t, st, ActionPauseAll))
}

// =========================================================================
// ResumeAll
// =========================================================================

func TestResumeAll_Success(t *testing.T) {
	st := newTestStore(t)
	m := NewPauseManager(st)

	seedRun(t, st, "r1", StatusPaused)
	seedRun(t, st, "r2", StatusPaused)
	seedRun(t, st, "r3", StatusPaused)
	seedRun(t, st, "r4", StatusRunning)   // ignored
	seedRun(t, st, "r5", StatusCompleted) // ignored

	result, err := m.ResumeAll(bgCtx(), "alice", allPerm("alice"))
	require.NoError(t, err)
	assert.Equal(t, ActionResumeAll, result.Action)
	assert.Empty(t, result.Failed)
	require.Len(t, result.Affected, 3)
	assert.ElementsMatch(t, []string{"r1", "r2", "r3"}, result.Affected)

	for _, id := range []string{"r1", "r2", "r3"} {
		run, err := st.GetRun(bgCtx(), id)
		require.NoError(t, err)
		assert.Equalf(t, StatusRunning, run.Status, "run %s should be running", id)
	}
	r4, err := st.GetRun(bgCtx(), "r4")
	require.NoError(t, err)
	assert.Equal(t, StatusRunning, r4.Status)

	audits := listAudits(t, st, ActionResumeAll)
	require.Len(t, audits, 4) // 3 per-run + 1 global
	var hasGlobal bool
	for _, a := range audits {
		if a.Target == TargetAll {
			hasGlobal = true
			assert.Equal(t, ResultSuccess, a.Result)
		}
	}
	assert.True(t, hasGlobal, "global summary audit entry should exist")
}

func TestResumeAll_EmptyStore(t *testing.T) {
	m := newTestManager(t)

	result, err := m.ResumeAll(bgCtx(), "alice", allPerm("alice"))
	require.NoError(t, err)
	assert.Empty(t, result.Affected)
	assert.Empty(t, result.Failed)
}

func TestResumeAll_Fail_PermissionDenied(t *testing.T) {
	st := newTestStore(t)
	m := NewPauseManager(st)
	seedRun(t, st, "r1", StatusPaused)

	t.Run("no permission", func(t *testing.T) {
		result, err := m.ResumeAll(bgCtx(), "alice", noPerm())
		assert.ErrorIs(t, err, ErrPermissionDenied)
		assert.Nil(t, result)
		run, err := st.GetRun(bgCtx(), "r1")
		require.NoError(t, err)
		assert.Equal(t, StatusPaused, run.Status)
	})

	t.Run("nil permission checker", func(t *testing.T) {
		result, err := m.ResumeAll(bgCtx(), "alice", nil)
		assert.ErrorIs(t, err, ErrPermissionDenied)
		assert.Nil(t, result)
	})

	t.Run("empty actor", func(t *testing.T) {
		result, err := m.ResumeAll(bgCtx(), "", allPerm(""))
		assert.ErrorIs(t, err, ErrEmptyActor)
		assert.Nil(t, result)
	})

	assert.Equal(t, 0, countAudits(t, st, ActionResumeAll))
}

// =========================================================================
// Round-trip: PauseRun then ResumeAll then PauseAll then ResumeRun
// =========================================================================

func TestPauseResume_RoundTrip(t *testing.T) {
	st := newTestStore(t)
	m := NewPauseManager(st)
	seedRun(t, st, "r1", StatusRunning)

	// Single pause.
	require.NoError(t, m.PauseRun(bgCtx(), "r1", "alice"))
	// Global resume should pick it up.
	result, err := m.ResumeAll(bgCtx(), "bob", allPerm("bob"))
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{"r1"}, result.Affected)
	// Global pause should pick it up again.
	result, err = m.PauseAll(bgCtx(), "carol", allPerm("carol"))
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{"r1"}, result.Affected)
	// Single resume.
	require.NoError(t, m.ResumeRun(bgCtx(), "r1", "dave"))

	run, err := st.GetRun(bgCtx(), "r1")
	require.NoError(t, err)
	assert.Equal(t, StatusRunning, run.Status)

	// Audit trail: 1 pause + 1 resume_all per-run + 1 resume_all global
	// + 1 pause_all per-run + 1 pause_all global + 1 resume.
	assert.Equal(t, 1, countAudits(t, st, ActionPause))
	assert.Equal(t, 1, countAudits(t, st, ActionResume))
	assert.Equal(t, 2, countAudits(t, st, ActionPauseAll))  // per-run + global
	assert.Equal(t, 2, countAudits(t, st, ActionResumeAll)) // per-run + global
}

// =========================================================================
// Sentinel errors are distinguishable
// =========================================================================

func TestSentinelErrors_Distinguishable(t *testing.T) {
	// Ensure the sentinels are distinct values, so callers can use
	// errors.Is to branch on the specific failure.
	assert.NotEqual(t, ErrRunNotFound, ErrNotPausable)
	assert.NotEqual(t, ErrNotPausable, ErrNotResumable)
	assert.NotEqual(t, ErrNotResumable, ErrPermissionDenied)
	assert.NotEqual(t, ErrPermissionDenied, ErrEmptyRunID)
	assert.NotEqual(t, ErrEmptyRunID, ErrEmptyActor)

	// Wrapped errors should still match via errors.Is.
	wrapped := errors.New("outer: " + ErrNotPausable.Error())
	assert.NotErrorIs(t, wrapped, ErrNotPausable) // not a wrap, just a message
}

// keys returns the keys of a map[string]struct{} as a sorted-agnostic
// slice for use with assert.ElementsMatch.
func keys(m map[string]struct{}) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
