// Boundary test between the irreversible checker and the approval vocabulary.
//
// The checker's whole output is a suggested tier, and a suggestion is only
// actionable if the approval service accepts that name. Asserting membership of
// dsl.ApprovalLevels (rather than equality with a locally restated string) is
// what makes the alias on ApprovalLevelHigh load-bearing: without it, a tier
// rename would leave the checker suggesting a tier that Create rejects with
// ErrInvalidLevel, and every test here would still pass.
package executor

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nexus/levee/internal/dsl"
)

func TestSuggestedApprovalLevelIsAcceptedByTheApprovalVocabulary(t *testing.T) {
	require.Equal(t, dsl.ApprovalLevelHigh, ApprovalLevelHigh,
		"an irreversible step suggests the high tier: two approvers, four hours")
	require.True(t, dsl.IsApprovalLevel(ApprovalLevelHigh),
		"ApprovalLevelHigh must name a tier the approval service accepts")

	c := NewIrreversibleChecker()
	c.RegisterWhitelist("pkg", "remove")

	for _, step := range []Step{
		{Module: "pkg", Action: "remove"},                   // whitelist path
		{Module: "custom", Action: "x", Irreversible: true}, // explicit author declaration
	} {
		res := c.Check(step)
		require.True(t, res.Irreversible, "%s.%s must be reported irreversible", step.Module, step.Action)
		assert.Equal(t, dsl.ApprovalLevelHigh, res.SuggestLevel,
			"suggesting another tier would change how many people have to say yes")
		assert.True(t, dsl.IsApprovalLevel(res.SuggestLevel),
			"suggested tier %q is not in the accepted vocabulary", res.SuggestLevel)
	}

	// The reversible path suggests nothing, and the empty string must not be
	// mistaken for a tier name by a caller that forwards it to the service.
	res := c.Check(Step{Module: "file", Action: "read"})
	assert.False(t, res.Irreversible)
	assert.Empty(t, res.SuggestLevel)
	assert.False(t, dsl.IsApprovalLevel(res.SuggestLevel))
}
