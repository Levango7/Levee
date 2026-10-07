// agents.go — the LEVEE agent registry, persisted.
//
// Why this exists: the registry was a map inside whichever process built it
// (internal/agent/registry.go), so an agent that registered against the daemon
// was invisible to every other process — `levee agent list` printed an empty
// table however many agents were live. Persistence is what makes the answer
// independent of who asks: one table, both backends, read by any process.
//
// AgentStore is a SEPARATE interface from Store on purpose. Store is
// implemented by a score of hand-written fakes across the repository
// (internal/grpc, internal/audit, internal/credential, internal/lock, …), so
// widening it would fail compilation in every one of them. A backend opts into
// AgentStore instead; the compile-time assertions at the bottom of this file
// are what pin that both real backends satisfy it.
package state

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"
)

// Agent is one registered LEVEE agent, as the master sees it. It is the
// persisted twin of agent.AgentInfo (internal/agent/registry.go): same fields,
// plain string status rather than the typed constant, because the row is read
// by a process that does not import the agent package.
type Agent struct {
	// ID is the agent's unique identifier (typically a UUID). It is the
	// primary key: re-registering the same ID updates the record instead of
	// creating a second one.
	ID string

	// Address is the host:port the agent listens on for task dispatch.
	// Supplied at registration.
	Address string

	// Capabilities are the module names the agent can execute (e.g.
	// "shell", "file", "pkg"). Stored as a JSON array, so a capability
	// containing a comma survives the round trip — a delimiter-joined column
	// would not.
	Capabilities []string

	// Status is the lifecycle state. The vocabulary is owned by the caller
	// and NOT constrained here: this layer has never validated a status
	// string (runs.status, targets.status and cluster_nodes.status are all
	// free text) and internal/grpc's AgentService already refuses anything
	// outside the registry's own set (agent.AgentStatusValues):
	// "registered" | "idle" | "busy" | "offline".
	Status string

	// LastHeartbeat is the instant of the most recent heartbeat. The zero
	// value means "no heartbeat has ever arrived" and is stored as SQL NULL,
	// which is what keeps a never-been-seen agent from reading as if it
	// heartbeated at the epoch.
	LastHeartbeat time.Time

	// RegisteredAt is when this agent first registered. UpsertAgent keeps the
	// ORIGINAL value across a re-register: an agent that restarts (or a
	// master that restarts) must not look younger than it is.
	RegisteredAt time.Time

	// ActiveTasks, CompletedTasks and FailedTasks are the agent's own load
	// counters as last reported. Heartbeat-reported.
	ActiveTasks    int
	CompletedTasks int64
	FailedTasks    int64

	// MaxConcurrent is the agent's configured concurrency limit; the
	// scheduler uses it to compute spare capacity.
	MaxConcurrent int
}

// AgentStore persists the agent registry. Both backends implement it —
// SQLite for a single-node deployment (so the daemon and the CLI in another
// process still share the registry) and PostgreSQL for cluster mode.
//
// It follows the repository-wide Store conventions:
//   - Get* returns (nil, nil) when the row does not exist;
//   - List* returns a deterministic order and an empty slice when empty;
//   - a mutation that matches no row reports that rather than pretending.
type AgentStore interface {
	// UpsertAgent inserts, or updates by ID. On update, RegisteredAt keeps
	// the ORIGINAL registration time (re-registering must not reset age) and
	// LastHeartbeat is left untouched (heartbeats own that column).
	UpsertAgent(ctx context.Context, a *Agent) error
	// TouchAgentHeartbeat writes heartbeat-reported fields and stamps
	// LastHeartbeat. Returns false, nil when no such agent exists.
	TouchAgentHeartbeat(ctx context.Context, id string, status string, activeTasks int, completedTasks, failedTasks int64, at time.Time) (bool, error)
	// GetAgent returns (nil, nil) when the agent does not exist.
	GetAgent(ctx context.Context, id string) (*Agent, error)
	// ListAgents returns every agent, ordered by ID ascending so two callers
	// reading the same data see the same order.
	ListAgents(ctx context.Context) ([]*Agent, error)
	// DeleteAgent returns false when nothing was deleted.
	DeleteAgent(ctx context.Context, id string) (bool, error)
}

// Compile-time assertions that the real backends satisfy the contract. Same
// form as `var _ Store = (*SQLiteStore)(nil)` in store_test.go and
// `var _ state.Store = (*TenantStore)(nil)` in internal/tenant.
var (
	_ AgentStore = (*SQLiteStore)(nil)
	_ AgentStore = (*PGStore)(nil)
)

// encodeAgentCapabilities renders the capability list as the JSON array text
// the column holds. Nil and empty both encode as '[]', never as the empty
// string and never as 'null', because the decode side has to tell "no
// capabilities" apart from "a single capability whose name is the empty
// string" — and a JSON null decoding into a nil slice is exactly the
// phantom-element shape this repository has been bitten by before (see
// decodeLabels' treatment of the empty string and '{}').
func encodeAgentCapabilities(caps []string) (string, error) {
	if len(caps) == 0 {
		return "[]", nil
	}
	b, err := json.Marshal(caps)
	if err != nil {
		return "", fmt.Errorf("state: encode agent capabilities: %w", err)
	}
	return string(b), nil
}

// decodeAgentCapabilities is encodeAgentCapabilities' inverse. Every "no
// capabilities" spelling — the empty string, '[]', 'null' — comes back as an
// empty slice, and the result is non-nil so callers can range over it without
// a nil check
// and cannot mistake it for a one-element list containing "".
func decodeAgentCapabilities(s string) ([]string, error) {
	if s == "" || s == "[]" || s == "null" {
		return []string{}, nil
	}
	var out []string
	if err := json.Unmarshal([]byte(s), &out); err != nil {
		return nil, fmt.Errorf("state: decode agent capabilities %q: %w", s, err)
	}
	if out == nil {
		out = []string{}
	}
	return out, nil
}

// agentColumns is the read projection for the agents table, in the column
// order scanAgent expects. Both backends use the same list (only the
// placeholder markers differ), so it lives here rather than being duplicated
// per dialect — the same sharing encodeLabels/decodeLabels already do between
// inventory_sqlite.go and inventory_pg.go.
const agentColumns = `id, address, capabilities, status, last_heartbeat,
		registered_at, active_tasks, completed_tasks, failed_tasks, max_concurrent`

// heartbeatArg maps a zero heartbeat timestamp onto SQL NULL. The column is
// nullable precisely so that "never" stays distinguishable from "at some
// instant", and inventing an instant here would be the persistence layer
// making up liveness it was not given.
func heartbeatArg(at time.Time) any {
	if at.IsZero() {
		return nil
	}
	return at
}

// scanAgent maps one agents row onto an Agent. Both backends select
// agentColumns in the same order, so one scanner serves both.
func scanAgent(row interface{ Scan(...any) error }) (*Agent, error) {
	a := &Agent{}
	var caps string
	var lastHeartbeat sql.NullTime
	if err := row.Scan(&a.ID, &a.Address, &caps, &a.Status, &lastHeartbeat,
		&a.RegisteredAt, &a.ActiveTasks, &a.CompletedTasks, &a.FailedTasks,
		&a.MaxConcurrent); err != nil {
		return nil, err
	}
	var derr error
	if a.Capabilities, derr = decodeAgentCapabilities(caps); derr != nil {
		return nil, derr
	}
	if lastHeartbeat.Valid {
		a.LastHeartbeat = lastHeartbeat.Time
	}
	return a, nil
}
