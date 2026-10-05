package dsl

// gate_position_test.go — the position table itself: routing, defaults, and the
// refusal of an unknown keyword at every declaration site.

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func workflowWithGatePosition(position string) string {
	return `name: gp
target:
  type: host
  query: "os=linux"
gates:
  - position: ` + position + `
    cmd:
      run: "true"
steps:
  - name: reload
    action: shell.exec
    args:
      cmd: "true"
`
}

func stepVerifyWithGatePosition(position string) string {
	return `name: gp
target:
  type: host
  query: "os=linux"
steps:
  - name: reload
    action: shell.exec
    args:
      cmd: "true"
    verify:
      position: ` + position + `
      cmd:
        run: "true"
`
}

func batchesGateWithPosition(position string) string {
	return `name: gp
target:
  type: host
  query: "os=linux"
batches:
  strategy: percent
  steps: [1, 100]
  gate:
    position: ` + position + `
    cmd:
      run: "true"
steps:
  - name: reload
    action: shell.exec
    args:
      cmd: "true"
`
}

// TestGatePositions_UnknownKeywordRefusedAtEverySite is the fail-closed half.
// Refusing belongs at all three sites for the same reason: a check filed in the
// wrong slot still reaches the plan and plan_hash, so the workflow looks gated
// while the engine visits a different phase — and a default would decide THAT.
//
// Before the shared table existed only the workflow-level `gates:` loop refused;
// step `verify:` and `batches.gate` silently filed anything anywhere, which is
// how a typo'd position survived to production as a check that never runs.
func TestGatePositions_UnknownKeywordRefusedAtEverySite(t *testing.T) {
	for name, src := range map[string]string{
		"gates[]":       workflowWithGatePosition("post-batch"), // the dash is not the keyword
		"step verify":   stepVerifyWithGatePosition("after_batch"),
		"batches.gate":  batchesGateWithPosition("post_batc"),
		"gates[] empty": workflowWithGatePosition("preapply"),
	} {
		_, err := NewParser().ParseBytes([]byte(src))
		require.Error(t, err, "%s: an unknown position must be refused", name)
		assert.Contains(t, err.Error(), "LE051", "%s: %v", name, err)
		assert.Contains(t, err.Error(), "unknown gate position", "%s: %v", name, err)
		// The advice must come from the table, not from a hand-copied list: an
		// operator reading it learns exactly the keywords that work today.
		for _, kw := range GatePositions() {
			assert.Contains(t, err.Error(), kw, "%s: the refusal must list %q", name, kw)
		}
	}
}

// TestGatePositions_AcceptsTheCataloguedVocabulary is the accepting side of the
// same rule — every keyword the table lists must parse at every site. A one-sided
// test would pass if one site quietly gained a keyword the others lack.
func TestGatePositions_AcceptsTheCataloguedVocabulary(t *testing.T) {
	sites := map[string]func(string) string{
		"gates[]":      workflowWithGatePosition,
		"step verify":  stepVerifyWithGatePosition,
		"batches.gate": batchesGateWithPosition,
	}
	for site, build := range sites {
		for _, kw := range GatePositions() {
			_, err := NewParser().ParseBytes([]byte(build(kw)))
			require.NoError(t, err, "%s must accept position %q", site, kw)
		}
	}
}

// TestGatePositions_EachKeywordMeansADifferentPhase keeps the mapping injective
// and pinned: the slot is what internal/engine binds a verify phase to, so two
// keywords collapsing onto one slot would silently move a check.
func TestGatePositions_EachKeywordMeansADifferentPhase(t *testing.T) {
	// An out-of-range default slot means nothing is appended unless the keyword
	// itself decides the slot — so a fill proves the routing came from the table,
	// not from a default.
	const sentinel = GateSlot(99)
	seen := map[GateSlot]string{}
	for _, kw := range GatePositions() {
		spec := &GateSpec{}
		require.NoError(t, AppendGateCheck(spec, kw, GateCheck{Type: "cmd"}, sentinel))
		var filled GateSlot
		switch {
		case len(spec.Pre) == 1 && len(spec.Batch) == 0 && len(spec.Post) == 0:
			filled = GateSlotPre
		case len(spec.Batch) == 1 && len(spec.Pre) == 0 && len(spec.Post) == 0:
			filled = GateSlotBatch
		case len(spec.Post) == 1 && len(spec.Pre) == 0 && len(spec.Batch) == 0:
			filled = GateSlotPost
		default:
			t.Fatalf("position %q filled %d pre / %d batch / %d post", kw, len(spec.Pre), len(spec.Batch), len(spec.Post))
		}
		prev, dup := seen[filled]
		assert.False(t, dup, "positions %q and %q both map to slot %d", prev, kw, filled)
		seen[filled] = kw
	}
	assert.Len(t, seen, len(gateSlotsByPosition), "every slot must be reachable from some position")
}

// TestGatePositions_AbsentPositionUsesTheSiteDefault documents that the two
// defaults are different things on purpose: a step's verify block checks the
// step (post_apply), while batches.gate IS the between-batches check (§4.3).
func TestGatePositions_AbsentPositionUsesTheSiteDefault(t *testing.T) {
	wf, err := NewParser().ParseBytes([]byte(batchesGateWithPosition("")))
	require.NoError(t, err)
	require.NotNil(t, wf.Batches.Gate)
	assert.Len(t, wf.Batches.Gate.Batch, 1, "batches.gate without a position is the post_batch check")
	assert.Empty(t, wf.Batches.Gate.Post)

	wf, err = NewParser().ParseBytes([]byte(stepVerifyWithGatePosition("")))
	require.NoError(t, err)
	require.NotNil(t, wf.Steps[0].Gate)
	assert.Len(t, wf.Steps[0].Gate.Post, 1, "a step verify block without a position stays post_apply")
	assert.Empty(t, wf.Steps[0].Gate.Batch)

	wf, err = NewParser().ParseBytes([]byte(workflowWithGatePosition("")))
	require.NoError(t, err)
	require.NotNil(t, wf.Gate)
	assert.Len(t, wf.Gate.Pre, 1, "a bare gates[] entry keeps its historical pre_apply default")
}

// TestGatePositions_TableIsTheOnlyDefinition guards against a second copy of the
// vocabulary reappearing in a consumer: the sorted list, the membership judge and
// the error text must all read the same map.
func TestGatePositions_TableIsTheOnlyDefinition(t *testing.T) {
	listed := GatePositions()
	require.NotEmpty(t, listed)
	assert.Equal(t, listed, func() []string {
		sorted := append([]string(nil), listed...)
		for i := range sorted {
			for j := i + 1; j < len(sorted); j++ {
				if sorted[j] < sorted[i] {
					sorted[i], sorted[j] = sorted[j], sorted[i]
				}
			}
		}
		return sorted
	}(), "GatePositions must return the sorted order it claims")

	for _, kw := range listed {
		assert.True(t, IsGatePosition(kw), "%q is listed but not accepted", kw)
	}
	assert.False(t, IsGatePosition(""), "the empty position means absent, not a keyword")
	assert.False(t, IsGatePosition("PRE_APPLY"), "positions are case sensitive")
}
