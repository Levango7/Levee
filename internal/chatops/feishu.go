// feishu.go implements the Feishu (Lark) ChatOps adapter. It delivers LEVEE
// events as Feishu interactive cards over the bot webhook URL and accepts
// user-originated callbacks through ParseFeishuCallback, which converts the
// platform-native event payload into the canonical IncomingMessage shape
// consumed by the command router.
//
// The adapter is intentionally minimal: it does not implement Feishu ticket
// verification or token refresh. Those concerns belong to the ingress
// (webhook receiver) layer; this adapter only knows how to render cards and
// POST them to a webhook URL.

package chatops

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/nexus/levee/internal/log"
)

// --- FeishuConfig ----------------------------------------------------------

// FeishuConfig is the configuration for a FeishuBot.
type FeishuConfig struct {
	Name        string        // bot name (registration key)
	WebhookURL  string        // bot webhook URL (required)
	Secret      string        // signing secret (optional, for outbound webhooks)
	Timeout     time.Duration // per-request timeout
	MaxRetries  int           // retry attempts after the initial POST
	RetryDelay  time.Duration // delay between retries
	EventBuffer int           // event channel buffer size
}

// --- FeishuBot -------------------------------------------------------------

// FeishuBot implements the Bot interface for the Feishu platform. It is safe
// for concurrent use.
type FeishuBot struct {
	baseBot
	cfg    FeishuConfig
	client *http.Client

	// onEvent is invoked for each event in the run loop. Tests override it
	// to capture rendered cards without going over the wire.
	onEvent func(ctx context.Context, evt Event) error
}

// NewFeishuBot constructs a FeishuBot from the given config. The router may
// be nil, in which case a fresh CommandRouter with the built-in handlers is
// used.
func NewFeishuBot(cfg FeishuConfig, router *CommandRouter) (*FeishuBot, error) {
	if cfg.Name == "" {
		return nil, fmt.Errorf("chatops: new feishu: %w", ErrBotNotFound)
	}
	if cfg.WebhookURL == "" {
		return nil, fmt.Errorf("chatops: new feishu: %w", ErrEmptyWebhookURL)
	}
	buf := cfg.EventBuffer
	if buf < 0 {
		buf = 0
	}
	bot := &FeishuBot{
		baseBot: newBaseBot(cfg.Name, PlatformFeishu, router, buf),
		cfg:     cfg,
		client:  newHTTPClient(cfg.Timeout),
	}
	bot.onEvent = bot.deliverEvent
	return bot, nil
}

// Start begins consuming events from the internal channel and rendering them
// as Feishu cards. It returns an error if the bot is already started.
func (b *FeishuBot) Start(ctx context.Context) error {
	if err := b.markStarted(); err != nil {
		return err
	}
	go b.eventLoop(ctx)
	log.Info("chatops: feishu bot started", "name", b.name)
	return nil
}

// Stop signals the event loop to exit and drains pending events.
func (b *FeishuBot) Stop() error {
	b.markStopped()
	b.drainEvents()
	log.Info("chatops: feishu bot stopped", "name", b.name)
	return nil
}

// eventLoop is the main consume-and-deliver loop. It exits when stopCh is
// closed.
func (b *FeishuBot) eventLoop(ctx context.Context) {
	for {
		select {
		case <-b.stopChan():
			return
		case evt := <-b.eventCh:
			if b.onEvent == nil {
				continue
			}
			if err := b.onEvent(ctx, evt); err != nil {
				log.Warn("chatops: feishu deliver event failed",
					"bot", b.name, "event", evt.Type, "err", err)
			}
		}
	}
}

// deliverEvent renders the event as a Feishu card and POSTs it to the webhook.
func (b *FeishuBot) deliverEvent(ctx context.Context, evt Event) error {
	card := BuildCardForEvent(evt)
	return b.SendCard(ctx, "", card)
}

// SendMessage posts a plain-text message to the Feishu webhook. The channel
// argument is unused because Feishu bot webhooks deliver to the bound group;
// it is kept for interface symmetry.
func (b *FeishuBot) SendMessage(ctx context.Context, channel, text string) error {
	if strings.TrimSpace(text) == "" {
		return nil
	}
	payload := map[string]any{
		"msg_type": "text",
		"content":  map[string]any{"text": text},
	}
	return postJSON(ctx, b.client, b.cfg.WebhookURL, payload, b.httpOpts())
}

// SendCard renders the card as a Feishu interactive card and POSTs it.
func (b *FeishuBot) SendCard(ctx context.Context, channel string, card *Card) error {
	if card == nil {
		return nil
	}
	payload := map[string]any{
		"msg_type": "interactive",
		"card":     card.ToFeishu(),
	}
	return postJSON(ctx, b.client, b.cfg.WebhookURL, payload, b.httpOpts())
}

// httpOpts builds the httpOptions from the bot config.
func (b *FeishuBot) httpOpts() httpOptions {
	return httpOptions{
		timeout:    b.cfg.Timeout,
		maxRetries: b.cfg.MaxRetries,
		retryDelay: b.cfg.RetryDelay,
	}
}

// --- Feishu callback parsing -----------------------------------------------

// FeishuCallback is the minimal subset of the Feishu event payload we need to
// reconstruct an IncomingMessage. The full schema is large and version-
// dependent; we only decode the fields used by the command router.
type FeishuCallback struct {
	EventType string `json:"event_type,omitempty"`
	Event     struct {
		Message struct {
			Content   string `json:"content"`
			MessageID string `json:"message_id"`
			ChatID    string `json:"chat_id"`
		} `json:"message"`
		Sender struct {
			SenderID struct {
				OpenID string `json:"open_id"`
			} `json:"sender_id"`
			SenderNick string `json:"sender_nick"`
		} `json:"sender"`
	} `json:"event"`

	// Card action callback: when a user clicks a button, Feishu sends a
	// value object instead of an event.message.
	Value struct {
		Command string `json:"command"`
	} `json:"value"`
	Operator struct {
		OpenID string `json:"open_id"`
	} `json:"operator"`
}

// ParseFeishuCallback decodes a Feishu callback body into an IncomingMessage.
// It supports both message events (text content) and card action callbacks
// (button clicks). The returned IncomingMessage has Platform set to
// PlatformFeishu and Text set to the canonical command string.
func ParseFeishuCallback(body []byte) (IncomingMessage, error) {
	var cb FeishuCallback
	if err := json.Unmarshal(body, &cb); err != nil {
		return IncomingMessage{}, fmt.Errorf("chatops: parse feishu callback: %w", err)
	}

	msg := IncomingMessage{Platform: PlatformFeishu, Raw: cb}

	// Card action callback (button click).
	if cb.Value.Command != "" {
		msg.Text = cb.Value.Command
		msg.User = cb.Operator.OpenID
		msg.Channel = ""
		return msg, nil
	}

	// Message event: content is a JSON string like {"text":"@_user_1 /levee list"}.
	msg.User = cb.Event.Sender.SenderID.OpenID
	msg.UserName = cb.Event.Sender.SenderNick
	msg.Channel = cb.Event.Message.ChatID

	text, err := decodeFeishuText(cb.Event.Message.Content)
	if err != nil {
		return IncomingMessage{}, err
	}
	msg.Text = text
	return msg, nil
}

// decodeFeishuText extracts the plain text from a Feishu message content
// blob. The content is a JSON object with a "text" field that may contain
// @-mentions; we strip the mentions to recover the command tail.
func decodeFeishuText(content string) (string, error) {
	if content == "" {
		return "", nil
	}
	var wrapper struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal([]byte(content), &wrapper); err != nil {
		// Some clients send raw text; fall back to the raw string.
		return strings.TrimSpace(content), nil
	}
	return stripFeishuMentions(wrapper.Text), nil
}

// stripFeishuMentions removes @-mention tokens (e.g. "@_user_1") from a
// message so the command parser only sees the command tail.
func stripFeishuMentions(s string) string {
	var b strings.Builder
	for _, field := range strings.Fields(s) {
		if strings.HasPrefix(field, "@_") {
			continue
		}
		if b.Len() > 0 {
			b.WriteByte(' ')
		}
		b.WriteString(field)
	}
	return b.String()
}

// --- Feishu approval interaction -------------------------------------------

// FeishuApprovalButtons builds the Approve / Reject button pair for a Feishu
// card. The Value field carries the canonical /levee command so the callback
// can be routed without a separate mapping table.
func FeishuApprovalButtons(changeID string) []map[string]any {
	return []map[string]any{
		{
			"tag":   "button",
			"text":  map[string]any{"tag": "plain_text", "content": "通过"},
			"type":  "primary",
			"value": map[string]any{"command": fmt.Sprintf("/levee approve %s", changeID)},
		},
		{
			"tag":   "button",
			"text":  map[string]any{"tag": "plain_text", "content": "驳回"},
			"type":  "danger",
			"value": map[string]any{"command": fmt.Sprintf("/levee reject %s", changeID)},
		},
	}
}

// --- HandleMessage ---------------------------------------------------------

// HandleMessage routes a user-originated message through the command router
// and sends the reply back to the same Feishu chat.
func (b *FeishuBot) HandleMessage(ctx context.Context, msg IncomingMessage) error {
	result, err := b.router.Dispatch(ctx, msg)
	if err != nil {
		// Unknown / failed command: still send a reply so the user gets
		// feedback, but surface the error in the reply text.
		if result.Reply == "" {
			result.Reply = fmt.Sprintf("error: %v", err)
		}
	}
	if result.Reply == "" && result.Card == nil {
		return nil
	}
	if result.Card != nil {
		if e := b.SendCard(ctx, msg.Channel, result.Card); e != nil {
			log.Warn("chatops: feishu send card reply failed", "bot", b.name, "err", e)
		}
	}
	if result.Reply != "" {
		if e := b.SendMessage(ctx, msg.Channel, result.Reply); e != nil {
			return fmt.Errorf("chatops: feishu reply: %w", e)
		}
	}
	return nil
}

// --- Compile-time interface check -----------------------------------------

var _ Bot = (*FeishuBot)(nil)
