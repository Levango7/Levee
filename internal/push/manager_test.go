package push

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// --- NewPushManager --------------------------------------------------------

func TestNewPushManager_NilClientsAllowed(t *testing.T) {
	pm := NewPushManager(nil, nil)
	assert.NotNil(t, pm)
	assert.Empty(t, pm.ListDevices("anyone"))
}

// --- RegisterDevice --------------------------------------------------------

func TestPushManager_RegisterDevice_Success(t *testing.T) {
	pm := NewPushManager(nil, nil)
	require.NoError(t, pm.RegisterDevice("alice", "token-1", "ios"))
	devices := pm.ListDevices("alice")
	require.Len(t, devices, 1)
	assert.Equal(t, "token-1", devices[0].Token)
	assert.Equal(t, PlatformIOS, devices[0].Platform)
	assert.Equal(t, "alice", devices[0].UserID)
}

func TestPushManager_RegisterDevice_PlatformNormalised(t *testing.T) {
	pm := NewPushManager(nil, nil)
	require.NoError(t, pm.RegisterDevice("alice", "t1", "iOS"))
	require.NoError(t, pm.RegisterDevice("alice", "t2", "ANDROID"))
	devices := pm.ListDevices("alice")
	require.Len(t, devices, 2)
	assert.Equal(t, PlatformIOS, devices[0].Platform)
	assert.Equal(t, PlatformAndroid, devices[1].Platform)
}

func TestPushManager_RegisterDevice_DuplicateIsNoop(t *testing.T) {
	pm := NewPushManager(nil, nil)
	require.NoError(t, pm.RegisterDevice("alice", "t1", "ios"))
	require.NoError(t, pm.RegisterDevice("alice", "t1", "ios"))
	assert.Len(t, pm.ListDevices("alice"), 1)
}

func TestPushManager_RegisterDevice_DifferentPlatformsCoexist(t *testing.T) {
	pm := NewPushManager(nil, nil)
	require.NoError(t, pm.RegisterDevice("alice", "t1", "ios"))
	require.NoError(t, pm.RegisterDevice("alice", "t1", "android"))
	assert.Len(t, pm.ListDevices("alice"), 2)
}

func TestPushManager_RegisterDevice_EmptyUserID(t *testing.T) {
	pm := NewPushManager(nil, nil)
	err := pm.RegisterDevice("", "t", "ios")
	assert.ErrorIs(t, err, ErrEmptyUserID)
}

func TestPushManager_RegisterDevice_EmptyToken(t *testing.T) {
	pm := NewPushManager(nil, nil)
	err := pm.RegisterDevice("u", "", "ios")
	assert.ErrorIs(t, err, ErrEmptyDeviceToken)
}

func TestPushManager_RegisterDevice_UnknownPlatform(t *testing.T) {
	pm := NewPushManager(nil, nil)
	err := pm.RegisterDevice("u", "t", "windows-phone")
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrUnknownPlatform)
}

// --- UnregisterDevice ------------------------------------------------------

func TestPushManager_UnregisterDevice_Success(t *testing.T) {
	pm := NewPushManager(nil, nil)
	require.NoError(t, pm.RegisterDevice("alice", "t1", "ios"))
	require.NoError(t, pm.RegisterDevice("alice", "t2", "android"))
	require.NoError(t, pm.UnregisterDevice("alice", "t1"))
	devices := pm.ListDevices("alice")
	require.Len(t, devices, 1)
	assert.Equal(t, "t2", devices[0].Token)
}

func TestPushManager_UnregisterDevice_NotFound(t *testing.T) {
	pm := NewPushManager(nil, nil)
	require.NoError(t, pm.RegisterDevice("alice", "t1", "ios"))
	err := pm.UnregisterDevice("alice", "no-such-token")
	assert.ErrorIs(t, err, ErrDeviceNotFound)
}

func TestPushManager_UnregisterDevice_LastDeviceDeletesUser(t *testing.T) {
	pm := NewPushManager(nil, nil)
	require.NoError(t, pm.RegisterDevice("alice", "t1", "ios"))
	require.NoError(t, pm.UnregisterDevice("alice", "t1"))
	assert.Empty(t, pm.ListDevices("alice"))
}

func TestPushManager_UnregisterDevice_EmptyArgs(t *testing.T) {
	pm := NewPushManager(nil, nil)
	assert.ErrorIs(t, pm.UnregisterDevice("", "t"), ErrEmptyUserID)
	assert.ErrorIs(t, pm.UnregisterDevice("u", ""), ErrEmptyDeviceToken)
}

// --- ListDevices -----------------------------------------------------------

func TestPushManager_ListDevices_ReturnsCopy(t *testing.T) {
	pm := NewPushManager(nil, nil)
	require.NoError(t, pm.RegisterDevice("alice", "t1", "ios"))
	devices := pm.ListDevices("alice")
	devices[0].Token = "mutated"
	// Original is unaffected.
	assert.Equal(t, "t1", pm.ListDevices("alice")[0].Token)
}

func TestPushManager_ListDevices_UnknownUserReturnsNil(t *testing.T) {
	pm := NewPushManager(nil, nil)
	assert.Nil(t, pm.ListDevices("nobody"))
}

// --- Send ------------------------------------------------------------------

func TestPushManager_Send_NoDevicesReturnsErrDeviceNotFound(t *testing.T) {
	pm := NewPushManager(nil, nil)
	err := pm.Send(context.Background(), PushMessage{UserID: "nobody", Title: "t"})
	assert.ErrorIs(t, err, ErrDeviceNotFound)
}

func TestPushManager_Send_EmptyUserID(t *testing.T) {
	pm := NewPushManager(nil, nil)
	err := pm.Send(context.Background(), PushMessage{UserID: "", Title: "t"})
	assert.ErrorIs(t, err, ErrEmptyUserID)
}

func TestPushManager_Send_RoutesIOSAndAndroid(t *testing.T) {
	pemBytes, _ := generateTestECDSAKey(t)
	apns, err := NewAPNSClient("t", "k", "b", pemBytes, false)
	require.NoError(t, err)
	fcm := NewFCMClient("proj", nil)
	fcm.SetAccessTokenForTest("tok", time.Now().Add(time.Hour))

	var apnsCount, fcmCount atomic.Int32
	apnsSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		apnsCount.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer apnsSrv.Close()
	apns.endpoint = apnsSrv.URL

	fcmSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fcmCount.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer fcmSrv.Close()
	fcm.SetEndpointForTest(fcmSrv.URL)

	pm := NewPushManager(apns, fcm)
	require.NoError(t, pm.RegisterDevice("alice", "ios-token", "ios"))
	require.NoError(t, pm.RegisterDevice("alice", "android-token", "android"))

	err = pm.Send(context.Background(), PushMessage{
		UserID:   "alice",
		Title:    "审批请求",
		Body:     "run-1 待审批",
		Data:     map[string]string{"run_id": "run-1"},
		Category: "APPROVE",
	})
	require.NoError(t, err)
	assert.Equal(t, int32(1), apnsCount.Load())
	assert.Equal(t, int32(1), fcmCount.Load())
}

func TestPushManager_Send_PartialFailureReturnsAggregateError(t *testing.T) {
	pemBytes, _ := generateTestECDSAKey(t)
	apns, err := NewAPNSClient("t", "k", "b", pemBytes, false)
	require.NoError(t, err)

	apnsSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"reason":"BadDeviceToken"}`))
	}))
	defer apnsSrv.Close()
	apns.endpoint = apnsSrv.URL

	pm := NewPushManager(apns, nil)
	require.NoError(t, pm.RegisterDevice("alice", "ios-1", "ios"))
	require.NoError(t, pm.RegisterDevice("alice", "ios-2", "ios"))

	err = pm.Send(context.Background(), PushMessage{UserID: "alice", Title: "t"})
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrPushFailed)
}

func TestPushManager_Send_NilClientsSkipped(t *testing.T) {
	// Manager with nil APNs/FCM; sending to registered devices should not
	// error, just log a warning and skip.
	pm := NewPushManager(nil, nil)
	require.NoError(t, pm.RegisterDevice("alice", "t1", "ios"))
	require.NoError(t, pm.RegisterDevice("alice", "t2", "android"))
	err := pm.Send(context.Background(), PushMessage{UserID: "alice", Title: "t"})
	require.NoError(t, err)
}

// --- SendToUser ------------------------------------------------------------

func TestPushManager_SendToUser_DelegatesToSend(t *testing.T) {
	pm := NewPushManager(nil, nil)
	err := pm.SendToUser(context.Background(), "nobody", "t", "b", nil)
	assert.ErrorIs(t, err, ErrDeviceNotFound)
}

// --- normalisePlatform -----------------------------------------------------

func TestNormalisePlatform_LowerCases(t *testing.T) {
	assert.Equal(t, "ios", normalisePlatform("iOS"))
	assert.Equal(t, "android", normalisePlatform("ANDROID"))
	assert.Equal(t, "ios", normalisePlatform("ios"))
	assert.Equal(t, "", normalisePlatform(""))
}

// --- toStringInterfaceMap --------------------------------------------------

func TestToStringInterfaceMap_EmptyReturnsNil(t *testing.T) {
	assert.Nil(t, toStringInterfaceMap(nil))
	assert.Nil(t, toStringInterfaceMap(map[string]string{}))
}

func TestToStringInterfaceMap_PreservesEntries(t *testing.T) {
	m := map[string]string{"a": "1", "b": "2"}
	out := toStringInterfaceMap(m)
	assert.Equal(t, "1", out["a"])
	assert.Equal(t, "2", out["b"])
}
