package grpc

// rest_chain_mode_test.go pins GET /audit/chain-mode.
//
// The endpoint exists because the console's "哈希链完整" label used to read as
// the STRONG guarantee (keyed HMAC) on deployments that had no key at all
// (bare SHA-256). Those two differ in what they actually promise:
//
//   - keyed (HMAC-SHA256): someone who can write to the store but cannot read
//     the passphrase cannot recompute a rewritten chain.
//   - unkeyed (SHA-256): anyone who can write to the store can recompute the
//     whole chain; it still catches accidental corruption and a record whose
//     content was rewritten without rebuilding the hashes.
//
// So the mode is a fact the console has to be told, not a detail the docs can
// carry. The tests below pin both modes and the shape of the answer.

import (
	"encoding/json"
	"net/http"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nexus/levee/internal/audit"
)

// chainModeBody is the shape the handler returns.
type chainModeBody struct {
	Mode      string `json:"mode"`
	Algorithm string `json:"algorithm"`
	Keyed     bool   `json:"keyed"`
}

func getChainMode(t *testing.T, url string) chainModeBody {
	t.Helper()
	resp, err := http.Get(url)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	var body chainModeBody
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&body))
	return body
}

// withHMACKey sets the audit passphrase for the duration of one test and
// re-arms the lazy key resolution, so the process reports the mode the
// endpoint is supposed to report.
//
// The key is resolved once per process (the first digest decides), which is
// why the reset hook exists at all.
func withHMACKey(t *testing.T, value string) {
	t.Helper()
	old, hadOld := os.LookupEnv(audit.EnvHMACKey)
	require.NoError(t, os.Setenv(audit.EnvHMACKey, value))
	audit.ResetKeyForTest()
	t.Cleanup(func() {
		if hadOld {
			_ = os.Setenv(audit.EnvHMACKey, old)
		} else {
			_ = os.Unsetenv(audit.EnvHMACKey)
		}
		audit.ResetKeyForTest()
	})
}

// TestAuditChainMode_Unkeyed pins the answer for a deployment with no
// passphrase — the default, and the case the console used to overstate.
func TestAuditChainMode_Unkeyed(t *testing.T) {
	old, hadOld := os.LookupEnv(audit.EnvHMACKey)
	require.NoError(t, os.Unsetenv(audit.EnvHMACKey))
	audit.ResetKeyForTest()
	t.Cleanup(func() {
		if hadOld {
			_ = os.Setenv(audit.EnvHMACKey, old)
		}
		audit.ResetKeyForTest()
	})

	_, srv, _ := startTestGateway(t, ServeGatewayConfig{})
	body := getChainMode(t, srv.URL+"/audit/chain-mode")
	assert.Equal(t, "unkeyed", body.Mode)
	assert.Equal(t, "SHA-256", body.Algorithm)
	assert.False(t, body.Keyed)
}

// TestAuditChainMode_Keyed pins the answer for a deployment that DID
// configure a passphrase. Without this, the keyed branch of the handler would
// be untested — and it is the branch that carries the stronger guarantee, so
// it is the branch that must not silently degrade to "unkeyed".
func TestAuditChainMode_Keyed(t *testing.T) {
	withHMACKey(t, "0123456789abcdef0123456789abcdef") // >= 16 raw bytes
	require.True(t, audit.Keyed(), "the passphrase must be in effect for this test")

	_, srv, _ := startTestGateway(t, ServeGatewayConfig{})
	body := getChainMode(t, srv.URL+"/audit/chain-mode")
	assert.Equal(t, "keyed", body.Mode)
	assert.Equal(t, "HMAC-SHA256", body.Algorithm)
	assert.True(t, body.Keyed)
}

// TestAuditChainMode_ShortKeyFallsBackToUnkeyed pins that a passphrase too
// short to be accepted does NOT turn the chain into a keyed one: the audit
// package ignores a weak key (and says so in a warning), so the endpoint has
// to report the truth — unkeyed — rather than "keyed" because an environment
// variable happened to be set.
func TestAuditChainMode_ShortKeyFallsBackToUnkeyed(t *testing.T) {
	withHMACKey(t, "too-short")
	require.False(t, audit.Keyed(), "a short passphrase must not count as a key")

	_, srv, _ := startTestGateway(t, ServeGatewayConfig{})
	body := getChainMode(t, srv.URL+"/audit/chain-mode")
	assert.Equal(t, "unkeyed", body.Mode)
	assert.Equal(t, "SHA-256", body.Algorithm)
	assert.False(t, body.Keyed)
}
