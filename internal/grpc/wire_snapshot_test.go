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
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/nexus/levee/internal/conversation"
	"github.com/nexus/levee/internal/grpc/pb"
	"github.com/nexus/levee/internal/plan"
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
	Method string   `json:"method"`
	Path   string   `json:"path"`
	Keys   []string `json:"keys,omitempty"`
	// Items records the key set of the ELEMENTS when the payload itself is an
	// array (GET /agents, GET /groups). keysOf unmarshals into a map and sees
	// nothing for a bare list, so without this field those two payloads would
	// be contract-blind while still passing the "contributed something" check
	// below if it only looked at Keys.
	Items  []string            `json:"items,omitempty"`
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

	// --- endpoints the console calls that this contract never recorded ---

	// /agents returns a bare []*pb.AgentRecord marshalled by encoding/json, so
	// omitempty drops every zero-valued field per row. Seeding them non-zero is
	// what makes the recorded key set one a live registry actually produces.
	agentStore, ok := store.(state.AgentStore)
	require.True(t, ok, "the wire fixture needs a store with the agent registry; the served path is what is under contract")
	require.NoError(t, agentStore.UpsertAgent(ctx, &state.Agent{
		ID: "agent-wire", Address: "10.0.1.11:7000",
		Capabilities: []string{"shell", "pkg"}, Status: "idle",
		LastHeartbeat: now, RegisteredAt: now,
		ActiveTasks: 1, CompletedTasks: 2, FailedTasks: 1, MaxConcurrent: 4,
	}))

	// Two groups, because parent_id and created_at_nanos are omitempty: a lone
	// root row would record a shape that drops the very keys `levee group list`
	// prints for a nested group.
	require.NoError(t, store.UpsertInventoryGroup(ctx, &state.InventoryGroup{
		ID: "grp-wire", Name: "prod", CreatedAt: now,
	}))
	require.NoError(t, store.UpsertInventoryGroup(ctx, &state.InventoryGroup{
		ID: "grp-wire-db", Name: "db", ParentID: "grp-wire",
		CreatedAt: now.Add(time.Nanosecond),
	}))

	// Traces carry their payload inside Detail, and /audit/traces decodes it into
	// target/input/output/duration. protojson omits every one of those keys when
	// the JSON is empty — a bare row would record an entry shape no recorded
	// step produces. Both rows carry the payload for the same reason: the
	// snapshot reads rows[0], and ordering is the store's business.
	traceDetail := `{"target":"host:10.0.1.11","input":{"cmd":"uptime"},"output":{"ok":"true"},"duration_ms":42,"metadata":{"batch":"1"}}`
	require.NoError(t, store.CreateTrace(ctx, &state.Trace{
		ID: "tr-wire", RunID: "run-wire", Event: "apply", Actor: "liwg",
		Detail: traceDetail, PrevHash: "prev-wire", CurrHash: "curr-wire",
		Timestamp: now,
	}))
	require.NoError(t, store.CreateTrace(ctx, &state.Trace{
		ID: "tr-wire-2", RunID: "run-wire", Event: "verify", Actor: "liwg",
		Detail: traceDetail, PrevHash: "prev-wire-2", CurrHash: "curr-wire-2",
		Timestamp: now.Add(time.Second),
	}))
}

// wireTestServices registers the three services the newly covered endpoints
// need. startTestGatewayFull serves /agents and /groups as 503 until these are
// set, and the conversation session endpoints read the engine directly rather
// than the gRPC service — so a test that only builds the default gateway would
// record a shape produced by an error path.
func wireTestServices(t *testing.T, gw *Gateway, store state.Store) {
	t.Helper()
	agentStore, ok := store.(state.AgentStore)
	require.True(t, ok, "the wire fixture needs a store with the agent registry")
	gw.SetAgents(NewAgentService(agentStore))
	gw.SetInventory(NewInventoryService(store))

	engine := conversation.NewConversationEngine(conversation.ConversationEngineConfig{})
	_, err := engine.NewSession("dev-user")
	require.NoError(t, err)
	gw.SetConversationEngine(engine)
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

// itemKeys returns the union of keys across the elements of a JSON array — the
// root-array twin of arrayKeys, for endpoints that return a bare list.
//
// Union rather than rows[0]: /agents and /groups marshal generated structs with
// encoding/json, whose omitempty drops zero-valued fields per row. A first-row
// sample would record a shape missing keys the endpoint does send as soon as
// that row happened to carry a zero.
func itemKeys(raw []byte) []string {
	var rows []any
	if err := json.Unmarshal(raw, &rows); err != nil {
		return nil
	}
	seen := make(map[string]bool)
	out := make([]string, 0, 8)
	for _, row := range rows {
		obj, ok := row.(map[string]any)
		if !ok {
			continue
		}
		for k := range obj {
			if !seen[k] {
				seen[k] = true
				out = append(out, k)
			}
		}
	}
	sort.Strings(out)
	return out
}

// nestedArrayKeys records the key set of an array that lives INSIDE a nested
// object (here: plan.batches). rowsOf only reaches top-level arrays, so an item
// shape one level down was contract-blind — the same level at which the cluster
// DTO drifted, which is why objects/ exists at all.
//
// Unlike arrayKeys, this unions across every row rather than reading rows[0].
// The difference is not cosmetic: protojson omits unset fields, so batch 0 has
// no `index` key at all, and a first-row-only sample would have recorded an item
// shape missing a field the wire does send. The top-level helper keeps its
// existing behaviour because eight other entries were snapshotted under it and
// re-driving them is a separate change with its own review.
func nestedArrayKeys(raw []byte, field, item string) []string {
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil
	}
	nested, ok := m[field].(map[string]any)
	if !ok {
		return nil
	}
	rows, ok := nested[item].([]any)
	if !ok || len(rows) == 0 {
		return nil
	}
	seen := make(map[string]bool)
	out := make([]string, 0, 8)
	for _, row := range rows {
		obj, ok := row.(map[string]any)
		if !ok {
			continue
		}
		for k := range obj {
			if !seen[k] {
				seen[k] = true
				out = append(out, k)
			}
		}
	}
	sort.Strings(out)
	return out
}

// seedPlannedRun stores a canonical two-batch plan on run-wire so the includePlan
// response shows its real shape.
//
// The two batches are the point, not decoration. protojson omits unset fields,
// and `Batch.index` is 0 for the first batch — so with a single-batch artifact
// the item shape would be recorded as {hosts, maxConcurrency} and the contract
// would claim `index` is not on the wire. This is the same reason the fixture
// above seeds one audit row instead of none: an omitted key and an empty
// endpoint look identical from the outside, and only the seeded one tells the
// truth. The shared testPlan() stays single-batch because other cases assert on
// its exact counts.
func seedPlannedRun(t *testing.T, store state.Store, runID string) {
	t.Helper()
	ctx := context.Background()
	p := &plan.Plan{
		ID: "plan-wire-1", WorkflowName: "gateway-sync",
		Batches: []plan.Batch{
			{Index: 0, Targets: []string{"10.0.1.11"}, Steps: []plan.PlanStep{{Name: "restart", Module: "svc", Action: "restart"}}, MaxConcurrency: 1},
			{Index: 1, Targets: []string{"10.0.1.12", "10.0.1.13"}, Steps: []plan.PlanStep{{Name: "verify", Module: "svc", Action: "verify"}}, MaxConcurrency: 2},
		},
		TotalTargets: 3,
		CreatedAt:    time.Now().UTC(),
	}
	raw, err := json.Marshal(p)
	require.NoError(t, err)
	run, err := store.GetRun(ctx, runID)
	require.NoError(t, err)
	run.PlanJSON = string(raw)
	run.PlanHash = plan.ComputeHash(p)
	run.UpdatedAt = time.Now().UTC()
	require.NoError(t, store.UpdateRun(ctx, run))
}

// postRest drives the gateway's REST route and returns the response body.
func postRest(t *testing.T, gw *Gateway, path, body string) []byte {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	rec := httptest.NewRecorder()
	gw.restRoute().ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code, "POST %s: %s", path, rec.Body.String())
	return rec.Body.Bytes()
}

// rollbackWireResponses drives the rollback endpoint twice — once as a
// preview, once as a commit — against a gateway whose engine is a stub, and
// returns the two bodies.
//
// The endpoint needs an engine: without one the service can only say "no
// execution engine is wired", and the host lists the console reads would never
// appear on the wire. A stub engine answers with two hosts so the shape is
// the real one.
//
// The two responses differ: protojson omits unset fields, so a preview mints
// no rollback run and carries no `rollbackRunId`, while the commit does. The
// contract records the UNION — what the browser may read from this endpoint —
// and the preview's own subset is pinned by the sibling wire test in
// rollback_dryrun_test.go.
func rollbackWireResponses(t *testing.T) (preview, commit []byte) {
	t.Helper()
	engine := &recordingEngine{
		rollbackID: "rb-wire",
		rbHosts:    []string{"10.0.1.11"},
		rbSkipped:  []string{"10.0.1.12"},
	}
	store := newTestStore(t)
	change := NewChangeService(store, engine.adapter(), nil, nil)
	gw := NewGateway(ServeGatewayConfig{Addr: "127.0.0.1:0"})
	gw.SetServices(change, nil, nil, nil, nil, nil, nil, nil)

	created, err := change.CreateChange(context.Background(), &pb.CreateChangeRequest{Label: "wire-rollback"})
	require.NoError(t, err)
	setRunStatus(t, store, created.GetId(), runstatus.StatusRolledBackPartial)

	preview = postRest(t, gw, "/changes/"+created.GetId()+"/rollback", `{"dryRun":true}`)
	commit = postRest(t, gw, "/changes/"+created.GetId()+"/rollback", `{}`)
	return preview, commit
}

// unionKeys merges key sets, sorted and de-duplicated.
func unionKeys(sets ...[]string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range sets {
		for _, k := range s {
			if !seen[k] {
				seen[k] = true
				out = append(out, k)
			}
		}
	}
	sort.Strings(out)
	return out
}

func TestWireSnapshot_MatchesContract(t *testing.T) {
	gw, srv, store := startTestGatewayFull(t, ServeGatewayConfig{})
	gw.SetStore(store)
	seedWireFixture(t, store)
	wireTestServices(t, gw, store)

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
	// The stored plan rides the SAME endpoint behind an opt-in, and the console
	// now reads it — so the contract has to record it. An object nobody snapshots
	// is a level these two guards are blind to, and that blindness is exactly what
	// produced the batchNo/batch_no drift this file exists for.
	//
	// Seeded BEFORE both fetches on purpose: with a real artifact stored, the plain
	// "change" entry below is no longer vacuous — it proves the default request
	// does not carry a plan (protojson omits the unset field), which is the
	// property that let #155 widen pb.Change without widening every caller.
	seedPlannedRun(t, store, "run-wire")
	detail := get("/changes/run-wire")
	detailPlanned := get("/changes/run-wire?includePlan=true")
	takeover := get("/system/takeover-events")
	// The rollback endpoint is a POST, and it is the one the remediation
	// wizard depends on: the console reads the two host lists out of it. It
	// was the one shape the contract could not see — the snapshot covered
	// GET endpoints only — so a key the server never sent (skippedHosts was
	// exactly that, until the dry_run fix) could not fail anything here.
	rbPreview, rbCommit := rollbackWireResponses(t)
	rbKeys := unionKeys(keysOf(rbPreview), keysOf(rbCommit))

	// The union is only honest if the two shapes really differ this way:
	// protojson drops the unset rollback run id from a preview.
	require.NotContains(t, keysOf(rbPreview), "rollbackRunId",
		"a preview mints no rollback run")
	require.Contains(t, keysOf(rbCommit), "rollbackRunId",
		"a commit names the rollback run it minted")
	agents := get("/agents")
	groups := get("/groups")
	// pageSize=1 against two seeded traces: protojson omits an empty
	// next_page_token, so a single-page request would record an envelope with
	// no token key at all — and the console's toPageResult reads that key.
	traces := get("/audit/traces?pageSize=1")
	version := get("/system/version")
	sessions := get("/conversation/sessions?user_id=dev-user")

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
		"change-with-plan": {
			Method: "GET", Path: "/changes/{id}?includePlan=true",
			Keys: keysOf(detailPlanned),
			// `plan` is a nested object and `plan.batches` its item array: the
			// level one below is where the cluster DTO drifted before, so the
			// plan's own spelling is recorded rather than inferred from proto.
			Objects: map[string][]string{
				"plan":         objectKeys(detailPlanned, "plan"),
				"plan.batches": nestedArrayKeys(detailPlanned, "plan", "batches"),
			},
			Forbid: []string{"Plan", "ChangeId", "TargetHosts", "ImpactSummary", "MaxConcurrency"},
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
			Forbid:  []string{"Nodes", "Summary", "Backend", "ClusterEnabled"},
		},
		"takeover-events": {
			Method: "GET", Path: "/system/takeover-events",
			Keys:   keysOf(takeover),
			Rows:   rowsOf("events", takeover),
			Forbid: []string{"Events", "Days", "WindowFrom"},
		},
		"rollback": {
			Method: "POST", Path: "/changes/{id}/rollback",
			Keys:   rbKeys,
			Forbid: []string{"Change", "RollbackRunId", "Success", "RolledBackHosts", "SkippedHosts", "Message"},
		},
		// The five endpoints the console calls that had no recorded shape.
		// /agents and /groups return a bare array: their keys live on the
		// elements, which is what Items exists for.
		"agents": {
			Method: "GET", Path: "/agents",
			Items: itemKeys(agents),
			Forbid: []string{"Id", "Address", "Capabilities", "Status", "LastHeartbeatUnix",
				"ActiveTasks", "CompletedTasks", "FailedTasks", "MaxConcurrent", "RegisteredAtUnix"},
		},
		"groups": {
			Method: "GET", Path: "/groups",
			Items:  itemKeys(groups),
			Forbid: []string{"Id", "Name", "ParentId", "CreatedAt", "CreatedAtNanos", "TenantId"},
		},
		"audit-traces": {
			Method: "GET", Path: "/audit/traces",
			Keys: keysOf(traces),
			Rows: rowsOf("entries", traces),
			Forbid: []string{"Entries", "NextPageToken", "TotalSize",
				"Id", "RunId", "Action", "TargetHost", "Input", "Output", "DurationMs",
				"Timestamp", "PrevHash", "CurrHash", "Actor", "Detail", "StoredDetail",
				"TenantId", "TimestampNanos"},
		},
		"system-version": {
			Method: "GET", Path: "/system/version",
			Keys:   keysOf(version),
			Forbid: []string{"Version", "GitCommit", "BuildDate", "GoVersion"},
		},
		"conversation-sessions": {
			Method: "GET", Path: "/conversation/sessions?user_id={user}",
			Keys:   keysOf(sessions),
			Rows:   rowsOf("sessions", sessions),
			Forbid: []string{"Sessions", "UserID", "AlertID", "CreatedAt", "UpdatedAt"},
		},
	}

	// The snapshot must be non-trivial: an endpoint that contributed nothing
	// (a route that moved, a payload that changed shape) fails here rather than
	// writing an empty contract.
	for name, s := range snap {
		// Array-rooted payloads contribute Items instead of Keys; everything
		// else must contribute at least one top-level key. An entry that
		// contributes nothing is a route that moved or a payload that changed
		// shape, and it must fail here rather than write an empty contract.
		require.True(t, len(s.Keys)+len(s.Items)+len(s.Rows)+len(s.Objects) > 0,
			"%s contributed nothing", name)
		// The forbid list has been decoration until now: neither this test nor
		// web/src/api/wireContract.spec.ts ever read it, so a PascalCase twin
		// could (and did) appear in a payload whose entry said it must not.
		// Checking it here is what makes the list a gate rather than a comment —
		// and it only bites when the key really is absent, so the entries below
		// are asserted against the payload that produced them.
		for _, banned := range s.Forbid {
			require.NotContains(t, s.Keys, banned,
				"%s: %q is on the wire while the entry lists it as forbidden", name, banned)
			for rowField, rowKeys := range s.Rows {
				require.NotContains(t, rowKeys, banned,
					"%s: %q is on the wire inside %s[] while the entry lists it as forbidden", name, banned, rowField)
			}
			for objField, objKeys := range s.Objects {
				require.NotContains(t, objKeys, banned,
					"%s: %q is on the wire inside %s while the entry lists it as forbidden", name, banned, objField)
			}
			require.NotContains(t, s.Items, banned,
				"%s: %q is on the wire inside an array element while the entry lists it as forbidden", name, banned)
		}
	}

	// The opt-in is the only way a plan appears. Without this, "the default
	// response has no plan key" would rest on the fixture having no stored plan
	// rather than on the flag — and the fixture now stores one, so the check has
	// something to rule out.
	require.NotContains(t, keysOf(detail), "plan",
		"GET /changes/{id} without includePlan must not carry the plan")
	require.Contains(t, keysOf(detailPlanned), "plan",
		"GET /changes/{id}?includePlan=true must carry the plan the run has stored")
	require.NotEmpty(t, objectKeys(detailPlanned, "plan"), "the plan object contributed no keys")
	require.NotEmpty(t, nestedArrayKeys(detailPlanned, "plan", "batches"),
		"plan.batches[] contributed no keys — the item shape would be contract-blind")

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
	// Without a stored plan the includePlan response has no `plan` subtree to
	// walk, so the walk below would pass while saying nothing about the plan's
	// own field spellings.
	seedPlannedRun(t, store, "run-wire")
	wireTestServices(t, gw, store)

	// A PascalCase key at any depth is a Go struct marshalled without json
	// tags — the frontend cannot read it, and it used to mean a silent zero.
	var walk func(where string, v any)
	walk = func(where string, v any) {
		switch t2 := v.(type) {
		case map[string]any:
			for k, child := range t2 {
				if k == "" {
					continue
				}
				first := k[0]
				if first >= 'A' && first <= 'Z' {
					t.Errorf("%s: PascalCase key %q leaked into the wire", where, k)
				}
				walk(where, child)
			}
		case []any:
			for _, child := range t2 {
				walk(where, child)
			}
		}
	}
	for _, path := range []string{
		"/changes", "/audit/log", "/system/status", "/system/cluster-status",
		"/system/batch-status?run_id=run-wire", "/system/change-trend",
		"/templates", "/targets",
		"/changes/run-wire", "/changes/run-wire?includePlan=true",
		// The endpoints the console calls, including the two bare arrays whose
		// element spellings would otherwise be walked by nothing at all.
		"/agents", "/groups", "/audit/traces", "/system/version",
		"/conversation/sessions?user_id=dev-user",
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

		var doc any
		require.NoError(t, json.Unmarshal(body, &doc))
		walk(path, doc)
	}

	// The rollback POST is covered by the same walk. Its keys are the ones
	// the remediation wizard spells, and the console reads them out of the
	// preview — so a PascalCase twin there would blank the evidence panel.
	rbPreview, rbCommit := rollbackWireResponses(t)
	for name, body := range map[string][]byte{"rollback preview": rbPreview, "rollback commit": rbCommit} {
		var doc any
		require.NoError(t, json.Unmarshal(body, &doc))
		walk(name, doc)
	}
	_ = strings.TrimSpace
}
