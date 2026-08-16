// slack.go implements the Slack ChatOps adapter. It delivers LEVEE events as
// Slack Block Kit messages over the incoming webhook URL and accepts user-
// originated callbacks through ParseSlackCallback, which converts the
// platform-native slash-command payload or interactive action payload into
// the canonical IncomingMessage shape consumed by the command router.
//
// The adapter is intentionally minimal: it does not implement Slack OAuth
// flow or token rotation. Those concerns belong to the ingress layer; this
// adapter only knows how to render Block Kit payloads and POST them to a
// webhook URL.

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

// --- SlackConfig -----------------------------------------------------------

// SlackConfig is the configuration for a SlackBot.
type SlackConfig struct {
	Name        string        // bot name (registration key)
	WebhookURL  string        // incoming webhook URL (required)
	BotToken    string        // xoxb token (optional, used for chat.postMessage)
	Timeout     time.Duration // per-request timeout
	MaxRetries  int           // retry attempts after the initial POST
	RetryDelay  time.Duration // delay between retries
	EventBuffer int           // event channel buffer size
}

// --- SlackBot --------------------------------------------------------------

// SlackBot implements the Bot interface for the Slack platform. It is safe
// for concurrent use.
type SlackBot struct {
	baseBot
	cfg    SlackConfig
	client *http.Client

	onEvent func(ctx context.Context, evt Event) error
}

// NewSlackBot constructs a SlackBot from the given config. The router may be
// nil, in which case a fresh CommandRouter with the built-in handlers is
// used.
func NewSlackBot(cfg SlackConfig, router *CommandRouter) (*SlackBot, error) {
	if cfg.Name == "" {
		return nil, fmt.Errorf("chatops: new slack: %w", ErrBotNotFound)
	}
	if cfg.WebhookURL == "" {
		return nil, fmt.Errorf("chatops: new slack: %w", ErrEmptyWebhookURL)
	}
	buf := cfg.EventBuffer
	if buf < 0 {
		buf = 0
	}
	bot := &SlackBot{
		baseBot: newBaseBot(cfg.Name, PlatformSlack, router, buf),
		cfg:     cfg,
		client:  newHTTPClient(cfg.Timeout),
	}
	bot.onEvent = bot.deliverEvent
	return bot, nil
}

// Start begins consuming events from the internal channel and rendering them
// as Slack Block Kit messages.
func (b *SlackBot) Start(ctx context.Context) error {
	if err := b.markStarted(); err != nil {
		return err
	}
	go b.eventLoop(ctx)
	log.Info("chatops: slack bot started", "name", b.name)
	return nil
}

// Stop signals the event loop to exit and drains pending events.
func (b *SlackBot) Stop() error {
	b.markStopped()
	b.drainEvents()
	log.Info("chatops: slack bot stopped", "name", b.name)
	return nil
}

// eventLoop is the main consume-and-deliver loop.
func (b *SlackBot) eventLoop(ctx context.Context) {
	for {
		select {
		case <-b.stopChan():
			return
		case evt := <-b.eventCh:
			if b.onEvent == nil {
				continue
			}
			if err := b.onEvent(ctx, evt); err != nil {
				log.Warn("chatops: slack deliver event failed",
					"bot", b.name, "event", evt.Type, "err", err)
			}
		}
	}
}

// deliverEvent renders the event as a Slack Block Kit message and POSTs it.
func (b *SlackBot) deliverEvent(ctx context.Context, evt Event) error {
	card := BuildCardForEvent(evt)
	return b.SendCard(ctx, "", card)
}

// SendMessage posts a plain-text message to the Slack incoming webhook. The
// channel argument is unused because Slack incoming webhooks deliver to the
// bound channel; it is kept for interface symmetry.
func (b *SlackBot) SendMessage(ctx context.Context, channel, text string) error {
	if strings.TrimSpace(text) == "" {
		return nil
	}
	payload := map[string]any{
		"text": text,
	}
	return postJSON(ctx, b.client, b.cfg.WebhookURL, payload, b.httpOpts())
}

// SendCard renders the card as a Slack Block Kit message and POSTs it.
func (b *SlackBot) SendCard(ctx context.Context, channel string, card *Card) error {
	if card == nil {
		return nil
	}
	return postJSON(ctx, b.client, b.cfg.WebhookURL, card.ToSlack(), b.httpOpts())
}

// httpOpts builds the httpOptions from the bot config.
func (b *SlackBot) httpOpts() httpOptions {
	return httpOptions{
		timeout:    b.cfg.Timeout,
		maxRetries: b.cfg.MaxRetries,
		retryDelay: b.cfg.RetryDelay,
	}
}

// --- Slack callback parsing -----------------------------------------------

// SlackCallback is the minimal subset of the Slack payload we need to
// reconstruct an IncomingMessage. It supports both slash-command payloads
// (top-level Text / UserName / ChannelID) and interactive action payloads
// (Actions[].Value).
type SlackCallback struct {
	// Slash command fields.
	Token       string `json:"token"`
	TeamID      string `json:"team_id"`
	ChannelID   string `json:"channel_id"`
	UserID      string `json:"user_id"`
	UserName    string `json:"user_name"`
	Command     string `json:"command"`
	Text        string `json:"text"`
	ResponseURL string `json:"response_url"`
	TriggerID   string `json:"trigger_id"`

	// Interactive action fields.
	Type    string `json:"type"`
	Actions []struct {
		ActionID string `json:"action_id"`
		Value    string `json:"value"`
	} `json:"actions"`
	Channel struct {
		ID string `json:"id"`
	} `json:"channel"`
	User struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	} `json:"user"`
}

// ParseSlackCallback decodes a Slack callback body into an IncomingMessage.
// It supports both slash-command payloads and interactive action payloads
// (button clicks). The returned IncomingMessage has Platform set to
// PlatformSlack and Text set to the canonical command string.
func ParseSlackCallback(body []byte) (IncomingMessage, error) {
	var cb SlackCallback
	if err := json.Unmarshal(body, &cb); err != nil {
		return IncomingMessage{}, fmt.Errorf("chatops: parse slack callback: %w", err)
	}

	msg := IncomingMessage{Platform: PlatformSlack, Raw: cb}

	// Interactive action (button click).
	if len(cb.Actions) > 0 && cb.Actions[0].Value != "" {
		msg.Text = cb.Actions[0].Value
		msg.User = cb.User.ID
		msg.UserName = cb.User.Name
		msg.Channel = cb.Channel.ID
		return msg, nil
	}

	// Slash command: combine the command verb and text tail so the router
	// sees a single canonical string. Slack sends "/levee" in Command and
	// "approve run-123" in Text; we join them into "/levee approve run-123".
	msg.User = cb.UserID
	msg.UserName = cb.UserName
	msg.Channel = cb.ChannelID

	cmd := strings.TrimSpace(cb.Command)
	tail := strings.TrimSpace(cb.Text)
	if cmd != "" {
		if tail != "" {
			msg.Text = cmd + " " + tail
		} else {
			msg.Text = cmd
		}
	} else {
		msg.Text = tail
	}
	return msg, nil
}

// --- Slack approval interaction -------------------------------------------

// SlackApprovalBlocks builds a Block Kit payload with Approve / Reject
// buttons. The button value fields carry the canonical /levee commands so
// the interactive callback can be routed without a mapping table.
func SlackApprovalBlocks(changeID, title, summary string) map[string]any {
	blocks := []any{
		map[string]any{
			"type": "header",
			"text": map[string]any{"type": "plain_text", "text": title},
		},
	}
	if summary != "" {
		blocks = append(blocks, map[string]any{
			"type": "section",
			"text": map[string]any{"type": "mrkdwn", "text": summary},
		})
	}
	blocks = append(blocks, map[string]any{
		"type": "actions",
		"elements": []map[string]any{
			{
				"type":  "button",
				"text":  map[string]any{"type": "plain_text", "text": "通过"},
				"style": "primary",
				"value": fmt.Sprintf("/levee approve %s", changeID),
			},
			{
				"type":  "button",
				"text":  map[string]any{"type": "plain_text", "text": "驳回"},
				"style": "danger",
				"value": fmt.Sprintf("/levee reject %s", changeID),
			},
		},
	})
	return map[string]any{"blocks": blocks}
}

// --- HandleMessage ---------------------------------------------------------

// HandleMessage routes a user-originated message through the command router
// and sends the reply back to the same Slack channel.
func (b *SlackBot) HandleMessage(ctx context.Context, msg IncomingMessage) error {
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
			log.Warn("chatops: slack send card reply failed", "bot", b.name, "err", e)
		}
	}
	if result.Reply != "" {
		if e := b.SendMessage(ctx, msg.Channel, result.Reply); e != nil {
			return fmt.Errorf("chatops: slack reply: %w", e)
		}
	}
	return nil
}

// --- Compile-time interface check -----------------------------------------

var _ Bot = (*SlackBot)(nil)
