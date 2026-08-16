// im_adapter_test.go exercises IMAdapter, the bridge between ChatOps Bot
// adapters and the ConversationEngine. The suite targets full line coverage
// of im_adapter.go; every public method and every error branch is exercised
// at least once.
//
// The test suite reuses the newTestEngine / newTestEngineNoDiagnose helpers
// from engine_test.go (same package) so the conversation engine is wired up
// exactly as in the engine tests.

package conversation

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/nexus/levee/internal/chatops"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// --- mockBot ---------------------------------------------------------------

// mockBot is a test double for chatops.Bot. It records every SendMessage /
// SendCard call and can be configured to return errors on demand. It is safe
// for concurrent use.
type mockBot struct {
	name     string
	platform chatops.Platform
	eventCh  chan chatops.Event

	mu         sync.Mutex
	sentText   []string // "channel|text"
	sentCard   []*chatops.Card
	sentCardCh []string // channels for SendCard calls

	startErr  error
	stopErr   error
	sendErr   error
	cardErr   error
	handleErr error
}

func newMockBot(name string, platform chatops.Platform) *mockBot {
	return &mockBot{
		name:     name,
		platform: platform,
		eventCh:  make(chan chatops.Event, 16),
	}
}

func (b *mockBot) Name() string                 { return b.name }
func (b *mockBot) Platform() chatops.Platform   { return b.platform }
func (b *mockBot) Start(_ context.Context) error { return b.startErr }
func (b *mockBot) Stop() error                   { return b.stopErr }
func (b *mockBot) SubscribeEvents() <-chan chatops.Event { return b.eventCh }

func (b *mockBot) PublishEvent(evt chatops.Event) error {
	select {
	case b.eventCh <- evt:
		return nil
	default:
		return chatops.ErrBotClosed
	}
}

func (b *mockBot) SendMessage(_ context.Context, channel, text string) error {
	if b.sendErr != nil {
		return b.sendErr
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.sentText = append(b.sentText, fmt.Sprintf("%s|%s", channel, text))
	return nil
}

func (b *mockBot) SendCard(_ context.Context, channel string, card *chatops.Card) error {
	if b.cardErr != nil {
		return b.cardErr
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.sentCard = append(b.sentCard, card)
	b.sentCardCh = append(b.sentCardCh, channel)
	return nil
}

func (b *mockBot) HandleMessage(_ context.Context, _ chatops.IncomingMessage) error {
	return b.handleErr
}

// sentTextCount returns the number of recorded SendMessage calls.
func (b *mockBot) sentTextCount() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.sentText)
}

// sentCardCount returns the number of recorded SendCard calls.
func (b *mockBot) sentCardCount() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.sentCard)
}

// --- test helpers ----------------------------------------------------------

// newTestAdapter builds an IMAdapter with a fresh conversation engine and a
// BotManager containing a single mockBot for the given platform. The returned
// values are the adapter, the engine, the bot manager, the mock bot and the
// platform string used for the channel.
func newTestAdapter(t *testing.T, platform chatops.Platform) (*IMAdapter, *ConversationEngine, *chatops.BotManager, *mockBot) {
	t.Helper()
	engine := newTestEngine()
	bots := chatops.NewBotManager()
	bot := newMockBot("test-bot", platform)
	require.NoError(t, bots.Register(bot))
	adapter, err := NewIMAdapter(IMAdapterConfig{
		Engine: engine,
		Bots:   bots,
	})
	require.NoError(t, err)
	return adapter, engine, bots, bot
}

// --- NewIMAdapter ----------------------------------------------------------

func TestNewIMAdapter_OK(t *testing.T) {
	engine := newTestEngine()
	bots := chatops.NewBotManager()
	adapter, err := NewIMAdapter(IMAdapterConfig{
		Engine: engine,
		Bots:   bots,
	})
	require.NoError(t, err)
	require.NotNil(t, adapter)
	assert.Equal(t, 0, adapter.SessionCount())
}

func TestNewIMAdapter_NilEngine(t *testing.T) {
	bots := chatops.NewBotManager()
	_, err := NewIMAdapter(IMAdapterConfig{
		Engine: nil,
		Bots:   bots,
	})
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrIMNilEngine), "expected ErrIMNilEngine, got %v", err)
}

func TestNewIMAdapter_NilBots(t *testing.T) {
	engine := newTestEngine()
	_, err := NewIMAdapter(IMAdapterConfig{
		Engine: engine,
		Bots:   nil,
	})
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrIMNilBots), "expected ErrIMNilBots, got %v", err)
}

func TestNewIMAdapter_WithLogger(t *testing.T) {
	engine := newTestEngine()
	bots := chatops.NewBotManager()
	adapter, err := NewIMAdapter(IMAdapterConfig{
		Engine: engine,
		Bots:   bots,
	})
	require.NoError(t, err)
	require.NotNil(t, adapter)
	assert.NotNil(t, adapter.log)
}

// --- GetOrCreateSession ----------------------------------------------------

func TestIMAdapter_GetOrCreateSession_New(t *testing.T) {
	adapter, engine, _, _ := newTestAdapter(t, chatops.PlatformFeishu)

	sid, err := adapter.GetOrCreateSession("ch1", "u1")
	require.NoError(t, err)
	assert.NotEmpty(t, sid)
	assert.Equal(t, 1, adapter.SessionCount())

	// The session should exist in the engine.
	sess, err := engine.GetSession(sid)
	require.NoError(t, err)
	assert.Equal(t, "u1", sess.UserID)
}

func TestIMAdapter_GetOrCreateSession_Existing(t *testing.T) {
	adapter, _, _, _ := newTestAdapter(t, chatops.PlatformFeishu)

	sid1, err := adapter.GetOrCreateSession("ch1", "u1")
	require.NoError(t, err)

	// Second call for the same channel returns the same session id.
	sid2, err := adapter.GetOrCreateSession("ch1", "u1")
	require.NoError(t, err)
	assert.Equal(t, sid1, sid2)
	assert.Equal(t, 1, adapter.SessionCount())
}

func TestIMAdapter_GetOrCreateSession_EmptyUser(t *testing.T) {
	adapter, _, _, _ := newTestAdapter(t, chatops.PlatformFeishu)

	// An empty user id causes engine.NewSession to fail.
	_, err := adapter.GetOrCreateSession("ch1", "")
	require.Error(t, err)
	assert.Equal(t, 0, adapter.SessionCount())
}

// --- GetSessionID ---------------------------------------------------------

func TestIMAdapter_GetSessionID_Found(t *testing.T) {
	adapter, _, _, _ := newTestAdapter(t, chatops.PlatformFeishu)

	sid, err := adapter.GetOrCreateSession("ch1", "u1")
	require.NoError(t, err)

	got, ok := adapter.GetSessionID("ch1")
	require.True(t, ok)
	assert.Equal(t, sid, got)
}

func TestIMAdapter_GetSessionID_NotFound(t *testing.T) {
	adapter, _, _, _ := newTestAdapter(t, chatops.PlatformFeishu)

	_, ok := adapter.GetSessionID("nonexistent")
	assert.False(t, ok)
}

// --- CloseSession ---------------------------------------------------------

func TestIMAdapter_CloseSession_OK(t *testing.T) {
	adapter, engine, _, _ := newTestAdapter(t, chatops.PlatformFeishu)

	sid, err := adapter.GetOrCreateSession("ch1", "u1")
	require.NoError(t, err)
	require.Equal(t, 1, adapter.SessionCount())

	err = adapter.CloseSession("ch1")
	require.NoError(t, err)
	assert.Equal(t, 0, adapter.SessionCount())

	// The session should be gone from the engine too.
	_, err = engine.GetSession(sid)
	assert.Error(t, err)
}

func TestIMAdapter_CloseSession_NotFound(t *testing.T) {
	adapter, _, _, _ := newTestAdapter(t, chatops.PlatformFeishu)

	err := adapter.CloseSession("nonexistent")
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrIMChannelNotFound), "expected ErrIMChannelNotFound, got %v", err)
}

// --- HandleIMMessage: text reply ------------------------------------------

func TestIMAdapter_HandleIMMessage_TextReply(t *testing.T) {
	adapter, _, _, bot := newTestAdapter(t, chatops.PlatformFeishu)

	msg := chatops.IncomingMessage{
		Platform: chatops.PlatformFeishu,
		Channel:  "ch1",
		User:     "u1",
		Text:     "hello",
	}
	result, err := adapter.HandleIMMessage(context.Background(), msg)
	require.NoError(t, err)
	require.NotNil(t, result)
	assert.NotEmpty(t, result.Reply)
	assert.Nil(t, result.Card)

	// The bot should have received a text message.
	assert.Equal(t, 1, bot.sentTextCount())
	assert.Equal(t, 0, bot.sentCardCount())
}

func TestIMAdapter_HandleIMMessage_HelpCommand(t *testing.T) {
	adapter, _, _, bot := newTestAdapter(t, chatops.PlatformFeishu)

	msg := chatops.IncomingMessage{
		Platform: chatops.PlatformFeishu,
		Channel:  "ch1",
		User:     "u1",
		Text:     "/help",
	}
	result, err := adapter.HandleIMMessage(context.Background(), msg)
	require.NoError(t, err)
	require.NotNil(t, result)
	assert.Contains(t, result.Reply, "LEVEE")
	assert.Equal(t, 1, bot.sentTextCount())
}

// --- HandleIMMessage: card reply ------------------------------------------

func TestIMAdapter_HandleIMMessage_CardReply(t *testing.T) {
	adapter, _, _, bot := newTestAdapter(t, chatops.PlatformFeishu)

	// /diagnose localhost drives the engine through diagnose + recommend
	// and produces a Reply with a non-nil Card (approval card).
	msg := chatops.IncomingMessage{
		Platform: chatops.PlatformFeishu,
		Channel:  "ch1",
		User:     "u1",
		Text:     "/diagnose localhost",
	}
	result, err := adapter.HandleIMMessage(context.Background(), msg)
	require.NoError(t, err)
	require.NotNil(t, result)
	require.NotNil(t, result.Card, "expected a card reply from /diagnose")
	assert.Equal(t, chatops.CardKindApproval, result.Card.Kind)

	// The bot should have received a card, not a text message.
	assert.Equal(t, 1, bot.sentCardCount())
	assert.Equal(t, 0, bot.sentTextCount())
}

// --- HandleIMMessage: errors ----------------------------------------------

func TestIMAdapter_HandleIMMessage_EmptyMessage(t *testing.T) {
	adapter, _, _, _ := newTestAdapter(t, chatops.PlatformFeishu)

	msg := chatops.IncomingMessage{
		Platform: chatops.PlatformFeishu,
		Channel:  "ch1",
		User:     "u1",
		Text:     "",
	}
	_, err := adapter.HandleIMMessage(context.Background(), msg)
	require.Error(t, err)
	// The engine returns ErrEmptyMessage; the adapter wraps it.
	assert.True(t, errors.Is(err, ErrEmptyMessage), "expected ErrEmptyMessage, got %v", err)
}

func TestIMAdapter_HandleIMMessage_GetOrCreateSessionError(t *testing.T) {
	adapter, _, _, _ := newTestAdapter(t, chatops.PlatformFeishu)

	// An empty user id causes GetOrCreateSession -> engine.NewSession to
	// fail before the message text is even forwarded.
	msg := chatops.IncomingMessage{
		Platform: chatops.PlatformFeishu,
		Channel:  "ch1",
		User:     "",
		Text:     "hello",
	}
	_, err := adapter.HandleIMMessage(context.Background(), msg)
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrEmptyMessage), "expected ErrEmptyMessage, got %v", err)
	// No mapping should have been recorded.
	assert.Equal(t, 0, adapter.SessionCount())
}

func TestIMAdapter_HandleIMMessage_SessionNotFound(t *testing.T) {
	adapter, engine, _, _ := newTestAdapter(t, chatops.PlatformFeishu)

	// First message creates the channel→session mapping.
	msg := chatops.IncomingMessage{
		Platform: chatops.PlatformFeishu,
		Channel:  "ch1",
		User:     "u1",
		Text:     "hello",
	}
	_, err := adapter.HandleIMMessage(context.Background(), msg)
	require.NoError(t, err)

	// Close the underlying engine session while keeping the adapter mapping.
	sid, ok := adapter.GetSessionID("ch1")
	require.True(t, ok)
	require.NoError(t, engine.CloseSession(sid))

	// The next message should fail because the engine session is gone.
	_, err = adapter.HandleIMMessage(context.Background(), msg)
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrSessionNotFound), "expected ErrSessionNotFound, got %v", err)
}

func TestIMAdapter_HandleIMMessage_BotNotFound(t *testing.T) {
	// Adapter with a bot manager that has no bot for the message platform.
	engine := newTestEngine()
	bots := chatops.NewBotManager()
	adapter, err := NewIMAdapter(IMAdapterConfig{
		Engine: engine,
		Bots:   bots,
	})
	require.NoError(t, err)

	msg := chatops.IncomingMessage{
		Platform: chatops.PlatformSlack,
		Channel:  "ch1",
		User:     "u1",
		Text:     "hello",
	}
	result, err := adapter.HandleIMMessage(context.Background(), msg)
	// The engine produced a reply so the result is returned, but delivery
	// fails with ErrIMBotNotFound.
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrIMBotNotFound), "expected ErrIMBotNotFound, got %v", err)
	require.NotNil(t, result)
	assert.NotEmpty(t, result.Reply)
}

func TestIMAdapter_HandleIMMessage_NilContext(t *testing.T) {
	adapter, _, _, bot := newTestAdapter(t, chatops.PlatformFeishu)

	msg := chatops.IncomingMessage{
		Platform: chatops.PlatformFeishu,
		Channel:  "ch1",
		User:     "u1",
		Text:     "hello",
	}
	// A nil context should be replaced with context.Background().
	result, err := adapter.HandleIMMessage(nil, msg)
	require.NoError(t, err)
	require.NotNil(t, result)
	assert.Equal(t, 1, bot.sentTextCount())
}

func TestIMAdapter_HandleIMMessage_SendError(t *testing.T) {
	adapter, _, _, bot := newTestAdapter(t, chatops.PlatformFeishu)
	bot.sendErr = errors.New("network down")

	msg := chatops.IncomingMessage{
		Platform: chatops.PlatformFeishu,
		Channel:  "ch1",
		User:     "u1",
		Text:     "hello",
	}
	result, err := adapter.HandleIMMessage(context.Background(), msg)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "send text")
	require.NotNil(t, result)
}

func TestIMAdapter_HandleIMMessage_SendCardError(t *testing.T) {
	adapter, _, _, bot := newTestAdapter(t, chatops.PlatformFeishu)
	bot.cardErr = errors.New("card rejected")

	msg := chatops.IncomingMessage{
		Platform: chatops.PlatformFeishu,
		Channel:  "ch1",
		User:     "u1",
		Text:     "/diagnose localhost",
	}
	result, err := adapter.HandleIMMessage(context.Background(), msg)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "send card")
	require.NotNil(t, result)
	require.NotNil(t, result.Card)
}

// --- BroadcastToChannel ---------------------------------------------------

func TestIMAdapter_BroadcastToChannel_OK(t *testing.T) {
	adapter, _, _, bot := newTestAdapter(t, chatops.PlatformFeishu)

	err := adapter.BroadcastToChannel(context.Background(), chatops.PlatformFeishu, "ch1", "ping")
	require.NoError(t, err)
	assert.Equal(t, 1, bot.sentTextCount())
}

func TestIMAdapter_BroadcastToChannel_BotNotFound(t *testing.T) {
	adapter, _, _, _ := newTestAdapter(t, chatops.PlatformFeishu)

	err := adapter.BroadcastToChannel(context.Background(), chatops.PlatformSlack, "ch1", "ping")
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrIMBotNotFound), "expected ErrIMBotNotFound, got %v", err)
}

func TestIMAdapter_BroadcastToChannel_NilContext(t *testing.T) {
	adapter, _, _, bot := newTestAdapter(t, chatops.PlatformFeishu)

	err := adapter.BroadcastToChannel(nil, chatops.PlatformFeishu, "ch1", "ping")
	require.NoError(t, err)
	assert.Equal(t, 1, bot.sentTextCount())
}

func TestIMAdapter_BroadcastToChannel_SendError(t *testing.T) {
	adapter, _, _, bot := newTestAdapter(t, chatops.PlatformFeishu)
	bot.sendErr = errors.New("offline")

	err := adapter.BroadcastToChannel(context.Background(), chatops.PlatformFeishu, "ch1", "ping")
	require.Error(t, err)
}

// --- SendCardToChannel ----------------------------------------------------

func TestIMAdapter_SendCardToChannel_OK(t *testing.T) {
	adapter, _, _, bot := newTestAdapter(t, chatops.PlatformFeishu)
	card := chatops.NewCardBuilder().
		WithKind(chatops.CardKindNotification).
		WithTitle("test").
		Build()

	err := adapter.SendCardToChannel(context.Background(), chatops.PlatformFeishu, "ch1", card)
	require.NoError(t, err)
	assert.Equal(t, 1, bot.sentCardCount())
}

func TestIMAdapter_SendCardToChannel_BotNotFound(t *testing.T) {
	adapter, _, _, _ := newTestAdapter(t, chatops.PlatformFeishu)
	card := chatops.NewCardBuilder().WithTitle("test").Build()

	err := adapter.SendCardToChannel(context.Background(), chatops.PlatformSlack, "ch1", card)
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrIMBotNotFound), "expected ErrIMBotNotFound, got %v", err)
}

func TestIMAdapter_SendCardToChannel_NilCard(t *testing.T) {
	adapter, _, _, bot := newTestAdapter(t, chatops.PlatformFeishu)

	err := adapter.SendCardToChannel(context.Background(), chatops.PlatformFeishu, "ch1", nil)
	require.NoError(t, err)
	assert.Equal(t, 0, bot.sentCardCount())
}

func TestIMAdapter_SendCardToChannel_NilContext(t *testing.T) {
	adapter, _, _, bot := newTestAdapter(t, chatops.PlatformFeishu)
	card := chatops.NewCardBuilder().WithTitle("test").Build()

	err := adapter.SendCardToChannel(nil, chatops.PlatformFeishu, "ch1", card)
	require.NoError(t, err)
	assert.Equal(t, 1, bot.sentCardCount())
}

func TestIMAdapter_SendCardToChannel_SendError(t *testing.T) {
	adapter, _, _, bot := newTestAdapter(t, chatops.PlatformFeishu)
	bot.cardErr = errors.New("card endpoint down")
	card := chatops.NewCardBuilder().WithTitle("test").Build()

	err := adapter.SendCardToChannel(context.Background(), chatops.PlatformFeishu, "ch1", card)
	require.Error(t, err)
}

// --- SessionCount ---------------------------------------------------------

func TestIMAdapter_SessionCount(t *testing.T) {
	adapter, _, _, _ := newTestAdapter(t, chatops.PlatformFeishu)

	assert.Equal(t, 0, adapter.SessionCount())

	_, err := adapter.GetOrCreateSession("ch1", "u1")
	require.NoError(t, err)
	assert.Equal(t, 1, adapter.SessionCount())

	_, err = adapter.GetOrCreateSession("ch2", "u1")
	require.NoError(t, err)
	assert.Equal(t, 2, adapter.SessionCount())

	require.NoError(t, adapter.CloseSession("ch1"))
	assert.Equal(t, 1, adapter.SessionCount())
}

// --- Channels -------------------------------------------------------------

func TestIMAdapter_Channels(t *testing.T) {
	adapter, _, _, _ := newTestAdapter(t, chatops.PlatformFeishu)

	_, err := adapter.GetOrCreateSession("ch2", "u1")
	require.NoError(t, err)
	_, err = adapter.GetOrCreateSession("ch1", "u1")
	require.NoError(t, err)
	_, err = adapter.GetOrCreateSession("ch3", "u1")
	require.NoError(t, err)

	channels := adapter.Channels()
	assert.Equal(t, []string{"ch1", "ch2", "ch3"}, channels)
}

func TestIMAdapter_Channels_Empty(t *testing.T) {
	adapter, _, _, _ := newTestAdapter(t, chatops.PlatformFeishu)
	channels := adapter.Channels()
	assert.Empty(t, channels)
}

// --- Close ----------------------------------------------------------------

func TestIMAdapter_Close(t *testing.T) {
	adapter, _, _, _ := newTestAdapter(t, chatops.PlatformFeishu)

	_, err := adapter.GetOrCreateSession("ch1", "u1")
	require.NoError(t, err)
	_, err = adapter.GetOrCreateSession("ch2", "u1")
	require.NoError(t, err)
	require.Equal(t, 2, adapter.SessionCount())

	err = adapter.Close()
	require.NoError(t, err)
	assert.Equal(t, 0, adapter.SessionCount())
	assert.Empty(t, adapter.Channels())

	// Close is idempotent.
	err = adapter.Close()
	require.NoError(t, err)
}

// --- Concurrent safety ----------------------------------------------------

func TestIMAdapter_Concurrent_HandleIMMessage(t *testing.T) {
	adapter, _, _, bot := newTestAdapter(t, chatops.PlatformFeishu)

	const goroutines = 20
	const messagesPerGoroutine = 10

	var wg sync.WaitGroup
	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		go func(id int) {
			defer wg.Done()
			ch := fmt.Sprintf("ch-%d", id)
			for j := 0; j < messagesPerGoroutine; j++ {
				msg := chatops.IncomingMessage{
					Platform: chatops.PlatformFeishu,
					Channel:  ch,
					User:     "u1",
					Text:     fmt.Sprintf("msg-%d", j),
				}
				_, err := adapter.HandleIMMessage(context.Background(), msg)
				assert.NoError(t, err)
			}
		}(i)
	}
	wg.Wait()

	// Each goroutine used its own channel, so we expect exactly goroutines
	// channel→session mappings.
	assert.Equal(t, goroutines, adapter.SessionCount())

	// Every message should have been delivered to the bot.
	totalSent := bot.sentTextCount()
	assert.Equal(t, goroutines*messagesPerGoroutine, totalSent)
}

func TestIMAdapter_Concurrent_GetOrCreateSession(t *testing.T) {
	adapter, _, _, _ := newTestAdapter(t, chatops.PlatformFeishu)

	// Many goroutines race to create the mapping for the same channel.
	const goroutines = 50
	var wg sync.WaitGroup
	wg.Add(goroutines)
	sids := make([]string, goroutines)
	for i := 0; i < goroutines; i++ {
		go func(idx int) {
			defer wg.Done()
			sid, err := adapter.GetOrCreateSession("shared-ch", "u1")
			if err == nil {
				sids[idx] = sid
			}
		}(i)
	}
	wg.Wait()

	// All successful callers must observe the same session id.
	first := ""
	for _, sid := range sids {
		if sid == "" {
			continue
		}
		if first == "" {
			first = sid
		} else {
			assert.Equal(t, first, sid, "concurrent callers observed different session ids")
		}
	}
	assert.NotEmpty(t, first)
	assert.Equal(t, 1, adapter.SessionCount())
}

// --- CloseSession with engine error ---------------------------------------

func TestIMAdapter_CloseSession_EngineError(t *testing.T) {
	adapter, engine, _, _ := newTestAdapter(t, chatops.PlatformFeishu)

	sid, err := adapter.GetOrCreateSession("ch1", "u1")
	require.NoError(t, err)

	// Close the engine session first so the adapter's CloseSession finds
	// it gone and returns an error.
	require.NoError(t, engine.CloseSession(sid))

	err = adapter.CloseSession("ch1")
	// The mapping is removed but the engine returns ErrSessionNotFound.
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrSessionNotFound), "expected ErrSessionNotFound, got %v", err)
	// The mapping should still be removed.
	assert.Equal(t, 0, adapter.SessionCount())
}