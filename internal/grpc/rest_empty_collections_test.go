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
