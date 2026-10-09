package grpc

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nexus/levee/internal/state"
)

// TestRunToPBLabelsTheWorkflowNotItsSource: state.Run.WorkflowName's contract
// is the workflow SOURCE — wiring.resolveWorkflow parses it, and template
// instantiation stores the rendered document there. Mapping it straight onto
// pb.Change.Label put a YAML blob where the board prints a change's name, so
// the label now goes through the same rendering the CLI uses (dsl.DisplayName)
// while WorkflowFile keeps carrying the source verbatim.
func TestRunToPBLabelsTheWorkflowNotItsSource(t *testing.T) {
	// A document the parser accepts (target is required) — the shape a
	// template-instantiated run actually stores.
	src := "name: rolling-patch\nversion: \"1.0\"\ntarget:\n  type: host\n  query: \"env=prod\"\nsteps:\n  - name: deploy\n    action: shell.exec\n    args:\n      cmd: \"true\"\n"
	got := runToPB(&state.Run{ID: "run-1", WorkflowName: src})
	require.NotNil(t, got)
	assert.Equal(t, "rolling-patch", got.GetLabel())
	assert.Equal(t, src, got.GetWorkflowFile(),
		"the source still travels, in the field whose contract is the source")
	assert.NotContains(t, got.GetLabel(), "\n", "the label is printed into a table cell")

	// A path — and an operator-typed label, which arrives the same way — is
	// already a name and passes through unchanged.
	plain := runToPB(&state.Run{ID: "run-2", WorkflowName: "workflows/patch.yaml"})
	assert.Equal(t, "workflows/patch.yaml", plain.GetLabel())
	assert.Equal(t, "workflows/patch.yaml", plain.GetWorkflowFile())
}
