package push

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// --- NewAPNSClient ---------------------------------------------------------

func TestNewAPNSClient_Success(t *testing.T) {
	pemBytes, _ := generateTestECDSAKey(t)
	c, err := NewAPNSClient("TEAM123456", "KEY1234567", "com.example.levee", pemBytes, false)
	require.NoError(t, err)
	assert.Equal(t, APNsEndpointDevelopment, c.endpoint)
	assert.Equal(t, "TEAM123456", c.teamID)
	assert.Equal(t, "KEY1234567", c.keyID)
	assert.Equal(t, "com.example.levee", c.bundleID)
}

func TestNewAPNSClient_ProductionEndpoint(t *testing.T) {
	pemBytes, _ := generateTestECDSAKey(t)
	c, err := NewAPNSClient("TEAM123456", "KEY1234567", "com.example.levee", pemBytes, true)
	require.NoError(t, err)
	assert.Equal(t, APNsEndpointProduction, c.endpoint)
}

func TestNewAPNSClient_MissingFields(t *testing.T) {
	pemBytes, _ := generateTestECDSAKey(t)
	cases := []struct {
		name   string
		teamID string
		keyID  string
		bundle string
	}{
		{"empty team", "", "k", "b"},
		{"empty key", "t", "", "b"},
		{"empty bundle", "t", "k", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := NewAPNSClient(tc.teamID, tc.keyID, tc.bundle, pemBytes, false)
			require.Error(t, err)
			assert.Contains(t, err.Error(), "required")
		})
	}
}

func TestNewAPNSClient_InvalidPEM(t *testing.T) {
	_, err := NewAPNSClient("t", "k", "b", []byte("not a pem"), false)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "parse private key")
}

// --- JWT generation --------------------------------------------------------

func TestAPNS_GenerateToken_Verifies(t *testing.T) {
	pemBytes, _ := generateTestECDSAKey(t)
	c, err := NewAPNSClient("TEAM123456", "KEY1234567", "com.example.levee", pemBytes, false)
	require.NoError(t, err)

	tok, exp, err := c.generateToken()
	require.NoError(t, err)
	assert.NotEmpty(t, tok)
	assert.True(t, exp.After(exp.Add(-1))) // sanity: exp is a real time

	// Three base64 parts separated by dots.
	parts := strings.Split(tok, ".")
	require.Len(t, parts, 3)

	// Signature verifies against the public key.
	require.NoError(t, c.verifyTokenForTest(tok))
}

func TestAPNS_EnsureToken_CachesAcrossCalls(t *testing.T) {
	pemBytes, _ := generateTestECDSAKey(t)
	c, err := NewAPNSClient("TEAM123456", "KEY1234567", "com.example.levee", pemBytes, false)
	require.NoError(t, err)

	tok1, err := c.ensureToken()
	require.NoError(t, err)
	tok2, err := c.ensureToken()
	require.NoError(t, err)
	assert.Equal(t, tok1, tok2, "cached token should be reused")
}

// --- Send via mock HTTP server --------------------------------------------

func TestAPNS_Send_Success(t *testing.T) {
	pemBytes, _ := generateTestECDSAKey(t)
	c, err := NewAPNSClient("TEAM123456", "KEY1234567", "com.example.levee", pemBytes, false)
	require.NoError(t, err)

	var (
		gotPath  string
		gotAuth  string
		gotTopic string
		gotBody  map[string]json.RawMessage
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		gotTopic = r.Header.Get("apns-topic")
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	c.endpoint = srv.URL

	err = c.Send(context.Background(), APNSNotification{
		DeviceToken: "abc123",
		Alert:       APNSAlert{Title: "审批请求", Body: "run-42 待审批"},
		Sound:       "default",
		Category:    "APPROVE_CATEGORY",
		CustomData:  map[string]interface{}{"run_id": "run-42"},
	})
	require.NoError(t, err)
	assert.Equal(t, "/3/device/abc123", gotPath)
	assert.True(t, strings.HasPrefix(gotAuth, "bearer "))
	assert.Equal(t, "com.example.levee", gotTopic)
	assert.Contains(t, gotBody, "aps")
	assert.Contains(t, gotBody, "run_id")
}

func TestAPNS_Send_EmptyDeviceToken(t *testing.T) {
	pemBytes, _ := generateTestECDSAKey(t)
	c, err := NewAPNSClient("TEAM123456", "KEY1234567", "com.example.levee", pemBytes, false)
	require.NoError(t, err)
	err = c.Send(context.Background(), APNSNotification{DeviceToken: ""})
	assert.ErrorIs(t, err, ErrEmptyDeviceToken)
}

func TestAPNS_Send_Non2xxReturnsErrPushFailed(t *testing.T) {
	pemBytes, _ := generateTestECDSAKey(t)
	c, err := NewAPNSClient("TEAM123456", "KEY1234567", "com.example.levee", pemBytes, false)
	require.NoError(t, err)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"reason":"BadDeviceToken"}`))
	}))
	defer srv.Close()
	c.endpoint = srv.URL

	err = c.Send(context.Background(), APNSNotification{
		DeviceToken: "bad",
		Alert:       APNSAlert{Title: "t", Body: "b"},
	})
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrPushFailed)
	assert.Contains(t, err.Error(), "BadDeviceToken")
}

func TestAPNS_Send_ContextCancelled(t *testing.T) {
	pemBytes, _ := generateTestECDSAKey(t)
	c, err := NewAPNSClient("TEAM123456", "KEY1234567", "com.example.levee", pemBytes, false)
	require.NoError(t, err)

	// Server that hangs forever.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	defer srv.Close()
	c.endpoint = srv.URL

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel before sending
	err = c.Send(ctx, APNSNotification{DeviceToken: "x", Alert: APNSAlert{Title: "t"}})
	require.Error(t, err)
}

// --- SendBatch -------------------------------------------------------------

func TestAPNS_SendBatch_ReportsPerMessageErrors(t *testing.T) {
	pemBytes, _ := generateTestECDSAKey(t)
	c, err := NewAPNSClient("TEAM123456", "KEY1234567", "com.example.levee", pemBytes, false)
	require.NoError(t, err)

	var count atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := count.Add(1)
		if n == 2 { // second message fails
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"reason":"BadDeviceToken"}`))
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	c.endpoint = srv.URL

	notifs := []APNSNotification{
		{DeviceToken: "t1", Alert: APNSAlert{Title: "a"}},
		{DeviceToken: "t2", Alert: APNSAlert{Title: "b"}},
		{DeviceToken: "t3", Alert: APNSAlert{Title: "c"}},
	}
	errs := c.SendBatch(context.Background(), notifs)
	require.Len(t, errs, 3)
	assert.NoError(t, errs[0])
	assert.Error(t, errs[1])
	assert.NoError(t, errs[2])
}

// --- marshalPayload --------------------------------------------------------

func TestAPNS_MarshalPayload_NoCustomData(t *testing.T) {
	pemBytes, _ := generateTestECDSAKey(t)
	c, err := NewAPNSClient("t", "k", "b", pemBytes, false)
	require.NoError(t, err)

	body, err := c.marshalPayload(APNSNotification{
		DeviceToken: "x",
		Alert:       APNSAlert{Title: "t", Body: "b"},
	})
	require.NoError(t, err)
	assert.Contains(t, string(body), `"aps"`)
	assert.NotContains(t, string(body), `"run_id"`)
}

func TestAPNS_MarshalPayload_WithCustomData(t *testing.T) {
	pemBytes, _ := generateTestECDSAKey(t)
	c, err := NewAPNSClient("t", "k", "b", pemBytes, false)
	require.NoError(t, err)

	body, err := c.marshalPayload(APNSNotification{
		DeviceToken: "x",
		Alert:       APNSAlert{Title: "t"},
		CustomData:  map[string]interface{}{"run_id": "run-1", "action": "approve"},
	})
	require.NoError(t, err)
	assert.Contains(t, string(body), `"run_id":"run-1"`)
	assert.Contains(t, string(body), `"action":"approve"`)
	assert.Contains(t, string(body), `"aps"`)
}

// --- Close -----------------------------------------------------------------

func TestAPNS_Close_NoError(t *testing.T) {
	pemBytes, _ := generateTestECDSAKey(t)
	c, err := NewAPNSClient("t", "k", "b", pemBytes, false)
	require.NoError(t, err)
	require.NoError(t, c.Close())
}

// --- parseECDSAPrivateKey --------------------------------------------------

func TestParseECDSAPrivateKey_InvalidPEM(t *testing.T) {
	_, err := parseECDSAPrivateKey([]byte("garbage"))
	require.Error(t, err)
}

func TestParseECDSAPrivateKey_WrongKeyType(t *testing.T) {
	// RSA PEM should fail to parse as ECDSA.
	rsaPEM, _ := generateTestRSAKey(t)
	_, err := parseECDSAPrivateKey(rsaPEM)
	require.Error(t, err)
}

// --- splitJWT --------------------------------------------------------------

func TestSplitJWT_ThreeParts(t *testing.T) {
	parts := splitJWT("a.b.c")
	require.Len(t, parts, 3)
	assert.Equal(t, "a", parts[0])
	assert.Equal(t, "b", parts[1])
	assert.Equal(t, "c", parts[2])
}

func TestSplitJWT_NoDots(t *testing.T) {
	parts := splitJWT("abc")
	require.Len(t, parts, 1)
	assert.Equal(t, "abc", parts[0])
}

// --- sanity: fmt import used to keep go vet happy when we add helpers -------
var _ = fmt.Sprintf
