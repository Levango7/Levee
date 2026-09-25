package main

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nexus/levee/internal/approval"
	"github.com/nexus/levee/internal/dsl"
	"github.com/nexus/levee/internal/runstatus"
)

// TestReplaceBlockIsIdempotent is the property the whole tool rests on: running
// the generator twice must produce the same bytes, or CI would flag a diff on
// every run and people would start ignoring the check.
func TestReplaceBlockIsIdempotent(t *testing.T) {
	const doc = "intro\n\n<!-- BEGIN GENERATED: x -->\nstale content\n<!-- END GENERATED: x -->\n\noutro\n"
	b := block{"x", func() string { return "fresh\n" }}

	once, err := replaceBlock(doc, b)
	require.NoError(t, err)
	twice, err := replaceBlock(once, b)
	require.NoError(t, err)

	assert.Equal(t, once, twice)
	assert.Contains(t, once, "fresh")
	assert.NotContains(t, once, "stale content")
	assert.Contains(t, once, "intro")
	assert.Contains(t, once, "outro", "text outside the markers must survive")
}

// TestReplaceBlockRejectsMissingMarkers: a generator that silently stops
// finding its anchor is the same failure it was built to prevent, wearing a
// disguise.
func TestReplaceBlockRejectsMissingMarkers(t *testing.T) {
	b := block{"x", func() string { return "fresh\n" }}

	_, err := replaceBlock("no markers here\n", b)
	require.Error(t, err, "missing BEGIN must be an error")

	_, err = replaceBlock("<!-- BEGIN GENERATED: x -->\nbody\n", b)
	require.Error(t, err, "missing END must be an error")

	// A block whose END precedes its BEGIN is malformed, not "empty".
	_, err = replaceBlock("<!-- END GENERATED: x -->\n<!-- BEGIN GENERATED: x -->\n", b)
	require.Error(t, err)
}

// TestRunStatusTableCoversEveryStatus guards the gloss switch: a status added
// to runstatus without documentation renders the placeholder, and this test
// turns that into a failure instead of a quietly unhelpful table row.
func TestRunStatusTableCoversEveryStatus(t *testing.T) {
	table := renderRunStatus()
	for _, s := range runstatus.All {
		assert.Contains(t, table, "`"+s+"`", "status %q missing from the generated table", s)
		assert.NotContains(t, statusMeaning(s), "缺少说明",
			"status %q has no gloss in docgen.statusMeaning", s)
	}
	assert.NotContains(t, table, "缺少说明")
}

// TestRunStatusTableStatesTheDoubleUndoRule pins the two facts an operator
// most needs and that the hand-written table got wrong: only rolled_back means
// restored, and a clean rollback cannot be rolled back again.
func TestRunStatusTableStatesTheDoubleUndoRule(t *testing.T) {
	row := func(status string) string {
		for _, line := range strings.Split(renderRunStatus(), "\n") {
			if strings.HasPrefix(line, "| `"+status+"`") {
				return line
			}
		}
		return ""
	}
	require.NotEmpty(t, row(runstatus.StatusRolledBack))
	assert.Contains(t, row(runstatus.StatusRolledBack), "状态已恢复")

	rb := row(runstatus.StatusRolledBack)
	assert.Contains(t, rb, "| 是 |", "a clean rollback IS terminal")
	assert.True(t, strings.HasSuffix(strings.TrimSpace(rb), "| 否 |"),
		"a clean rollback must NOT be re-drivable by RollbackChange, got: %s", rb)

	assert.True(t, strings.HasSuffix(strings.TrimSpace(row(runstatus.StatusRolledBackPartial)), "| 是 |"),
		"a partial rollback IS a rollback remedy entry point")
}

// TestBatchStrategyTableMatchesTheSingleList keeps the generated table and the
// parser/generator vocabulary mechanically identical.
func TestBatchStrategyTableMatchesTheSingleList(t *testing.T) {
	table := renderBatchStrategy()
	for _, s := range dsl.BatchStrategies {
		assert.Contains(t, table, "`"+s+"`")
		assert.NotContains(t, batchSemantics(s), "缺少说明",
			"batch strategy %q has no gloss in docgen.batchSemantics", s)
	}
	for _, retired := range []string{"count", "by-tag", "by-group"} {
		assert.NotContains(t, table, "`"+retired+"`", "retired strategy must not reappear")
	}
}

// TestApprovalTableReflectsTheCode pins the three columns that were wrong
// before generation: the emergency timeout, and the fact that standard does
// NOT reject on timeout.
func TestApprovalTableReflectsTheCode(t *testing.T) {
	table := renderApprovalLevels()
	mgr := approval.NewLevelManager()

	for _, level := range []string{approval.LevelStandard, approval.LevelHigh, approval.LevelEmergency} {
		cfg, err := mgr.Get(level)
		require.NoError(t, err)
		assert.Contains(t, table, "`"+level+"`")
		assert.Contains(t, table, humanDuration(cfg.Timeout))
		assert.Contains(t, table, escalationText(cfg.EscalationPolicy))
	}

	std, err := mgr.Get(approval.LevelStandard)
	require.NoError(t, err)
	high, err := mgr.Get(approval.LevelHigh)
	require.NoError(t, err)

	assert.Contains(t, escalationText(std.EscalationPolicy),
		"不会自动驳回", "standard must not claim it rejects on timeout")
	assert.Contains(t, escalationText(high.EscalationPolicy),
		approval.LevelEmergency, "high escalates to emergency")
}

// TestHumanDurationDoesNotHalveTheWindow is a regression test for a bug this
// generator itself shipped on its first run: dividing by 24h while printing an
// "h" suffix rendered the standard tier's 24-hour timeout as "1h".
func TestHumanDurationDoesNotHalveTheWindow(t *testing.T) {
	assert.Equal(t, "1d", humanDuration(24*time.Hour))
	assert.Equal(t, "4h", humanDuration(4*time.Hour))
	assert.Equal(t, "30m", humanDuration(30*time.Minute))
	assert.Equal(t, "15m", humanDuration(15*time.Minute))
	assert.Equal(t, "无超时", humanDuration(0))
	assert.Equal(t, "1m30s", humanDuration(90*time.Second)) // falls through to Duration.String()
}
