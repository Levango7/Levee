package push

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// --- NewFCMClient ----------------------------------------------------------

func TestNewFCMClient_Success(t *testing.T) {
	c := NewFCMClient("my-project", []byte(`{"private_key":"x","client_email":"y"}`))
	assert.Equal(t, "my-project", c.projectID)
	assert.Contains(t, c.endpoint, "my-project")
	assert.Contains(t, c.endpoint, "messages:send")
}

func TestNewFCMClient_EndpointFormat(t *testing.T) {
	c := NewFCMClient("proj-123", nil)
	want := "https://fcm.googleapis.com/v1/projects/proj-123/messages:send"
	assert.Equal(t, want, c.endpoint)
}

// --- Send via mock HTTP server --------------------------------------------

func TestFCM_Send_Success(t *testing.T) {
	rsaPEM, _ := generateTestRSAKey(t)
	saJSON := buildServiceAccountJSON(t, rsaPEM, "test-proj", "https://oauth2.googleapis.com/token")
	c := NewFCMClient("test-proj", saJSON)

	// Inject a precomputed access token so we skip the OAuth2 dance.
	c.SetAccessTokenForTest("test-access-token", time.Now().Add(time.Hour))

	var (
		gotAuth string
		gotBody map[string]json.RawMessage
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	c.SetEndpointForTest(srv.URL)

	err := c.Send(context.Background(), FCMMessage{
		Token:        "device-token-1",
		Notification: &FCMNotification{Title: "审批请求", Body: "run-42 待审批"},
		Data:         map[string]string{"run_id": "run-42"},
	})
	require.NoError(t, err)
	assert.Equal(t, "Bearer test-access-token", gotAuth)
	assert.Contains(t, gotBody, "message")
}

func TestFCM_Send_EmptyToken(t *testing.T) {
	c := NewFCMClient("p", nil)
	err := c.Send(context.Background(), FCMMessage{Token: ""})
	assert.ErrorIs(t, err, ErrEmptyDeviceToken)
}

func TestFCM_Send_Non2xxReturnsErrPushFailed(t *testing.T) {
	c := NewFCMClient("p", nil)
	c.SetAccessTokenForTest("tok", time.Now().Add(time.Hour))

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"message":"INVALID_ARGUMENT","status":"INVALID_ARGUMENT"}}`))
	}))
	defer srv.Close()
	c.SetEndpointForTest(srv.URL)

	err := c.Send(context.Background(), FCMMessage{
		Token:        "x",
		Notification: &FCMNotification{Title: "t"},
	})
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrPushFailed)
	assert.Contains(t, err.Error(), "INVALID_ARGUMENT")
}

func TestFCM_Send_ContextCancelled(t *testing.T) {
	c := NewFCMClient("p", nil)
	c.SetAccessTokenForTest("tok", time.Now().Add(time.Hour))

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	defer srv.Close()
	c.SetEndpointForTest(srv.URL)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := c.Send(ctx, FCMMessage{Token: "x"})
	require.Error(t, err)
}

// --- SendBatch -------------------------------------------------------------

func TestFCM_SendBatch_ReportsPerMessageErrors(t *testing.T) {
	c := NewFCMClient("p", nil)
	c.SetAccessTokenForTest("tok", time.Now().Add(time.Hour))

	var count int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		count++
		if count == 2 {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":{"message":"fail"}}`))
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	c.SetEndpointForTest(srv.URL)

	msgs := []FCMMessage{
		{Token: "a", Notification: &FCMNotification{Title: "1"}},
		{Token: "b", Notification: &FCMNotification{Title: "2"}},
		{Token: "c", Notification: &FCMNotification{Title: "3"}},
	}
	errs := c.SendBatch(context.Background(), msgs)
	require.Len(t, errs, 3)
	assert.NoError(t, errs[0])
	assert.Error(t, errs[1])
	assert.NoError(t, errs[2])
}

// --- OAuth2 token ----------------------------------------------------------

func TestFCM_GetAccessToken_Success(t *testing.T) {
	rsaPEM, _ := generateTestRSAKey(t)
	saJSON := buildServiceAccountJSON(t, rsaPEM, "proj", "https://oauth2.googleapis.com/token")
	c := NewFCMClient("proj", saJSON)

	tokenSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Verify the request is a JWT-bearer grant.
		require.NoError(t, r.ParseForm())
		assert.Equal(t, "urn:ietf:params:oauth:grant-type:jwt-bearer", r.Form.Get("grant_type"))
		assertion := r.Form.Get("assertion")
		require.NotEmpty(t, assertion)
		parts := strings.Split(assertion, ".")
		require.Len(t, parts, 3, "assertion must be a JWT")

		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"abc123","expires_in":3600,"token_type":"Bearer"}`))
	}))
	defer tokenSrv.Close()

	// Override the token_uri in the service-account key to point at our mock.
	saJSON = buildServiceAccountJSON(t, rsaPEM, "proj", tokenSrv.URL)
	c.WithServiceAccountKeyForTest(saJSON)

	tok, exp, err := c.getAccessToken()
	require.NoError(t, err)
	assert.Equal(t, "abc123", tok)
	assert.True(t, exp.After(time.Now()))
}

func TestFCM_GetAccessToken_TokenEndpointError(t *testing.T) {
	rsaPEM, _ := generateTestRSAKey(t)
	tokenSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"invalid_client"}`))
	}))
	defer tokenSrv.Close()

	saJSON := buildServiceAccountJSON(t, rsaPEM, "proj", tokenSrv.URL)
	c := NewFCMClient("proj", saJSON)

	_, _, err := c.getAccessToken()
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrPushFailed)
}

func TestFCM_GetAccessToken_InvalidServiceAccount(t *testing.T) {
	c := NewFCMClient("p", []byte(`{"private_key":"","client_email":""}`))
	_, _, err := c.getAccessToken()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "parse service account")
}

func TestFCM_MakeAssertionForTest_BuildsValidJWT(t *testing.T) {
	rsaPEM, _ := generateTestRSAKey(t)
	saJSON := buildServiceAccountJSON(t, rsaPEM, "proj", "https://oauth2.googleapis.com/token")
	c := NewFCMClient("proj", saJSON)

	assertion, err := c.MakeAssertionForTest()
	require.NoError(t, err)
	parts := strings.Split(assertion, ".")
	require.Len(t, parts, 3)
}

// --- ensureAccessToken caching --------------------------------------------

func TestFCM_EnsureAccessToken_CachesAcrossCalls(t *testing.T) {
	rsaPEM, _ := generateTestRSAKey(t)
	tokenSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"cached","expires_in":3600,"token_type":"Bearer"}`))
	}))
	defer tokenSrv.Close()

	saJSON := buildServiceAccountJSON(t, rsaPEM, "proj", tokenSrv.URL)
	c := NewFCMClient("proj", saJSON)

	tok1, err := c.ensureAccessToken()
	require.NoError(t, err)
	tok2, err := c.ensureAccessToken()
	require.NoError(t, err)
	assert.Equal(t, tok1, tok2)
	assert.Equal(t, "cached", tok1)
}

// --- parseServiceAccountKey ------------------------------------------------

func TestParseServiceAccountKey_MissingPrivateKey(t *testing.T) {
	_, err := parseServiceAccountKey([]byte(`{"client_email":"x"}`))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "private_key")
}

func TestParseServiceAccountKey_MissingClientEmail(t *testing.T) {
	_, err := parseServiceAccountKey([]byte(`{"private_key":"x"}`))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "client_email")
}

func TestParseServiceAccountKey_InvalidJSON(t *testing.T) {
	_, err := parseServiceAccountKey([]byte(`not json`))
	require.Error(t, err)
}

// --- parseRSAPrivateKey ----------------------------------------------------

func TestParseRSAPrivateKey_InvalidPEM(t *testing.T) {
	_, err := parseRSAPrivateKey([]byte("garbage"))
	require.Error(t, err)
}

func TestParseRSAPrivateKey_ECDSAKeyFails(t *testing.T) {
	ecPEM, _ := generateTestECDSAKey(t)
	_, err := parseRSAPrivateKey(ecPEM)
	require.Error(t, err)
}

// --- Close -----------------------------------------------------------------

func TestFCM_Close_NoError(t *testing.T) {
	c := NewFCMClient("p", nil)
	require.NoError(t, c.Close())
}
