package wiring

// rollback_notify_transport_test.go — 装上真实渠道后，回滚分级必须真的投递出去。
//
// 同族的 rollback_grade_test.go 用 fakeSink 证明"分级在正确的 grade 上派发、缺
// 传输层时降级为记日志"。这里补它没覆盖的那一段：sink 背后的
// notify.RollbackNotifier → NotificationManager → WebhookNotifier → HTTP 接收端，
// 一整条真实链路。另两个降级条件也在这里钉住，因为"有传输却寄给一个编造的收件人"
// 比不寄更坏（操作员会停止追查）。

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nexus/levee/internal/notify"
	"github.com/nexus/levee/internal/rollback"
)

type deliveredGrade struct {
	event string
	msg   notify.Message
}

func captureGrades(t *testing.T, out chan<- deliveredGrade) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, "read body", http.StatusBadRequest)
			return
		}
		var msg notify.Message
		if err := json.Unmarshal(body, &msg); err != nil {
			http.Error(w, "bad json", http.StatusBadRequest)
			return
		}
		out <- deliveredGrade{event: r.Header.Get(notify.HeaderEvent), msg: msg}
		w.WriteHeader(http.StatusNoContent)
	}))
}

// engineWithWebhookTransport builds the engine the way serve does once
// notify.webhook is configured: a real notifier registered on a real manager.
func engineWithWebhookTransport(t *testing.T, url string) *Engine {
	t.Helper()
	n, err := notify.NewWebhookNotifier(notify.WebhookConfig{
		Name:       "webhook",
		URL:        url,
		Timeout:    2 * time.Second,
		MaxRetries: 0,
	})
	require.NoError(t, err)
	mgr := notify.NewNotificationManager()
	require.NoError(t, mgr.Register(n))
	return NewEngine(nil, WithNotificationManager(mgr))
}

func TestNewRunNotifySinkDeliversFailureGradeOverTransport(t *testing.T) {
	hits := make(chan deliveredGrade, 4)
	srv := captureGrades(t, hits)
	defer srv.Close()

	e := engineWithWebhookTransport(t, srv.URL)
	sink := e.newRunNotifySink(RollbackActors{Initiator: "alice", Approver: "bob"})
	require.NotNil(t, sink, "configured transport plus a known initiator must yield a live sink")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	require.NoError(t, sink.NotifyGrade(ctx, rollback.GradeFailure, "run-12", "1 of 3 compensations completed"))

	select {
	case got := <-hits:
		assert.Equal(t, string(notify.TriggerRollbackFailed), got.event)
		assert.Equal(t, "run-12", got.msg.Metadata["run_id"])
		assert.Equal(t, "alice", got.msg.Metadata["initiator"])
		assert.Equal(t, "bob", got.msg.Metadata["approver"])
	case <-time.After(5 * time.Second):
		t.Fatal("failure grade never reached the webhook receiver")
	}
}

func TestNewRunNotifySinkStaysNilWithoutTransport(t *testing.T) {
	e := NewEngine(nil)
	assert.Nil(t, e.newRunNotifySink(RollbackActors{Initiator: "alice"}),
		"no manager installed means the log-only path, not a sink that drops messages")
}

func TestNewRunNotifySinkStaysNilWithoutInitiator(t *testing.T) {
	hits := make(chan deliveredGrade, 4)
	srv := captureGrades(t, hits)
	defer srv.Close()

	e := engineWithWebhookTransport(t, srv.URL)
	assert.Nil(t, e.newRunNotifySink(RollbackActors{}),
		"a transport with an unknown initiator must suppress rather than address an invented recipient")

	select {
	case got := <-hits:
		t.Fatalf("nothing should have been delivered, got event %q", got.event)
	case <-time.After(200 * time.Millisecond):
	}
}
