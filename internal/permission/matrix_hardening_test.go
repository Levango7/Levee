package permission

// SA-013 / SA-016 hardening tests: unknown-action warnings with an
// assertable sink, strict-mode rejection, and the admin-on-env-wildcard
// warning that lists the affected teams.

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// warnCollector captures matrix warnings for assertions.
type warnCollector struct {
	msgs []string
}

func (w *warnCollector) hook(msg string, kv ...any) {
	w.msgs = append(w.msgs, fmt.Sprint(append([]any{msg}, kv...)))
}

func TestGrant_UnknownActionWarnsAndStillApplies(t *testing.T) {
	w := &warnCollector{}
	m := NewPermissionMatrix()
	m.warn = w.hook

	require.NoError(t, m.Grant("sre", "prod", "teleport"))
	assert.True(t, m.Allow("sre", "prod", "teleport"), "warn mode must not break legacy behaviour")
	require.Len(t, w.msgs, 1)
	assert.Contains(t, w.msgs[0], "unknown permission action granted")
	assert.Contains(t, w.msgs[0], "teleport")

	// Known action: silent.
	require.NoError(t, m.Grant("sre", "prod", ActionApply))
	assert.Len(t, w.msgs, 1)
}

func TestGrant_StrictModeRejectsUnknownAction(t *testing.T) {
	m := NewPermissionMatrix()
	m.StrictActions = true

	err := m.Grant("sre", "prod", "teleport")
	require.ErrorIs(t, err, ErrUnknownAction)
	assert.False(t, m.Allow("sre", "prod", "teleport"), "rejected grant must not be recorded")

	require.NoError(t, m.Grant("sre", "prod", ActionApply))
	assert.True(t, m.Allow("sre", "prod", ActionApply))
}

func TestRevoke_UnknownActionWarnsAndStrictRejects(t *testing.T) {
	w := &warnCollector{}
	m := NewPermissionMatrix()
	m.warn = w.hook
	require.NoError(t, m.Revoke("sre", "prod", "teleport"))
	require.Len(t, w.msgs, 1)
	assert.Contains(t, w.msgs[0], "unknown permission action revoked")

	m.StrictActions = true
	require.ErrorIs(t, m.Revoke("sre", "prod", "teleport"), ErrUnknownAction)
}

func TestGrant_EmptyArgsRemainNoOps(t *testing.T) {
	m := NewPermissionMatrix()
	m.StrictActions = true
	require.NoError(t, m.Grant("", "prod", ActionApply))
	require.NoError(t, m.Grant("sre", "", ActionApply))
	require.NoError(t, m.Grant("sre", "prod", ""))
}

func TestLoadFromConfig_AdminWildcardWarnsListingTeams(t *testing.T) {
	w := &warnCollector{}
	m := NewPermissionMatrix()
	m.warn = w.hook

	err := m.LoadFromConfig(PermissionConfig{Teams: []TeamRule{
		{Name: "security", Environments: []EnvPermission{{Name: Wildcard, Actions: []string{ActionAdmin}}}},
		{Name: "sre", Environments: []EnvPermission{{Name: "prod", Actions: []string{ActionAdmin}}}},
	}})
	require.NoError(t, err)

	require.Len(t, w.msgs, 1, "exactly one admin-wildcard warning")
	assert.Contains(t, w.msgs[0], "security", "team holding admin on '*' must be listed")
	assert.NotContains(t, w.msgs[0], "sre", "per-env admin (prod only) must not be flagged")
}

func TestLoadFromConfig_UnknownActionWarnsPerOccurrence(t *testing.T) {
	w := &warnCollector{}
	m := NewPermissionMatrix()
	m.warn = w.hook

	err := m.LoadFromConfig(PermissionConfig{Teams: []TeamRule{
		{Name: "sre", Environments: []EnvPermission{
			{Name: "dev", Actions: []string{ActionApply, "teleport"}},
			{Name: "prod", Actions: []string{"teleport"}},
		}},
	}})
	require.NoError(t, err)
	assert.Len(t, w.msgs, 2, "one warning per unknown-action occurrence, no cross-call dedup")
	assert.True(t, m.Allow("sre", "dev", "teleport"), "warn mode still loads the entry")
}

func TestLoadFromConfig_StrictModeFailsAtomically(t *testing.T) {
	m := NewPermissionMatrix()
	require.NoError(t, m.Grant("sre", "prod", ActionApply)) // pre-existing state

	m.StrictActions = true
	err := m.LoadFromConfig(PermissionConfig{Teams: []TeamRule{
		{Name: "sre", Environments: []EnvPermission{{Name: "prod", Actions: []string{ActionView}}}},
		{Name: "dba", Environments: []EnvPermission{{Name: "prod", Actions: []string{"teleport"}}}},
	}})
	require.ErrorIs(t, err, ErrUnknownAction)

	assert.True(t, m.Allow("sre", "prod", ActionApply),
		"failed load must leave the previous matrix untouched (build outside the lock)")
	assert.False(t, m.Allow("sre", "prod", ActionView),
		"the valid part of a rejected config must not be partially applied")
}
