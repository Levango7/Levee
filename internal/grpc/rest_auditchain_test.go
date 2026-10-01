package grpc

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nexus/levee/internal/audit"
	"github.com/nexus/levee/internal/state"
)

// These cover the audit-chain member added to GET /audit/verify. The
// response is assembled by splicing the audit chain into the marshalled proto
// (levee.proto cannot be regenerated here — protoc is unavailable — and its
// VerifyHashChainResponse fields are all run-scoped), so the tests check both
// the new member and that the pre-existing proto fields survive the splice
// byte-for-byte in meaning.

// auditVerifyBody is the shape the handler returns.
type auditVerifyBody struct {
	Valid         bool   `json:"valid"`
	EntriesVerifd int64  `json:"entriesVerified"`
	BrokenEntryID string `json:"brokenEntryId"`
	BrokenReason  string `json:"brokenReason"`
	Runs          []struct {
		RunID string `json:"runId"`
		Valid bool   `json:"valid"`
	} `json:"runs"`
	AuditChain struct {
		Valid           bool   `json:"valid"`
		EntriesVerified int    `json:"entriesVerified"`
		Unsealed        int    `json:"unsealed"`
		FirstFailure    string `json:"firstFailureType"`
		Failures        []struct {
			AuditID string `json:"auditId"`
			Index   int    `json:"index"`
			Type    string `json:"type"`
			Actual  string `json:"actual"`
		} `json:"failures"`
	} `json:"auditChain"`
}

func getAuditVerify(t *testing.T, url string) auditVerifyBody {
	t.Helper()
	resp, err := http.Get(url)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	var body auditVerifyBody
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&body))
	return body
}

// seedAuditRows writes n audit rows through the production write path, so the
// chain is sealed exactly as it is in a running deployment.
func seedAuditRows(t *testing.T, store state.Store, n int) {
	t.Helper()
	ctx := context.Background()
	base := time.Now().UTC()
	for i := 0; i < n; i++ {
		require.NoError(t, audit.Record(ctx, store, &state.Audit{
			ID:        fmt.Sprintf("rest-aud-%d", i),
			RunID:     fmt.Sprintf("run-%d", i),
			Action:    "apply",
			Actor:     "alice",
			Target:    fmt.Sprintf("host-%d", i),
			Result:    "success",
			Timestamp: base.Add(time.Duration(i) * time.Millisecond),
		}))
	}
}

func TestRESTAuditVerify_ReportsIntactAuditChain(t *testing.T) {
	_, srv, store := startTestGateway(t, ServeGatewayConfig{})
	seedAuditRows(t, store, 3)

	body := getAuditVerify(t, srv.URL+"/audit/verify")
	assert.True(t, body.Valid, "a freshly sealed chain must verify")
	require.NotNil(t, body.AuditChain.Failures, "failures must serialise as [] not null")
	assert.Empty(t, body.AuditChain.Failures)
	assert.True(t, body.AuditChain.Valid)
	assert.Equal(t, 3, body.AuditChain.EntriesVerified)
	assert.Equal(t, 0, body.AuditChain.Unsealed)
	assert.Empty(t, body.AuditChain.FirstFailure)
	assert.Empty(t, body.BrokenEntryID)
	assert.Empty(t, body.BrokenReason)
}

func TestRESTAuditVerify_ReportsTamperedChain(t *testing.T) {
	_, srv, store := startTestGateway(t, ServeGatewayConfig{})
	seedAuditRows(t, store, 4)

	sqlStore, ok := store.(*state.SQLiteStore)
	require.True(t, ok, "test needs the concrete store to reach the raw handle")
	ctx := context.Background()
	// Model an operator who removed the database-level guard first.
	_, err := sqlStore.DB().ExecContext(ctx, `DROP TRIGGER worm_prevent_audit_update`)
	require.NoError(t, err)
	_, err = sqlStore.DB().ExecContext(ctx,
		`UPDATE audit SET actor = 'mallory' WHERE id = 'rest-aud-2'`)
	require.NoError(t, err)

	body := getAuditVerify(t, srv.URL+"/audit/verify")
	assert.False(t, body.Valid, "the top-level verdict must fold in the audit chain")
	assert.False(t, body.AuditChain.Valid)
	require.Len(t, body.AuditChain.Failures, 1)
	assert.Equal(t, "rest-aud-2", body.AuditChain.Failures[0].AuditID)
	assert.Equal(t, 2, body.AuditChain.Failures[0].Index)
	assert.Equal(t, "hash_mismatch", body.AuditChain.Failures[0].Type)
	assert.Equal(t, "hash_mismatch", body.AuditChain.FirstFailure)
	assert.Equal(t, "rest-aud-2", body.BrokenEntryID)
	assert.Equal(t, "audit_chain:hash_mismatch", body.BrokenReason)
}

func TestRESTAuditVerify_ReportsUnsealedRows(t *testing.T) {
	_, srv, store := startTestGateway(t, ServeGatewayConfig{})
	seedAuditRows(t, store, 2)

	// A row written straight to the store never passes through Record, so it
	// carries no chain hash. That must be surfaced, not treated as "nothing
	// to check, therefore fine".
	require.NoError(t, store.CreateAudit(context.Background(), &state.Audit{
		ID: "rest-aud-raw", Action: "config", Actor: "root", Target: "cfg",
		Result: "ok", Timestamp: time.Now().UTC().Add(time.Hour),
	}))

	body := getAuditVerify(t, srv.URL+"/audit/verify")
	assert.False(t, body.Valid)
	assert.False(t, body.AuditChain.Valid)
	assert.Equal(t, 1, body.AuditChain.Unsealed)
	assert.Equal(t, 3, body.AuditChain.EntriesVerified)
	require.Len(t, body.AuditChain.Failures, 1)
	assert.Equal(t, "rest-aud-raw", body.AuditChain.Failures[0].AuditID)
	assert.Equal(t, "empty_hash", body.AuditChain.Failures[0].Type)
}

// TestRESTAuditVerify_EmptyLog: a deployment with no audit rows yet must still
// answer, and vacuously so.
func TestRESTAuditVerify_EmptyLog(t *testing.T) {
	_, srv, _ := startTestGateway(t, ServeGatewayConfig{})
	body := getAuditVerify(t, srv.URL+"/audit/verify")
	assert.True(t, body.Valid)
	assert.Equal(t, 0, body.AuditChain.EntriesVerified)
	assert.Empty(t, body.AuditChain.Failures)
}
