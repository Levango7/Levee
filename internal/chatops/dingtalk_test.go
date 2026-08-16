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

func TestNewDingtalkBot_Validation(t *testing.T) {
	_, err := NewDingtalkBot(DingtalkConfig{Name: "", WebhookURL: "http://x"}, nil)
	require.Error(t, err)
	assert.True(t, errIs(err, ErrBotNotFound))

	_, err = NewDingtalkBot(DingtalkConfig{Name: "d", WebhookURL: ""}, nil)
	require.Error(t, err)
	assert.True(t, errIs(err, ErrEmptyWebhookURL))
}

func TestNewDingtalkBot_Defaults(t *testing.T) {
	b, err := NewDingtalkBot(DingtalkConfig{Name: "d", WebhookURL: "http://x"}, nil)
	require.NoError(t, err)
	assert.Equal(t, "d", b.Name())
	assert.Equal(t, PlatformDingtalk, b.Platform())
}

// --- Start / Stop ---------------------------------------------------------

func TestDingtalkBot_StartStop(t *testing.T) {
	b, err := NewDingtalkBot(DingtalkConfig{Name: "d", WebhookURL: "http://x"}, nil)
	require.NoError(t, err)

	require.NoError(t, b.Start(context.Background()))
	require.Error(t, b.Start(context.Background()))
	require.NoError(t, b.Stop())
	require.NoError(t, b.Stop())
}

// --- HTTP server helper ---------------------------------------------------

func newDingtalkTestServer(t *testing.T) (*httptest.Server, *atomic.Value) {
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

// --- SendMessage / SendCard -----------------------------------------------

func TestDingtalkBot_SendMessage(t *testing.T) {
	srv, last := newDingtalkTestServer(t)
	b, err := NewDingtalkBot(DingtalkConfig{Name: "d", WebhookURL: srv.URL, Timeout: time.Second}, nil)
	require.NoError(t, err)

	require.NoError(t, b.SendMessage(context.Background(), "ch", "hello"))
	body := last.Load().(string)
	assert.Contains(t, body, "hello")
	assert.Contains(t, body, "text")
}

func TestDingtalkBot_SendMessage_EmptyIsNoop(t *testing.T) {
	srv, last := newDingtalkTestServer(t)
	b, err := NewDingtalkBot(DingtalkConfig{Name: "d", WebhookURL: srv.URL, Timeout: time.Second}, nil)
	require.NoError(t, err)

	require.NoError(t, b.SendMessage(context.Background(), "ch", "   "))
	assert.Nil(t, last.Load())
}

func TestDingtalkBot_SendCard(t *testing.T) {
	srv, last := newDingtalkTestServer(t)
	b, err := NewDingtalkBot(DingtalkConfig{Name: "d", WebhookURL: srv.URL, Timeout: time.Second}, nil)
	require.NoError(t, err)

	card := BuildApprovalCard(Event{Type: EventApprovalRequested, ChangeID: "ch-1", Title: "T", Summary: "S"})
	require.NoError(t, b.SendCard(context.Background(), "ch", card))

	body := last.Load().(string)
	assert.Contains(t, body, "actionCard")
	assert.Contains(t, body, "T")
}

func TestDingtalkBot_SendCard_NilIsNoop(t *testing.T) {
	srv, last := newDingtalkTestServer(t)
	b, err := NewDingtalkBot(DingtalkConfig{Name: "d", WebhookURL: srv.URL, Timeout: time.Second}, nil)
	require.NoError(t, err)

	require.NoError(t, b.SendCard(context.Background(), "ch", nil))
	assert.Nil(t, last.Load())
}

// --- Event delivery -------------------------------------------------------

func TestDingtalkBot_DeliversEventOnStart(t *testing.T) {
	srv, last := newDingtalkTestServer(t)
	b, err := NewDingtalkBot(DingtalkConfig{
		Name: "d", WebhookURL: srv.URL, Timeout: time.Second, EventBuffer: 4,
	}, nil)
	require.NoError(t, err)

	require.NoError(t, b.Start(context.Background()))
	defer b.Stop()

	require.NoError(t, b.PublishEvent(Event{
		Type: EventApprovalRequested, RunID: "run-1", ChangeID: "ch-1", Title: "T",
	}))

	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if v := last.Load(); v != nil && strings.Contains(v.(string), "actionCard") {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	body, ok := last.Load().(string)
	require.True(t, ok)
	assert.Contains(t, body, "actionCard")
}

// --- ParseDingtalkCallback ------------------------------------------------

func TestParseDingtalkCallback_TextMessage(t *testing.T) {
	body, _ := json.Marshal(map[string]any{
		"msgtype":        "text",
		"text":           map[string]any{"content": "@bot /levee approve ch-1"},
		"senderStaffId":  "u-1",
		"senderNick":     "bob",
		"conversationId": "c-1",
	})

	msg, err := ParseDingtalkCallback(body)
	require.NoError(t, err)
	assert.Equal(t, PlatformDingtalk, msg.Platform)
	assert.Equal(t, "u-1", msg.User)
	assert.Equal(t, "bob", msg.UserName)
	assert.Equal(t, "c-1", msg.Channel)
	assert.Equal(t, "/levee approve ch-1", msg.Text)
}

func TestParseDingtalkCallback_ActionCard(t *testing.T) {
	body, _ := json.Marshal(map[string]any{
		"actionCard": map[string]any{
			"title":     "审批",
			"text":      "summary",
			"actionURL": "/levee approve ch-2",
		},
	})

	msg, err := ParseDingtalkCallback(body)
	require.NoError(t, err)
	assert.Equal(t, "/levee approve ch-2", msg.Text)
}

func TestParseDingtalkCallback_InvalidJSON(t *testing.T) {
	_, err := ParseDingtalkCallback([]byte("xxx"))
	require.Error(t, err)
}

func TestStripDingtalkMentions(t *testing.T) {
	assert.Equal(t, "/levee list", stripDingtalkMentions("@bot /levee list"))
	assert.Equal(t, "/levee approve ch-1", stripDingtalkMentions("@all /levee approve ch-1"))
	assert.Equal(t, "", stripDingtalkMentions("@bot @all"))
}

// --- Approval ActionCard --------------------------------------------------

func TestDingtalkApprovalActionCard(t *testing.T) {
	payload := DingtalkApprovalActionCard("ch-9", "审批", "summary")
	assert.Equal(t, "actionCard", payload["msgtype"])
	ac := payload["actionCard"].(map[string]any)
	btns := ac["btns"].([]map[string]any)
	require.Len(t, btns, 2)
	assert.Equal(t, "通过", btns[0]["title"])
	assert.Contains(t, btns[0]["actionURL"], "/levee approve ch-9")
	assert.Equal(t, "驳回", btns[1]["title"])
	assert.Contains(t, btns[1]["actionURL"], "/levee reject ch-9")
}

// --- HandleMessage --------------------------------------------------------

func TestDingtalkBot_HandleMessage_Help(t *testing.T) {
	srv, last := newDingtalkTestServer(t)
	b, err := NewDingtalkBot(DingtalkConfig{Name: "d", WebhookURL: srv.URL, Timeout: time.Second}, nil)
	require.NoError(t, err)

	require.NoError(t, b.HandleMessage(context.Background(), IncomingMessage{
		Platform: PlatformDingtalk,
		Text:     "/levee help",
	}))
	body, ok := last.Load().(string)
	require.True(t, ok)
	assert.Contains(t, body, "available commands")
}