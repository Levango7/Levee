package executor

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestDefaultIrreversibleActions pins the inherent destructive vocabulary:
// sorted, deduplicated, and containing the five pairs the engine has always
// treated as irreversible by nature. Both the plan generator's checker and
// the dsl compile gate (V14/LE082) consume this one list — a regression here
// silently changes what both layers authorize.
func TestDefaultIrreversibleActions(t *testing.T) {
	assert.Equal(t, []string{
		"file.delete",
		"mysql.pt_osc",
		"mysql.replica_switch",
		"pkg.remove",
		"user.remove",
	}, DefaultIrreversibleActions())
}

// TestIsDefaultIrreversible pins the lookup semantics: hits on the five
// inherent pairs, misses on everything else (including empty inputs and the
// same module's reversible actions).
func TestIsDefaultIrreversible(t *testing.T) {
	assert.True(t, IsDefaultIrreversible("pkg", "remove"))
	assert.True(t, IsDefaultIrreversible("file", "delete"))
	assert.True(t, IsDefaultIrreversible("user", "remove"))
	assert.True(t, IsDefaultIrreversible("mysql", "replica_switch"))
	assert.True(t, IsDefaultIrreversible("mysql", "pt_osc"))

	assert.False(t, IsDefaultIrreversible("pkg", "install"))
	assert.False(t, IsDefaultIrreversible("mysql", "exec"))
	assert.False(t, IsDefaultIrreversible("", ""))
}
