// trace_map_test.go pins the trace→wire mapping. Two facts are load-bearing:
//
//   - the target column carries the HOST, not the actor. The inline copy this
//     replaced mapped TargetHost from t.Actor, so a console reading a trace
//     could not say which machine a step ran on;
//   - Detail is decoded rather than dropped: the recorder writes target,
//     input, output, duration, error and metadata into the row's Detail column
//     and that column is inside the hash chain, so nothing here may change the
//     shape — only read it.
package grpc

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nexus/levee/internal/state"
)

func TestTraceEntryToPB_ReportsTheHostNotTheActor(t *testing.T) {
	detail, err := json.Marshal(map[string]any{
		"target":      "host:web-01.example.com",
		"input":       map[string]any{"cmd": "systemctl reload nginx"},
		"output":      map[string]any{"exit_code": "0"},
		"duration_ms": 4200,
		"metadata":    map[string]string{"batch_no": "2", "step_name": "reload"},
	})
	require.NoError(t, err)

	entry := traceEntryToPB(&state.Trace{
		ID: "tr-1", RunID: "run-1", Event: "step_execute",
		// The actor is deliberately NOT the host: that swap is the bug.
		Actor:     "op-1",
		Detail:    string(detail),
		PrevHash:  "aa",
		CurrHash:  "bb",
		Timestamp: time.Unix(1791609600, 0),
	})
	require.NotNil(t, entry)
	assert.Equal(t, "web-01.example.com", entry.GetTargetHost(), "the target column is the host")
	assert.Equal(t, "op-1", entry.GetActor(), "the actor is a separate column")
	assert.Equal(t, int64(4200), entry.GetDurationMs())
	assert.JSONEq(t, `{"cmd":"systemctl reload nginx"}`, string(entry.GetInput()))
	assert.JSONEq(t, `{"exit_code":"0"}`, string(entry.GetOutput()))
	// Detail is the readable context: the metadata when there is no error.
	assert.Contains(t, entry.GetDetail(), "batch_no")
}

func TestTraceEntryToPB_ErrorBecomesDetail(t *testing.T) {
	detail, err := json.Marshal(map[string]any{
		"target": "host:web-03",
		"error":  "exit status 1: package conflict",
	})
	require.NoError(t, err)
	entry := traceEntryToPB(&state.Trace{
		ID: "tr-2", Event: "step_execute", Detail: string(detail),
	})
	assert.Equal(t, "error: exit status 1: package conflict", entry.GetDetail())
}

func TestTraceEntryToPB_UnprefixedTargetPassesThrough(t *testing.T) {
	// A group target (or a future non-host target) must not be truncated to
	// garbage by the prefix strip.
	detail, err := json.Marshal(map[string]any{"target": "web-tier"})
	require.NoError(t, err)
	entry := traceEntryToPB(&state.Trace{ID: "tr-3", Detail: string(detail)})
	assert.Equal(t, "web-tier", entry.GetTargetHost())
}

func TestTraceEntryToPB_UndecodableDetailKeepsTheRow(t *testing.T) {
	// A row whose Detail is not the recorder's JSON is still part of the chain:
	// it keeps its event, actor, hashes and time; only the parsed parts are
	// empty. Failing the whole response would hide the chains around it.
	entry := traceEntryToPB(&state.Trace{
		ID: "tr-4", Event: "gate_check", Actor: "system", Detail: "not json",
		PrevHash: "cc", CurrHash: "dd", Timestamp: time.Unix(1, 0),
	})
	assert.Equal(t, "gate_check", entry.GetAction())
	assert.Empty(t, entry.GetTargetHost())
	assert.Nil(t, entry.GetInput())
}

func TestTraceEntryToPB_NilSafe(t *testing.T) {
	assert.Nil(t, traceEntryToPB(nil))
}
