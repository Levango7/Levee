package grpc

// agent_service_test.go — the registry RPC's own behaviour, on a store fake that
// keeps the same rules the real stores keep (registered_at survives a
// re-register, heartbeats do not touch identity columns).
//
// The point of these cases is not "the handler returns what the fake returned".
// Each one pins a decision that an operator or an agent would otherwise have to
// rediscover by reading code: which statuses may be written, what an unknown
// agent means, when a removal is refused, and that this service is never
// reachable without a subject.

import (
	"context"
	"errors"
	"sort"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/nexus/levee/internal/agent"
	"github.com/nexus/levee/internal/grpc/pb"
	"github.com/nexus/levee/internal/state"
)

// fakeAgentStore is an in-memory AgentStore that enforces the column-ownership
// rules the persistent stores are specified with, so the service cannot pass a
// test here and then lose data on the real store.
type fakeAgentStore struct {
	rows        map[string]*state.Agent
	upsertCalls int
	touchCalls  int
	deleteCalls int
	failOn      error
}

func newFakeAgentStore() *fakeAgentStore {
	return &fakeAgentStore{rows: map[string]*state.Agent{}}
}

func (f *fakeAgentStore) UpsertAgent(_ context.Context, a *state.Agent) error {
	f.upsertCalls++
	if f.failOn != nil {
		return f.failOn
	}
	cp := *a
	cp.Capabilities = append([]string(nil), a.Capabilities...)
	if prev, ok := f.rows[a.ID]; ok {
		// Contract: re-register updates identity/config but never the age
		// (registered_at) and never liveness (last_heartbeat).
		cp.RegisteredAt = prev.RegisteredAt
		cp.LastHeartbeat = prev.LastHeartbeat
	}
	f.rows[a.ID] = &cp
	return nil
}

func (f *fakeAgentStore) TouchAgentHeartbeat(_ context.Context, id, status string,
	active int, completed, failed int64, at time.Time) (bool, error) {
	f.touchCalls++
	if f.failOn != nil {
		return false, f.failOn
	}
	row, ok := f.rows[id]
	if !ok {
		return false, nil
	}
	row.Status = status
	row.ActiveTasks = active
	row.CompletedTasks = completed
	row.FailedTasks = failed
	row.LastHeartbeat = at
	return true, nil
}

func (f *fakeAgentStore) GetAgent(_ context.Context, id string) (*state.Agent, error) {
	row, ok := f.rows[id]
	if !ok {
		return nil, nil
	}
	cp := *row
	cp.Capabilities = append([]string(nil), row.Capabilities...)
	return &cp, nil
}

func (f *fakeAgentStore) ListAgents(_ context.Context) ([]*state.Agent, error) {
	out := make([]*state.Agent, 0, len(f.rows))
	for _, r := range f.rows {
		cp := *r
		cp.Capabilities = append([]string(nil), r.Capabilities...)
		out = append(out, &cp)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func (f *fakeAgentStore) DeleteAgent(_ context.Context, id string) (bool, error) {
	f.deleteCalls++
	if _, ok := f.rows[id]; !ok {
		return false, nil
	}
	delete(f.rows, id)
	return true, nil
}

var _ state.AgentStore = (*fakeAgentStore)(nil)

func newTestAgentService() (*AgentService, *fakeAgentStore) {
	st := newFakeAgentStore()
	svc := NewAgentService(st)
	// fixed clock, so the staleness arithmetic below is checked rather than slept through
	svc.now = func() time.Time { return time.Unix(1700000000, 0) }
	return svc, st
}

func mustRegister(t *testing.T, svc *AgentService, id, addr string, caps []string, max int) *pb.AgentRecord {
	t.Helper()
	rec, err := svc.RegisterAgent(context.Background(), &pb.RegisterAgentRequest{
		Id: id, Address: addr, Capabilities: caps, MaxConcurrent: int32(max),
	})
	require.NoError(t, err)
	return rec
}

// --- registration -----------------------------------------------------------

func TestAgentServiceRegisterStoresAsRegistered(t *testing.T) {
	svc, st := newTestAgentService()

	rec := mustRegister(t, svc, "web-1", "10.0.0.5:9099", []string{"shell", "file"}, 4)
	assert.Equal(t, "web-1", rec.GetId())
	assert.Equal(t, "10.0.0.5:9099", rec.GetAddress())
	assert.Equal(t, []string{"shell", "file"}, rec.GetCapabilities())
	assert.Equal(t, string(agent.StatusRegistered), rec.GetStatus(),
		"a record nobody has ever heard from must not look idle and get work")
	assert.Equal(t, int64(1700000000), rec.GetRegisteredAtUnix())
	assert.Zero(t, rec.GetLastHeartbeatUnix(), "zero means never, and must not be faked")

	stored, err := st.GetAgent(context.Background(), "web-1")
	require.NoError(t, err)
	require.NotNil(t, stored)
	assert.Equal(t, 4, stored.MaxConcurrent)
}

func TestAgentServiceReregisterKeepsAgeAndLiveness(t *testing.T) {
	svc, st := newTestAgentService()
	mustRegister(t, svc, "web-1", "10.0.0.5:9099", []string{"shell"}, 2)

	first := st.rows["web-1"]
	first.RegisteredAt = time.Unix(1600000000, 0)
	first.LastHeartbeat = time.Unix(1600000500, 0)

	rec := mustRegister(t, svc, "web-1", "10.0.0.9:9099", []string{"shell", "pkg"}, 8)
	assert.Equal(t, int64(1600000000), rec.GetRegisteredAtUnix(),
		"re-register is an update, not a rebirth: age must survive")
	assert.Equal(t, int64(1600000500), rec.GetLastHeartbeatUnix(),
		"heartbeats own that column; a register must not clear it")
	assert.Equal(t, "10.0.0.9:9099", rec.GetAddress())
	assert.Equal(t, []string{"shell", "pkg"}, rec.GetCapabilities())
}

func TestAgentServiceRegisterValidation(t *testing.T) {
	cases := []struct {
		name    string
		req     *pb.RegisterAgentRequest
		wantSub string
	}{
		{"missing id", &pb.RegisterAgentRequest{Address: "h:1"}, "agent id is required"},
		{"blank id", &pb.RegisterAgentRequest{Id: "   ", Address: "h:1"}, "agent id is required"},
		{"missing address", &pb.RegisterAgentRequest{Id: "a"}, "address is required"},
		{"address without port", &pb.RegisterAgentRequest{Id: "a", Address: "just-a-host"}, "not host:port"},
		{"empty host", &pb.RegisterAgentRequest{Id: "a", Address: ":9099"}, "empty host"},
		{"empty port", &pb.RegisterAgentRequest{Id: "a", Address: "h:"}, "empty port"},
		{"empty capability entry", &pb.RegisterAgentRequest{Id: "a", Address: "h:1", Capabilities: []string{"shell", "  "}},
			"must not be empty"},
		{"negative ceiling", &pb.RegisterAgentRequest{Id: "a", Address: "h:1", MaxConcurrent: -1}, "must not be negative"},
	}
	svc, _ := newTestAgentService()
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := svc.RegisterAgent(context.Background(), c.req)
			require.Error(t, err)
			assert.Equal(t, codes.InvalidArgument, status.Code(err), c.name)
			assert.Contains(t, err.Error(), c.wantSub)
		})
	}
}

func TestAgentServiceRegisterDeduplicatesCapabilities(t *testing.T) {
	svc, st := newTestAgentService()
	rec := mustRegister(t, svc, "web-1", "h:1", []string{"shell", "shell", " file "}, 1)
	assert.Equal(t, []string{"shell", "file"}, rec.GetCapabilities(),
		"a duplicated capability makes one agent look like two to the scheduler")
	assert.Len(t, st.rows["web-1"].Capabilities, 2)
}

// --- heartbeat --------------------------------------------------------------

func TestAgentServiceHeartbeatDerivesStatus(t *testing.T) {
	svc, st := newTestAgentService()
	mustRegister(t, svc, "web-1", "h:1", []string{"shell"}, 2)

	_, err := svc.AgentHeartbeat(context.Background(), &pb.AgentHeartbeatRequest{Id: "web-1", ActiveTasks: 1})
	require.NoError(t, err)
	assert.Equal(t, string(agent.StatusIdle), st.rows["web-1"].Status,
		"1 of 2 is spare capacity, so not busy")

	resp, err := svc.AgentHeartbeat(context.Background(), &pb.AgentHeartbeatRequest{Id: "web-1", ActiveTasks: 2})
	require.NoError(t, err)
	assert.Equal(t, string(agent.StatusBusy), st.rows["web-1"].Status)
	assert.Equal(t, resp.GetStatus(), string(agent.StatusBusy))
}

func TestAgentServiceHeartbeatHonoursClientTimestamp(t *testing.T) {
	svc, st := newTestAgentService()
	mustRegister(t, svc, "web-1", "h:1", nil, 1)

	_, err := svc.AgentHeartbeat(context.Background(), &pb.AgentHeartbeatRequest{
		Id: "web-1", ClientTimestampUnix: 1600000900,
	})
	require.NoError(t, err)
	assert.Equal(t, int64(1600000900), st.rows["web-1"].LastHeartbeat.Unix(),
		"the agent's own clock is what staleness is measured from")
}

func TestAgentServiceHeartbeatRejectsUnknownStatus(t *testing.T) {
	svc, st := newTestAgentService()
	mustRegister(t, svc, "web-1", "h:1", nil, 1)

	_, err := svc.AgentHeartbeat(context.Background(), &pb.AgentHeartbeatRequest{
		Id: "web-1", Status: "healthy",
	})
	require.Error(t, err)
	assert.Equal(t, codes.InvalidArgument, status.Code(err))
	assert.Contains(t, err.Error(), "idle")
	assert.NotEqual(t, "healthy", st.rows["web-1"].Status,
		"an accepted-never status must not be stored, or the agent reads healthy forever")
}

func TestAgentServiceHeartbeatForUnknownAgentIsNotFound(t *testing.T) {
	svc, st := newTestAgentService()

	_, err := svc.AgentHeartbeat(context.Background(), &pb.AgentHeartbeatRequest{Id: "ghost"})
	require.Error(t, err)
	assert.Equal(t, codes.NotFound, status.Code(err))
	assert.Contains(t, err.Error(), "RegisterAgent again")
	assert.Zero(t, st.touchCalls, "the read-before-write must not persist anything")
}

func TestAgentServiceHeartbeatReportsStaleWindow(t *testing.T) {
	svc, _ := newTestAgentService()
	mustRegister(t, svc, "web-1", "h:1", nil, 1)

	resp, err := svc.AgentHeartbeat(context.Background(), &pb.AgentHeartbeatRequest{Id: "web-1"})
	require.NoError(t, err)
	want := time.Unix(1700000000, 0).Add(agentStaleWindow).Unix()
	assert.Equal(t, want, resp.GetStaleAfterUnix())
	assert.Equal(t, int64(agentStaleWindow.Seconds()), resp.GetStaleAfterUnix()-1700000000,
		"the window is derived from the agent's own interval and miss threshold")
}

// --- read side --------------------------------------------------------------

func TestAgentServiceListAndGet(t *testing.T) {
	svc, _ := newTestAgentService()
	mustRegister(t, svc, "web-2", "h:2", []string{"shell"}, 1)
	mustRegister(t, svc, "web-1", "h:1", []string{"pkg"}, 1)
	_, err := svc.AgentHeartbeat(context.Background(), &pb.AgentHeartbeatRequest{Id: "web-1", ActiveTasks: 1})
	require.NoError(t, err)

	resp, err := svc.ListAgents(context.Background(), &pb.ListAgentsRequest{})
	require.NoError(t, err)
	require.Len(t, resp.GetAgents(), 2)
	assert.Equal(t, "web-1", resp.GetAgents()[0].GetId(), "list order is by id, so output is diffable")

	caps, err := svc.ListAgents(context.Background(), &pb.ListAgentsRequest{Capability: "pkg"})
	require.NoError(t, err)
	require.Len(t, caps.GetAgents(), 1)
	assert.Equal(t, "web-1", caps.GetAgents()[0].GetId())

	busy, err := svc.ListAgents(context.Background(), &pb.ListAgentsRequest{Status: string(agent.StatusBusy)})
	require.NoError(t, err)
	require.Len(t, busy.GetAgents(), 1, "web-1 reported 1 active task against its ceiling of 1 → busy")
	assert.Equal(t, "web-1", busy.GetAgents()[0].GetId())

	// The same filter on a state nobody is in must come back empty rather than
	// ignoring the filter — "no agents match" and "filter dropped" look alike
	// on screen and mean opposite things to an operator.
	idle, err := svc.ListAgents(context.Background(), &pb.ListAgentsRequest{Status: string(agent.StatusIdle)})
	require.NoError(t, err)
	assert.Len(t, idle.GetAgents(), 0)

	one, err := svc.GetAgent(context.Background(), &pb.GetAgentRequest{Id: "web-2"})
	require.NoError(t, err)
	assert.Equal(t, "h:2", one.GetAddress())

	_, err = svc.GetAgent(context.Background(), &pb.GetAgentRequest{Id: "nope"})
	require.Error(t, err)
	assert.Equal(t, codes.NotFound, status.Code(err))
}

func TestAgentServiceListRejectsNonsenseFilter(t *testing.T) {
	svc, _ := newTestAgentService()
	_, err := svc.ListAgents(context.Background(), &pb.ListAgentsRequest{Status: "not-a-status"})
	require.Error(t, err)
	assert.Equal(t, codes.InvalidArgument, status.Code(err))
	assert.Contains(t, err.Error(), "unknown status")
}

// --- removal ---------------------------------------------------------------

func TestAgentServiceRemoveRefusesInFlightWithoutForce(t *testing.T) {
	svc, st := newTestAgentService()
	mustRegister(t, svc, "web-1", "h:1", nil, 2)
	_, err := svc.AgentHeartbeat(context.Background(), &pb.AgentHeartbeatRequest{Id: "web-1", ActiveTasks: 1})
	require.NoError(t, err)

	resp, err := svc.RemoveAgent(context.Background(), &pb.RemoveAgentRequest{Id: "web-1"})
	require.NoError(t, err, "a refusal is an outcome, not a server fault")
	assert.False(t, resp.GetRemoved())
	assert.Contains(t, resp.GetRefusal(), "in-flight")
	assert.Contains(t, resp.GetRefusal(), "web-1")
	require.NotNil(t, st.rows["web-1"], "refusal must leave the only evidence of the running work in place")

	forced, err := svc.RemoveAgent(context.Background(), &pb.RemoveAgentRequest{Id: "web-1", Force: true})
	require.NoError(t, err)
	assert.True(t, forced.GetRemoved())
	assert.Empty(t, st.rows)
}

func TestAgentServiceRemoveAbsentIsRefusalNotError(t *testing.T) {
	svc, _ := newTestAgentService()
	resp, err := svc.RemoveAgent(context.Background(), &pb.RemoveAgentRequest{Id: "ghost"})
	require.NoError(t, err, "removing something already gone is not a second failure")
	assert.False(t, resp.GetRemoved())
	assert.Contains(t, resp.GetRefusal(), "not registered")
}

// --- wiring invariants ------------------------------------------------------

func TestAgentServiceNilStoreIsUnimplemented(t *testing.T) {
	svc := NewAgentService(nil)
	_, err := svc.ListAgents(context.Background(), &pb.ListAgentsRequest{})
	require.Error(t, err)
	assert.Equal(t, codes.Unimplemented, status.Code(err),
		"no store must never look like an empty registry")
}

func TestAgentServiceIsNotAuthExempt(t *testing.T) {
	// Registration writes operator-visible state; the skip list is the only
	// place a method can become reachable without a subject. If anyone adds an
	// agent method there, this fails and forces the conversation.
	for m := range skipAuthMethods {
		assert.NotContains(t, m, "AgentService",
			"agent registry methods must stay authenticated: "+m)
	}
}

func TestAgentServiceStoreErrorIsInternal(t *testing.T) {
	svc, st := newTestAgentService()
	st.failOn = errors.New("disk wedged")
	_, err := svc.RegisterAgent(context.Background(), &pb.RegisterAgentRequest{Id: "a", Address: "h:1"})
	require.Error(t, err)
	assert.Equal(t, codes.Internal, status.Code(err))
	assert.Contains(t, err.Error(), "disk wedged")
}

// --- permission matrix -------------------------------------------------------
//
// The registry is a fleet surface: an agent record says which host:port will
// execute LEVEE's tasks. So it goes through the same posture the other served
// services are pinned to (internal/grpc/resource_authz.go, service_policy_test.go)
// — reads take `view`, writes take `admin`, judged in permission.default_env
// because an agent declares no environment. Nothing here re-derives that rule.

// newPolicyAgentService wires the service on the shared matrix fixture:
// alice = view+plan in dev, admin nowhere; dave = admin in dev;
// carol = approve in dev only (not even view); mallory = not registered.
func newPolicyAgentService(t *testing.T) (*AgentService, *fakeAgentStore) {
	t.Helper()
	st := newFakeAgentStore()
	svc := NewAgentService(st).WithAuthorizer(newServiceAuthorizer(t))
	svc.now = func() time.Time { return time.Unix(1700000000, 0) }
	return svc, st
}

func TestAgentServicePolicyWritesRequireAdmin(t *testing.T) {
	svc, _ := newPolicyAgentService(t)

	_, err := svc.RegisterAgent(asSubject("alice"), &pb.RegisterAgentRequest{
		Id: "a1", Address: "10.0.0.5:9099", Capabilities: []string{"shell"}, MaxConcurrent: 2,
	})
	requireDenied(t, err, "RegisterAgent")
	assert.Contains(t, err.Error(), `"admin"`, "the refusal must name the action it judged")

	// Each write RPC is refused for the same subject, listed by name so a future
	// method cannot be added without one of these appearing.
	for _, tc := range []struct {
		rpc  string
		call func() error
	}{
		{"AgentHeartbeat", func() error {
			_, e := svc.AgentHeartbeat(asSubject("alice"), &pb.AgentHeartbeatRequest{Id: "a1"})
			return e
		}},
		{"DeregisterAgent", func() error {
			_, e := svc.DeregisterAgent(asSubject("alice"), &pb.DeregisterAgentRequest{Id: "a1"})
			return e
		}},
		{"RemoveAgent", func() error {
			_, e := svc.RemoveAgent(asSubject("alice"), &pb.RemoveAgentRequest{Id: "a1", Force: true})
			return e
		}},
	} {
		requireDenied(t, tc.call(), tc.rpc)
	}

	// mallory is named but absent from users.yaml: PermissionDenied, not
	// Unauthenticated — the same split the shared fixture documents.
	_, err = svc.RemoveAgent(asSubject("mallory"), &pb.RemoveAgentRequest{Id: "a1", Force: true})
	requireDenied(t, err, "RemoveAgent(mallory)")

	// A shared-token caller has no identity to judge, and a write must refuse
	// rather than fall through to the default environment.
	_, err = svc.RegisterAgent(context.Background(), &pb.RegisterAgentRequest{Id: "a2", Address: "10.0.0.6:9099"})
	assert.Equal(t, codes.Unauthenticated, codeOf(t, err),
		"an unattributable write must be refused, never guessed past")
}

func TestAgentServicePolicyAdminSubjectMayWrite(t *testing.T) {
	svc, st := newPolicyAgentService(t)

	rec, err := svc.RegisterAgent(asSubject("dave"), &pb.RegisterAgentRequest{
		Id: "a1", Address: "10.0.0.5:9099", Capabilities: []string{"shell"}, MaxConcurrent: 1,
	})
	require.NoError(t, err)
	require.Equal(t, "a1", rec.GetId())

	resp, err := svc.RemoveAgent(asSubject("dave"), &pb.RemoveAgentRequest{Id: "a1"})
	require.NoError(t, err)
	require.True(t, resp.GetRemoved(), "refusal: %s", resp.GetRefusal())
	assert.Empty(t, st.rows, "the write must land on the store, not just be permitted")
}

func TestAgentServicePolicyReadsUseViewScope(t *testing.T) {
	svc, st := newPolicyAgentService(t)
	// Seeded through the store: this test is about the read path.
	st.rows["a1"] = &state.Agent{
		ID: "a1", Address: "10.0.0.5:9099", Status: "idle",
		RegisteredAt: time.Unix(1600000000, 0),
	}

	list, err := svc.ListAgents(asSubject("alice"), &pb.ListAgentsRequest{})
	require.NoError(t, err)
	require.Len(t, list.GetAgents(), 1)

	one, err := svc.GetAgent(asSubject("alice"), &pb.GetAgentRequest{Id: "a1"})
	require.NoError(t, err)
	assert.Equal(t, "10.0.0.5:9099", one.GetAddress())

	// carol holds approve in dev and nothing else, so not even `view`: the
	// registry is dark to her on both read RPCs.
	_, err = svc.ListAgents(asSubject("carol"), &pb.ListAgentsRequest{})
	requireDenied(t, err, "ListAgents(carol)")
	_, err = svc.GetAgent(asSubject("carol"), &pb.GetAgentRequest{Id: "a1"})
	requireDenied(t, err, "GetAgent(carol)")

	// An unattributable caller keeps passing reads — the documented posture for
	// a shared token (there is no identity to narrow by), pinned here so it
	// stays a decision rather than drifting into an accident.
	_, err = svc.ListAgents(context.Background(), &pb.ListAgentsRequest{})
	require.NoError(t, err)
}

func TestAgentServicePolicyNilAuthorizerKeepsPreviousBehaviour(t *testing.T) {
	svc, _ := newTestAgentService()

	// No matrix configured: nothing to decide against, and that state is
	// announced at startup rather than replayed per request.
	_, err := svc.RegisterAgent(asSubject("mallory"), &pb.RegisterAgentRequest{
		Id: "a1", Address: "10.0.0.5:9099",
	})
	require.NoError(t, err)
}
