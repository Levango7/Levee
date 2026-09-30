package plan

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nexus/levee/internal/dsl"
)

// irreversibleFixture builds a workflow with one step using the given
// module/action/irreversible declaration, plus a fixed single batch so
// Generate never fails on target division.
func irreversibleFixture(module, action string, declared bool) *dsl.Workflow {
	return &dsl.Workflow{
		Meta: dsl.WorkflowMeta{Name: "irr-fixture"},
		Steps: []dsl.Step{{
			Name:         "s1",
			Module:       module,
			Action:       action,
			Args:         map[string]any{"x": "y"},
			Irreversible: declared,
		}},
		Batches: dsl.BatchConfig{Strategy: "serial"},
	}
}

// TestGeneratorStampsWhitelistIrreversible pins that the default engine
// whitelist (pkg.remove, file.delete, user.remove, mysql.replica_switch,
// mysql.pt_osc) is stamped onto PlanStep at plan time — the R4/R2 wiring
// that was previously missing from production code (the checker existed
// but had zero production consumers).
func TestGeneratorStampsWhitelistIrreversible(t *testing.T) {
	g := NewGenerator()

	whitelisted := []struct{ module, action string }{
		{"pkg", "remove"},
		{"file", "delete"},
		{"user", "remove"},
		{"mysql", "replica_switch"},
		{"mysql", "pt_osc"},
	}
	for _, tc := range whitelisted {
		p, err := g.Generate(irreversibleFixture(tc.module, tc.action, false), []string{"h1"})
		require.NoError(t, err, tc.module+"."+tc.action)
		require.Len(t, p.Batches, 1)
		step := p.Batches[0].Steps[0]
		assert.True(t, step.Irreversible, "expected irreversible via whitelist: %s.%s", tc.module, tc.action)
		assert.NotEmpty(t, step.IrreversibleReason)
		assert.Contains(t, step.IrreversibleReason, "whitelist")
	}
}

// TestGeneratorStampsExplicitIrreversible pins that an explicit author
// declaration wins (checked first) and records its own reason.
func TestGeneratorStampsExplicitIrreversible(t *testing.T) {
	g := NewGenerator()
	p, err := g.Generate(irreversibleFixture("shell", "exec", true), []string{"h1"})
	require.NoError(t, err)
	step := p.Batches[0].Steps[0]
	assert.True(t, step.Irreversible)
	assert.Contains(t, step.IrreversibleReason, "explicitly marked")
}

// TestGeneratorReversibleStepStaysClean pins that ordinary steps carry
// Irreversible=false with a reversible reason.
func TestGeneratorReversibleStepStaysClean(t *testing.T) {
	g := NewGenerator()
	p, err := g.Generate(irreversibleFixture("pkg", "install", false), []string{"h1"})
	require.NoError(t, err)
	step := p.Batches[0].Steps[0]
	assert.False(t, step.Irreversible)
	assert.Contains(t, step.IrreversibleReason, "reversible")
}

// TestGeneratorCustomCheckerWiring pins that the whitelist includes the
// mysql actions registered by NewGenerator (the module this wiring was
// built for) — guarding against accidental whitelist regressions.
func TestGeneratorCustomCheckerWiring(t *testing.T) {
	g := NewGenerator()
	wl := g.irreversible.Whitelist()
	assert.Contains(t, wl, "mysql.replica_switch")
	assert.Contains(t, wl, "mysql.pt_osc")
	assert.Contains(t, wl, "pkg.remove")
}
