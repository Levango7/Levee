package plan

// generator_window_concurrency_test.go pins the consumption of
// `window.max_concurrency`, which for a long time was accepted by the parser
// and emitted into the IR with no reader at all: the plan's per-batch cap came
// only from `batches.max_concurrency`. A declaration nobody reads is worse than
// an absent one — the author believes they have constrained parallelism and
// the platform runs unconstrained.
//
// These tests bypass wiring's change-window gate on purpose: the question here
// is what the GENERATOR does with the value, not whether the window is open.

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const windowConcurrencyFixture = `
name: window-concurrency-demo
version: "1.0"
target:
  name: web-tier
  type: ssh
  hosts:
    - host-a
    - host-b
    - host-c
window:
  start: "00:00"
  end: "23:59"
  timezone: UTC
  max_concurrency: %d
batches:
  max_concurrency: %d
steps:
  - name: restart
    action: svc.restart
    args:
      unit: nginx
`

func generateWindowFixture(t *testing.T, windowCap, batchCap int) *Plan {
	t.Helper()
	wf := parseYAML(t, fmt.Sprintf(windowConcurrencyFixture, windowCap, batchCap))
	validateWorkflow(t, wf)
	targets := hostsFrom(t, wf)
	p, err := NewGenerator().Generate(wf, targets)
	require.NoError(t, err)
	return p
}

// sprintf keeps the fixture readable without importing fmt into every case.

func TestWindowMaxConcurrency_ClampsALargerBatchCap(t *testing.T) {
	p := generateWindowFixture(t, 2, 8)
	require.NotEmpty(t, p.Batches)
	for _, b := range p.Batches {
		assert.Equal(t, 2, b.MaxConcurrency,
			"the window caps parallelism for the whole run; a batch asking for 8 must run at most 2")
	}
}

// A zero batch cap means "unlimited". The window cap is exactly the knob that
// makes that safe, so it must apply rather than being skipped as "no preference".
func TestWindowMaxConcurrency_CapsAnUnlimitedBatch(t *testing.T) {
	p := generateWindowFixture(t, 3, 0)
	require.NotEmpty(t, p.Batches)
	for _, b := range p.Batches {
		assert.Equal(t, 3, b.MaxConcurrency,
			"an unlimited batch under a capped window must come back capped")
	}
}

// A narrower batch configuration wins: the window is a ceiling, not a target.
func TestWindowMaxConcurrency_DoesNotRaiseABatchCap(t *testing.T) {
	p := generateWindowFixture(t, 8, 2)
	require.NotEmpty(t, p.Batches)
	for _, b := range p.Batches {
		assert.Equal(t, 2, b.MaxConcurrency,
			"a stricter batch cap must not be relaxed by a looser window")
	}
}

// Zero and negative mean "no additional cap". This is the backwards-compatible
// shape: a workflow that does not mention the field must produce the same plan
// it always did, including a genuinely unlimited batch configuration.
func TestWindowMaxConcurrency_NoDeclarationLeavesBatchCapUntouched(t *testing.T) {
	for _, windowCap := range []int{0, -1} {
		p := generateWindowFixture(t, windowCap, 0)
		require.NotEmpty(t, p.Batches)
		for _, b := range p.Batches {
			assert.Equal(t, 0, b.MaxConcurrency,
				"window cap %d must not serialise an unlimited batch", windowCap)
		}
	}
}

// The clamp feeds the approved artifact, so it must be part of what plan_hash
// binds: otherwise two plans identical in every visible field could differ in
// how many targets may run at once.
func TestWindowMaxConcurrency_IsBoundByPlanHash(t *testing.T) {
	capped := generateWindowFixture(t, 2, 8)
	uncapped := generateWindowFixture(t, 0, 8)
	assert.NotEqual(t, ComputeHash(capped), ComputeHash(uncapped),
		"the effective concurrency is part of what approval blesses")
}
