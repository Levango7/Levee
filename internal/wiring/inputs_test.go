// inputs_test.go pins the plan-time half of LEVEELang input resolution
// (docs/leveelang-spec.md §2.1): the run's parameters are substituted into the
// plan's step args inside the one funnel every plan path goes through, so the
// plan hash binds the exact values that were approved.
package wiring

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/nexus/levee/internal/plan"
	"github.com/nexus/levee/internal/state"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const inputsWorkflowYAML = `name: inputs-wiring
input:
  - name: table
    type: string
target:
  type: host
  query: "env=test"
steps:
  - name: migrate
    action: shell.exec
    args:
      cmd: "DESCRIBE {{input.table}}"
`

// seedRunWithParams seeds a draft run whose parameter bag carries the input
// values (the same run.Params the gRPC/CLI create paths write).
func seedRunWithParams(t *testing.T, store state.Store, id, src, params string) {
	t.Helper()
	now := time.Now().UTC()
	require.NoError(t, store.CreateRun(context.Background(), &state.Run{
		ID:           id,
		WorkflowName: src,
		Params:       params,
		Status:       "draft",
		CreatedAt:    now,
		UpdatedAt:    now,
	}))
}

// TestGeneratePlanResolvesInputsIntoTheHash is the core property: the value is
// IN the plan artifact, different values are different plans (approval binds
// the value), and the same source + same value is reproducible.
func TestGeneratePlanResolvesInputsIntoTheHash(t *testing.T) {
	e, store := newTestEngine(t, "web-1")
	seedRunWithParams(t, store, "run-a", inputsWorkflowYAML, `{"table":"orders"}`)
	seedRunWithParams(t, store, "run-b", inputsWorkflowYAML, `{"table":"customers"}`)

	_, a, err := e.GeneratePlan(context.Background(), "run-a", []string{"web-1"})
	require.NoError(t, err)
	_, b, err := e.GeneratePlan(context.Background(), "run-b", []string{"web-1"})
	require.NoError(t, err)

	var pa plan.Plan
	require.NoError(t, json.Unmarshal([]byte(a.JSON), &pa))
	require.Len(t, pa.Batches, 1)
	require.Len(t, pa.Batches[0].Steps, 1)
	assert.Equal(t, "DESCRIBE orders", pa.Batches[0].Steps[0].Args["cmd"],
		"the resolved value must be the arg the engine actually runs")

	assert.NotEqual(t, a.Hash, b.Hash,
		"the same workflow with a different input value must be a different plan (approval is per value)")

	// Reproducible: same source + same value hashes equal.
	_, a2, err := e.GeneratePlan(context.Background(), "run-a", []string{"web-1"})
	require.NoError(t, err)
	assert.Equal(t, a.Hash, a2.Hash)
}

// TestGeneratePlanRefusesUnsatisfiedInput pins the governance gate: a required
// input with no value and no default refuses the plan rather than substituting
// nothing (or the literal reference).
func TestGeneratePlanRefusesUnsatisfiedInput(t *testing.T) {
	e, store := newTestEngine(t, "web-1")
	seedRunWithParams(t, store, "run-c", inputsWorkflowYAML, `{}`)

	_, _, err := e.GeneratePlan(context.Background(), "run-c", []string{"web-1"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "table")
}

// TestGeneratePlanUsesDeclaredDefault pins that a workflow with a default still
// plans when the run supplies nothing.
func TestGeneratePlanUsesDeclaredDefault(t *testing.T) {
	const withDefault = `name: inputs-default
input:
  - name: table
    type: string
    default: orders
target:
  type: host
  query: "env=test"
steps:
  - name: migrate
    action: shell.exec
    args:
      cmd: "DESCRIBE {{input.table}}"
`
	e, store := newTestEngine(t, "web-1")
	seedRunWithParams(t, store, "run-d", withDefault, `{}`)

	_, stored, err := e.GeneratePlan(context.Background(), "run-d", []string{"web-1"})
	require.NoError(t, err)
	var p plan.Plan
	require.NoError(t, json.Unmarshal([]byte(stored.JSON), &p))
	assert.Equal(t, "DESCRIBE orders", p.Batches[0].Steps[0].Args["cmd"])
}

// TestGeneratePlanIgnoresUnrelatedParams pins the carrier boundary: the run's
// parameter bag also holds unrelated keys (alert_id, target, …), and they must
// not leak into or disturb resolution.
func TestGeneratePlanIgnoresUnrelatedParams(t *testing.T) {
	e, store := newTestEngine(t, "web-1")
	seedRunWithParams(t, store, "run-e", inputsWorkflowYAML,
		`{"table":"orders","alert_id":"alert-9","target":"web-1"}`)

	_, stored, err := e.GeneratePlan(context.Background(), "run-e", []string{"web-1"})
	require.NoError(t, err)
	var p plan.Plan
	require.NoError(t, json.Unmarshal([]byte(stored.JSON), &p))
	assert.Equal(t, "DESCRIBE orders", p.Batches[0].Steps[0].Args["cmd"])
}
