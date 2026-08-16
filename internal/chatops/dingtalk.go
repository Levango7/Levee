// dingtalk.go implements the DingTalk ChatOps adapter. It delivers LEVEE
// events as DingTalk ActionCard messages over the bot webhook URL and
// accepts user-originated callbacks through ParseDingtalkCallback, which
// converts the platform-native event payload into the canonical
// IncomingMessage shape consumed by the command router.
//
// The adapter is intentionally minimal: it does not implement DingTalk
// signature verification or token refresh. Those concerns belong to the
// ingress (webhook receiver) layer; this adapter only knows how to render
// ActionCards and POST them to a webhook URL.

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

// --- DingtalkConfig --------------------------------------------------------

// DingtalkConfig is the configuration for a DingtalkBot.
type DingtalkConfig struct {
	Name        string        // bot name (registration key)
	WebhookURL  string        // bot webhook URL (required)
	Secret      string        // signing secret (optional, enables timestamp+sign)
	Timeout     time.Duration // per-request timeout
	MaxRetries  int           // retry attempts after the initial POST
	RetryDelay  time.Duration // delay between retries
	EventBuffer int           // event channel buffer size
}

// --- DingtalkBot -----------------------------------------------------------

// DingtalkBot implements the Bot interface for the DingTalk platform. It is
// safe for concurrent use.
type DingtalkBot struct {
	baseBot
	cfg    DingtalkConfig
	client *http.Client

	onEvent func(ctx context.Context, evt Event) error
}

// NewDingtalkBot constructs a DingtalkBot from the given config. The router
// may be nil, in which case a fresh CommandRouter with the built-in handlers
// is used.
func NewDingtalkBot(cfg DingtalkConfig, router *CommandRouter) (*DingtalkBot, error) {
	if cfg.Name == "" {
		return nil, fmt.Errorf("chatops: new dingtalk: %w", ErrBotNotFound)
	}
	if cfg.WebhookURL == "" {
		return nil, fmt.Errorf("chatops: new dingtalk: %w", ErrEmptyWebhookURL)
	}
	buf := cfg.EventBuffer
	if buf < 0 {
		buf = 0
	}
	bot := &DingtalkBot{
		baseBot: newBaseBot(cfg.Name, PlatformDingtalk, router, buf),
		cfg:     cfg,
		client:  newHTTPClient(cfg.Timeout),
	}
	bot.onEvent = bot.deliverEvent
	return bot, nil
}

// Start begins consuming events from the internal channel and rendering them
// as DingTalk ActionCards.
func (b *DingtalkBot) Start(ctx context.Context) error {
	if err := b.markStarted(); err != nil {
		return err
	}
	go b.eventLoop(ctx)
	log.Info("chatops: dingtalk bot started", "name", b.name)
	return nil
}

// Stop signals the event loop to exit and drains pending events.
func (b *DingtalkBot) Stop() error {
	b.markStopped()
	b.drainEvents()
	log.Info("chatops: dingtalk bot stopped", "name", b.name)
	return nil
}

// eventLoop is the main consume-and-deliver loop.
func (b *DingtalkBot) eventLoop(ctx context.Context) {
	for {
		select {
		case <-b.stopChan():
			return
		case evt := <-b.eventCh:
			if b.onEvent == nil {
				continue
			}
			if err := b.onEvent(ctx, evt); err != nil {
				log.Warn("chatops: dingtalk deliver event failed",
					"bot", b.name, "event", evt.Type, "err", err)
			}
		}
	}
}

// deliverEvent renders the event as a DingTalk ActionCard and POSTs it.
func (b *DingtalkBot) deliverEvent(ctx context.Context, evt Event) error {
	card := BuildCardForEvent(evt)
	return b.SendCard(ctx, "", card)
}

// SendMessage posts a plain-text message to the DingTalk webhook. The channel
// argument is unused because DingTalk bot webhooks deliver to the bound group.
func (b *DingtalkBot) SendMessage(ctx context.Context, channel, text string) error {
	if strings.TrimSpace(text) == "" {
		return nil
	}
	payload := map[string]any{
		"msgtype": "text",
		"text":    map[string]any{"content": text},
	}
	return postJSON(ctx, b.client, b.cfg.WebhookURL, payload, b.httpOpts())
}

// SendCard renders the card as a DingTalk ActionCard and POSTs it.
func (b *DingtalkBot) SendCard(ctx context.Context, channel string, card *Card) error {
	if card == nil {
		return nil
	}
	return postJSON(ctx, b.client, b.cfg.WebhookURL, card.ToDingtalk(), b.httpOpts())
}

// httpOpts builds the httpOptions from the bot config.
func (b *DingtalkBot) httpOpts() httpOptions {
	return httpOptions{
		timeout:    b.cfg.Timeout,
		maxRetries: b.cfg.MaxRetries,
		retryDelay: b.cfg.RetryDelay,
	}
}

// --- DingTalk callback parsing ---------------------------------------------

// DingtalkCallback is the minimal subset of the DingTalk outgoing robot
// callback payload we need to reconstruct an IncomingMessage.
type DingtalkCallback struct {
	MsgType string `json:"msgtype"`
	Text    struct {
		Content string `json:"content"`
	} `json:"text"`
	SenderStaffID  string `json:"senderStaffId"`
	SenderNick     string `json:"senderNick"`
	ConversationID string `json:"conversationId"`
	IsAdmin        bool   `json:"isAdmin"`
	ChatbotUserID  string `json:"chatbotUserId"`

	// Card action callback (when using the actionCard with btns).
	ActionCard struct {
		Title     string `json:"title"`
		Text      string `json:"text"`
		ActionURL string `json:"actionURL"`
	} `json:"actionCard"`
}

// ParseDingtalkCallback decodes a DingTalk callback body into an
// IncomingMessage. It supports text messages and ActionCard callbacks.
func ParseDingtalkCallback(body []byte) (IncomingMessage, error) {
	var cb DingtalkCallback
	if err := json.Unmarshal(body, &cb); err != nil {
		return IncomingMessage{}, fmt.Errorf("chatops: parse dingtalk callback: %w", err)
	}

	msg := IncomingMessage{
		Platform: PlatformDingtalk,
		User:     cb.SenderStaffID,
		UserName: cb.SenderNick,
		Channel:  cb.ConversationID,
		Raw:      cb,
	}

	if cb.ActionCard.ActionURL != "" {
		// Button click: the actionURL carries the canonical /levee command.
		msg.Text = cb.ActionCard.ActionURL
		return msg, nil
	}

	msg.Text = stripDingtalkMentions(cb.Text.Content)
	return msg, nil
}

// stripDingtalkMentions removes @-mention tokens from a DingTalk message.
func stripDingtalkMentions(s string) string {
	var b strings.Builder
	for _, field := range strings.Fields(s) {
		if strings.HasPrefix(field, "@") {
			continue
		}
		if b.Len() > 0 {
			b.WriteByte(' ')
		}
		b.WriteString(field)
	}
	return b.String()
}

// --- DingTalk approval interaction -----------------------------------------

// DingtalkApprovalActionCard builds an ActionCard payload with Approve /
// Reject buttons. The btns' actionURL fields carry the canonical /levee
// commands so the callback can be routed without a mapping table.
func DingtalkApprovalActionCard(changeID, title, summary string) map[string]any {
	text := summary
	if text == "" {
		text = fmt.Sprintf("变更 %s 等待审批", changeID)
	}
	return map[string]any{
		"msgtype": "actionCard",
		"actionCard": map[string]any{
			"title":          title,
			"text":           text,
			"btnOrientation": "0",
			"btns": []map[string]any{
				{"title": "通过", "actionURL": fmt.Sprintf("/levee approve %s", changeID)},
				{"title": "驳回", "actionURL": fmt.Sprintf("/levee reject %s", changeID)},
			},
		},
	}
}

// --- HandleMessage ---------------------------------------------------------

// HandleMessage routes a user-originated message through the command router
// and sends the reply back to the same DingTalk conversation.
func (b *DingtalkBot) HandleMessage(ctx context.Context, msg IncomingMessage) error {
	result, err := b.router.Dispatch(ctx, msg)
	if err != nil {
		if result.Reply == "" {
			result.Reply = fmt.Sprintf("error: %v", err)
		}
	}
	if result.Reply == "" && result.Card == nil {
		return nil
	}
	if result.Card != nil {
		if e := b.SendCard(ctx, msg.Channel, result.Card); e != nil {
			log.Warn("chatops: dingtalk send card reply failed", "bot", b.name, "err", e)
		}
	}
	if result.Reply != "" {
		if e := b.SendMessage(ctx, msg.Channel, result.Reply); e != nil {
			return fmt.Errorf("chatops: dingtalk reply: %w", e)
		}
	}
	return nil
}

// --- Compile-time interface check -----------------------------------------

var _ Bot = (*DingtalkBot)(nil)
