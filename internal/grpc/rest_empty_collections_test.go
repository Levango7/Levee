package grpc

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/nexus/levee/internal/grpc/pb"
)

// B-07: an empty collection is an answer, not a missing field. protojson omits
// empty repeated fields by default, which made an empty /changes answer {} and
// an empty /agents answer null. The served contract is always [] / {key: []}.
func TestRESTEmptyCollectionsAreExplicitArrays(t *testing.T) {
	_, srv, _ := startTestGateway(t, ServeGatewayConfig{})

	envelopes := []struct {
		path string
		key  string
	}{
		{path: "/changes", key: "changes"},
		{path: "/templates", key: "templates"},
		{path: "/targets", key: "targets"},
		{path: "/audit/log", key: "entries"},
		{path: "/audit/traces", key: "entries"},
	}
	for _, tc := range envelopes {
		t.Run(tc.path, func(t *testing.T) {
			resp, err := http.Get(srv.URL + tc.path)
			require.NoError(t, err)
			defer resp.Body.Close()
			require.Equal(t, http.StatusOK, resp.StatusCode)
			var body map[string]json.RawMessage
			require.NoError(t, json.NewDecoder(resp.Body).Decode(&body))
			raw, ok := body[tc.key]
			require.True(t, ok, "%s must still name its empty collection, body=%v", tc.path, body)
			require.JSONEq(t, "[]", string(raw))
		})
	}

	for _, path := range []string{"/agents", "/groups"} {
		t.Run(path, func(t *testing.T) {
			resp, err := http.Get(srv.URL + path)
			require.NoError(t, err)
			defer resp.Body.Close()
			require.Equal(t, http.StatusOK, resp.StatusCode)
			raw, err := io.ReadAll(resp.Body)
			require.NoError(t, err)
			require.JSONEq(t, "[]", string(raw), "a bare empty list is [], never null")
		})
	}
}

// The B-07 rule is deliberately top-level only. EmitDefaults would also invent
// empty lists inside a stored plan and zero scalar fields across every response;
// both are contract changes unrelated to empty-list hygiene.
func TestWriteProtoEmptyCollectionsDoNotAlterNestedOmissions(t *testing.T) {
	rec := httptest.NewRecorder()
	writeProto(rec, &pb.ListChangesResponse{Changes: []*pb.Change{{
		Id:   "run-1",
		Plan: &pb.Plan{},
	}}})
	require.Equal(t, http.StatusOK, rec.Code)

	var body map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	_, hasTotalSize := body["totalSize"]
	require.False(t, hasTotalSize, "scalar zero values keep their existing omission semantics")

	var rows []map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(body["changes"], &rows))
	require.Len(t, rows, 1)
	var plan map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(rows[0]["plan"], &plan))
	for _, key := range []string{"batches", "targetHosts"} {
		_, present := plan[key]
		require.False(t, present, "nested plan.%s stays omitted when unset", key)
	}
}

// The approvals chain needed the B-07 rule ONE level deeper: a fresh pending
// chain has no votes, protojson drops that empty list, and the console's
// approval summary crashed reading decisions.length on real backend data
// (real-backend e2e probe, 2026-11-21). The served contract is that
// approvals[].decisions and approvals[].approvers are always-present
// collections; the rule stays scoped to this endpoint's writer.
func TestRESTApprovalsChainCollectionsAreExplicitArrays(t *testing.T) {
	resp := &pb.GetApprovalsResponse{Approvals: []*pb.ApprovalInfo{
		// A fresh pending chain: no votes yet — the exact shape that crashed.
		{Id: "approval-1", ChangeId: "run-1", Level: "standard", Status: "pending",
			Approvers: []string{"admin"}, MinApprovers: 1},
		// A chain with a vote keeps its decisions AND still gets an explicit
		// empty list where a repeated field is unset.
		{Id: "approval-2", ChangeId: "run-1", Level: "standard", Status: "pending",
			Approvers: []string{"admin", "bob"}, MinApprovers: 2,
			Decisions: []*pb.ApprovalDecision{{Approver: "admin", Action: "approve"}}},
	}}
	rec := httptest.NewRecorder()
	writeProtoNested(rec, resp)
	require.Equal(t, http.StatusOK, rec.Code)

	var body struct {
		Approvals []struct {
			ID        string           `json:"id"`
			Approvers []string         `json:"approvers"`
			Decisions []map[string]any `json:"decisions"`
		} `json:"approvals"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	require.Len(t, body.Approvals, 2)

	first := body.Approvals[0]
	require.NotNil(t, first.Decisions, "a fresh pending chain must still name its empty decisions collection, body=%s", rec.Body.String())
	require.Len(t, first.Decisions, 0)
	require.Equal(t, []string{"admin"}, first.Approvers)

	second := body.Approvals[1]
	require.Len(t, second.Decisions, 1, "existing votes survive the pass")
	require.Equal(t, "admin", second.Decisions[0]["approver"])
}

// The nesting rule is scoped: writeProto (top-level only) must NOT grow
// nested arrays — plan.batches keeps its B-07-era omission semantics.
func TestWriteProtoNestedRuleStaysScopedToItsWriter(t *testing.T) {
	rec := httptest.NewRecorder()
	writeProto(rec, &pb.GetApprovalsResponse{Approvals: []*pb.ApprovalInfo{
		{Id: "approval-1", Approvers: []string{"admin"}},
	}})
	var body map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	var rows []map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(body["approvals"], &rows))
	_, hasDecisions := rows[0]["decisions"]
	require.False(t, hasDecisions, "writeProto keeps the top-level-only rule; the nested pass belongs to writeProtoNested only")
}
