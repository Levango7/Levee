package plan

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nexus/levee/internal/dsl"
)

// TestBatchStrategyVocabulariesAgree is the guard that ended the
// batches.strategy drift. The dsl parser and the plan generator each used to
// carry their own list: the parser accepted one-per-target/count/by-tag/
// by-group (none of which the generator implemented), while the generator
// implemented fixed/serial (which the parser rejected). A workflow could
// therefore parse cleanly and then die one layer later with a Fatal LE034.
//
// Every strategy dsl accepts must survive plan generation, and every strategy
// the generator implements must be one the parser accepts.
func TestBatchStrategyVocabulariesAgree(t *testing.T) {
	targets := []string{"h1", "h2", "h3"}

	for _, strategy := range dsl.BatchStrategies {
		strategy := strategy
		t.Run("parser_accepts/"+strategy, func(t *testing.T) {
			src := "name: vocab-demo\n" +
				"target:\n  type: host\n  hosts:\n    - h1\n" +
				"batches:\n  strategy: " + strategy + "\n" +
				"steps:\n  - name: s\n    action: shell.exec\n    args:\n      cmd: true\n"
			wf, err := dsl.NewParser().ParseBytes([]byte(src))
			require.NoError(t, err, "dsl must accept %q", strategy)

			p, gerr := NewGenerator().Generate(wf, targets)
			require.NoError(t, gerr,
				"strategy %q parses but plan generation rejects it — the two vocabularies drifted", strategy)
			require.NotNil(t, p)
			assert.NotEmpty(t, p.Batches, "strategy %q produced no batches", strategy)
		})
	}

	// The generated drafts themselves must survive the same round trip.
	t.Run("generator_output_parses", func(t *testing.T) {
		for _, strategy := range dsl.BatchStrategies {
			assert.True(t, dsl.IsBatchStrategy(strategy))
		}
		assert.False(t, dsl.IsBatchStrategy("by-tag"), "retired vocabulary must not be accepted")
		assert.False(t, dsl.IsBatchStrategy("count"))
	})
}

// TestOnePerTargetSplitsStrictlySerially pins the semantics the spec describes
// (每批一台目标机，串行) for the strategy examples/gate-templates/mysql.yaml
// relies on.
func TestOnePerTargetSplitsStrictlySerially(t *testing.T) {
	targets := []string{"db-a", "db-b", "db-c"}
	src := "name: rollout\n" +
		"target:\n  type: host\n  hosts:\n    - db-a\n" +
		"batches:\n  strategy: one-per-target\n" +
		"steps:\n  - name: switch\n    action: shell.exec\n    args:\n      cmd: true\n"
	wf, err := dsl.NewParser().ParseBytes([]byte(src))
	require.NoError(t, err)

	p, gerr := NewGenerator().Generate(wf, targets)
	require.NoError(t, gerr)
	require.Len(t, p.Batches, 3, "one batch per target")
	for i, b := range p.Batches {
		assert.Equal(t, []string{targets[i]}, b.Targets)
	}
}
