// Tests for the compile-time advisory channel.
//
// The property under test is not "these four rules fire" but the two claims the
// channel exists for: an advisory never blocks (so Validate must stay empty for
// documents that only trigger advisories), and an advisory's severity is the
// catalogue's, not a set restated here.
package dsl

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nexus/levee/internal/errors"
)

// minimalWorkflow declares only the required blocks, so it is the baseline every
// advisory is expected against: no window, no approval, no batches.
const minimalWorkflow = `name: adv-baseline
target:
  type: host
  query: "env=test"
steps:
  - name: reload
    action: shell.exec
    args:
      cmd: "systemctl reload nginx"
`

func adviseCodes(t *testing.T, src string) []string {
	t.Helper()
	wf, err := NewParser().ParseBytes([]byte(src))
	require.NoError(t, err, "test fixture must parse")
	var codes []string
	for _, a := range NewValidator().Advise(wf) {
		codes = append(codes, a.Code)
	}
	return codes
}

func mustAdviseParse(t *testing.T, src string) *Workflow {
	t.Helper()
	wf, err := NewParser().ParseBytes([]byte(src))
	require.NoError(t, err, "test fixture must parse")
	return wf
}

func TestAdviseFiresForUndeclaredGovernanceBlocks(t *testing.T) {
	codes := adviseCodes(t, minimalWorkflow)
	assert.Equal(t, []string{codeMissingWindowBlock, codeMissingApprovalBlock, codeMissingBatchesBlock}, codes,
		"a workflow declaring none of window/approval/batches is told about all three")
}

func TestAdviseSilentWhenGovernanceIsDeclared(t *testing.T) {
	src := `name: adv-declared
target:
  type: host
  query: "env=test"
window:
  start: "02:00"
  end: "04:00"
  timezone: UTC
approval:
  level: high
batches:
  strategy: percent
  steps: [1, 10, 100]
  gate:
    cmd:
      run: "true"
steps:
  - name: reload
    action: shell.exec
    args:
      cmd: "true"
`
	assert.Empty(t, NewValidator().Advise(mustAdviseParse(t, src)),
		"a fully declared workflow has nothing to advise about — this is the shape shipped examples should keep")
}

func TestAdviseNeverBlocksWhatValidateAccepts(t *testing.T) {
	// The claim the whole channel rests on: advisories must not appear in
	// Validate, because every consumer treats a non-empty Validate as fatal.
	wf := mustAdviseParse(t, minimalWorkflow)
	require.Empty(t, NewValidator().Validate(wf),
		"the same document must produce no fatal finding, or the advisories are a behaviour break")
	require.NotEmpty(t, NewValidator().Advise(wf))
}

func TestCanaryAdvisoryIsPercentStrategyOnly(t *testing.T) {
	cases := []struct {
		name string
		src  string
		want bool
	}{
		{
			name: "percent first batch above 5",
			src:  "batches:\n  strategy: percent\n  steps: [20, 60, 100]\n",
			want: true,
		},
		{
			name: "percent first batch at the guidance",
			src:  "batches:\n  strategy: percent\n  steps: [5, 50, 100]\n",
			want: false,
		},
		{
			name: "fixed first batch of twenty targets",
			// The fleet size is unknown until the target query resolves at plan
			// time, so the 5% guidance cannot be evaluated from the document.
			src:  "batches:\n  strategy: fixed\n  steps: [20, 40, 100]\n",
			want: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			src := "name: canary\n" + tc.src + `target:
  type: host
  query: "env=test"
steps:
  - name: x
    action: shell.exec
    args:
      cmd: "true"
`
			codes := adviseCodes(t, src)
			got := false
			for _, c := range codes {
				if c == codeCanaryRatio {
					got = true
				}
			}
			assert.Equal(t, tc.want, got, "codes: %v", codes)
		})
	}
}

func TestAdvisoriesAreCatalogueWarningsNotRestatedHere(t *testing.T) {
	// Every code this file can emit must be catalogued CompileWarning, and each
	// must be a code the catalogue knows at all. An unregistered code would make
	// IsCompileWarning false and the "advisory" would behave like a fatal finding
	// the moment anything filters by severity.
	for _, code := range []string{codeCanaryRatio, codeMissingApprovalBlock, codeMissingWindowBlock, codeMissingBatchesBlock} {
		ci, ok := errors.Lookup(code)
		require.True(t, ok, "%s must exist in the catalogue", code)
		assert.Equal(t, errors.CompileWarning, ci.Compile, "%s must be catalogued as a warning", code)
		assert.True(t, IsCompileWarning(code))
	}

	// The fatal side of the same boundary, so the claim is not one-directional.
	for _, code := range []string{codeRequiredField, codeMissingTarget, codeMissingStep, codeWindowClock} {
		assert.False(t, IsCompileWarning(code), "%s must stay fatal", code)
	}

	// And the direction a hand-written list survives: LE052 is catalogued as a
	// warning but this package deliberately never emits it, so it is not one of
	// the four constants above. Only a function that asks the catalogue can
	// know it is a warning. Without this case, replacing IsCompileWarning with a
	// literal list of the codes produced here passes every assertion in this
	// file — which is the difference between "severity has one source" and a
	// second copy that will drift the day a code is reclassified.
	assert.True(t, IsCompileWarning("LE052"),
		"severity is the catalogue's verdict, not this package's emit list")
}

// TestLE052FiresOnlyWhenNoPostBatchCheckIsDeclared is the advisory half of the
// between-batches gate. LE052 was withheld for as long as its remedy did not
// exist: nothing populated GateSpec.Batch and batch-level declarations were never
// materialised, so telling an operator to declare a post_batch check was advice
// that could not work. Position routing and the engine walk now cover all three
// declaration sites, so the warning names something real — and it must stay a
// warning: a workflow without a between-batches check is legal per spec §4.3.
func TestLE052FiresOnlyWhenNoPostBatchCheckIsDeclared(t *testing.T) {
	ci, ok := errors.Lookup("LE052")
	require.True(t, ok, "LE052 stays catalogued")
	assert.Equal(t, errors.CompileWarning, ci.Compile)

	const batchesNoGate = `name: g
target:
  type: host
  query: "env=test"
batches:
  strategy: percent
  steps: [1, 100]
steps:
  - name: x
    action: shell.exec
    args:
      cmd: "true"
`
	codes := []string{}
	for _, a := range NewValidator().Advise(mustAdviseParse(t, batchesNoGate)) {
		codes = append(codes, a.Code)
	}
	assert.Contains(t, codes, "LE052",
		"a staged rollout with nothing verified between batches must be advised, got %v", codes)

	// Each of the three declaration sites satisfies it — the same sites the
	// engine walks.
	for name, src := range map[string]string{
		"batches.gate": `name: g
target:
  type: host
  query: "env=test"
batches:
  strategy: percent
  steps: [1, 100]
  gate:
    cmd:
      run: "true"
steps:
  - name: x
    action: shell.exec
`,
		"gates[] position": `name: g
target:
  type: host
  query: "env=test"
batches:
  strategy: percent
  steps: [1, 100]
gates:
  - position: post_batch
    cmd:
      run: "true"
steps:
  - name: x
    action: shell.exec
`,
		"step verify position": `name: g
target:
  type: host
  query: "env=test"
batches:
  strategy: percent
  steps: [1, 100]
steps:
  - name: x
    action: shell.exec
    verify:
      position: post_batch
      cmd:
        run: "true"
`,
	} {
		for _, a := range NewValidator().Advise(mustAdviseParse(t, src)) {
			assert.NotEqual(t, "LE052", a.Code, "%s declares a between-batches check", name)
		}
	}

	// A gates[] entry that is not a between-batches check does not satisfy it: the
	// predicate asks specifically about the batch slot, so a workflow declaring only
	// pre_apply and post_apply checks is still told it verifies nothing in between.
	for _, src := range map[string]string{
		"post_apply only": `name: g
target:
  type: host
  query: "env=test"
batches:
  strategy: percent
  steps: [1, 100]
gates:
  - position: post_apply
    cmd:
      run: "true"
steps:
  - name: x
    action: shell.exec
`,
		"pre_apply only": `name: g
target:
  type: host
  query: "env=test"
batches:
  strategy: percent
  steps: [1, 100]
gates:
  - position: pre_apply
    cmd:
      run: "true"
steps:
  - name: x
    action: shell.exec
`,
	} {
		codes := []string{}
		for _, a := range NewValidator().Advise(mustAdviseParse(t, src)) {
			codes = append(codes, a.Code)
		}
		assert.Contains(t, codes, "LE052", "a non-batch declaration is not a between-batches check, got %v", codes)
	}
	// With no batches block at all the question does not arise: LE096 already
	// says the change lands in one batch.
	for _, a := range NewValidator().Advise(mustAdviseParse(t, minimalWorkflow)) {
		assert.NotEqual(t, "LE052", a.Code, "a single-batch workflow must not be told about between-batches checks")
	}
}

func TestValidateAndAdviseNeverAgreeOnSeverity(t *testing.T) {
	// No document may produce the same finding from both entry points, and the
	// two sets must be strictly separated by catalogue severity.
	docs := []string{
		minimalWorkflow,
		"name: broken\nwindow:\n  start: \"25:99\"\n",
		"name: ok\ntarget:\n  type: host\n  query: \"e=1\"\napproval:\n  level: platinum\nsteps:\n  - name: x\n    action: shell.exec\n    args:\n      cmd: \"true\"\n",
	}
	for _, src := range docs {
		wf, err := NewParser().ParseBytes([]byte(src))
		if err != nil {
			continue // parse-level failures are Validate's business, not the advisories'
		}
		fatal := NewValidator().Validate(wf)
		advise := NewValidator().Advise(wf)
		for _, f := range fatal {
			assert.False(t, IsCompileWarning(f.Code), "Validate emitted warning-coded %s", f.Code)
		}
		for _, a := range advise {
			assert.True(t, IsCompileWarning(a.Code), "Advise emitted fatal-coded %s", a.Code)
		}
		fat := map[string]bool{}
		for _, f := range fatal {
			fat[f.Code+"/"+f.Field] = true
		}
		for _, a := range advise {
			assert.False(t, fat[a.Code+"/"+a.Field], "both entry points report %s on %s", a.Code, a.Field)
		}
	}
}

func TestAdviseNilWorkflowIsNotAdvised(t *testing.T) {
	assert.Empty(t, NewValidator().Advise(nil), "there is nothing to advise on; Validate reports the nil")
}

func TestHalfDeclaredWindowIsFatalNotAdvisory(t *testing.T) {
	// A window that exists but is malformed must be refused (LE020) and must not
	// additionally be told "you declared no window" — double reporting the same
	// block with contradictory messages is how gate noise trains people to ignore it.
	src := `name: half-window
target:
  type: host
  query: "env=test"
window:
  start: "02:00"
steps:
  - name: x
    action: shell.exec
    args:
      cmd: "true"
`
	wf := mustAdviseParse(t, src)
	assert.NotEmpty(t, NewValidator().Validate(wf), "a half-declared window is fatal")
	for _, a := range NewValidator().Advise(wf) {
		assert.NotEqual(t, codeMissingWindowBlock, a.Code,
			"an author who wrote a window must not be told they wrote none")
	}
}
