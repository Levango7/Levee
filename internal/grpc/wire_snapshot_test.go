package grpc

// wire_snapshot_test.go — the frontend's contract, produced by the server.
//
// The hand-written DTOs in web/src/api/index.ts mirror the gateway's JSON, and
// a mirror drifts silently: this session's ghost third column came from the
// client spelling `batchNo` while the wire says `batch_no`, and a panel that
// reads zero rows raises no error — it just renders nothing. Unit tests on
// either side cannot see that, because each side only sees its own spelling.
//
// So the SERVER writes the contract down: this test drives the real handlers
// with a seeded store and writes the exact key sets to
// web/src/api/__contract__/wire.json. Two guards then read that file:
//
//   - this test fails when the live handlers disagree with the committed
//     snapshot (drift on the Go side: regenerate with -update),
//   - web/src/api/wireContract.spec.ts fails when a TS interface declares a
//     key the wire does not have, or misses one it does (drift on the TS side).
//
// The snapshot records KEY SETS and value KINDS, not values, so it stays small
// and does not churn with every data change.
import (
	"database/sql"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/nexus/levee/internal/runstatus"
	"github.com/nexus/levee/internal/state"
)

// snapshotRelPath is where the contract lives: inside web/src so the TS guard
// can read it next to the DTOs it checks.
const snapshotRelPath = "web/src/api/__contract__/wire.json"

// dtoSnapshot is one endpoint's contribution: the keys the browser may read,
// plus the keys that must NOT appear (the PascalCase twins Go marshalling
// produces when a type has no json tags).
type dtoSnapshot struct {
	Method string              `json:"method"`
	Path   string              `json:"path"`
	Keys   []string            `json:"keys"`
	Forbid []string            `json:"forbid,omitempty"`
	Rows   map[string][]string `json:"rows,omitempty"`
	// Objects records the key set of a nested OBJECT field, for the DTOs that
	// declare one. Without it a nested field is contract-blind: the snapshot said
	// cluster-status has a "summary" key and stopped there, so
	// AssignmentSummaryDTO could declare `totalActive` while the wire sent
	// `total_active` and every guard stayed green. That is exactly the drift this
	// pair of tests exists to catch, and it slipped through one level down.
	Objects map[string][]string `json:"objects,omitempty"`
}

func seedWireFixture(t *testing.T, store state.Store) {
	t.Helper()
	ctx := t.Context()
	now := time.Date(2026, 10, 12, 10, 0, 0, 0, time.UTC)

	require.NoError(t, store.CreateRun(ctx, &state.Run{
		ID: "run-wire", WorkflowName: "gateway-sync", TemplateName: "tpl",
		Status: runstatus.StatusCompleted, Creator: "liwg",
		CreatedAt: now, UpdatedAt: now.Add(time.Minute),
	}))
	require.NoError(t, store.CreateBatch(ctx, &state.Batch{
		ID: "bat-wire", RunID: "run-wire", BatchNo: 1,
		Status: state.BatchStateCompleted, TotalHosts: 2, Succeeded: 2,
	}))
	require.NoError(t, store.CreateStep(ctx, &state.Step{
		ID: "step-wire", RunID: "run-wire", BatchID: "bat-wire",
		Host: "10.0.1.11", StepName: "apply", Status: state.StepStatusSuccess,
	}))
	require.NoError(t, store.UpsertTarget(ctx, &state.Target{
		ID: "tgt-wire", Hostname: "10.0.1.11", ChannelType: "ssh", Port: 22,
		Labels: map[string]string{"env": "prod"},
	}))
	// An audit row, because protojson omits zero-valued fields: an EMPTY list
	// response marshals to {} (this is why the first run of this test saw
	// /audit/log contribute no keys at all). Seeding one makes the envelope
	// show its real keys instead of an empty object.
	require.NoError(t, store.CreateAudit(ctx, &state.Audit{
		ID: "au-wire", RunID: "run-wire", Action: "apply",
		Actor: "liwg", Target: "10.0.1.11", Result: state.AuditResultSuccess,
		Timestamp: now,
		// The chain pair: protojson omits empty strings, so a row without them
		// would hide the prev_hash/curr_hash keys the audit view renders. In
		// production internal/audit seals every row, so they are always set.
		PrevHash: "prev-wire", CurrHash: "curr-wire",
	}))
	// A cluster node. The cluster_nodes table is PostgreSQL-only in production
	// (dbschema's DDL runs on PG), so the fixture creates the same shape on the
	// SQLite store directly — ListClusterNodes tolerates a missing table by
	// returning (nil, nil), so WITHOUT this row the endpoint answers with
	// nodes: null, arrayKeys returns nil, and the PascalCase guard below never
	// inspects a single field of the nodes array. That is exactly how the third
	// PascalCase leak survived: state.ClusterNode carried no json tags at all,
	// so GET /system/cluster-status emitted ID/Address/LastHeartbeat while
	// ClusterView read id/address/lastHeartbeat and every node card rendered
	// blank on a real PG cluster.
	if dber, ok := store.(interface{ DB() *sql.DB }); ok {
		db := dber.DB()
		_, err := db.ExecContext(ctx, `
CREATE TABLE IF NOT EXISTS cluster_nodes (
	id             TEXT PRIMARY KEY,
	address        TEXT NOT NULL,
	role           TEXT NOT NULL,
	status         TEXT NOT NULL,
	last_heartbeat DATETIME NOT NULL,
	joined_at      DATETIME NOT NULL,
	capabilities   TEXT NOT NULL DEFAULT '[]'
)`)
		require.NoError(t, err)
		_, err = db.ExecContext(ctx,
			`INSERT INTO cluster_nodes (id, address, role, status, last_heartbeat, joined_at, capabilities)
			 VALUES (?, ?, ?, ?, ?, ?, ?)`,
			"node-wire", "10.0.1.11:9090", "worker", "active", now, now, `["shell","pkg"]`)
		require.NoError(t, err)
	}
}

// keysOf returns the sorted top-level keys of a JSON object.
func keysOf(raw []byte) []string {
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil
	}
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// arrayKeys returns the sorted keys of the first object in a JSON array field,
// which is what a per-row DTO declares.
func arrayKeys(raw []byte, field string) []string {
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil
	}
	rows, ok := m[field].([]any)
	if !ok || len(rows) == 0 {
		return nil
	}
	first, ok := rows[0].(map[string]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(first))
	for k := range first {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// objectKeys returns the sorted keys of a nested JSON object field — the
// one-level-down twin of arrayKeys for payloads that embed an object rather
// than a list. A null or non-object contributes nothing, so the contract never
// records a shape no consumer can read.
func objectKeys(raw []byte, field string) []string {
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil
	}
	nested, ok := m[field].(map[string]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(nested))
	for k := range nested {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func TestWireSnapshot_MatchesContract(t *testing.T) {
	gw, srv, store := startTestGatewayFull(t, ServeGatewayConfig{})
	gw.SetStore(store)
	seedWireFixture(t, store)

	get := func(path string) []byte {
		t.Helper()
		resp := doReq(t, http.MethodGet, srv.URL+path, "")
		defer func() { _ = resp.Body.Close() }()
		require.Equal(t, http.StatusOK, resp.StatusCode, "GET %s", path)
		buf := make([]byte, 0, 4096)
		tmp := make([]byte, 4096)
		for {
			n, err := resp.Body.Read(tmp)
			buf = append(buf, tmp[:n]...)
			if err != nil {
				break
			}
		}
		return buf
	}

	// rowsOf records an array's key set only when the array is non-empty: a nil
	// slice marshals to null, and a null in the contract makes every consumer
	// that iterates it crash.
	rowsOf := func(field string, raw []byte) map[string][]string {
		keys := arrayKeys(raw, field)
		if len(keys) == 0 {
			return nil
		}
		return map[string][]string{field: keys}
	}

	batches := get("/system/batch-status?run_id=run-wire")
	trend := get("/system/change-trend?days=3")
	changes := get("/changes")
	audit := get("/audit/log")
	status := get("/system/status")
	cluster := get("/system/cluster-status")
	detail := get("/changes/run-wire")
	takeover := get("/system/takeover-events")

	snap := map[string]dtoSnapshot{
		"batch-status": {
			Method: "GET", Path: "/system/batch-status",
			Keys:   keysOf(batches),
			Rows:   rowsOf("batches", batches),
			Forbid: []string{"Batches", "CurrentBatchNo", "TotalBatches", "DoneBatches"},
		},
		"change-trend": {
			Method: "GET", Path: "/system/change-trend",
			Keys:   keysOf(trend),
			Rows:   rowsOf("buckets", trend),
			Forbid: []string{"Buckets", "Series", "Truncated"},
		},
		"changes": {
			Method: "GET", Path: "/changes",
			Keys:   keysOf(changes),
			Rows:   rowsOf("changes", changes),
			Forbid: []string{"Changes", "NextPageToken", "TotalSize"},
		},
		"change": {
			Method: "GET", Path: "/changes/{id}",
			Keys:   keysOf(detail),
			Forbid: []string{"PlanHash", "TemplateName", "CreatedAt"},
		},
		"audit-log": {
			Method: "GET", Path: "/audit/log",
			Keys:   keysOf(audit),
			Rows:   rowsOf("entries", audit),
			Forbid: []string{"Entries", "NextPageToken", "TotalSize"},
		},
		"system-status": {
			Method: "GET", Path: "/system/status",
			Keys:   keysOf(status),
			Forbid: []string{"Version", "Uptime", "ActiveRuns"},
		},
		"cluster-status": {
			Method: "GET", Path: "/system/cluster-status",
			Keys: keysOf(cluster),
			Rows: rowsOf("nodes", cluster),
			// summary is an object, not a list, and the page reads three numbers
			// out of it — the level where this contract was previously blind.
			Objects: map[string][]string{"summary": objectKeys(cluster, "summary")},
			Forbid:  []string{"Nodes", "Summary", "Backend"},
		},
		"takeover-events": {
			Method: "GET", Path: "/system/takeover-events",
			Keys:   keysOf(takeover),
			Rows:   rowsOf("events", takeover),
			Forbid: []string{"Events", "Days", "WindowFrom"},
		},
	}

	// The snapshot must be non-trivial: an endpoint that contributed nothing
	// (a route that moved, a payload that changed shape) fails here rather than
	// writing an empty contract.
	for name, s := range snap {
		require.NotEmpty(t, s.Keys, "%s contributed no keys", name)
	}

	live, err := json.MarshalIndent(snap, "", "  ")
	require.NoError(t, err)

	// The test runs with the package directory as its cwd, so the repo root is
	// found by walking up to go.mod — writing the snapshot next to the package
	// would put it in internal/grpc/web/... where the TS guard cannot see it.
	root := func() string {
		dir, err := os.Getwd()
		require.NoError(t, err)
		for {
			if _, statErr := os.Stat(filepath.Join(dir, "go.mod")); statErr == nil {
				return dir
			}
			parent := filepath.Dir(dir)
			require.NotEqual(t, parent, dir, "go.mod not found above the test working directory")
			dir = parent
		}
	}()
	path := filepath.Join(root, filepath.FromSlash(snapshotRelPath))
	if os.Getenv("UPDATE_WIRE_CONTRACT") == "1" {
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o750))
		require.NoError(t, os.WriteFile(path, append(live, '\n'), 0o600))
		t.Logf("wire contract rewritten: %s", path)
		return
	}

	committed, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read committed contract: %v\nrun with UPDATE_WIRE_CONTRACT=1 to write it", err)
	}
	require.JSONEq(t, string(committed), string(live),
		"the wire shape changed: re-run with UPDATE_WIRE_CONTRACT=1 and review the diff (a TS DTO in web/src/api/index.ts probably needs the same change)")
}

// TestWireSnapshot_NoPascalCaseLeaks is the cheap half of the same guarantee,
// kept separate so it can cover the endpoints that contribute rows to the
// snapshot as well as the ones that do not.
func TestWireSnapshot_NoPascalCaseLeaks(t *testing.T) {
	gw, srv, store := startTestGatewayFull(t, ServeGatewayConfig{})
	gw.SetStore(store)
	seedWireFixture(t, store)

	for _, path := range []string{
		"/changes", "/audit/log", "/system/status", "/system/cluster-status",
		"/system/batch-status?run_id=run-wire", "/system/change-trend",
		"/templates", "/targets",
	} {
		resp := doReq(t, http.MethodGet, srv.URL+path, "")
		body := make([]byte, 0, 8192)
		tmp := make([]byte, 8192)
		for {
			n, err := resp.Body.Read(tmp)
			body = append(body, tmp[:n]...)
			if err != nil {
				break
			}
		}
		_ = resp.Body.Close()
		require.Equal(t, http.StatusOK, resp.StatusCode, "GET %s", path)

		// A PascalCase key at any depth is a Go struct marshalled without json
		// tags — the frontend cannot read it, and it used to mean a silent zero.
		var walk func(v any)
		walk = func(v any) {
			switch t2 := v.(type) {
			case map[string]any:
				for k, child := range t2 {
					if k == "" {
						continue
					}
					first := k[0]
					if first >= 'A' && first <= 'Z' {
						t.Errorf("%s: PascalCase key %q leaked into the wire", path, k)
					}
					walk(child)
				}
			case []any:
				for _, child := range t2 {
					walk(child)
				}
			}
		}
		var doc any
		require.NoError(t, json.Unmarshal(body, &doc))
		walk(doc)
	}
	_ = strings.TrimSpace
}
