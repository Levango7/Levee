// agents_test.go — AgentStore persistence tests. SQLite paths run everywhere
// against the real store (no mocks: the point of this registry is that a
// second process reads what a first one wrote, so only the database can prove
// it). PG paths are env-gated on LEVEE_PG_TEST_DSN like the rest of the PG
// suite (see pgstore_test.go).

package state

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newAgent returns a registration-shaped record: what the gRPC service writes
// before any heartbeat has arrived.
func newAgent(id string, registeredAt time.Time) *Agent {
	return &Agent{
		ID:            id,
		Address:       "10.0.0.1:9000",
		Capabilities:  []string{"shell", "file"},
		Status:        "registered",
		RegisteredAt:  registeredAt,
		MaxConcurrent: 4,
	}
}

func TestAgentCRUD_RoundTrip(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	// Truncated to the second: the DATETIME column keeps second precision, and
	// the neighbours in this package do the same (see store_extra_test.go).
	registered := time.Now().UTC().Truncate(time.Second)

	// --- upsert → get: every field survives.
	require.NoError(t, store.UpsertAgent(ctx, newAgent("agent-1", registered)))
	got, err := store.GetAgent(ctx, "agent-1")
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, "agent-1", got.ID)
	assert.Equal(t, "10.0.0.1:9000", got.Address)
	assert.Equal(t, []string{"shell", "file"}, got.Capabilities)
	assert.Equal(t, "registered", got.Status)
	assert.Equal(t, registered, got.RegisteredAt)
	assert.Equal(t, 0, got.ActiveTasks)
	assert.Equal(t, int64(0), got.CompletedTasks)
	assert.Equal(t, int64(0), got.FailedTasks)
	assert.Equal(t, 4, got.MaxConcurrent)
	assert.True(t, got.LastHeartbeat.IsZero(),
		"a never-heartbeated agent must read back the zero value, not an invented instant")

	// --- the (nil, nil) rule for a missing id, repo-wide convention for Get*.
	missing, err := store.GetAgent(ctx, "no-such-agent")
	require.NoError(t, err)
	assert.Nil(t, missing, "Get* returns (nil, nil) when the row does not exist")

	// --- heartbeat stamps liveness and the agent's own counters.
	hb := registered.Add(30 * time.Second)
	known, err := store.TouchAgentHeartbeat(ctx, "agent-1", "busy", 3, 17, 2, hb)
	require.NoError(t, err)
	assert.True(t, known, "the agent exists, so the heartbeat must land")

	got, err = store.GetAgent(ctx, "agent-1")
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, "busy", got.Status)
	assert.Equal(t, 3, got.ActiveTasks)
	assert.Equal(t, int64(17), got.CompletedTasks)
	assert.Equal(t, int64(2), got.FailedTasks)
	assert.Equal(t, hb, got.LastHeartbeat)

	// --- a heartbeat for an unknown id is false, nil: not an error, because
	// the caller's answer is "register again".
	known, err = store.TouchAgentHeartbeat(ctx, "ghost-agent", "idle", 0, 0, 0, time.Now().UTC())
	require.NoError(t, err)
	assert.False(t, known)

	// --- re-registering must not reset the age and must not move liveness.
	// Both sabotage values are deliberately wrong so a write of either is a
	// visible failure rather than an accident of equal values.
	rereg := newAgent("agent-1", registered.Add(72*time.Hour))
	rereg.Address = "10.0.0.9:9100"
	rereg.Capabilities = []string{"pkg"}
	rereg.Status = "idle"
	rereg.MaxConcurrent = 8
	rereg.ActiveTasks = 1
	rereg.LastHeartbeat = registered.Add(-time.Hour)
	require.NoError(t, store.UpsertAgent(ctx, rereg))

	got, err = store.GetAgent(ctx, "agent-1")
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, registered, got.RegisteredAt,
		"registered_at keeps the ORIGINAL registration time across a re-register")
	assert.Equal(t, hb, got.LastHeartbeat,
		"last_heartbeat is owned by heartbeats; an upsert must not move it")
	// Everything the registration actually declares does update.
	assert.Equal(t, "10.0.0.9:9100", got.Address)
	assert.Equal(t, []string{"pkg"}, got.Capabilities)
	assert.Equal(t, "idle", got.Status)
	assert.Equal(t, 1, got.ActiveTasks)
	assert.Equal(t, 8, got.MaxConcurrent)

	// --- list: deterministic order by ID ascending, fed in unsorted.
	require.NoError(t, store.UpsertAgent(ctx, newAgent("c-agent", registered)))
	require.NoError(t, store.UpsertAgent(ctx, newAgent("a-agent", registered)))
	require.NoError(t, store.UpsertAgent(ctx, newAgent("b-agent", registered)))

	list, err := store.ListAgents(ctx)
	require.NoError(t, err)
	require.Len(t, list, 4)
	assert.Equal(t, []string{"a-agent", "agent-1", "b-agent", "c-agent"},
		[]string{list[0].ID, list[1].ID, list[2].ID, list[3].ID},
		"ListAgents must be ordered by ID ascending so two readers agree")

	// --- delete: true for a row that was there, false for one that was not.
	removed, err := store.DeleteAgent(ctx, "c-agent")
	require.NoError(t, err)
	assert.True(t, removed)

	removed, err = store.DeleteAgent(ctx, "c-agent")
	require.NoError(t, err)
	assert.False(t, removed, "deleting nothing must report false, not an error")

	gone, err := store.GetAgent(ctx, "c-agent")
	require.NoError(t, err)
	assert.Nil(t, gone)

	list, err = store.ListAgents(ctx)
	require.NoError(t, err)
	assert.Len(t, list, 3)
}

// TestAgentUpsert_NilAndEmptyID pins the argument guards. Both would otherwise
// reach the database as a confusing failure: a nil *Agent panics on the first
// field read, and an empty ID would land a row nothing can ever read back by
// name while ListAgents shows a blank.
func TestAgentUpsert_NilAndEmptyID(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	require.Error(t, store.UpsertAgent(ctx, nil))

	err := store.UpsertAgent(ctx, &Agent{Address: "10.0.0.1:9000"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "empty id")
}

// TestAgentUpsert_ZeroRegisteredAtIsStamped documents the insert-side default:
// a caller that brought no timestamp still gets a record with an age, exactly
// as UpsertTarget stamps a zero CreatedAt.
func TestAgentUpsert_ZeroRegisteredAtIsStamped(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	before := time.Now().UTC().Truncate(time.Second)

	a := newAgent("stamp-1", time.Time{})
	require.NoError(t, store.UpsertAgent(ctx, a))
	require.False(t, a.RegisteredAt.IsZero(), "the struct is stamped for the caller, as CreatedAt is")

	got, err := store.GetAgent(ctx, "stamp-1")
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.False(t, got.RegisteredAt.IsZero())
	assert.True(t, !got.RegisteredAt.Before(before),
		"registered_at = %v, want >= %v", got.RegisteredAt, before)
}

// TestAgentCapabilities_RoundTripEdges is the shape test: Capabilities is a
// JSON array TEXT rather than a joined string, so neither the empty case nor a
// value containing the delimiter may come back wrong. The empty case is the
// one with history — the empty and 'null' spellings decoding into a
// one-element slice containing "" (or into nil, which a caller ranges over
// differently) is the NULL-vs-empty-string class of bug this repository has
// pinned before (decodeLabels,
// runs.plan_json, cluster_nodes.capabilities).
func TestAgentCapabilities_RoundTripEdges(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	registered := time.Now().UTC().Truncate(time.Second)

	cases := []struct {
		name string
		in   []string
		want []string
	}{
		{"nil", nil, []string{}},
		{"empty", []string{}, []string{}},
		{"one", []string{"shell"}, []string{"shell"}},
		{"several", []string{"shell", "file", "pkg", "service"},
			[]string{"shell", "file", "pkg", "service"}},
		// The reason for JSON instead of a joined column: none of these may be
		// split into extra phantom capabilities on the way back.
		{"comma inside a name", []string{"shell,privileged"}, []string{"shell,privileged"}},
		{"comma plus ordinary names", []string{"file", "a,b,c", "pkg"},
			[]string{"file", "a,b,c", "pkg"}},
		{"quote and bracket", []string{`we"ird`, "a[b]"}, []string{`we"ird`, "a[b]"}},
		{"empty-string element survives as data", []string{""}, []string{""}},
		{"unicode", []string{"部署", "shell"}, []string{"部署", "shell"}},
	}

	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			id := "caps-" + tc.name
			a := newAgent(id, registered.Add(time.Duration(i)*time.Second))
			a.Capabilities = tc.in
			require.NoError(t, store.UpsertAgent(ctx, a))

			got, err := store.GetAgent(ctx, id)
			require.NoError(t, err)
			require.NotNil(t, got)
			assert.Equal(t, tc.want, got.Capabilities, "capabilities round trip")
			// Explicitly: an empty registry must never read as [""] .
			if len(tc.want) == 0 {
				assert.Empty(t, got.Capabilities)
				assert.NotNil(t, got.Capabilities,
					"the empty case decodes to an empty non-nil slice, so a range and a len agree")
			}
		})
	}

	// Re-registering with no capabilities must clear the stored list rather
	// than leaving the previous one behind.
	require.NoError(t, store.UpsertAgent(ctx, &Agent{
		ID: "caps-clear", Address: "10.0.0.5:9000",
		Capabilities: []string{"shell", "file"}, Status: "idle", RegisteredAt: registered,
	}))
	require.NoError(t, store.UpsertAgent(ctx, &Agent{
		ID: "caps-clear", Address: "10.0.0.5:9000",
		Capabilities: nil, Status: "idle", RegisteredAt: registered,
	}))
	got, err := store.GetAgent(ctx, "caps-clear")
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, []string{}, got.Capabilities, "a re-register with none clears the list")
}

// TestAgentHeartbeat_ZeroTimestampStaysUnknown pins that a heartbeat arriving
// with no timestamp does not fabricate liveness: last_heartbeat stays NULL, so
// a reader still sees "never heartbeated" while the counters are recorded.
func TestAgentHeartbeat_ZeroTimestampStaysUnknown(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	require.NoError(t, store.UpsertAgent(ctx, newAgent("hb-zero", time.Now().UTC().Truncate(time.Second))))
	known, err := store.TouchAgentHeartbeat(ctx, "hb-zero", "draining", 1, 4, 0, time.Time{})
	require.NoError(t, err)
	require.True(t, known)

	got, err := store.GetAgent(ctx, "hb-zero")
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.True(t, got.LastHeartbeat.IsZero())
	assert.Equal(t, "draining", got.Status)
	assert.Equal(t, 1, got.ActiveTasks)
	assert.Equal(t, int64(4), got.CompletedTasks)
}

// TestAgentStore_ConcreteTypesSatisfyContract is the compile-time half that
// lives as a test so the failure names the contract. agents.go asserts the same
// thing at package scope; this variant also proves a *SQLiteStore and a
// *PGStore value can be handed to anything taking an AgentStore, which is
// exactly what the gRPC service does.
func TestAgentStore_ConcreteTypesSatisfyContract(t *testing.T) {
	var stores = []AgentStore{(*SQLiteStore)(nil), (*PGStore)(nil)}
	require.Len(t, stores, 2)
}

// TestPG_AgentsRoundTrip is the PostgreSQL twin of TestAgentCRUD_RoundTrip,
// gated exactly like the rest of the PG suite (skipped unless
// LEVEE_PG_TEST_DSN is set).
//
// It deletes only its own rows rather than truncating: `agents` is a table
// several test binaries can be writing to the shared database while this one
// runs, which is why newPGTestStore truncates a fixed list and no more.
func TestPG_AgentsRoundTrip(t *testing.T) {
	store, cleanup := newPGTestStore(t)
	defer cleanup()
	ctx := context.Background()

	ids := []string{"pg-agent-a", "pg-agent-b", "pg-agent-c", "pg-agent-ghost"}
	for _, id := range ids {
		_, err := store.DB().ExecContext(ctx, `DELETE FROM agents WHERE id = $1`, id)
		require.NoError(t, err, "clear stale fixture row %s", id)
	}

	registered := time.Now().UTC().Truncate(time.Microsecond)
	require.NoError(t, store.UpsertAgent(ctx, newAgent("pg-agent-a", registered)))

	got, err := store.GetAgent(ctx, "pg-agent-a")
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, "10.0.0.1:9000", got.Address)
	assert.Equal(t, []string{"shell", "file"}, got.Capabilities)
	assert.Equal(t, "registered", got.Status)
	requireTimeEqual(t, "registered_at", registered, got.RegisteredAt)
	assert.Equal(t, 4, got.MaxConcurrent)
	assert.True(t, got.LastHeartbeat.IsZero(),
		"a NULL last_heartbeat must read back as the zero value on PostgreSQL too")

	missing, err := store.GetAgent(ctx, "pg-agent-ghost")
	require.NoError(t, err)
	assert.Nil(t, missing)

	// Capabilities edge cases on the real server: the column is TEXT holding a
	// JSON array, so the comma case is what proves nothing joined it.
	for _, caps := range [][]string{nil, {}, {"shell"}, {"a,b", "c"}} {
		a := newAgent("pg-agent-b", registered)
		a.Capabilities = caps
		require.NoError(t, store.UpsertAgent(ctx, a))
		back, err := store.GetAgent(ctx, "pg-agent-b")
		require.NoError(t, err)
		require.NotNil(t, back)
		want := caps
		if len(caps) == 0 {
			want = []string{}
		}
		assert.Equal(t, want, back.Capabilities, "capabilities %v", caps)
	}

	// Heartbeat: liveness stamped, unknown id reported as false.
	hb := registered.Add(20 * time.Second)
	known, err := store.TouchAgentHeartbeat(ctx, "pg-agent-a", "busy", 2, 9, 1, hb)
	require.NoError(t, err)
	require.True(t, known)
	got, err = store.GetAgent(ctx, "pg-agent-a")
	require.NoError(t, err)
	require.NotNil(t, got)
	requireTimeEqual(t, "last_heartbeat", hb, got.LastHeartbeat)
	assert.Equal(t, "busy", got.Status)
	assert.Equal(t, 2, got.ActiveTasks)
	assert.Equal(t, int64(9), got.CompletedTasks)
	assert.Equal(t, int64(1), got.FailedTasks)

	known, err = store.TouchAgentHeartbeat(ctx, "pg-agent-ghost", "idle", 0, 0, 0, hb)
	require.NoError(t, err)
	assert.False(t, known)

	// Re-register: age preserved, heartbeat untouched, declared fields moved.
	rereg := newAgent("pg-agent-a", registered.Add(24*time.Hour))
	rereg.Address = "10.0.0.8:9200"
	rereg.Capabilities = []string{"pkg"}
	rereg.MaxConcurrent = 2
	rereg.LastHeartbeat = registered.Add(-time.Hour)
	require.NoError(t, store.UpsertAgent(ctx, rereg))
	got, err = store.GetAgent(ctx, "pg-agent-a")
	require.NoError(t, err)
	require.NotNil(t, got)
	requireTimeEqual(t, "registered_at after re-register", registered, got.RegisteredAt)
	requireTimeEqual(t, "last_heartbeat after upsert", hb, got.LastHeartbeat)
	assert.Equal(t, "10.0.0.8:9200", got.Address)
	assert.Equal(t, []string{"pkg"}, got.Capabilities)

	// List ordering over the fixture ids, whatever else the table holds.
	require.NoError(t, store.UpsertAgent(ctx, newAgent("pg-agent-c", registered)))
	require.NoError(t, store.UpsertAgent(ctx, newAgent("pg-agent-b", registered)))
	list, err := store.ListAgents(ctx)
	require.NoError(t, err)
	var mine []string
	for _, a := range list {
		if a.ID == "pg-agent-a" || a.ID == "pg-agent-b" || a.ID == "pg-agent-c" {
			mine = append(mine, a.ID)
		}
	}
	assert.Equal(t, []string{"pg-agent-a", "pg-agent-b", "pg-agent-c"}, mine,
		"ListAgents orders by id ascending")

	// Delete: true, then false.
	removed, err := store.DeleteAgent(ctx, "pg-agent-c")
	require.NoError(t, err)
	assert.True(t, removed)
	removed, err = store.DeleteAgent(ctx, "pg-agent-c")
	require.NoError(t, err)
	assert.False(t, removed)

	for _, id := range ids {
		_, err := store.DB().ExecContext(ctx, `DELETE FROM agents WHERE id = $1`, id)
		require.NoError(t, err, "clean fixture row %s", id)
	}
}
