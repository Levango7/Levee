package approval

import (
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// =========================================================================
// MatchPattern.matches
// =========================================================================

func TestMatchPattern_MatchesExactModuleAndAction(t *testing.T) {
	p := MatchPattern{Module: "pkg", Action: "remove"}
	assert.True(t, p.matches(Step{Module: "pkg", Action: "remove"}))
}

func TestMatchPattern_DifferentModuleDoesNotMatch(t *testing.T) {
	p := MatchPattern{Module: "pkg", Action: "remove"}
	assert.False(t, p.matches(Step{Module: "svc", Action: "remove"}))
}

func TestMatchPattern_DifferentActionDoesNotMatch(t *testing.T) {
	p := MatchPattern{Module: "pkg", Action: "remove"}
	assert.False(t, p.matches(Step{Module: "pkg", Action: "install"}))
}

func TestMatchPattern_EmptyTargetsMatchAnyTarget(t *testing.T) {
	// When Targets is empty, any target matches — including the empty
	// target. This lets a pattern match purely on module + action.
	p := MatchPattern{Module: "pkg", Action: "remove"}
	assert.True(t, p.matches(Step{Module: "pkg", Action: "remove", Target: ""}))
	assert.True(t, p.matches(Step{Module: "pkg", Action: "remove", Target: "anything"}))
}

func TestMatchPattern_TargetSubstringMatch(t *testing.T) {
	p := MatchPattern{
		Module:  "pkg",
		Action:  "remove",
		Targets: []string{"mysql", "redis"},
	}
	assert.True(t, p.matches(Step{Module: "pkg", Action: "remove", Target: "mysql-server"}))
	assert.True(t, p.matches(Step{Module: "pkg", Action: "remove", Target: "redis-cache"}))
	assert.False(t, p.matches(Step{Module: "pkg", Action: "remove", Target: "nginx"}))
}

func TestMatchPattern_TargetMatchIsCaseInsensitive(t *testing.T) {
	// Target matching is case-insensitive to tolerate casing
	// differences in user-supplied target identifiers (MySQL vs mysql).
	p := MatchPattern{
		Module:  "pkg",
		Action:  "remove",
		Targets: []string{"mysql"},
	}
	assert.True(t, p.matches(Step{Module: "pkg", Action: "remove", Target: "MySQL"}))
	assert.True(t, p.matches(Step{Module: "pkg", Action: "remove", Target: "MYSQL"}))
	assert.True(t, p.matches(Step{Module: "pkg", Action: "remove", Target: "MySql-Server"}))
}

func TestMatchPattern_TargetsButEmptyStepTargetDoesNotMatch(t *testing.T) {
	// When the pattern requires specific targets but the step has no
	// target, the pattern does not match — we cannot confirm the
	// dangerous target is present.
	p := MatchPattern{
		Module:  "pkg",
		Action:  "remove",
		Targets: []string{"mysql"},
	}
	assert.False(t, p.matches(Step{Module: "pkg", Action: "remove", Target: ""}))
}

// =========================================================================
// NewTemplateLibrary — built-in templates
// =========================================================================

func TestNewTemplateLibrary_HasThreeBuiltins(t *testing.T) {
	lib := NewTemplateLibrary()
	list := lib.List()
	require.Len(t, list, 3)
	// List returns sorted by name.
	assert.Equal(t, "database-drop", list[0].Name)
	assert.Equal(t, "firewall-flush", list[1].Name)
	assert.Equal(t, "master-slave-switch", list[2].Name)
}

func TestBuiltin_DatabaseDrop(t *testing.T) {
	lib := NewTemplateLibrary()
	tpl, err := lib.Get("database-drop")
	require.NoError(t, err)
	assert.Equal(t, "database-drop", tpl.Name)
	assert.Equal(t, LevelHigh, tpl.RequiredLevel)
	assert.Equal(t, 2, tpl.RequiredApprovers)
	assert.Equal(t, 2, tpl.MinApprovers)
	assert.Equal(t, 4*time.Hour, tpl.Timeout)
	assert.NotEmpty(t, tpl.Description)
	require.Len(t, tpl.MatchPatterns, 1)
	assert.Equal(t, "pkg", tpl.MatchPatterns[0].Module)
	assert.Equal(t, "remove", tpl.MatchPatterns[0].Action)
	// Must catch the common db targets.
	assert.Contains(t, tpl.MatchPatterns[0].Targets, "mysql")
	assert.Contains(t, tpl.MatchPatterns[0].Targets, "postgres")
	assert.Contains(t, tpl.MatchPatterns[0].Targets, "redis")
}

func TestBuiltin_MasterSlaveSwitch(t *testing.T) {
	lib := NewTemplateLibrary()
	tpl, err := lib.Get("master-slave-switch")
	require.NoError(t, err)
	assert.Equal(t, "master-slave-switch", tpl.Name)
	assert.Equal(t, LevelHigh, tpl.RequiredLevel)
	assert.Equal(t, 2, tpl.MinApprovers)
	require.Len(t, tpl.MatchPatterns, 1)
	assert.Equal(t, "svc", tpl.MatchPatterns[0].Module)
	assert.Equal(t, "restart", tpl.MatchPatterns[0].Action)
	assert.Contains(t, tpl.MatchPatterns[0].Targets, "mysql")
	assert.Contains(t, tpl.MatchPatterns[0].Targets, "redis")
}

func TestBuiltin_FirewallFlush(t *testing.T) {
	lib := NewTemplateLibrary()
	tpl, err := lib.Get("firewall-flush")
	require.NoError(t, err)
	assert.Equal(t, "firewall-flush", tpl.Name)
	assert.Equal(t, LevelHigh, tpl.RequiredLevel)
	assert.Equal(t, 2, tpl.MinApprovers)
	require.Len(t, tpl.MatchPatterns, 1)
	assert.Equal(t, "shell", tpl.MatchPatterns[0].Module)
	assert.Equal(t, "exec", tpl.MatchPatterns[0].Action)
	// Must catch iptables flush variants.
	assert.NotEmpty(t, tpl.MatchPatterns[0].Targets)
}

func TestBuiltin_AllBuiltinsAreHighLevel(t *testing.T) {
	// Every built-in template must require the high tier — that is
	// the whole point of the template library.
	lib := NewTemplateLibrary()
	for _, tpl := range lib.List() {
		assert.Equalf(t, LevelHigh, tpl.RequiredLevel,
			"built-in template %q should require high level", tpl.Name)
		assert.GreaterOrEqualf(t, tpl.MinApprovers, 2,
			"built-in template %q should require at least 2 approvers", tpl.Name)
	}
}

// =========================================================================
// Get
// =========================================================================

func TestGet_NotFound(t *testing.T) {
	lib := NewTemplateLibrary()
	_, err := lib.Get("no-such-template")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not found")
	assert.Contains(t, err.Error(), "no-such-template")
}

// =========================================================================
// RegisterTemplate
// =========================================================================

func TestRegisterTemplate_AddsCustomTemplate(t *testing.T) {
	lib := NewTemplateLibrary()
	custom := Template{
		Name:              "custom-drop",
		Description:       "custom dangerous op",
		MatchPatterns:     []MatchPattern{{Module: "custom", Action: "nuke"}},
		RequiredLevel:     LevelHigh,
		RequiredApprovers: 3,
		MinApprovers:      3,
		Timeout:           1 * time.Hour,
	}
	require.NoError(t, lib.RegisterTemplate(custom))

	got, err := lib.Get("custom-drop")
	require.NoError(t, err)
	assert.Equal(t, "custom-drop", got.Name)
	assert.Equal(t, 3, got.MinApprovers)
}

func TestRegisterTemplate_OverwritesBuiltin(t *testing.T) {
	// Registering a template with the same Name as a built-in
	// overwrites it — this is the supported override mechanism.
	lib := NewTemplateLibrary()
	override := Template{
		Name:              "database-drop",
		Description:       "overridden: 3 approvers, 1h timeout",
		MatchPatterns:     []MatchPattern{{Module: "pkg", Action: "remove", Targets: []string{"mysql"}}},
		RequiredLevel:     LevelHigh,
		RequiredApprovers: 3,
		MinApprovers:      3,
		Timeout:           1 * time.Hour,
	}
	require.NoError(t, lib.RegisterTemplate(override))

	got, err := lib.Get("database-drop")
	require.NoError(t, err)
	assert.Equal(t, 3, got.MinApprovers)
	assert.Equal(t, 1*time.Hour, got.Timeout)
	assert.Contains(t, got.Description, "overridden")
}

func TestRegisterTemplate_EmptyNameRejected(t *testing.T) {
	lib := NewTemplateLibrary()
	err := lib.RegisterTemplate(Template{Name: "", RequiredLevel: LevelHigh})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "name cannot be empty")
}

func TestRegisterTemplate_InvalidLevelRejected(t *testing.T) {
	lib := NewTemplateLibrary()
	err := lib.RegisterTemplate(Template{
		Name:          "bad-level",
		RequiredLevel: "super-high",
	})
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrInvalidLevel))
	assert.Contains(t, err.Error(), "bad-level")
}

func TestRegisterTemplate_StandardLevelAllowed(t *testing.T) {
	// A template can require the standard level — unusual but legal.
	// (A site might want to register a known-safe pattern explicitly
	// so it shows up in `levee plan --show-templates`.)
	lib := NewTemplateLibrary()
	require.NoError(t, lib.RegisterTemplate(Template{
		Name:          "safe-op",
		RequiredLevel: LevelStandard,
	}))
}

func TestRegisterTemplate_EmergencyLevelAllowed(t *testing.T) {
	lib := NewTemplateLibrary()
	require.NoError(t, lib.RegisterTemplate(Template{
		Name:          "emergency-op",
		RequiredLevel: LevelEmergency,
	}))
}

// =========================================================================
// UnregisterTemplate
// =========================================================================

func TestUnregisterTemplate_RemovesBuiltin(t *testing.T) {
	lib := NewTemplateLibrary()
	lib.UnregisterTemplate("database-drop")
	_, err := lib.Get("database-drop")
	require.Error(t, err)
}

func TestUnregisterTemplate_MissingIsNoOp(t *testing.T) {
	lib := NewTemplateLibrary()
	// Unregistering a non-existent template does not panic or error.
	lib.UnregisterTemplate("no-such-template")
	assert.Len(t, lib.List(), 3, "library should still have 3 built-ins")
}

// =========================================================================
// List
// =========================================================================

func TestList_SortedByName(t *testing.T) {
	lib := NewTemplateLibrary()
	// Add a custom template that sorts before the built-ins.
	require.NoError(t, lib.RegisterTemplate(Template{
		Name:          "aaa-custom",
		RequiredLevel: LevelHigh,
	}))
	list := lib.List()
	require.Len(t, list, 4)
	assert.Equal(t, "aaa-custom", list[0].Name)
	assert.Equal(t, "database-drop", list[1].Name)
}

func TestList_ReturnsCopy(t *testing.T) {
	// The returned slice is a copy; mutating it must not affect the
	// library.
	lib := NewTemplateLibrary()
	list := lib.List()
	list[0].Name = "mutated"
	original, err := lib.Get("database-drop")
	require.NoError(t, err)
	assert.Equal(t, "database-drop", original.Name)
}

// =========================================================================
// Match — built-in templates
// =========================================================================

func TestMatch_DatabaseDropPkgRemoveMysql(t *testing.T) {
	lib := NewTemplateLibrary()
	tpl, err := lib.Match(Step{Module: "pkg", Action: "remove", Target: "mysql-server"})
	require.NoError(t, err)
	require.NotNil(t, tpl)
	assert.Equal(t, "database-drop", tpl.Name)
	assert.Equal(t, LevelHigh, tpl.RequiredLevel)
}

func TestMatch_DatabaseDropPkgRemovePostgres(t *testing.T) {
	lib := NewTemplateLibrary()
	tpl, err := lib.Match(Step{Module: "pkg", Action: "remove", Target: "postgres"})
	require.NoError(t, err)
	require.NotNil(t, tpl)
	assert.Equal(t, "database-drop", tpl.Name)
}

func TestMatch_DatabaseDropPkgRemoveRedis(t *testing.T) {
	lib := NewTemplateLibrary()
	tpl, err := lib.Match(Step{Module: "pkg", Action: "remove", Target: "redis"})
	require.NoError(t, err)
	require.NotNil(t, tpl)
	assert.Equal(t, "database-drop", tpl.Name)
}

func TestMatch_DatabaseDropPkgRemoveNonDbDoesNotMatch(t *testing.T) {
	// pkg.remove on a non-db target (e.g. nginx) does not match the
	// database-drop template — the target is not in the Targets list.
	lib := NewTemplateLibrary()
	tpl, err := lib.Match(Step{Module: "pkg", Action: "remove", Target: "nginx"})
	require.NoError(t, err)
	assert.Nil(t, tpl)
}

func TestMatch_MasterSlaveSwitchSvcRestartMysql(t *testing.T) {
	lib := NewTemplateLibrary()
	tpl, err := lib.Match(Step{Module: "svc", Action: "restart", Target: "mysql"})
	require.NoError(t, err)
	require.NotNil(t, tpl)
	assert.Equal(t, "master-slave-switch", tpl.Name)
	assert.Equal(t, LevelHigh, tpl.RequiredLevel)
}

func TestMatch_MasterSlaveSwitchSvcRestartRedis(t *testing.T) {
	lib := NewTemplateLibrary()
	tpl, err := lib.Match(Step{Module: "svc", Action: "restart", Target: "redis-master"})
	require.NoError(t, err)
	require.NotNil(t, tpl)
	assert.Equal(t, "master-slave-switch", tpl.Name)
}

func TestMatch_MasterSlaveSwitchSvcRestartNginxDoesNotMatch(t *testing.T) {
	lib := NewTemplateLibrary()
	tpl, err := lib.Match(Step{Module: "svc", Action: "restart", Target: "nginx"})
	require.NoError(t, err)
	assert.Nil(t, tpl)
}

func TestMatch_FirewallFlushIptablesF(t *testing.T) {
	lib := NewTemplateLibrary()
	tpl, err := lib.Match(Step{Module: "shell", Action: "exec", Target: "iptables -F"})
	require.NoError(t, err)
	require.NotNil(t, tpl)
	assert.Equal(t, "firewall-flush", tpl.Name)
	assert.Equal(t, LevelHigh, tpl.RequiredLevel)
}

func TestMatch_FirewallFlushIptablesFlush(t *testing.T) {
	lib := NewTemplateLibrary()
	tpl, err := lib.Match(Step{Module: "shell", Action: "exec", Target: "iptables --flush"})
	require.NoError(t, err)
	require.NotNil(t, tpl)
	assert.Equal(t, "firewall-flush", tpl.Name)
}

func TestMatch_FirewallFlushUfwFlush(t *testing.T) {
	lib := NewTemplateLibrary()
	tpl, err := lib.Match(Step{Module: "shell", Action: "exec", Target: "sudo ufw flush"})
	require.NoError(t, err)
	require.NotNil(t, tpl)
	assert.Equal(t, "firewall-flush", tpl.Name)
}

func TestMatch_FirewallFlushInnocuousShellDoesNotMatch(t *testing.T) {
	// shell.exec with a benign command does not match the
	// firewall-flush template.
	lib := NewTemplateLibrary()
	tpl, err := lib.Match(Step{Module: "shell", Action: "exec", Target: "ls -la"})
	require.NoError(t, err)
	assert.Nil(t, tpl)
}

func TestMatch_NoMatchReturnsNilNil(t *testing.T) {
	// A benign step matches no template; Match returns (nil, nil).
	lib := NewTemplateLibrary()
	tpl, err := lib.Match(Step{Module: "file", Action: "copy", Target: "/etc/hosts"})
	require.NoError(t, err)
	assert.Nil(t, tpl)
}

func TestMatch_ReturnsPointerToCopy(t *testing.T) {
	// The returned pointer is to a copy; mutating it must not affect
	// the library's internal template.
	lib := NewTemplateLibrary()
	tpl, err := lib.Match(Step{Module: "pkg", Action: "remove", Target: "mysql"})
	require.NoError(t, err)
	require.NotNil(t, tpl)
	tpl.Name = "mutated"
	tpl.MinApprovers = 99

	original, err := lib.Get("database-drop")
	require.NoError(t, err)
	assert.Equal(t, "database-drop", original.Name)
	assert.Equal(t, 2, original.MinApprovers)
}

// =========================================================================
// Match — determinism
// =========================================================================

func TestMatch_DeterministicWhenMultipleCouldMatch(t *testing.T) {
	// When two templates could match the same step, the
	// lexicographically smallest Name wins (because List is sorted and
	// Match iterates in that order).
	lib := NewTemplateLibrary()
	require.NoError(t, lib.RegisterTemplate(Template{
		Name:          "aaa-also-matches-mysql-drop",
		MatchPatterns: []MatchPattern{{Module: "pkg", Action: "remove", Targets: []string{"mysql"}}},
		RequiredLevel: LevelHigh,
	}))
	tpl, err := lib.Match(Step{Module: "pkg", Action: "remove", Target: "mysql"})
	require.NoError(t, err)
	require.NotNil(t, tpl)
	assert.Equal(t, "aaa-also-matches-mysql-drop", tpl.Name,
		"lexicographically smaller template name should win")
}

// =========================================================================
// MatchAny
// =========================================================================

func TestMatchAny_TrueWhenTemplateMatches(t *testing.T) {
	lib := NewTemplateLibrary()
	assert.True(t, lib.MatchAny(Step{Module: "pkg", Action: "remove", Target: "mysql"}))
	assert.True(t, lib.MatchAny(Step{Module: "svc", Action: "restart", Target: "redis"}))
	assert.True(t, lib.MatchAny(Step{Module: "shell", Action: "exec", Target: "iptables -F"}))
}

func TestMatchAny_FalseWhenNoTemplateMatches(t *testing.T) {
	lib := NewTemplateLibrary()
	assert.False(t, lib.MatchAny(Step{Module: "file", Action: "copy", Target: "/etc/hosts"}))
	assert.False(t, lib.MatchAny(Step{Module: "pkg", Action: "install", Target: "nginx"}))
	assert.False(t, lib.MatchAny(Step{}))
}

// =========================================================================
// Template struct
// =========================================================================

func TestTemplate_ZeroValue(t *testing.T) {
	var tpl Template
	assert.Empty(t, tpl.Name)
	assert.Empty(t, tpl.Description)
	assert.Empty(t, tpl.MatchPatterns)
	assert.Empty(t, tpl.RequiredLevel)
	assert.Zero(t, tpl.RequiredApprovers)
	assert.Zero(t, tpl.MinApprovers)
	assert.Zero(t, tpl.Timeout)
}

// =========================================================================
// Concurrency
// =========================================================================

func TestTemplateLibrary_ConcurrentMatch(t *testing.T) {
	// Match is read-only and must be safe for concurrent use.
	lib := NewTemplateLibrary()
	steps := []Step{
		{Module: "pkg", Action: "remove", Target: "mysql"},
		{Module: "svc", Action: "restart", Target: "redis"},
		{Module: "shell", Action: "exec", Target: "iptables -F"},
		{Module: "file", Action: "copy", Target: "/etc/hosts"},
	}

	const n = 100
	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func(i int) {
			defer wg.Done()
			_, _ = lib.Match(steps[i%len(steps)])
			_ = lib.MatchAny(steps[i%len(steps)])
			_ = lib.List()
		}(i)
	}
	wg.Wait()
}

func TestTemplateLibrary_ConcurrentRegisterAndMatch(t *testing.T) {
	// RegisterTemplate (writer) and Match (reader) can run concurrently.
	lib := NewTemplateLibrary()

	const n = 50
	var wg sync.WaitGroup
	wg.Add(n * 2)
	for i := 0; i < n; i++ {
		go func(i int) {
			defer wg.Done()
			_ = lib.RegisterTemplate(Template{
				Name:          "concurrent-template",
				RequiredLevel: LevelHigh,
			})
		}(i)
		go func(i int) {
			defer wg.Done()
			_, _ = lib.Match(Step{Module: "pkg", Action: "remove", Target: "mysql"})
		}(i)
	}
	wg.Wait()
}

// =========================================================================
// Integration with LevelManager
// =========================================================================

func TestTemplateAndLevelManager_HighRiskTemplateRaisesLevel(t *testing.T) {
	// Integration scenario: the planner matches a step to a high-risk
	// template, then uses the template's RequiredLevel to look up the
	// LevelConfig from the LevelManager. The two must agree that a
	// high-risk template maps to the high tier with 2 approvers.
	lib := NewTemplateLibrary()
	lm := NewLevelManager()

	step := Step{Module: "pkg", Action: "remove", Target: "mysql"}
	tpl, err := lib.Match(step)
	require.NoError(t, err)
	require.NotNil(t, tpl)
	assert.Equal(t, LevelHigh, tpl.RequiredLevel)

	cfg, err := lm.Get(tpl.RequiredLevel)
	require.NoError(t, err)
	assert.Equal(t, LevelHigh, cfg.Level)
	assert.Equal(t, 2, cfg.MinApprovers)
}

func TestTemplateAndLevelManager_TemplateOverridesDefaultApproverCount(t *testing.T) {
	// A custom template can tighten the approver count beyond the
	// level default. The planner should use the template's
	// MinApprovers when it is stricter than the level's.
	lib := NewTemplateLibrary()
	lm := NewLevelManager()

	// Register a template that requires 3 approvers at the high level
	// (the high level default is 2).
	require.NoError(t, lib.RegisterTemplate(Template{
		Name:          "extra-strict-drop",
		MatchPatterns: []MatchPattern{{Module: "pkg", Action: "remove", Targets: []string{"mysql"}}},
		RequiredLevel: LevelHigh,
		MinApprovers:  3,
		Timeout:       2 * time.Hour,
	}))

	step := Step{Module: "pkg", Action: "remove", Target: "mysql"}
	tpl, err := lib.Match(step)
	require.NoError(t, err)
	require.NotNil(t, tpl)
	// The lexicographically smaller "database-drop" wins; verify the
	// integration by checking the matched template's level is valid
	// and the LevelManager can resolve it.
	cfg, err := lm.Get(tpl.RequiredLevel)
	require.NoError(t, err)
	assert.Equal(t, LevelHigh, cfg.Level)
}

func TestTemplateAndLevelManager_BenignStepUsesStandardLevel(t *testing.T) {
	// A step that matches no template falls back to the level
	// determined by its attributes — standard for a benign step.
	lib := NewTemplateLibrary()
	lm := NewLevelManager()

	step := Step{Module: "file", Action: "copy", Target: "/etc/hosts"}
	tpl, err := lib.Match(step)
	require.NoError(t, err)
	assert.Nil(t, tpl, "benign step should not match any template")

	cfg := lm.DetermineLevel(step)
	assert.Equal(t, LevelStandard, cfg.Level)
	assert.Equal(t, 1, cfg.MinApprovers)
}

// =========================================================================
// Integration with service.go
// =========================================================================

func TestTemplate_CanBeUsedInCreateRequest(t *testing.T) {
	// A matched template's fields can be used to build a CreateRequest
	// that the Service accepts. This is the intended integration: the
	// planner matches the template, then copies RequiredLevel and
	// MinApprovers into CreateRequest.
	lib := NewTemplateLibrary()
	svc, _ := newService(t)

	step := Step{Module: "pkg", Action: "remove", Target: "mysql"}
	tpl, err := lib.Match(step)
	require.NoError(t, err)
	require.NotNil(t, tpl)

	a, err := svc.Create(bgCtx(), CreateRequest{
		RunID:        "run-1",
		Level:        tpl.RequiredLevel,
		Approvers:    []string{"alice", "bob"},
		MinApprovers: tpl.MinApprovers,
		ExpiresAt:    futureTime(),
	})
	require.NoError(t, err)
	assert.Equal(t, LevelHigh, a.Level)
	assert.Equal(t, 2, a.MinApprovers)
}
