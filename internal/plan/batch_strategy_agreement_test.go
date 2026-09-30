package plan

import (
	"strings"
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

	// Legs three and four. The parser and the generator agreeing is not
	// enough: `levee compile` runs the validator and the type checker too, and
	// both carried their own copy of this list. one-per-target was implemented
	// in the generator and listed in the vocabulary while the validator and
	// the type checker still rejected it — so a workflow that planned fine
	// over gRPC could never be compiled, and the guard above stayed green
	// because it only compared the two layers that already agreed.
	t.Run("validator_accepts", func(t *testing.T) {
		for _, strategy := range dsl.BatchStrategies {
			errs := dsl.NewValidator().Validate(workflowWithStrategy(t, strategy))
			for _, e := range errs {
				assert.NotEqual(t, "batches.strategy", e.Field,
					"validator rejects %q that the parser accepts and the generator implements: %s",
					strategy, e.Message)
			}
		}
	})

	t.Run("typechecker_accepts", func(t *testing.T) {
		for _, strategy := range dsl.BatchStrategies {
			wf := workflowWithStrategy(t, strategy)
			checker := dsl.NewTypeChecker(dsl.NewTypeRegistry(), "vocab-demo")
			errs := checker.CheckWithMode(wf, dsl.ModeStrict)
			for _, e := range errs {
				assert.NotContains(t, e.Error(), "batch_strategy",
					"type checker rejects %q as an enum violation: %s", strategy, e.Error())
			}
		}
	})

	// Acceptance agreement is only half of it: a layer that accepts MORE than
	// the vocabulary is just as drifted (it lets a document through compile
	// that the rest of the system then refuses). Every layer must refuse an
	// unknown value.
	t.Run("all_layers_refuse_unknown", func(t *testing.T) {
		const bogus = "one-per-target-typo"

		_, perr := dsl.NewParser().ParseBytes([]byte("name: vocab-demo\n" +
			"target:\n  type: host\n  hosts:\n    - h1\n" +
			"batches:\n  strategy: " + bogus + "\n" +
			"steps:\n  - name: s\n    action: shell.exec\n    args:\n      cmd: true\n"))
		require.Error(t, perr, "parser must refuse an unknown strategy")

		var refused bool
		for _, e := range dsl.NewValidator().Validate(workflowWithStrategyAST(bogus)) {
			if e.Field == "batches.strategy" {
				refused = true
			}
		}
		assert.True(t, refused, "validator must refuse an unknown strategy")

		wf := workflowWithStrategyAST(bogus)
		checker := dsl.NewTypeChecker(dsl.NewTypeRegistry(), "vocab-demo")
		var typeRefused bool
		for _, e := range checker.CheckWithMode(wf, dsl.ModeStrict) {
			if strings.Contains(e.Error(), "batch_strategy") {
				typeRefused = true
			}
		}
		assert.True(t, typeRefused, "type checker must refuse an unknown strategy")
	})

	// Built as an AST on purpose: the parser rightly refuses "count", and this
	// leg is about the validator refusing it independently.
	t.Run("refusal_names_the_real_vocabulary", func(t *testing.T) {
		msg := ""
		for _, e := range dsl.NewValidator().Validate(workflowWithStrategyAST("count")) {
			if e.Field == "batches.strategy" {
				msg = e.Message
			}
		}
		require.NotEmpty(t, msg, "an unknown strategy must still be refused")
		for _, strategy := range dsl.BatchStrategies {
			assert.Contains(t, msg, strategy,
				"the error text must list every accepted strategy, not a hand-copied subset")
		}
	})
}

// workflowWithStrategy parses the smallest workflow whose only interesting
// field is its batch strategy.
func workflowWithStrategy(t *testing.T, strategy string) *dsl.Workflow {
	t.Helper()
	src := "name: vocab-demo\n" +
		"target:\n  type: host\n  hosts:\n    - h1\n" +
		"batches:\n  strategy: " + strategy + "\n" +
		"steps:\n  - name: s\n    action: shell.exec\n    args:\n      cmd: true\n"
	wf, err := dsl.NewParser().ParseBytes([]byte(src))
	require.NoError(t, err, "the parser must accept %q for this test to mean anything", strategy)
	return wf
}

// workflowWithStrategyAST is the same document without the parser in the way,
// for the cases where the strategy is deliberately not a real one.
func workflowWithStrategyAST(strategy string) *dsl.Workflow {
	return &dsl.Workflow{
		Meta:    dsl.WorkflowMeta{Name: "vocab-demo"},
		Targets: []dsl.TargetGroup{{Name: "t", Type: "host", Hosts: []string{"h1"}}},
		Steps:   []dsl.Step{{Name: "s", Module: "shell", Action: "exec"}},
		Batches: dsl.BatchConfig{Strategy: strategy},
	}
}

// TestApprovalLevelVocabulariesAgree applies the same test to the sibling
// vocabulary. Approval levels are not implemented by the generator, so the
// legs are parser / validator / type checker — the three copies that lived
// inside internal/dsl before they were sourced from dsl.ApprovalLevels.
func TestApprovalLevelVocabulariesAgree(t *testing.T) {
	require.Equal(t, []string{"standard", "high", "emergency"}, dsl.ApprovalLevels,
		"the vocabulary itself is what §4.4 declares; changing it is a spec change")

	build := func(level string) *dsl.Workflow {
		src := "name: approval-vocab\n" +
			"target:\n  type: host\n  hosts:\n    - h1\n" +
			"approval:\n  level: " + level + "\n" +
			"steps:\n  - name: s\n    action: shell.exec\n    args:\n      cmd: true\n"
		wf, err := dsl.NewParser().ParseBytes([]byte(src))
		require.NoError(t, err, "parser must accept level %q", level)
		return wf
	}

	for _, level := range dsl.ApprovalLevels {
		for _, e := range dsl.NewValidator().Validate(build(level)) {
			assert.NotEqual(t, "approval.level", e.Field,
				"validator rejects %q the parser accepts: %s", level, e.Message)
		}
		checker := dsl.NewTypeChecker(dsl.NewTypeRegistry(), "approval-vocab")
		for _, e := range checker.CheckWithMode(build(level), dsl.ModeStrict) {
			assert.NotContains(t, e.Error(), "approval_level",
				"type checker rejects %q as an enum violation: %s", level, e.Error())
		}
	}

	_, err := dsl.NewParser().ParseBytes([]byte("name: bad\n" +
		"target:\n  type: host\n  hosts:\n    - h1\n" +
		"approval:\n  level: critical\n" +
		"steps:\n  - name: s\n    action: shell.exec\n    args:\n      cmd: true\n"))
	require.Error(t, err, "an unknown approval level must be refused at parse time")
	assert.Contains(t, err.Error(), "LE044")

	// The other two layers must refuse it too. A layer that accepts MORE than
	// the vocabulary drifts just as badly as one that accepts less — that is
	// how an unimplementable value gets into a stored workflow.
	const bogus = "critical"
	bad := &dsl.Workflow{
		Meta:     dsl.WorkflowMeta{Name: "approval-vocab"},
		Targets:  []dsl.TargetGroup{{Name: "t", Type: "host", Hosts: []string{"h1"}}},
		Steps:    []dsl.Step{{Name: "s", Module: "shell", Action: "exec"}},
		Approval: &dsl.ApprovalSpec{Level: bogus},
	}
	var vRefused bool
	for _, e := range dsl.NewValidator().Validate(bad) {
		if e.Field == "approval.level" {
			vRefused = true
		}
	}
	assert.True(t, vRefused, "validator must refuse an unknown approval level")

	checker := dsl.NewTypeChecker(dsl.NewTypeRegistry(), "approval-vocab")
	var tRefused bool
	for _, e := range checker.CheckWithMode(bad, dsl.ModeStrict) {
		if strings.Contains(e.Error(), "approval_level") {
			tRefused = true
		}
	}
	assert.True(t, tRefused, "type checker must refuse an unknown approval level")
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
