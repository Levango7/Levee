package permission

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// sampleConfig returns a PermissionConfig that covers the main scenarios:
// regular team with multiple envs, a restricted team, and a wildcard-env
// admin team.
func sampleConfig() PermissionConfig {
	return PermissionConfig{
		Teams: []TeamRule{
			{
				Name: "sre",
				Environments: []EnvPermission{
					{Name: "dev", Actions: []string{ActionPlan, ActionApply, ActionApprove, ActionRollback, ActionView}},
					{Name: "staging", Actions: []string{ActionPlan, ActionApply, ActionApprove, ActionRollback, ActionView}},
					{Name: "prod", Actions: []string{ActionPlan, ActionApprove, ActionRollback, ActionView}},
				},
			},
			{
				Name: "dba",
				Environments: []EnvPermission{
					{Name: "prod", Actions: []string{ActionApply, ActionRollback, ActionView}},
				},
			},
			{
				Name: "security",
				Environments: []EnvPermission{
					{Name: Wildcard, Actions: []string{ActionAdmin}},
				},
			},
		},
	}
}

func TestNewPermissionMatrix(t *testing.T) {
	m := NewPermissionMatrix()
	assert.NotNil(t, m)
	// Empty matrix should deny everything.
	assert.False(t, m.Allow("sre", "prod", ActionView))
	assert.Empty(t, m.Teams())
	assert.Empty(t, m.Environments())
	assert.Empty(t, m.ActionsFor("sre", "prod"))
}

func TestLoadFromConfig(t *testing.T) {
	m := NewPermissionMatrix()
	err := m.LoadFromConfig(sampleConfig())
	require.NoError(t, err)

	// sre on dev: has plan, apply, approve, rollback, view.
	assert.True(t, m.Allow("sre", "dev", ActionPlan))
	assert.True(t, m.Allow("sre", "dev", ActionApply))
	assert.True(t, m.Allow("sre", "dev", ActionView))

	// sre on prod: no apply.
	assert.False(t, m.Allow("sre", "prod", ActionApply))
	assert.True(t, m.Allow("sre", "prod", ActionPlan))

	// dba on prod: has apply, rollback, view; no plan.
	assert.True(t, m.Allow("dba", "prod", ActionApply))
	assert.False(t, m.Allow("dba", "prod", ActionPlan))

	// dba on dev: no permissions at all.
	assert.False(t, m.Allow("dba", "dev", ActionApply))
}

func TestLoadFromConfig_EmptyConfig(t *testing.T) {
	m := NewPermissionMatrix()
	err := m.LoadFromConfig(PermissionConfig{})
	assert.ErrorIs(t, err, ErrConfigInvalid)
}

func TestLoadFromConfig_EmptyTeamName(t *testing.T) {
	m := NewPermissionMatrix()
	cfg := PermissionConfig{
		Teams: []TeamRule{
			{Name: "", Environments: []EnvPermission{{Name: "dev", Actions: []string{ActionView}}}},
		},
	}
	err := m.LoadFromConfig(cfg)
	assert.ErrorIs(t, err, ErrConfigInvalid)
}

func TestLoadFromConfig_EmptyEnvName(t *testing.T) {
	m := NewPermissionMatrix()
	cfg := PermissionConfig{
		Teams: []TeamRule{
			{Name: "sre", Environments: []EnvPermission{{Name: "", Actions: []string{ActionView}}}},
		},
	}
	err := m.LoadFromConfig(cfg)
	assert.ErrorIs(t, err, ErrConfigInvalid)
}

func TestLoadFromConfig_EmptyAction(t *testing.T) {
	m := NewPermissionMatrix()
	cfg := PermissionConfig{
		Teams: []TeamRule{
			{Name: "sre", Environments: []EnvPermission{{Name: "dev", Actions: []string{""}}}},
		},
	}
	err := m.LoadFromConfig(cfg)
	assert.ErrorIs(t, err, ErrConfigInvalid)
}

func TestLoadFromYAML(t *testing.T) {
	yamlData := `
teams:
  - name: sre
    environments:
      - name: dev
        actions: [plan, apply, approve, rollback, view]
      - name: staging
        actions: [plan, apply, approve, rollback, view]
      - name: prod
        actions: [plan, approve, rollback, view]
  - name: dba
    environments:
      - name: prod
        actions: [apply, rollback, view]
  - name: security
    environments:
      - name: "*"
        actions: [admin]
`
	dir := t.TempDir()
	path := filepath.Join(dir, "permissions.yaml")
	require.NoError(t, os.WriteFile(path, []byte(yamlData), 0o644))

	m := NewPermissionMatrix()
	err := m.LoadFromYAML(path)
	require.NoError(t, err)

	// Verify loaded rules.
	assert.True(t, m.Allow("sre", "dev", ActionApply))
	assert.False(t, m.Allow("sre", "prod", ActionApply))
	assert.True(t, m.Allow("dba", "prod", ActionApply))
	// security has admin on "*" → all actions on prod.
	assert.True(t, m.Allow("security", "prod", ActionApply))
	assert.True(t, m.Allow("security", "dev", ActionPlan))
}

func TestLoadFromYAML_FileNotFound(t *testing.T) {
	m := NewPermissionMatrix()
	err := m.LoadFromYAML("/nonexistent/path/permissions.yaml")
	assert.Error(t, err)
}

func TestLoadFromJSON(t *testing.T) {
	jsonData := `{
  "teams": [
    {
      "name": "sre",
      "environments": [
        {"name": "dev", "actions": ["plan", "apply", "view"]},
        {"name": "prod", "actions": ["plan", "view"]}
      ]
    },
    {
      "name": "platform",
      "environments": [
        {"name": "*", "actions": ["admin"]}
      ]
    }
  ]
}`

	m := NewPermissionMatrix()
	err := m.LoadFromJSON([]byte(jsonData))
	require.NoError(t, err)

	assert.True(t, m.Allow("sre", "dev", ActionApply))
	assert.False(t, m.Allow("sre", "prod", ActionApply))
	// platform has admin on "*" → all actions.
	assert.True(t, m.Allow("platform", "prod", ActionApply))
	assert.True(t, m.Allow("platform", "staging", ActionRollback))
}

func TestLoadFromJSON_InvalidJSON(t *testing.T) {
	m := NewPermissionMatrix()
	err := m.LoadFromJSON([]byte("{invalid json"))
	assert.Error(t, err)
}

func TestAllow_Basic(t *testing.T) {
	m := NewPermissionMatrix()
	m.Grant("sre", "prod", ActionApply)

	assert.True(t, m.Allow("sre", "prod", ActionApply))
	assert.False(t, m.Allow("sre", "prod", ActionPlan))
	assert.False(t, m.Allow("dba", "prod", ActionApply))
	assert.False(t, m.Allow("sre", "dev", ActionApply))
}

func TestAllow_AdminSuperset(t *testing.T) {
	m := NewPermissionMatrix()
	m.Grant("security", "prod", ActionAdmin)

	// admin implies all other actions.
	assert.True(t, m.Allow("security", "prod", ActionPlan))
	assert.True(t, m.Allow("security", "prod", ActionApply))
	assert.True(t, m.Allow("security", "prod", ActionRollback))
	assert.True(t, m.Allow("security", "prod", ActionView))
	assert.True(t, m.Allow("security", "prod", ActionPauseAll))
	// admin itself is also allowed.
	assert.True(t, m.Allow("security", "prod", ActionAdmin))
	// other teams still denied.
	assert.False(t, m.Allow("sre", "prod", ActionPlan))
}

func TestAllow_AdminWithRevoke(t *testing.T) {
	m := NewPermissionMatrix()
	m.Grant("security", "prod", ActionAdmin)
	// Even with admin, an explicit revoke wins.
	m.Revoke("security", "prod", ActionApply)

	assert.True(t, m.Allow("security", "prod", ActionPlan))
	assert.False(t, m.Allow("security", "prod", ActionApply))
}

func TestAllow_WildcardTeam(t *testing.T) {
	m := NewPermissionMatrix()
	// All teams can view on prod.
	m.Grant(Wildcard, "prod", ActionView)

	assert.True(t, m.Allow("sre", "prod", ActionView))
	assert.True(t, m.Allow("dba", "prod", ActionView))
	assert.True(t, m.Allow("anybody", "prod", ActionView))
	// But not apply.
	assert.False(t, m.Allow("sre", "prod", ActionApply))
	// And not on dev.
	assert.False(t, m.Allow("sre", "dev", ActionView))
}

func TestAllow_WildcardEnv(t *testing.T) {
	m := NewPermissionMatrix()
	// sre can view on all environments.
	m.Grant("sre", Wildcard, ActionView)

	assert.True(t, m.Allow("sre", "dev", ActionView))
	assert.True(t, m.Allow("sre", "staging", ActionView))
	assert.True(t, m.Allow("sre", "prod", ActionView))
	assert.True(t, m.Allow("sre", "emergency", ActionView))
	// Other teams denied.
	assert.False(t, m.Allow("dba", "prod", ActionView))
}

func TestAllow_WildcardBoth(t *testing.T) {
	m := NewPermissionMatrix()
	// Everyone can view everywhere.
	m.Grant(Wildcard, Wildcard, ActionView)

	assert.True(t, m.Allow("sre", "dev", ActionView))
	assert.True(t, m.Allow("dba", "prod", ActionView))
	assert.True(t, m.Allow("any", "any", ActionView))
}

func TestAllow_WildcardAdmin(t *testing.T) {
	m := NewPermissionMatrix()
	// security has admin on all environments.
	m.Grant("security", Wildcard, ActionAdmin)

	assert.True(t, m.Allow("security", "prod", ActionApply))
	assert.True(t, m.Allow("security", "dev", ActionPlan))
	assert.True(t, m.Allow("security", "emergency", ActionRollback))
}

func TestGrantRevoke(t *testing.T) {
	m := NewPermissionMatrix()

	m.Grant("sre", "prod", ActionPlan)
	m.Grant("sre", "prod", ActionApply)
	assert.True(t, m.Allow("sre", "prod", ActionPlan))
	assert.True(t, m.Allow("sre", "prod", ActionApply))

	m.Revoke("sre", "prod", ActionPlan)
	assert.False(t, m.Allow("sre", "prod", ActionPlan))
	assert.True(t, m.Allow("sre", "prod", ActionApply))
}

func TestRevokePrecedence(t *testing.T) {
	m := NewPermissionMatrix()

	// Grant via wildcard, revoke specific.
	m.Grant(Wildcard, "prod", ActionApply)
	m.Revoke("dba", "prod", ActionApply)

	assert.True(t, m.Allow("sre", "prod", ActionApply))
	assert.False(t, m.Allow("dba", "prod", ActionApply))
}

func TestRevokePrecedence_OverAdmin(t *testing.T) {
	m := NewPermissionMatrix()

	m.Grant("security", "prod", ActionAdmin)
	m.Revoke("security", "prod", ActionApply)

	// Admin would normally allow apply, but revoke wins.
	assert.False(t, m.Allow("security", "prod", ActionApply))
	// Other actions still allowed via admin.
	assert.True(t, m.Allow("security", "prod", ActionPlan))
}

func TestRevokePrecedence_WildcardRevoke(t *testing.T) {
	m := NewPermissionMatrix()

	// Grant specific, revoke via wildcard env.
	m.Grant("sre", "prod", ActionApply)
	m.Revoke("sre", Wildcard, ActionApply)

	assert.False(t, m.Allow("sre", "prod", ActionApply))
	assert.False(t, m.Allow("sre", "dev", ActionApply))
}

func TestActionsFor(t *testing.T) {
	m := NewPermissionMatrix()
	err := m.LoadFromConfig(sampleConfig())
	require.NoError(t, err)

	// sre on dev: plan, apply, approve, rollback, view.
	actions := m.ActionsFor("sre", "dev")
	assert.ElementsMatch(t,
		[]string{ActionPlan, ActionApply, ActionApprove, ActionRollback, ActionView},
		actions,
	)

	// sre on prod: plan, approve, rollback, view (no apply).
	actions = m.ActionsFor("sre", "prod")
	assert.ElementsMatch(t,
		[]string{ActionPlan, ActionApprove, ActionRollback, ActionView},
		actions,
	)

	// dba on dev: nothing.
	actions = m.ActionsFor("dba", "dev")
	assert.Empty(t, actions)
}

func TestActionsFor_AdminExpansion(t *testing.T) {
	m := NewPermissionMatrix()
	m.Grant("security", "prod", ActionAdmin)

	actions := m.ActionsFor("security", "prod")
	// Admin should expand to all actions.
	assert.ElementsMatch(t, AllActions, actions)
}

func TestActionsFor_AdminWithRevoke(t *testing.T) {
	m := NewPermissionMatrix()
	m.Grant("security", "prod", ActionAdmin)
	m.Revoke("security", "prod", ActionApply)

	actions := m.ActionsFor("security", "prod")
	// All actions except apply.
	expected := make([]string, 0, len(AllActions))
	for _, a := range AllActions {
		if a != ActionApply {
			expected = append(expected, a)
		}
	}
	assert.ElementsMatch(t, expected, actions)
}

func TestActionsFor_Sorted(t *testing.T) {
	m := NewPermissionMatrix()
	m.Grant("sre", "dev", ActionView)
	m.Grant("sre", "dev", ActionApply)
	m.Grant("sre", "dev", ActionPlan)

	actions := m.ActionsFor("sre", "dev")
	assert.Equal(t, []string{ActionApply, ActionPlan, ActionView}, actions)
}

func TestActionsFor_Empty(t *testing.T) {
	m := NewPermissionMatrix()
	m.Grant("sre", "dev", ActionView)

	assert.Nil(t, m.ActionsFor("", "dev"))
	assert.Nil(t, m.ActionsFor("sre", ""))
}

func TestTeams(t *testing.T) {
	m := NewPermissionMatrix()
	err := m.LoadFromConfig(sampleConfig())
	require.NoError(t, err)

	teams := m.Teams()
	assert.ElementsMatch(t, []string{"sre", "dba", "security"}, teams)
}

func TestTeams_Sorted(t *testing.T) {
	m := NewPermissionMatrix()
	m.Grant("zeta", "dev", ActionView)
	m.Grant("alpha", "dev", ActionView)
	m.Grant("mid", "dev", ActionView)

	teams := m.Teams()
	assert.Equal(t, []string{"alpha", "mid", "zeta"}, teams)
}

func TestTeams_ExcludesWildcard(t *testing.T) {
	m := NewPermissionMatrix()
	m.Grant(Wildcard, "dev", ActionView)
	m.Grant("sre", "dev", ActionView)

	teams := m.Teams()
	assert.Equal(t, []string{"sre"}, teams)
}

func TestEnvironments(t *testing.T) {
	m := NewPermissionMatrix()
	err := m.LoadFromConfig(sampleConfig())
	require.NoError(t, err)

	envs := m.Environments()
	assert.ElementsMatch(t, []string{"dev", "staging", "prod"}, envs)
}

func TestEnvironments_Sorted(t *testing.T) {
	m := NewPermissionMatrix()
	m.Grant("sre", "prod", ActionView)
	m.Grant("sre", "dev", ActionView)
	m.Grant("sre", "staging", ActionView)

	envs := m.Environments()
	assert.Equal(t, []string{"dev", "prod", "staging"}, envs)
}

func TestEnvironments_ExcludesWildcard(t *testing.T) {
	m := NewPermissionMatrix()
	m.Grant("sre", Wildcard, ActionView)
	m.Grant("sre", "prod", ActionView)

	envs := m.Environments()
	assert.Equal(t, []string{"prod"}, envs)
}

func TestAllowAny(t *testing.T) {
	m := NewPermissionMatrix()
	m.Grant("sre", "prod", ActionPlan)
	m.Grant("sre", "prod", ActionView)

	// Has plan → true.
	assert.True(t, m.AllowAny("sre", "prod", ActionApply, ActionPlan))
	// Has view → true.
	assert.True(t, m.AllowAny("sre", "prod", ActionApply, ActionView))
	// Has neither apply nor rollback → false.
	assert.False(t, m.AllowAny("sre", "prod", ActionApply, ActionRollback))
	// Empty actions → false.
	assert.False(t, m.AllowAny("sre", "prod"))
}

func TestAllowAll(t *testing.T) {
	m := NewPermissionMatrix()
	m.Grant("sre", "prod", ActionPlan)
	m.Grant("sre", "prod", ActionView)

	// Has both plan and view → true.
	assert.True(t, m.AllowAll("sre", "prod", ActionPlan, ActionView))
	// Missing apply → false.
	assert.False(t, m.AllowAll("sre", "prod", ActionPlan, ActionView, ActionApply))
	// Empty actions → false.
	assert.False(t, m.AllowAll("sre", "prod"))
}

func TestAllow_EmptyParameters(t *testing.T) {
	m := NewPermissionMatrix()
	m.Grant("sre", "prod", ActionView)

	// Empty team.
	assert.False(t, m.Allow("", "prod", ActionView))
	// Empty env.
	assert.False(t, m.Allow("sre", "", ActionView))
	// Empty action.
	assert.False(t, m.Allow("sre", "prod", ""))
}

func TestGrant_EmptyParameters(t *testing.T) {
	m := NewPermissionMatrix()
	// Empty parameters should be silently ignored, not panic.
	m.Grant("", "prod", ActionView)
	m.Grant("sre", "", ActionView)
	m.Grant("sre", "prod", "")

	assert.Empty(t, m.Teams())
	assert.Empty(t, m.Environments())
}

func TestRevoke_EmptyParameters(t *testing.T) {
	m := NewPermissionMatrix()
	// Empty parameters should be silently ignored, not panic.
	m.Revoke("", "prod", ActionView)
	m.Revoke("sre", "", ActionView)
	m.Revoke("sre", "prod", "")

	// Nothing was revoked, so a grant still works.
	m.Grant("sre", "prod", ActionView)
	assert.True(t, m.Allow("sre", "prod", ActionView))
}

func TestLoadFromConfig_ResetsExisting(t *testing.T) {
	m := NewPermissionMatrix()
	m.Grant("oldteam", "prod", ActionView)
	assert.True(t, m.Allow("oldteam", "prod", ActionView))

	// Loading a new config should reset the matrix.
	err := m.LoadFromConfig(sampleConfig())
	require.NoError(t, err)

	// oldteam should no longer have any permissions.
	assert.False(t, m.Allow("oldteam", "prod", ActionView))
	// But sre should.
	assert.True(t, m.Allow("sre", "dev", ActionPlan))
}

func TestLoadFromConfig_ResetsRevokes(t *testing.T) {
	m := NewPermissionMatrix()
	m.Grant("sre", "prod", ActionView)
	m.Revoke("sre", "prod", ActionView)
	// Revoke blocks the grant.
	assert.False(t, m.Allow("sre", "prod", ActionView))

	// Loading config should reset revokes too.
	err := m.LoadFromConfig(sampleConfig())
	require.NoError(t, err)

	// sre on prod has view (from config), and the revoke is gone.
	assert.True(t, m.Allow("sre", "prod", ActionView))
}

func TestRoundTrip_ConfigToMatrix(t *testing.T) {
	m := NewPermissionMatrix()
	err := m.LoadFromConfig(sampleConfig())
	require.NoError(t, err)

	// Verify the full matrix via Teams/Environments/ActionsFor.
	for _, team := range m.Teams() {
		for _, env := range m.Environments() {
			actions := m.ActionsFor(team, env)
			for _, a := range actions {
				assert.True(t, m.Allow(team, env, a),
					"team %q env %q action %q should be allowed", team, env, a)
			}
		}
	}
}

// ---------------------------------------------------------------------------
// Concurrency tests (run with `go test -race`).
// ---------------------------------------------------------------------------

// TestConcurrent_GrantRevokeAllow exercises the matrix with many goroutines
// performing Grant, Revoke, and Allow concurrently. Under the race
// detector this fails if the mutex protection is missing or incorrect.
func TestConcurrent_GrantRevokeAllow(t *testing.T) {
	m := NewPermissionMatrix()
	require.NoError(t, m.LoadFromConfig(sampleConfig()))

	teams := []string{"sre", "dba", "security", "network", "platform"}
	envs := []string{"dev", "staging", "prod", "emergency"}
	actions := []string{ActionPlan, ActionApply, ActionApprove, ActionRollback,
		ActionPause, ActionResume, ActionView, ActionAdmin}

	const goroutines = 16
	const iterations = 500

	var wg sync.WaitGroup
	wg.Add(goroutines * 3)

	// Writer A: Grant.
	for i := 0; i < goroutines; i++ {
		go func(seed int) {
			defer wg.Done()
			for n := 0; n < iterations; n++ {
				team := teams[(seed+n)%len(teams)]
				env := envs[(seed*n+1)%len(envs)]
				act := actions[(seed+n*3)%len(actions)]
				m.Grant(team, env, act)
			}
		}(i)
	}

	// Writer B: Revoke.
	for i := 0; i < goroutines; i++ {
		go func(seed int) {
			defer wg.Done()
			for n := 0; n < iterations; n++ {
				team := teams[(seed+n*2)%len(teams)]
				env := envs[(seed+n+3)%len(envs)]
				act := actions[(seed+n*5)%len(actions)]
				m.Revoke(team, env, act)
			}
		}(i)
	}

	// Reader: Allow.
	for i := 0; i < goroutines; i++ {
		go func(seed int) {
			defer wg.Done()
			for n := 0; n < iterations; n++ {
				team := teams[(seed+n)%len(teams)]
				env := envs[(seed+n*7)%len(envs)]
				act := actions[(seed+n*11)%len(actions)]
				// Result is non-deterministic under concurrent writers,
				// but the call must not panic or race.
				_ = m.Allow(team, env, act)
			}
		}(i)
	}

	wg.Wait()
}

// TestConcurrent_LoadAndRead exercises LoadFromConfig racing against
// readers. LoadFromConfig rebuilds the whole map, so a missing write lock
// would cause the readers to observe a half-swapped map and panic.
func TestConcurrent_LoadAndRead(t *testing.T) {
	m := NewPermissionMatrix()
	require.NoError(t, m.LoadFromConfig(sampleConfig()))

	cfgs := []PermissionConfig{
		sampleConfig(),
		{
			Teams: []TeamRule{
				{Name: "alpha", Environments: []EnvPermission{
					{Name: "dev", Actions: []string{ActionPlan, ActionView}},
				}},
			},
		},
		{
			Teams: []TeamRule{
				{Name: "beta", Environments: []EnvPermission{
					{Name: Wildcard, Actions: []string{ActionAdmin}},
				}},
			},
		},
	}

	const goroutines = 8
	const iterations = 200

	var wg sync.WaitGroup
	wg.Add(goroutines * 2)

	// Writer: LoadFromConfig.
	for i := 0; i < goroutines; i++ {
		go func(seed int) {
			defer wg.Done()
			for n := 0; n < iterations; n++ {
				_ = m.LoadFromConfig(cfgs[(seed+n)%len(cfgs)])
			}
		}(i)
	}

	// Reader: Teams/Environments/ActionsFor/Allow.
	for i := 0; i < goroutines; i++ {
		go func(seed int) {
			defer wg.Done()
			for n := 0; n < iterations; n++ {
				for _, team := range m.Teams() {
					for _, env := range m.Environments() {
						_ = m.ActionsFor(team, env)
						_ = m.Allow(team, env, ActionView)
					}
				}
			}
		}(i)
	}

	wg.Wait()
}

// TestConcurrent_AllowAnyAllowAll races AllowAny/AllowAll (which iterate
// over actions and call Allow internally) against writers.
func TestConcurrent_AllowAnyAllowAll(t *testing.T) {
	m := NewPermissionMatrix()
	require.NoError(t, m.LoadFromConfig(sampleConfig()))

	teams := []string{"sre", "dba", "security"}
	envs := []string{"dev", "staging", "prod"}
	allActions := AllActions

	const goroutines = 8
	const iterations = 300

	var wg sync.WaitGroup
	wg.Add(goroutines * 3)

	for i := 0; i < goroutines; i++ {
		go func(seed int) {
			defer wg.Done()
			for n := 0; n < iterations; n++ {
				m.Grant(teams[(seed+n)%len(teams)], envs[(seed+n)%len(envs)],
					allActions[(seed+n)%len(allActions)])
			}
		}(i)
	}

	for i := 0; i < goroutines; i++ {
		go func(seed int) {
			defer wg.Done()
			for n := 0; n < iterations; n++ {
				m.Revoke(teams[(seed+n)%len(teams)], envs[(seed+n)%len(envs)],
					allActions[(seed+n*2)%len(allActions)])
			}
		}(i)
	}

	for i := 0; i < goroutines; i++ {
		go func(seed int) {
			defer wg.Done()
			for n := 0; n < iterations; n++ {
				team := teams[(seed+n)%len(teams)]
				env := envs[(seed+n)%len(envs)]
				_ = m.AllowAny(team, env, ActionPlan, ActionApply, ActionView)
				_ = m.AllowAll(team, env, ActionView)
			}
		}(i)
	}

	wg.Wait()
}

// TestConcurrent_NoPanicOnEmptyParameters ensures that concurrent calls
// with empty parameters (which take the early-return path before the
// lock) do not race with writers that hold the lock.
func TestConcurrent_NoPanicOnEmptyParameters(t *testing.T) {
	m := NewPermissionMatrix()
	require.NoError(t, m.LoadFromConfig(sampleConfig()))

	const goroutines = 8
	const iterations = 1000

	var wg sync.WaitGroup
	wg.Add(goroutines * 2)

	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			for n := 0; n < iterations; n++ {
				m.Grant("sre", "prod", ActionView)
				m.Revoke("dba", "prod", ActionApply)
			}
		}()
	}

	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			for n := 0; n < iterations; n++ {
				// Empty parameters hit the early-return path.
				_ = m.Allow("", "prod", ActionView)
				_ = m.Allow("sre", "", ActionView)
				_ = m.Allow("sre", "prod", "")
				_ = m.ActionsFor("", "prod")
				_ = m.ActionsFor("sre", "")
			}
		}()
	}

	wg.Wait()
}

// TestConcurrent_GrantThenReadConsistency is a sanity check that after a
// Grant the change is observable by a concurrent reader. It uses an
// atomic counter to confirm at least some grants are observed, proving
// the write lock and read lock operate on the same map instance.
func TestConcurrent_GrantThenReadConsistency(t *testing.T) {
	m := NewPermissionMatrix()

	const writers = 200
	const readerPasses = 50
	var observed int64

	var wg sync.WaitGroup
	wg.Add(2)

	// Writer: grant a unique action marker on a fixed team/env.
	go func() {
		defer wg.Done()
		for n := 0; n < writers; n++ {
			m.Grant("sre", "prod", fmt.Sprintf("marker_%d", n))
		}
	}()

	// Reader: repeatedly scan all markers and count how many are visible
	// across multiple passes. Because the writer is concurrently adding
	// markers, at least one pass should observe a non-empty set — unless
	// the writer has not started yet, in which case the final consistency
	// check below still proves correctness.
	go func() {
		defer wg.Done()
		for pass := 0; pass < readerPasses; pass++ {
			for n := 0; n < writers; n++ {
				if m.Allow("sre", "prod", fmt.Sprintf("marker_%d", n)) {
					atomic.AddInt64(&observed, 1)
				}
			}
		}
	}()

	wg.Wait()

	// After the writer finishes, every marker must be visible. This proves
	// that the write lock and read lock operate on the same underlying map
	// (i.e. grants are not silently dropped or written to a stale copy).
	visible := int64(0)
	for n := 0; n < writers; n++ {
		if m.Allow("sre", "prod", fmt.Sprintf("marker_%d", n)) {
			visible++
		}
	}
	assert.Equal(t, int64(writers), visible,
		"all grants must be visible after writers complete")
	assert.Greater(t, atomic.LoadInt64(&observed), int64(0),
		"reader should have observed at least one grant concurrently")
}
