package main

// serve_notify_test.go — D-20 的装配契约。
//
// 断言不停在"管理器构造出来了"，而是把一条真实回滚通知打到 httptest 接收端，
// 校验事件头、HMAC 签名与 metadata：notify.webhook.enabled 曾经是一个背后没有
// 接线的开关，这类缺陷只能靠"真的收到投递"来钉死。最后一条结构测试守的正是
// 开关与装配点断线的回归。

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nexus/levee/internal/config"
	"github.com/nexus/levee/internal/notify"
)

type webhookHit struct {
	event string
	sig   string
	raw   []byte
	msg   notify.Message
}

func serveCapturingWebhook(t *testing.T, hits chan<- webhookHit) *httptest.Server {
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
		hits <- webhookHit{
			event: r.Header.Get(notify.HeaderEvent),
			sig:   r.Header.Get(notify.HeaderSignature),
			raw:   body,
			msg:   msg,
		}
		w.WriteHeader(http.StatusNoContent)
	}))
}

func webhookNotifyConfig(url string) config.NotifyConfig {
	return config.NotifyConfig{Webhook: config.WebhookConfig{
		Enabled: true,
		URL:     url,
		Timeout: 2 * time.Second,
		Retry:   1,
	}}
}

func TestBuildServeNotifyManagerDisabledInstallsNothing(t *testing.T) {
	mgr, err := buildServeNotifyManager(config.NotifyConfig{})
	require.NoError(t, err)
	assert.Nil(t, mgr, "a disabled channel must not produce a manager; the engine's log-only path is the honest default")
}

func TestBuildServeNotifyManagerRefusesChannelThatCannotDeliver(t *testing.T) {
	cfg := webhookNotifyConfig("")
	mgr, err := buildServeNotifyManager(cfg)
	require.Error(t, err)
	assert.Nil(t, mgr)
	assert.Contains(t, err.Error(), "notify.webhook")
	assert.ErrorIs(t, err, notify.ErrEmptyURL)
}

func TestBuildServeNotifyManagerRegistersNamedChannel(t *testing.T) {
	hits := make(chan webhookHit, 4)
	srv := serveCapturingWebhook(t, hits)
	defer srv.Close()

	mgr, err := buildServeNotifyManager(webhookNotifyConfig(srv.URL))
	require.NoError(t, err)
	require.NotNil(t, mgr)
	assert.True(t, mgr.Has(webhookChannelName))
	assert.Equal(t, []string{webhookChannelName}, mgr.Names())
}

// TestServeNotifyDeliversRollbackGradeOverSignedWebhook is the delivery proof:
// the assembled manager, used the way the engine uses it (notify.RollbackNotifier
// → grade notification), must land an HMAC-signed payload the receiver can verify.
func TestServeNotifyDeliversRollbackGradeOverSignedWebhook(t *testing.T) {
	const secret = "s3cr3t-for-test"
	// 字面量而不是常量：环境变量名本身是对操作者的契约（config.example.yaml 与
	// CHANGELOG 都写了它），改名必须让这条测试变红。
	t.Setenv("LEVEE_WEBHOOK_SECRET", secret)

	hits := make(chan webhookHit, 4)
	srv := serveCapturingWebhook(t, hits)
	defer srv.Close()

	mgr, err := buildServeNotifyManager(webhookNotifyConfig(srv.URL))
	require.NoError(t, err)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	require.NoError(t, notify.NewRollbackNotifier(mgr).NotifyFailed(
		ctx, "run-7", "alice", "bob", "", "1 of 3 compensations completed"))

	var hit webhookHit
	select {
	case hit = <-hits:
	case <-time.After(5 * time.Second):
		t.Fatal("webhook receiver never got the rollback notification")
	}

	assert.Equal(t, string(notify.TriggerRollbackFailed), hit.event)
	assert.Equal(t, "run-7", hit.msg.Metadata["run_id"])
	assert.Equal(t, "alice", hit.msg.Metadata["initiator"])
	assert.Equal(t, "rollback", hit.msg.Metadata["scope"])

	want := notify.SignaturePrefix + sign(secret, hit.raw)
	assert.True(t, strings.HasPrefix(hit.sig, notify.SignaturePrefix),
		"signature header must be prefixed, got %q", hit.sig)
	assert.True(t, hmac.Equal([]byte(want), []byte(hit.sig)),
		"payload signature does not verify with %s", envWebhookSecret)
}

// TestServeNotifyUnsignedWhenNoSecret pins the other posture: no secret in the
// environment means the boot still works, payloads go out unsigned, and no
// bogus signature header is attached.
func TestServeNotifyUnsignedWhenNoSecret(t *testing.T) {
	t.Setenv("LEVEE_WEBHOOK_SECRET", "")

	hits := make(chan webhookHit, 4)
	srv := serveCapturingWebhook(t, hits)
	defer srv.Close()

	mgr, err := buildServeNotifyManager(webhookNotifyConfig(srv.URL))
	require.NoError(t, err)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	require.NoError(t, notify.NewRollbackNotifier(mgr).NotifyPartial(
		ctx, "run-8", "alice", "", "", "partial restore"))

	select {
	case hit := <-hits:
		assert.Empty(t, hit.sig, "no secret configured means no signature header, not a fake one")
	case <-time.After(5 * time.Second):
		t.Fatal("unsigned webhook delivery never reached the receiver")
	}
}

// TestNotifyWebhookSwitchHasAssemblyPoint is the structural guard. The defect
// this repo keeps flushing out is a config switch whose consumer was never
// written: notify.webhook had the notifier, the keys and the validation, and no
// serve-side assembly. cmd_serve.go must both consult the switch and install the
// manager into the engine, or the config is lying again.
func TestNotifyWebhookSwitchHasAssemblyPoint(t *testing.T) {
	src, err := os.ReadFile("cmd_serve.go")
	require.NoError(t, err)
	body := string(src)

	assert.Contains(t, body, "buildServeNotifyManager(",
		"notify.webhook.enabled must be consumed at serve assembly time")
	assert.Contains(t, body, "wiring.WithNotificationManager(",
		"the assembled manager must actually reach the engine options")
}

func sign(secret string, payload []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(payload)
	return hex.EncodeToString(mac.Sum(nil))
}
