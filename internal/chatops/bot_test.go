package chatops

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// --- ParsePlatform --------------------------------------------------------

func TestParsePlatform_Known(t *testing.T) {
	cases := []struct {
		in   string
		want Platform
	}{
		{"feishu", PlatformFeishu},
		{"Lark", PlatformFeishu},
		{"FEISHU", PlatformFeishu},
		{"dingtalk", PlatformDingtalk},
		{"dd", PlatformDingtalk},
		{"slack", PlatformSlack},
	}
	for _, c := range cases {
		got, err := ParsePlatform(c.in)
		require.NoError(t, err, "input %q", c.in)
		assert.Equal(t, c.want, got, "input %q", c.in)
	}
}

func TestParsePlatform_Unknown(t *testing.T) {
	_, err := ParsePlatform("telegram")
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrUnknownPlatform))
}

// --- CommandRouter --------------------------------------------------------

func TestCommandRouter_ParseAndDispatch(t *testing.T) {
	r := NewCommandRouter()

	// Register a custom handler that records its invocation.
	var called atomic.Int32
	r.Register("echo", func(ctx context.Context, msg IncomingMessage, args []string) (CommandResult, error) {
		called.Add(1)
		return CommandResult{Reply: "echo: " + joinArgs(args)}, nil
	})

	msg := IncomingMessage{Platform: PlatformFeishu, Text: "/levee echo hello world"}
	result, err := r.Dispatch(context.Background(), msg)
	require.NoError(t, err)
	assert.Equal(t, "echo: hello world", result.Reply)
	assert.Equal(t, int32(1), called.Load())
}

func TestCommandRouter_EmptyMessage(t *testing.T) {
	r := NewCommandRouter()
	result, err := r.Dispatch(context.Background(), IncomingMessage{Text: ""})
	require.NoError(t, err, "empty message should be a no-op")
	assert.Empty(t, result.Reply)
}

func TestCommandRouter_NonCommandMessage(t *testing.T) {
	r := NewCommandRouter()
	result, err := r.Dispatch(context.Background(), IncomingMessage{Text: "hello there"})
	require.NoError(t, err, "non-prefix message should be a no-op")
	assert.Empty(t, result.Reply)
}

func TestCommandRouter_UnknownVerb(t *testing.T) {
	r := NewCommandRouter()
	result, err := r.Dispatch(context.Background(), IncomingMessage{Text: "/levee frobnicate x"})
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrUnknownCommand))
	assert.Contains(t, result.Reply, "unknown command")
}

func TestCommandRouter_BuiltinHelp(t *testing.T) {
	r := NewCommandRouter()
	result, err := r.Dispatch(context.Background(), IncomingMessage{Text: "/levee help"})
	require.NoError(t, err)
	assert.Contains(t, result.Reply, "available commands")
}

func TestCommandRouter_BuiltinApproveNoArgs(t *testing.T) {
	r := NewCommandRouter()
	result, err := r.Dispatch(context.Background(), IncomingMessage{Text: "/levee approve"})
	require.NoError(t, err)
	assert.Contains(t, result.Reply, "usage")
}

func TestCommandRouter_BuiltinApproveWithArgs(t *testing.T) {
	r := NewCommandRouter()
	result, err := r.Dispatch(context.Background(), IncomingMessage{Text: "/levee approve run-123"})
	require.NoError(t, err)
	assert.Contains(t, result.Reply, "run-123")
}

func TestCommandRouter_SetPrefix(t *testing.T) {
	r := NewCommandRouter()
	require.NoError(t, r.SetPrefix("/lv"))
	result, err := r.Dispatch(context.Background(), IncomingMessage{Text: "/lv help"})
	require.NoError(t, err)
	assert.Contains(t, result.Reply, "available commands")

	// Old prefix no longer recognised.
	result, err = r.Dispatch(context.Background(), IncomingMessage{Text: "/levee help"})
	require.NoError(t, err)
	assert.Empty(t, result.Reply)
}

func TestCommandRouter_SetPrefix_Invalid(t *testing.T) {
	r := NewCommandRouter()
	err := r.SetPrefix("levee")
	require.Error(t, err)
}

func TestCommandRouter_Verbs(t *testing.T) {
	r := NewCommandRouter()
	verbs := r.Verbs()
	assert.Contains(t, verbs, "list")
	assert.Contains(t, verbs, "approve")
	assert.Contains(t, verbs, "reject")
	assert.Contains(t, verbs, "status")
	assert.Contains(t, verbs, "help")
}

// --- BotManager -----------------------------------------------------------

// fakeBot is a minimal Bot implementation for manager tests.
type fakeBot struct {
	name     string
	platform Platform
	started  atomic.Int32
	stopped  atomic.Int32
	eventCh  chan Event
}

func newFakeBot(name string, p Platform) *fakeBot {
	return &fakeBot{name: name, platform: p, eventCh: make(chan Event, 8)}
}

func (b *fakeBot) Name() string                                                 { return b.name }
func (b *fakeBot) Platform() Platform                                           { return b.platform }
func (b *fakeBot) Start(ctx context.Context) error                              { b.started.Add(1); return nil }
func (b *fakeBot) Stop() error                                                  { b.stopped.Add(1); return nil }
func (b *fakeBot) SubscribeEvents() <-chan Event                                { return b.eventCh }
func (b *fakeBot) PublishEvent(evt Event) error                                 { b.eventCh <- evt; return nil }
func (b *fakeBot) SendMessage(ctx context.Context, ch, text string) error       { return nil }
func (b *fakeBot) SendCard(ctx context.Context, ch string, card *Card) error    { return nil }
func (b *fakeBot) HandleMessage(ctx context.Context, msg IncomingMessage) error { return nil }

func TestBotManager_RegisterAndGet(t *testing.T) {
	m := NewBotManager()
	b := newFakeBot("f1", PlatformFeishu)
	require.NoError(t, m.Register(b))

	got, err := m.Get("f1")
	require.NoError(t, err)
	assert.Equal(t, b, got)

	_, err = m.Get("missing")
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrBotNotFound))
}

func TestBotManager_RegisterDuplicate(t *testing.T) {
	m := NewBotManager()
	require.NoError(t, m.Register(newFakeBot("dup", PlatformFeishu)))
	err := m.Register(newFakeBot("dup", PlatformFeishu))
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrBotExists))
}

func TestBotManager_RegisterNil(t *testing.T) {
	m := NewBotManager()
	err := m.Register(nil)
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrBotNotFound))
}

func TestBotManager_Unregister(t *testing.T) {
	m := NewBotManager()
	require.NoError(t, m.Register(newFakeBot("g", PlatformSlack)))
	require.NoError(t, m.Unregister("g"))
	_, err := m.Get("g")
	require.Error(t, err)

	err = m.Unregister("g")
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrBotNotFound))
}

func TestBotManager_ByPlatform(t *testing.T) {
	m := NewBotManager()
	require.NoError(t, m.Register(newFakeBot("s1", PlatformSlack)))
	require.NoError(t, m.Register(newFakeBot("s2", PlatformSlack)))
	require.NoError(t, m.Register(newFakeBot("f1", PlatformFeishu)))

	got, err := m.ByPlatform(PlatformSlack)
	require.NoError(t, err)
	assert.Equal(t, PlatformSlack, got.Platform())
	// Lexically smallest name wins.
	assert.Equal(t, "s1", got.Name())

	_, err = m.ByPlatform(PlatformDingtalk)
	require.Error(t, err)
}

func TestBotManager_Names(t *testing.T) {
	m := NewBotManager()
	require.NoError(t, m.Register(newFakeBot("b", PlatformSlack)))
	require.NoError(t, m.Register(newFakeBot("a", PlatformFeishu)))
	require.NoError(t, m.Register(newFakeBot("c", PlatformDingtalk)))
	assert.Equal(t, []string{"a", "b", "c"}, m.Names())
}

func TestBotManager_StartAllStopAll(t *testing.T) {
	m := NewBotManager()
	b1 := newFakeBot("b1", PlatformFeishu)
	b2 := newFakeBot("b2", PlatformSlack)
	require.NoError(t, m.Register(b1))
	require.NoError(t, m.Register(b2))

	require.NoError(t, m.StartAll(context.Background()))
	assert.Equal(t, int32(1), b1.started.Load())
	assert.Equal(t, int32(1), b2.started.Load())

	require.NoError(t, m.StopAll())
	assert.Equal(t, int32(1), b1.stopped.Load())
	assert.Equal(t, int32(1), b2.stopped.Load())
}

func TestBotManager_BroadcastEvent(t *testing.T) {
	m := NewBotManager()
	b1 := newFakeBot("b1", PlatformFeishu)
	b2 := newFakeBot("b2", PlatformSlack)
	require.NoError(t, m.Register(b1))
	require.NoError(t, m.Register(b2))

	evt := Event{Type: EventRunStarted, RunID: "run-1", Title: "started"}
	require.NoError(t, m.BroadcastEvent(evt))

	select {
	case got := <-b1.eventCh:
		assert.Equal(t, "run-1", got.RunID)
	case <-time.After(time.Second):
		t.Fatal("b1 did not receive event")
	}
	select {
	case got := <-b2.eventCh:
		assert.Equal(t, "run-1", got.RunID)
	case <-time.After(time.Second):
		t.Fatal("b2 did not receive event")
	}
}

func TestBotManager_HandleMessage(t *testing.T) {
	m := NewBotManager()
	require.NoError(t, m.Register(newFakeBot("f", PlatformFeishu)))

	err := m.HandleMessage(context.Background(), IncomingMessage{Platform: PlatformFeishu, Text: "/levee help"})
	require.NoError(t, err)

	err = m.HandleMessage(context.Background(), IncomingMessage{Platform: PlatformSlack, Text: "/levee help"})
	require.Error(t, err)
}

// --- baseBot --------------------------------------------------------------

func TestBaseBot_PublishEventAfterStop(t *testing.T) {
	b, err := NewFeishuBot(FeishuConfig{Name: "x", WebhookURL: "http://example.com"}, nil)
	require.NoError(t, err)
	require.NoError(t, b.Stop())

	err = b.PublishEvent(Event{Type: EventRunStarted})
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrBotClosed))
}

func TestBaseBot_StartTwice(t *testing.T) {
	b, err := NewFeishuBot(FeishuConfig{Name: "x", WebhookURL: "http://example.com"}, nil)
	require.NoError(t, err)
	require.NoError(t, b.Start(context.Background()))
	defer b.Stop()

	err = b.Start(context.Background())
	require.Error(t, err)
}

// --- helpers --------------------------------------------------------------

func joinArgs(args []string) string {
	out := ""
	for i, a := range args {
		if i > 0 {
			out += " "
		}
		out += a
	}
	return out
}
