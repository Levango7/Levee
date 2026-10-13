package chatops

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// captureBot records every IncomingMessage routed through HandleMessage so
// the ingress tests can assert not only the status code but whether the
// decision would have been dispatched at all.
type captureBot struct {
	fakeBot
	mu   sync.Mutex
	msgs []IncomingMessage
}

func newCaptureBot(name string, p Platform) *captureBot {
	return &captureBot{fakeBot: *newFakeBot(name, p)}
}

func (b *captureBot) HandleMessage(_ context.Context, msg IncomingMessage) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.msgs = append(b.msgs, msg)
	return nil
}

func (b *captureBot) received() []IncomingMessage {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]IncomingMessage(nil), b.msgs...)
}

const testSecret = "s3cr3t"

func signSlack(secret string, ts string, body []byte) (sig, timestamp string) {
	mac := hmac.New(sha256.New, []byte("v0:"+secret))
	mac.Write([]byte("v0:" + ts + ":"))
	mac.Write(body)
	return "v0=" + hex.EncodeToString(mac.Sum(nil)), ts
}

func signDDish(secret, ts string) string {
	mac := hmac.New(sha256.New, []byte(ts+"\n"+secret))
	mac.Write([]byte(secret))
	return hex.EncodeToString(mac.Sum(nil))
}

func nowSec() string   { return strconvItoa(time.Now().Unix()) }
func nowMilli() string { return strconvItoa(time.Now().UnixMilli()) }

func strconvItoa(n int64) string { return fmt.Sprintf("%d", n) }

func postCallback(t *testing.T, h http.Handler, headers map[string]string, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/chatops/callback/bot", strings.NewReader(body))
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// --- constructor -----------------------------------------------------------

func TestCallbackHandler_RefusesWithoutSecret(t *testing.T) {
	bot := newCaptureBot("b1", PlatformSlack)
	_, err := CallbackHandler(bot, "", nil)
	assert.ErrorIs(t, err, ErrCallbackSecretMissing,
		"an ingress that trusts every caller is worse than no ingress")
}

// --- Slack -----------------------------------------------------------------

func TestSlackCallback_ValidSignatureDispatches(t *testing.T) {
	bot := newCaptureBot("slack-bot", PlatformSlack)
	h, err := CallbackHandler(bot, testSecret, nil)
	require.NoError(t, err)

	body := `{"command":"/levee","text":"approve run-42","user_id":"U1","user_name":"alice","channel_id":"C1"}`
	ts := nowSec()
	sig, _ := signSlack(testSecret, ts, []byte(body))
	rec := postCallback(t, h, map[string]string{
		"X-Slack-Request-Timestamp": ts,
		"X-Slack-Signature":         sig,
	}, body)

	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
	msgs := bot.received()
	require.Len(t, msgs, 1)
	assert.Equal(t, "/levee approve run-42", msgs[0].Text)
	assert.Equal(t, "U1", msgs[0].User)
	assert.Equal(t, PlatformSlack, msgs[0].Platform)
}

func TestSlackCallback_TamperedBodyRejected(t *testing.T) {
	bot := newCaptureBot("slack-bot", PlatformSlack)
	h, err := CallbackHandler(bot, testSecret, nil)
	require.NoError(t, err)

	body := `{"command":"/levee","text":"approve run-42","user_id":"U1"}`
	ts := nowSec()
	sig, _ := signSlack(testSecret, ts, []byte(body))
	rec := postCallback(t, h, map[string]string{
		"X-Slack-Request-Timestamp": ts,
		"X-Slack-Signature":         sig,
	}, body+` `) // one space of tampering

	require.Equal(t, http.StatusUnauthorized, rec.Code)
	assert.Empty(t, bot.received(), "a forged decision must never reach the router")
}

func TestSlackCallback_WrongSecretRejected(t *testing.T) {
	bot := newCaptureBot("slack-bot", PlatformSlack)
	h, err := CallbackHandler(bot, testSecret, nil)
	require.NoError(t, err)

	body := `{"command":"/levee","text":"approve run-42","user_id":"U1"}`
	ts := nowSec()
	sig, _ := signSlack("another-secret", ts, []byte(body))
	rec := postCallback(t, h, map[string]string{
		"X-Slack-Request-Timestamp": ts,
		"X-Slack-Signature":         sig,
	}, body)

	require.Equal(t, http.StatusUnauthorized, rec.Code)
	assert.Empty(t, bot.received())
}

func TestSlackCallback_StaleTimestampRejected(t *testing.T) {
	bot := newCaptureBot("slack-bot", PlatformSlack)
	h, err := CallbackHandler(bot, testSecret, nil)
	require.NoError(t, err)

	body := `{"command":"/levee","text":"approve run-42","user_id":"U1"}`
	ts := strconvItoa(time.Now().Add(-time.Hour).Unix())
	sig, _ := signSlack(testSecret, ts, []byte(body))
	rec := postCallback(t, h, map[string]string{
		"X-Slack-Request-Timestamp": ts,
		"X-Slack-Signature":         sig,
	}, body)

	require.Equal(t, http.StatusUnauthorized, rec.Code)
	assert.Empty(t, bot.received(), "a captured callback must not replay an hour later")
}

func TestSlackCallback_MissingHeadersRejected(t *testing.T) {
	bot := newCaptureBot("slack-bot", PlatformSlack)
	h, err := CallbackHandler(bot, testSecret, nil)
	require.NoError(t, err)

	rec := postCallback(t, h, nil, `{"command":"/levee","text":"approve run-42"}`)
	require.Equal(t, http.StatusUnauthorized, rec.Code)
	assert.Empty(t, bot.received())
}

// --- DingTalk --------------------------------------------------------------

func TestDingtalkCallback_ValidSignatureDispatches(t *testing.T) {
	bot := newCaptureBot("dd-bot", PlatformDingtalk)
	h, err := CallbackHandler(bot, testSecret, nil)
	require.NoError(t, err)

	body := `{"text":{"content":"@bot /levee approve run-7"},"senderStaffId":"123","senderNick":"bob","conversationId":"cid-1"}`
	ts := nowMilli()
	rec := postCallback(t, h, map[string]string{
		"timestamp": ts,
		"sign":      signDDish(testSecret, ts),
	}, body)

	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
	msgs := bot.received()
	require.Len(t, msgs, 1)
	assert.Equal(t, "/levee approve run-7", msgs[0].Text, "@-mentions are stripped by the adapter")
	assert.Equal(t, "123", msgs[0].User)
}

func TestDingtalkCallback_ForgedSignatureRejected(t *testing.T) {
	bot := newCaptureBot("dd-bot", PlatformDingtalk)
	h, err := CallbackHandler(bot, testSecret, nil)
	require.NoError(t, err)

	ts := nowMilli()
	rec := postCallback(t, h, map[string]string{
		"timestamp": ts,
		"sign":      "deadbeef",
	}, `{"text":{"content":"/levee approve run-7"},"senderStaffId":"123"}`)

	require.Equal(t, http.StatusUnauthorized, rec.Code)
	assert.Empty(t, bot.received())
}

// --- Feishu ----------------------------------------------------------------

func TestFeishuCallback_ValidSignatureDispatches(t *testing.T) {
	bot := newCaptureBot("fs-bot", PlatformFeishu)
	h, err := CallbackHandler(bot, testSecret, nil)
	require.NoError(t, err)

	body := `{"event":{"message":{"content":"{\"text\":\"@_user_1 /levee reject run-9 --reason 窗口冲突\"}","chat_id":"oc_1"},"sender":{"sender_id":{"open_id":"ou_1"},"sender_nick":"carol"}}}`
	ts := nowMilli()
	rec := postCallback(t, h, map[string]string{
		"X-Lark-Request-Timestamp": ts,
		"X-Lark-Signature":         signDDish(testSecret, ts),
	}, body)

	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
	msgs := bot.received()
	require.Len(t, msgs, 1)
	assert.Equal(t, "/levee reject run-9 --reason 窗口冲突", msgs[0].Text)
	assert.Equal(t, "ou_1", msgs[0].User)
}

func TestFeishuCallback_StaleTimestampRejected(t *testing.T) {
	bot := newCaptureBot("fs-bot", PlatformFeishu)
	h, err := CallbackHandler(bot, testSecret, nil)
	require.NoError(t, err)

	ts := strconvItoa(time.Now().Add(-2 * time.Hour).UnixMilli())
	body := `{"event":{"message":{"content":"{\"text\":\"/levee approve run-9\"}"},"sender":{"sender_id":{"open_id":"ou_1"}}}}`
	rec := postCallback(t, h, map[string]string{
		"X-Lark-Request-Timestamp": ts,
		"X-Lark-Signature":         signDDish(testSecret, ts),
	}, body)

	require.Equal(t, http.StatusUnauthorized, rec.Code)
	assert.Empty(t, bot.received())
}

func TestFeishuCallback_UrlVerificationChallengeEchoed(t *testing.T) {
	bot := newCaptureBot("fs-bot", PlatformFeishu)
	h, err := CallbackHandler(bot, testSecret, nil)
	require.NoError(t, err)

	body := `{"type":"url_verification","challenge":"abc123","token":"v"}`
	ts := nowMilli()
	rec := postCallback(t, h, map[string]string{
		"X-Lark-Request-Timestamp": ts,
		"X-Lark-Signature":         signDDish(testSecret, ts),
	}, body)

	require.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), `"challenge":"abc123"`)
	assert.Empty(t, bot.received(), "a challenge is endpoint setup, not a command")
}

// --- body bound ------------------------------------------------------------

func TestCallbackHandler_OversizedBodyRefused(t *testing.T) {
	bot := newCaptureBot("slack-bot", PlatformSlack)
	h, err := CallbackHandler(bot, testSecret, nil)
	require.NoError(t, err)

	huge := `{"command":"/levee","text":"` + strings.Repeat("x", maxCallbackBytes+1024) + `"}`
	ts := nowSec()
	sig, _ := signSlack(testSecret, ts, []byte(huge))
	rec := postCallback(t, h, map[string]string{
		"X-Slack-Request-Timestamp": ts,
		"X-Slack-Signature":         sig,
	}, huge)

	assert.Equal(t, http.StatusRequestEntityTooLarge, rec.Code)
	assert.Empty(t, bot.received())
}

func TestCallbackHandler_UnparseableBodyIs400(t *testing.T) {
	bot := newCaptureBot("slack-bot", PlatformSlack)
	h, err := CallbackHandler(bot, testSecret, nil)
	require.NoError(t, err)

	body := `not json at all`
	ts := nowSec()
	sig, _ := signSlack(testSecret, ts, []byte(body))
	rec := postCallback(t, h, map[string]string{
		"X-Slack-Request-Timestamp": ts,
		"X-Slack-Signature":         sig,
	}, body)

	require.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Empty(t, bot.received())
}

// --- freshness parsing -----------------------------------------------------

func TestCheckFreshness_MalformedTimestampFailsClosed(t *testing.T) {
	err := checkFreshness("not-a-number", time.Minute, 1)
	assert.ErrorIs(t, err, ErrCallbackStaleTimestamp)
}

func TestDecodeChallenge_NonChallengeBodyReadsEmpty(t *testing.T) {
	assert.Equal(t, "", decodeChallenge([]byte(`{"type":"event_callback","challenge":"x"}`)))
	assert.Equal(t, "", decodeChallenge([]byte(`{"command":"/levee"}`)))
	assert.Equal(t, "", decodeChallenge([]byte(`not json`)))
	var probe struct {
		Challenge string `json:"challenge"`
	}
	require.NoError(t, json.Unmarshal([]byte(`{"challenge":"y"}`), &probe))
	assert.Equal(t, "y", probe.Challenge)
}
