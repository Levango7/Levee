// trace_map.go — the single mapping from a stored trace row to the wire entry.
//
// There were two mappers and they were both lossy, and a third copy inlined in
// GetTrace was worse than either:
//
//	TargetHost: t.Actor        // the "target" column showed who acted
//
// so an operator reading a trace could not tell WHICH HOST a step ran on,
// while the audit view's evidence fragments (input/output/detail) stayed empty
// for trace rows — the payload was already written and sealed in Detail, and
// nothing decoded it.
//
// Detail is what this function decodes. Its shape is the one the recorder
// writes (internal/audit/trace.go: target "host:xxx", input, output,
// duration_ms, error, metadata) — it is part of the hash chain, so it cannot
// change shape here without changing the chain.
package grpc

import (
	"encoding/json"
	"fmt"

	"github.com/nexus/levee/internal/grpc/pb"
	"github.com/nexus/levee/internal/state"
)

// traceRowDetail mirrors the recorder's Detail payload. A row whose Detail is
// empty or undecodable maps to an entry with the fields it can fill rather than
// failing the whole response: the row is still part of the chain and still has
// an event, an actor and a time.
type traceRowDetail struct {
	Target   string            `json:"target,omitempty"`
	Input    map[string]any    `json:"input,omitempty"`
	Output   map[string]any    `json:"output,omitempty"`
	Duration int64             `json:"duration_ms"`
	Error    string            `json:"error,omitempty"`
	Metadata map[string]string `json:"metadata,omitempty"`
}

// traceEntryToPB maps one stored trace row onto the wire entry.
func traceEntryToPB(t *state.Trace) *pb.TraceEntry {
	if t == nil {
		return nil
	}
	entry := &pb.TraceEntry{
		Id:        t.ID,
		RunId:     t.RunID,
		Action:    t.Event,
		PrevHash:  t.PrevHash,
		CurrHash:  t.CurrHash,
		Timestamp: t.Timestamp.Unix(),
	}
	detail := traceRowDetail{}
	if t.Detail != "" {
		// Undecodable Detail is not a reason to drop the row: keep the parts
		// that are readable and let the caller's own verification catch it.
		_ = json.Unmarshal([]byte(t.Detail), &detail)
	}
	entry.Actor = t.Actor
	entry.TargetHost = traceHost(detail.Target)
	entry.DurationMs = detail.Duration

	// The wire fields are bytes (JSON): the audit view renders them verbatim,
	// and a JSON string is the shape both writers already produce.
	if len(detail.Input) > 0 {
		if blob, err := json.Marshal(detail.Input); err == nil {
			entry.Input = blob
		}
	}
	if len(detail.Output) > 0 {
		if blob, err := json.Marshal(detail.Output); err == nil {
			entry.Output = blob
		}
	}
	// Detail is what the evidence card shows besides input/output: the failure
	// when there is one, otherwise the readable context the recorder stored
	// (batch number, step name), which is also what the target map groups by.
	switch {
	case detail.Error != "":
		entry.Detail = fmt.Sprintf("error: %s", detail.Error)
	case len(detail.Metadata) > 0:
		blob, err := json.Marshal(detail.Metadata)
		if err == nil {
			entry.Detail = string(blob)
		}
	}
	return entry
}

// traceHost strips the recorder's "host:" prefix. An unprefixed target (a
// group, or the future's non-host target) is passed through unchanged rather
// than guessed at.
func traceHost(target string) string {
	const prefix = "host:"
	if len(target) > len(prefix) && target[:len(prefix)] == prefix {
		return target[len(prefix):]
	}
	return target
}
