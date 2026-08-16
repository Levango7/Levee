
// im_adapter.go implements IMAdapter, the bridge between ChatOps Bot
// adapters (Feishu / DingTalk / Slack) and the ConversationEngine. It maps
// each IM channel to a conversation Session, forwards user messages to the
// engine and renders the resulting Reply back to the IM channel as a text
// message or an interactive Card.
//
// The adapter is the single integration point that turns an IM bot into a
// conversation entry point:
//
//	user → IM bot → IMAdapter.HandleIMMessage → ConversationEngine.HandleMessage
//	      → Reply → IMAdapter renders Card / text → IM bot → user
//
// Concurrency model:
//   - The sessions map (channel→sessionID) is guarded by IMAdapter.mu.
//   - All public methods are safe for concurrent use by any number of
//     goroutines. A slow ConversationEngine call in one channel never blocks
//     a HandleIMMessage call for a different channel.

package conversation

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"sync"

	"github.com/nexus/levee/internal/chatops"
	"github.com/nexus/levee/internal/log"
)

// --- Sentinel errors --------------------------------------------------------

var (
	// ErrIMNilEngine is returned when NewIMAdapter is called with a nil
	// ConversationEngine.
	ErrIMNilEngine = errors.New("conversation/im: nil conversation engine")
	// ErrIMNilBots is returned when NewIMAdapter is called with a nil
	// BotManager.
	ErrIMNilBots = errors.New("conversation/im: nil bot manager")
	// ErrIMChannelNotFound is returned when an operation targets a channel
	// that has no active session mapping.
	ErrIMChannelNotFound = errors.New("conversation/im: channel not found")
	// ErrIMBotNotFound is returned when no bot is registered for the requested
	// platform.
	ErrIMBotNotFound = errors.New("conversation/im: bot not found for platform")
)

// --- IMAdapterConfig --------------------------------------------------------

// IMAdapterConfig configures an IMAdapter. Both Engine and Bots are required;
// Logger is optional and defaults to the package-level singleton tagged with
// component=im_adapter.
type IMAdapterConfig struct {
	// Engine is the conversation engine that owns the Sessions. Must be
	// non-nil.
	Engine *ConversationEngine
	// Bots is the ChatOps bot manager used to look up platform adapters
	// for delivering replies. Must be non-nil.
	Bots *chatops.BotManager
	// Logger is the structured logger. When nil the package-level
	// singleton from internal/log is used.
	Logger *slog.Logger
}

// --- IMAdapter --------------------------------------------------------------

// IMAdapter bridges ChatOps Bot adapters to the ConversationEngine. Each IM
// channel is mapped to a single conversation Session; incoming messages are
// forwarded to the engine and the resulting Reply is rendered back to the
// channel as text or a Card. The adapter is safe for concurrent use.
type IMAdapter struct {
	engine   *ConversationEngine
	bots     *chatops.BotManager
	sessions map[string]string // channel→sessionID
	log      *slog.Logger
	mu       sync.RWMutex // guards sessions
}

// NewIMAdapter constructs an IMAdapter from the given config. It returns an
// error when Engine or Bots is nil. The returned adapter is ready to use;
// callers do not need to call Start.
func NewIMAdapter(cfg IMAdapterConfig) (*IMAdapter, error) {
	if cfg.Engine == nil {
		return nil, fmt.Errorf("conversation/im: new adapter: %w", ErrIMNilEngine)
	}
	if cfg.Bots == nil {
		return nil, fmt.Errorf("conversation/im: new adapter: %w", ErrIMNilBots)
	}
	lg := cfg.Logger
	if lg == nil {
		lg = log.With("component", "im_adapter")
	}
	return &IMAdapter{
		engine:   cfg.Engine,
		bots:     cfg.Bots,
		sessions: make(map[string]string),
		log:      lg,
	}, nil
}

// Close releases all channel→session mappings. It does not close the
// underlying ConversationEngine or BotManager; callers own those lifecycles
// separately. Close is idempotent and safe for concurrent use.
func (a *IMAdapter) Close() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.sessions = make(map[string]string)
	a.log.Info("conversation/im: adapter closed")
	return nil
}

// --- Session mapping --------------------------------------------------------

// GetOrCreateSession returns the session id mapped to the given channel.
// When no mapping exists it creates a new session via the ConversationEngine
// and records the mapping. The returned session id is always non-empty on
// success. Concurrent calls for the same channel are de-duplicated: the
// first caller creates the session and subsequent callers observe the
// existing mapping.
func (a *IMAdapter) GetOrCreateSession(channel, userID string) (string, error) {
	// Fast path: an existing mapping is read under RLock.
	a.mu.RLock()
	sid, ok := a.sessions[channel]
	a.mu.RUnlock()
	if ok {
		return sid, nil
	}

	// Slow path: create a new session. This happens outside the adapter
	// lock so two channels can create sessions in parallel.
	sess, err := a.engine.NewSession(userID)
	if err != nil {
		return "", fmt.Errorf("conversation/im: get or create session: %w", err)
	}

	a.mu.Lock()
	defer a.mu.Unlock()
	// Double-check: another goroutine may have created the mapping for
	// this channel while we were waiting on the write lock. Close the
	// session we just created to avoid leaking it in the engine.
	if existing, ok := a.sessions[channel]; ok {
		_ = a.engine.CloseSession(sess.ID)
		return existing, nil
	}
	a.sessions[channel] = sess.ID
	a.log.Info("conversation/im: channel mapped",
		"channel", channel, "session_id", sess.ID, "user_id", userID)
	return sess.ID, nil
}

// GetSessionID returns the session id mapped to the given channel. The bool
// result is false when no mapping exists. It is safe for concurrent use.
func (a *IMAdapter) GetSessionID(channel string) (string, bool) {
	a.mu.RLock()
	defer a.mu.RUnlock()
	sid, ok := a.sessions[channel]
	return sid, ok
}

// CloseSession closes the conversation session mapped to the given channel
// and removes the mapping. It returns ErrIMChannelNotFound when no mapping
// exists. Errors from the underlying ConversationEngine.CloseSession are
// wrapped and returned; in that case the mapping is still removed.
func (a *IMAdapter) CloseSession(channel string) error {
	a.mu.Lock()
	sid, ok := a.sessions[channel]
	if !ok {
		a.mu.Unlock()
		return fmt.Errorf("conversation/im: close session %q: %w", channel, ErrIMChannelNotFound)
	}
	delete(a.sessions, channel)
	a.mu.Unlock()

	if err := a.engine.CloseSession(sid); err != nil {
		return fmt.Errorf("conversation/im: close session %q: %w", channel, err)
	}
	a.log.Info("conversation/im: channel unmapped", "channel", channel, "session_id", sid)
	return nil
}

// --- Message handling -------------------------------------------------------

// HandleIMMessage is the core entry point. It looks up (or creates) the
// session for msg.Channel, forwards the message text to the
// ConversationEngine and renders the resulting Reply back to the IM channel.
// The returned CommandResult mirrors the Reply (Text→Reply, Card→Card).
//
// When the Reply carries a Card the card is delivered via bot.SendCard;
// otherwise the text is delivered via bot.SendMessage. When no bot is
// registered for msg.Platform the CommandResult is still returned (so the
// caller can inspect the engine reply) together with an ErrIMBotNotFound
// error. When the bot delivery fails the error is wrapped and returned
// alongside the CommandResult.
func (a *IMAdapter) HandleIMMessage(ctx context.Context, msg chatops.IncomingMessage) (*chatops.CommandResult, error) {
	if ctx == nil {
		ctx = context.Background()
	}

	// Look up or create the session for this channel.
	sessionID, err := a.GetOrCreateSession(msg.Channel, msg.User)
	if err != nil {
		return nil, fmt.Errorf("conversation/im: handle message: %w", err)
	}

	// Forward to the conversation engine.
	reply, err := a.engine.HandleMessage(ctx, sessionID, msg.User, msg.Text)
	if err != nil {
		return nil, fmt.Errorf("conversation/im: handle message: %w", err)
	}
	if reply == nil {
		reply = &Reply{}
	}

	result := &chatops.CommandResult{
		Reply: reply.Text,
		Card:  reply.Card,
	}

	// Render back to the IM channel. Locate the bot for the message
	// platform.
	bot, err := a.bots.ByPlatform(msg.Platform)
	if err != nil {
		// The engine produced a reply but we have no bot to deliver it.
		// Return the result so the caller can inspect it, but report
		// the delivery error.
		return result, fmt.Errorf("conversation/im: handle message: %w", ErrIMBotNotFound)
	}

	if reply.Card != nil {
		if err := bot.SendCard(ctx, msg.Channel, reply.Card); err != nil {
			return result, fmt.Errorf("conversation/im: handle message: send card: %w", err)
		}
	} else if reply.Text != "" {
		if err := bot.SendMessage(ctx, msg.Channel, reply.Text); err != nil {
			return result, fmt.Errorf("conversation/im: handle message: send text: %w", err)
		}
	}

	return result, nil
}

// --- Batch sending ----------------------------------------------------------

// BroadcastToChannel sends a text message to the given channel on the given
// platform. It returns ErrIMBotNotFound when no bot is registered for the
// platform. The adapter does not need an active session mapping for the
// channel; this method is intended for one-way notifications.
func (a *IMAdapter) BroadcastToChannel(ctx context.Context, platform chatops.Platform, channel, text string) error {
	if ctx == nil {
		ctx = context.Background()
	}
	bot, err := a.bots.ByPlatform(platform)
	if err != nil {
		return fmt.Errorf("conversation/im: broadcast: %w", ErrIMBotNotFound)
	}
	if err := bot.SendMessage(ctx, channel, text); err != nil {
		return fmt.Errorf("conversation/im: broadcast: %w", err)
	}
	return nil
}

// SendCardToChannel sends a card to the given channel on the given platform.
// It returns ErrIMBotNotFound when no bot is registered for the platform. A nil
// card is a no-op. The adapter does not need an active session mapping for
// the channel; this method is intended for one-way notifications.
func (a *IMAdapter) SendCardToChannel(ctx context.Context, platform chatops.Platform, channel string, card *chatops.Card) error {
	if card == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	bot, err := a.bots.ByPlatform(platform)
	if err != nil {
		return fmt.Errorf("conversation/im: send card: %w", ErrIMBotNotFound)
	}
	if err := bot.SendCard(ctx, channel, card); err != nil {
		return fmt.Errorf("conversation/im: send card: %w", err)
	}
	return nil
}

// --- Queries ----------------------------------------------------------------

// SessionCount returns the number of active IM channel→session mappings.
func (a *IMAdapter) SessionCount() int {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return len(a.sessions)
}

// Channels returns the list of all active IM channels. The returned slice is
// freshly allocated, sorted lexicographically and safe to mutate.
func (a *IMAdapter) Channels() []string {
	a.mu.RLock()
	defer a.mu.RUnlock()
	out := make([]string, 0, len(a.sessions))
	for ch := range a.sessions {
		out = append(out, ch)
	}
	sort.Strings(out)
	return out
}