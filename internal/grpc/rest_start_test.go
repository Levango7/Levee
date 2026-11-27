package grpc

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nexus/levee/internal/runstatus"
)

func TestRESTStartChangeAcceptedAndReplay(t *testing.T) {
	gw, srv, store := startTestGatewayFull(t, ServeGatewayConfig{})
	engine := newStartTestEngine()
	gw.change.engine = engine.adapter()
	changeID := createStartableChange(t, gw.change, store)
	endpoint := srv.URL + "/changes/" + changeID + "/start"

	response := doReq(t, http.MethodPost, endpoint,
		`{"autoApprove":true,"maxConcurrency":2,"idempotencyKey":"rest-start"}`)
	require.Equal(t, http.StatusAccepted, response.StatusCode)
	var accepted struct {
		ExecutionID string `json:"executionId"`
		Status      string `json:"status"`
		Duplicate   bool   `json:"duplicate"`
		Message     string `json:"message"`
	}
	require.NoError(t, json.NewDecoder(response.Body).Decode(&accepted))
	require.NoError(t, response.Body.Close())
	assert.Equal(t, changeID, accepted.ExecutionID)
	assert.Equal(t, runstatus.StatusRunning, accepted.Status)
	assert.False(t, accepted.Duplicate)
	assert.Contains(t, accepted.Message, "GetChange or WatchChange")
	engine.waitStarted(t)

	replayResponse := doReq(t, http.MethodPost, endpoint,
		`{"autoApprove":true,"maxConcurrency":2,"idempotencyKey":"rest-start"}`)
	require.Equal(t, http.StatusOK, replayResponse.StatusCode)
	var replay struct {
		ExecutionID string `json:"executionId"`
		Status      string `json:"status"`
		Duplicate   bool   `json:"duplicate"`
	}
	require.NoError(t, json.NewDecoder(replayResponse.Body).Decode(&replay))
	require.NoError(t, replayResponse.Body.Close())
	assert.Equal(t, changeID, replay.ExecutionID)
	assert.Equal(t, runstatus.StatusRunning, replay.Status)
	assert.True(t, replay.Duplicate)
	assert.Equal(t, int32(1), engine.calls.Load())

	conflict := doReq(t, http.MethodPost, endpoint,
		`{"autoApprove":true,"maxConcurrency":3,"idempotencyKey":"rest-start"}`)
	require.NoError(t, readAndClose(conflict))
	assert.Equal(t, http.StatusConflict, conflict.StatusCode)
	missingKey := doReq(t, http.MethodPost, endpoint, `{"autoApprove":true}`)
	require.NoError(t, readAndClose(missingKey))
	assert.Equal(t, http.StatusBadRequest, missingKey.StatusCode)
	wrongMethod := doReq(t, http.MethodGet, endpoint, "")
	require.NoError(t, readAndClose(wrongMethod))
	assert.Equal(t, http.StatusMethodNotAllowed, wrongMethod.StatusCode)

	close(engine.release)
	waitForChangeStatus(t, gw.change, changeID, runstatus.StatusCompleted)
	require.NoError(t, <-engine.finished)
}
