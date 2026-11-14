package main

// cmd_partition_status_test.go pins the Q3 disconnect-policy half that
// `system status` reports: the control-plane state paired with the curated
// read-only whitelist (internal/partition).
//
// Both halves are asserted, in both directions. A status surface that reports
// "reachable" next to a whitelist that was silently dropped is worse than one
// that omits the block; and an execution command appearing in the "safe during
// a partition" list is exactly the regression this guard exists to catch — the
// whitelist's value is that somebody asked each entry whether it causes a
// remote side effect, and a single wrong entry makes the whole list unsafe to
// hand to an operator.

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPartitionStatusBlock(t *testing.T) {
	blk := partitionStatusBlock("unreachable")
	assert.Equal(t, "unreachable", blk["control_plane"],
		"the block must carry the same db_status the rest of system status reports")

	cmds, ok := blk["readonly_commands"].([]string)
	require.True(t, ok, "readonly_commands must be a []string")
	require.NotEmpty(t, cmds, "the Q3 whitelist must not render empty")

	// The families an operator actually reaches for during an outage.
	for _, want := range []string{"show", "list", "audit verify", "cluster status"} {
		assert.Contains(t, cmds, want)
	}
	// And the commands that must never be described as safe.
	for _, banned := range []string{"apply", "plan", "retry", "approve", "new", "serve"} {
		assert.NotContains(t, cmds, banned, "%q produces a side effect", banned)
	}
}
