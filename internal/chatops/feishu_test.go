package chatops

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// --- Construction ---------------------------------------------------------

func TestNewFeishuBot_Validation(t *testing.T) {
	_, err := NewFeishuBot(FeishuConfig{Name: "", WebhookURL: "http://x"}, nil)
	require.Error(t, err)
	assert.True(t, errIs(err, ErrBotNotFound))

	_, err = NewFeishuBot(FeishuConfig{Name: "f", WebhookURL: ""}, nil)
	require.Error(t, err)
	assert.True(t, errIs(err, ErrEmptyWebhookURL))
}

func TestNewFeishuBot_Defaults(t *testing.T) {
	b, err := NewFeishuBot(FeishuConfig{Name: "f", WebhookURL: "http://x"}, nil)
	require.NoError(t, err)
	assert.Equal(t, "f", b.Name())
	assert.Equal(t, PlatformFeishu, b.Platform())
	assert.NotNil(t, b.client)
}

// --- Start / Stop ---------------------------------------------------------

func TestFeishuBot_StartStop(t *testing.T) {
	b, err := NewFeishuBot(FeishuConfig{Name: "f", WebhookURL: "http://x"}, nil)
	require.NoError(t, err)

	require.NoError(t, b.Start(context.Background()))
	// Starting twice should fail.
	require.Error(t, b.Start(context.Background()))
	require.NoError(t, b.Stop())
	// Stopping twice is a no-op (closeOnce guards).
	require.NoError(t, b.Stop())
}

// --- SendMessage / SendCard over HTTP ------------------------------------

// newFeishuTestServer returns a test server that records the last request body
// and responds 200.
func newFeishuTestServer(t *testing.T) (*httptest.Server, *atomic.Value) {
	t.Helper()
	var lastBody atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		lastBody.Store(string(body))
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	return srv, &lastBody
}

func TestFeishuBot_SendMessage(t *testing.T) {
	srv, last := newFeishuTestServer(t)
	b, err := NewFeishuBot(FeishuConfig{Name: "f", WebhookURL: srv.URL, Timeout: time.Second}, nil)
	require.NoError(t, err)

	require.NoError(t, b.SendMessage(context.Background(), "ch", "hello"))
	body := last.Load().(string)
	assert.Contains(t, body, "hello")
	assert.Contains(t, body, "text")
}

func TestFeishuBot_SendMessage_EmptyIsNoop(t *testing.T) {
	srv, last := newFeishuTestServer(t)
	b, err := NewFeishuBot(FeishuConfig{Name: "f", WebhookURL: srv.URL, Timeout: time.Second}, nil)
	require.NoError(t, err)

	require.NoError(t, b.SendMessage(context.Background(), "ch", "  "))
	assert.Nil(t, last.Load())
}

func TestFeishuBot_SendCard(t *testing.T) {
	srv, last := newFeishuTestServer(t)
	b, err := NewFeishuBot(FeishuConfig{Name: "f", WebhookURL: srv.URL, Timeout: time.Second}, nil)
	require.NoError(t, err)

	card := BuildApprovalCard(Event{Type: EventApprovalRequested, ChangeID: "ch-1", Title: "T"})
	require.NoError(t, b.SendCard(context.Background(), "ch", card))

	body := last.Load().(string)
	assert.Contains(t, body, "interactive")
	assert.Contains(t, body, "T")
}

func TestFeishuBot_SendCard_NilIsNoop(t *testing.T) {
	srv, last := newFeishuTestServer(t)
	b, err := NewFeishuBot(FeishuConfig{Name: "f", WebhookURL: srv.URL, Timeout: time.Second}, nil)
	require.NoError(t, err)

	require.NoError(t, b.SendCard(context.Background(), "ch", nil))
	assert.Nil(t, last.Load())
}

// --- Event delivery -------------------------------------------------------

func TestFeishuBot_DeliversEventOnStart(t *testing.T) {
	srv, last := newFeishuTestServer(t)
	b, err := NewFeishuBot(FeishuConfig{
		Name: "f", WebhookURL: srv.URL, Timeout: time.Second,
		EventBuffer: 4,
	}, nil)
	require.NoError(t, err)

	require.NoError(t, b.Start(context.Background()))
	defer b.Stop()

	require.NoError(t, b.PublishEvent(Event{
		Type: EventApprovalRequested, RunID: "run-1", ChangeID: "ch-1", Title: "T",
	}))

	// Wait for delivery.
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if v := last.Load(); v != nil && strings.Contains(v.(string), "interactive") {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	body, ok := last.Load().(string)
	require.True(t, ok, "no request received")
	assert.Contains(t, body, "interactive")
}

// --- ParseFeishuCallback --------------------------------------------------

func TestParseFeishuCallback_MessageEvent(t *testing.T) {
	content, _ := json.Marshal(map[string]any{"text": "@_user_1 /levee approve ch-1"})
	body, _ := json.Marshal(map[string]any{
		"event_type": "receive_message",
		"event": map[string]any{
			"message": map[string]any{
				"content":    string(content),
				"message_id": "m-1",
				"chat_id":    "c-1",
			},
			"sender": map[string]any{
				"sender_id":  map[string]any{"open_id": "ou-1"},
				"sender_nick": "alice",
			},
		},
	})

	msg, err := ParseFeishuCallback(body)
	require.NoError(t, err)
	assert.Equal(t, PlatformFeishu, msg.Platform)
	assert.Equal(t, "ou-1", msg.User)
	assert.Equal(t, "alice", msg.UserName)
	assert.Equal(t, "c-1", msg.Channel)
	assert.Equal(t, "/levee approve ch-1", msg.Text)
}

func TestParseFeishuCallback_CardAction(t *testing.T) {
	body, _ := json.Marshal(map[string]any{
		"value":    map[string]any{"command": "/levee approve ch-2"},
		"operator": map[string]any{"open_id": "ou-2"},
	})

	msg, err := ParseFeishuCallback(body)
	require.NoError(t, err)
	assert.Equal(t, "/levee approve ch-2", msg.Text)
	assert.Equal(t, "ou-2", msg.User)
}

func TestParseFeishuCallback_InvalidJSON(t *testing.T) {
	_, err := ParseFeishuCallback([]byte("not json"))
	require.Error(t, err)
}

func TestStripFeishuMentions(t *testing.T) {
	assert.Equal(t, "/levee list", stripFeishuMentions("@_user_1 /levee list"))
	assert.Equal(t, "/levee approve ch-1", stripFeishuMentions("@_user_1 /levee approve ch-1"))
	assert.Equal(t, "", stripFeishuMentions("@_user_1 @_user_2"))
}

// --- Approval buttons -----------------------------------------------------

func TestFeishuApprovalButtons(t *testing.T) {
	btns := FeishuApprovalButtons("ch-9")
	require.Len(t, btns, 2)
	assert.Equal(t, "button", btns[0]["tag"])
	assert.Equal(t, "primary", btns[0]["type"])
	val0 := btns[0]["value"].(map[string]any)
	assert.Equal(t, "/levee approve ch-9", val0["command"])

	assert.Equal(t, "danger", btns[1]["type"])
	val1 := btns[1]["value"].(map[string]any)
	assert.Equal(t, "/levee reject ch-9", val1["command"])
}

// --- HandleMessage --------------------------------------------------------

func TestFeishuBot_HandleMessage_Help(t *testing.T) {
	srv, last := newFeishuTestServer(t)
	b, err := NewFeishuBot(FeishuConfig{Name: "f", WebhookURL: srv.URL, Timeout: time.Second}, nil)
	require.NoError(t, err)

	require.NoError(t, b.HandleMessage(context.Background(), IncomingMessage{
		Platform: PlatformFeishu,
		Text:     "/levee help",
	}))
	body, ok := last.Load().(string)
	require.True(t, ok)
	assert.Contains(t, body, "available commands")
}

func TestFeishuBot_HandleMessage_UnknownCommand(t *testing.T) {
	srv, last := newFeishuTestServer(t)
	b, err := NewFeishuBot(FeishuConfig{Name: "f", WebhookURL: srv.URL, Timeout: time.Second}, nil)
	require.NoError(t, err)

	require.NoError(t, b.HandleMessage(context.Background(), IncomingMessage{
		Platform: PlatformFeishu,
		Text:     "/levee frobnicate",
	}))
	body, ok := last.Load().(string)
	require.True(t, ok)
	assert.Contains(t, body, "unknown command")
}

// --- helpers --------------------------------------------------------------

// errIs is a small helper that uses errors.Is without forcing every test file
// to import the errors package.
func errIs(err, target error) bool {
	for err != nil {
		if err == target {
			return true
		}
		type unwrapper interface{ Unwrap() error }
		u, ok := err.(unwrapper)
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}