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

func TestNewSlackBot_Validation(t *testing.T) {
	_, err := NewSlackBot(SlackConfig{Name: "", WebhookURL: "http://x"}, nil)
	require.Error(t, err)
	assert.True(t, errIs(err, ErrBotNotFound))

	_, err = NewSlackBot(SlackConfig{Name: "s", WebhookURL: ""}, nil)
	require.Error(t, err)
	assert.True(t, errIs(err, ErrEmptyWebhookURL))
}

func TestNewSlackBot_Defaults(t *testing.T) {
	b, err := NewSlackBot(SlackConfig{Name: "s", WebhookURL: "http://x"}, nil)
	require.NoError(t, err)
	assert.Equal(t, "s", b.Name())
	assert.Equal(t, PlatformSlack, b.Platform())
}

// --- Start / Stop ---------------------------------------------------------

func TestSlackBot_StartStop(t *testing.T) {
	b, err := NewSlackBot(SlackConfig{Name: "s", WebhookURL: "http://x"}, nil)
	require.NoError(t, err)

	require.NoError(t, b.Start(context.Background()))
	require.Error(t, b.Start(context.Background()))
	require.NoError(t, b.Stop())
	require.NoError(t, b.Stop())
}

// --- HTTP server helper ---------------------------------------------------

func newSlackTestServer(t *testing.T) (*httptest.Server, *atomic.Value) {
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

func TestSlackBot_SendMessage(t *testing.T) {
	srv, last := newSlackTestServer(t)
	b, err := NewSlackBot(SlackConfig{Name: "s", WebhookURL: srv.URL, Timeout: time.Second}, nil)
	require.NoError(t, err)

	require.NoError(t, b.SendMessage(context.Background(), "C123", "hello"))
	body := last.Load().(string)
	assert.Contains(t, body, "hello")
}

func TestSlackBot_SendMessage_EmptyIsNoop(t *testing.T) {
	srv, last := newSlackTestServer(t)
	b, err := NewSlackBot(SlackConfig{Name: "s", WebhookURL: srv.URL, Timeout: time.Second}, nil)
	require.NoError(t, err)

	require.NoError(t, b.SendMessage(context.Background(), "C123", "  "))
	assert.Nil(t, last.Load())
}

func TestSlackBot_SendCard(t *testing.T) {
	srv, last := newSlackTestServer(t)
	b, err := NewSlackBot(SlackConfig{Name: "s", WebhookURL: srv.URL, Timeout: time.Second}, nil)
	require.NoError(t, err)

	card := BuildApprovalCard(Event{Type: EventApprovalRequested, ChangeID: "ch-1", Title: "T", Summary: "S"})
	require.NoError(t, b.SendCard(context.Background(), "C123", card))

	body := last.Load().(string)
	assert.Contains(t, body, "blocks")
	assert.Contains(t, body, "T")
}

func TestSlackBot_SendCard_NilIsNoop(t *testing.T) {
	srv, last := newSlackTestServer(t)
	b, err := NewSlackBot(SlackConfig{Name: "s", WebhookURL: srv.URL, Timeout: time.Second}, nil)
	require.NoError(t, err)

	require.NoError(t, b.SendCard(context.Background(), "C123", nil))
	assert.Nil(t, last.Load())
}

// --- Event delivery -------------------------------------------------------

func TestSlackBot_DeliversEventOnStart(t *testing.T) {
	srv, last := newSlackTestServer(t)
	b, err := NewSlackBot(SlackConfig{
		Name: "s", WebhookURL: srv.URL, Timeout: time.Second, EventBuffer: 4,
	}, nil)
	require.NoError(t, err)

	require.NoError(t, b.Start(context.Background()))
	defer b.Stop()

	require.NoError(t, b.PublishEvent(Event{
		Type: EventApprovalRequested, RunID: "run-1", ChangeID: "ch-1", Title: "T",
	}))

	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if v := last.Load(); v != nil && strings.Contains(v.(string), "blocks") {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	body, ok := last.Load().(string)
	require.True(t, ok)
	assert.Contains(t, body, "blocks")
}

// --- ParseSlackCallback ---------------------------------------------------

func TestParseSlackCallback_SlashCommand(t *testing.T) {
	body, _ := json.Marshal(map[string]any{
		"token":       "x",
		"team_id":     "T1",
		"channel_id":  "C123",
		"user_id":     "U1",
		"user_name":   "alice",
		"command":     "/levee",
		"text":        "approve ch-1",
		"response_url": "http://x",
	})

	msg, err := ParseSlackCallback(body)
	require.NoError(t, err)
	assert.Equal(t, PlatformSlack, msg.Platform)
	assert.Equal(t, "U1", msg.User)
	assert.Equal(t, "alice", msg.UserName)
	assert.Equal(t, "C123", msg.Channel)
	assert.Equal(t, "/levee approve ch-1", msg.Text)
}

func TestParseSlackCallback_SlashCommandNoTail(t *testing.T) {
	body, _ := json.Marshal(map[string]any{
		"command": "/levee",
	})
	msg, err := ParseSlackCallback(body)
	require.NoError(t, err)
	assert.Equal(t, "/levee", msg.Text)
}

func TestParseSlackCallback_InteractiveAction(t *testing.T) {
	body, _ := json.Marshal(map[string]any{
		"type": "block_actions",
		"actions": []map[string]any{
			{"action_id": "approve", "value": "/levee approve ch-2"},
		},
		"channel": map[string]any{"id": "C456"},
		"user":    map[string]any{"id": "U2", "name": "bob"},
	})

	msg, err := ParseSlackCallback(body)
	require.NoError(t, err)
	assert.Equal(t, "/levee approve ch-2", msg.Text)
	assert.Equal(t, "U2", msg.User)
	assert.Equal(t, "bob", msg.UserName)
	assert.Equal(t, "C456", msg.Channel)
}

func TestParseSlackCallback_InvalidJSON(t *testing.T) {
	_, err := ParseSlackCallback([]byte("xxx"))
	require.Error(t, err)
}

// --- Approval blocks ------------------------------------------------------

func TestSlackApprovalBlocks(t *testing.T) {
	payload := SlackApprovalBlocks("ch-9", "审批", "summary")
	blocks := payload["blocks"].([]any)
	require.GreaterOrEqual(t, len(blocks), 2)

	// Header.
	header := blocks[0].(map[string]any)
	assert.Equal(t, "header", header["type"])

	// Actions block with two buttons.
	actions := blocks[len(blocks)-1].(map[string]any)
	assert.Equal(t, "actions", actions["type"])
	elements := actions["elements"].([]map[string]any)
	require.Len(t, elements, 2)
	assert.Equal(t, "primary", elements[0]["style"])
	assert.Contains(t, elements[0]["value"], "/levee approve ch-9")
	assert.Equal(t, "danger", elements[1]["style"])
	assert.Contains(t, elements[1]["value"], "/levee reject ch-9")
}

// --- HandleMessage --------------------------------------------------------

func TestSlackBot_HandleMessage_Help(t *testing.T) {
	srv, last := newSlackTestServer(t)
	b, err := NewSlackBot(SlackConfig{Name: "s", WebhookURL: srv.URL, Timeout: time.Second}, nil)
	require.NoError(t, err)

	require.NoError(t, b.HandleMessage(context.Background(), IncomingMessage{
		Platform: PlatformSlack,
		Text:     "/levee help",
	}))
	body, ok := last.Load().(string)
	require.True(t, ok)
	assert.Contains(t, body, "available commands")
}

func TestSlackBot_HandleMessage_UnknownCommand(t *testing.T) {
	srv, last := newSlackTestServer(t)
	b, err := NewSlackBot(SlackConfig{Name: "s", WebhookURL: srv.URL, Timeout: time.Second}, nil)
	require.NoError(t, err)

	require.NoError(t, b.HandleMessage(context.Background(), IncomingMessage{
		Platform: PlatformSlack,
		Text:     "/levee bogus",
	}))
	body, ok := last.Load().(string)
	require.True(t, ok)
	assert.Contains(t, body, "unknown command")
}