// cmd_serve_chatops_test.go — the notify.chatops wiring helper. Validation
// must be fail-closed and name the offending bot (a bot silently skipped at
// boot would look like a working mirror that never delivers), and the
// happy path must register + start every configured bot.
package main

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nexus/levee/internal/approval"
	"github.com/nexus/levee/internal/chatops"
	"github.com/nexus/levee/internal/config"
	"github.com/nexus/levee/internal/grpc"
	"github.com/nexus/levee/internal/state"
)

func strconvFormatNow() string  { return strconv.FormatInt(time.Now().Unix(), 10) }
func hexEncode(b []byte) string { return hex.EncodeToString(b) }

func TestBuildServeChatOpsBots_EnabledRequiresAtLeastOneBot(t *testing.T) {
	_, err := buildServeChatOpsBots(config.ChatOpsConfig{Enabled: true})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "at least one bot")
}

func TestBuildServeChatOpsBots_MissingWebhookNamesTheBot(t *testing.T) {
	_, err := buildServeChatOpsBots(config.ChatOpsConfig{
		Enabled: true,
		Bots:    []config.ChatOpsBotConfig{{Platform: "slack", Name: "approvals"}},
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), `"approvals"`, "the error must name the offending bot")
	assert.Contains(t, err.Error(), "webhook_url")
}

func TestBuildServeChatOpsBots_UnknownPlatformRefused(t *testing.T) {
	_, err := buildServeChatOpsBots(config.ChatOpsConfig{
		Enabled: true,
		Bots: []config.ChatOpsBotConfig{
			{Platform: "teams", Name: "t1", WebhookURL: "https://example.invalid/hook"},
		},
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "teams")
	assert.Contains(t, err.Error(), "slack, dingtalk, feishu")
}

func TestBuildServeChatOpsBots_HappyPathStartsEveryBot(t *testing.T) {
	mgr, err := buildServeChatOpsBots(config.ChatOpsConfig{
		Enabled: true,
		Bots: []config.ChatOpsBotConfig{
			{Platform: "slack", Name: "s", WebhookURL: "https://hooks.slack.invalid/x"},
			{Platform: "dingtalk", Name: "d", WebhookURL: "https://oapi.dingtalk.invalid/x", Secret: "ss"},
			{Platform: "feishu", Name: "f", WebhookURL: "https://open.feishu.invalid/x"},
		},
	})
	require.NoError(t, err)
	require.NotNil(t, mgr)
	t.Cleanup(func() { mgr.StopAll() })

	assert.ElementsMatch(t, []string{"s", "d", "f"}, mgr.Names(),
		"every configured bot must be registered and started")
}

// --- inbound wiring ----------------------------------------------------------

// newTestBotManager returns a manager with real (unstarted) adapters
// registered, which is all mgr.Get and the ingress constructor need.
func newTestBotManager(t *testing.T) *chatops.BotManager {
	t.Helper()
	mgr := chatops.NewBotManager()
	for _, spec := range []struct {
		platform, name, secret string
	}{
		{"slack", "approvals", "slack-secret"},
		{"dingtalk", "dd", "dd-secret"},
		{"feishu", "fs", ""}, // outbound-only: no secret, no ingress
	} {
		platform, err := chatops.ParsePlatform(spec.platform)
		require.NoError(t, err)
		var bot chatops.Bot
		switch platform {
		case chatops.PlatformSlack:
			bot, err = chatops.NewSlackBot(chatops.SlackConfig{
				Name: spec.name, WebhookURL: "https://hooks.slack.invalid/x", Secret: spec.secret,
			}, mgr.Router())
		case chatops.PlatformDingtalk:
			bot, err = chatops.NewDingtalkBot(chatops.DingtalkConfig{
				Name: spec.name, WebhookURL: "https://oapi.dingtalk.invalid/x", Secret: spec.secret,
			}, mgr.Router())
		default:
			bot, err = chatops.NewFeishuBot(chatops.FeishuConfig{
				Name: spec.name, WebhookURL: "https://open.feishu.invalid/x",
			}, mgr.Router())
		}
		require.NoError(t, err)
		require.NoError(t, mgr.Register(bot))
	}
	return mgr
}

func TestChatOpsCallbackRoutes_SignedBotsGetIngress(t *testing.T) {
	mgr := newTestBotManager(t)
	routes, err := chatOpsCallbackRoutes(mgr, config.ChatOpsConfig{
		Enabled: true,
		Bots: []config.ChatOpsBotConfig{
			{Platform: "slack", Name: "approvals", WebhookURL: "https://hooks.slack.invalid/x", Secret: "s"},
			{Platform: "dingtalk", Name: "dd", WebhookURL: "https://oapi.dingtalk.invalid/x", Secret: "s"},
			{Platform: "feishu", Name: "fs", WebhookURL: "https://open.feishu.invalid/x"}, // no secret
		},
	})
	require.NoError(t, err)
	require.Len(t, routes, 2, "a bot with no signing secret gets no inbound path")

	byBot := map[string]string{}
	for _, r := range routes {
		byBot[r.Bot] = r.Path
		assert.NotNil(t, r.Handler)
	}
	assert.Equal(t, "/chatops/callback/approvals", byBot["approvals"])
	assert.Equal(t, "/chatops/callback/dd", byBot["dd"])
	_, ok := byBot["fs"]
	assert.False(t, ok)
}

func TestChatOpsCallbackRoutes_NilManager(t *testing.T) {
	routes, err := chatOpsCallbackRoutes(nil, config.ChatOpsConfig{})
	require.NoError(t, err)
	assert.Empty(t, routes)
}

func TestChatOpsCallbackPath_EscapesRegistrationKey(t *testing.T) {
	assert.Equal(t, "/chatops/callback/approvals", chatOpsCallbackPath("approvals"))
	assert.Equal(t, "/chatops/callback/a%2Fb", chatOpsCallbackPath("a/b"))
}

func TestWireChatOpsDecisions_ReplacesTheNoOpVerbs(t *testing.T) {
	mgr := newTestBotManager(t)
	// Before wiring, the built-in no-op answers.
	before, err := mgr.Router().Dispatch(context.Background(), chatops.IncomingMessage{
		Platform: chatops.PlatformSlack, Text: "/levee approve run-1",
	})
	require.NoError(t, err)
	assert.Contains(t, before.Reply, "no handler registered")

	wireChatOpsDecisions(mgr, config.ChatOpsConfig{}, nil, nil)

	// After wiring, no verb answers with the stub text any more.
	for _, verb := range []string{"approve", "reject", "status"} {
		res, derr := mgr.Router().Dispatch(context.Background(), chatops.IncomingMessage{
			Platform: chatops.PlatformSlack, Text: "/levee " + verb + " run-1",
		})
		require.NoError(t, derr)
		assert.NotContains(t, res.Reply, "no handler registered",
			"verb %s must be backed by a real handler", verb)
	}
}

func TestWireChatOpsDecisions_NilManagerIsNoOp(t *testing.T) {
	wireChatOpsDecisions(nil, config.ChatOpsConfig{}, nil, nil) // must not panic
}

func TestWireChatOpsDecisions_UnknownPlatformIgnored(t *testing.T) {
	mgr := newTestBotManager(t)
	// A bot whose platform never parsed is skipped by the identity-table
	// builder; the bot itself was already refused at construction.
	wireChatOpsDecisions(mgr, config.ChatOpsConfig{
		Bots: []config.ChatOpsBotConfig{
			{Platform: "teams", Name: "t", IdentityMap: map[string]string{"x": "y"}},
		},
	}, nil, nil)
	res, err := mgr.Router().Dispatch(context.Background(), chatops.IncomingMessage{
		Platform: chatops.PlatformSlack, Text: "/levee status run-1",
	})
	require.NoError(t, err)
	assert.NotContains(t, res.Reply, "no handler registered")
}

// --- end to end: signed callback -> router -> decision -> settled run --------

// recordingBot implements chatops.Bot and routes inbound messages through
// the real command router, the way every adapter does, so the test exercises
// the production path (verify → parse → dispatch → verb → service) rather
// than calling the decision handler directly.
type recordingBot struct {
	mu      sync.Mutex
	replies []string
	router  *chatops.CommandRouter
}

func (b *recordingBot) Name() string                { return "e2e" }
func (b *recordingBot) Platform() chatops.Platform  { return chatops.PlatformSlack }
func (b *recordingBot) Start(context.Context) error { return nil }
func (b *recordingBot) Stop() error                 { return nil }
func (b *recordingBot) SubscribeEvents() <-chan chatops.Event {
	return make(chan chatops.Event)
}
func (b *recordingBot) PublishEvent(chatops.Event) error { return nil }
func (b *recordingBot) SendMessage(_ context.Context, _, text string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.replies = append(b.replies, text)
	return nil
}
func (b *recordingBot) SendCard(context.Context, string, *chatops.Card) error { return nil }
func (b *recordingBot) HandleMessage(ctx context.Context, msg chatops.IncomingMessage) error {
	res, _ := b.router.Dispatch(ctx, msg)
	if res.Reply != "" {
		return b.SendMessage(ctx, msg.Channel, res.Reply)
	}
	return nil
}

func (b *recordingBot) lastReply() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.replies) == 0 {
		return ""
	}
	return b.replies[len(b.replies)-1]
}

func TestChatOpsInbound_SignedSlackCallbackDecidesRun(t *testing.T) {
	ctx := context.Background()
	store, err := state.NewSQLiteStore(ctx, t.TempDir()+"/e2e.db")
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })

	require.NoError(t, store.CreateRun(ctx, &state.Run{
		ID: "run-e2e", WorkflowName: "wf", Status: "draft", Creator: "alice",
	}))
	appr := approval.NewService(newApprovalStoreAdapter(store))
	_, err = appr.Create(ctx, approval.CreateRequest{
		RunID: "run-e2e", Level: approval.LevelStandard,
		Approvers: []string{"alice"}, MinApprovers: 1,
		ExpiresAt: time.Now().UTC().Add(time.Hour),
	})
	require.NoError(t, err)
	change := grpc.NewChangeService(store, nil, appr, nil)

	mgr := chatops.NewBotManager()
	bot := &recordingBot{router: mgr.Router()}
	require.NoError(t, mgr.Register(bot))
	const secret = "slack-signing-secret"
	wireChatOpsDecisions(mgr, config.ChatOpsConfig{
		Bots: []config.ChatOpsBotConfig{{
			Platform: "slack", Name: "e2e", WebhookURL: "https://hooks.slack.invalid/x",
			Secret: secret, IdentityMap: map[string]string{"U_ALICE": "alice"},
		}},
	}, appr, change)
	routes, err := chatOpsCallbackRoutes(mgr, config.ChatOpsConfig{
		Bots: []config.ChatOpsBotConfig{{
			Platform: "slack", Name: "e2e", WebhookURL: "https://hooks.slack.invalid/x",
			Secret: secret, IdentityMap: map[string]string{"U_ALICE": "alice"},
		}},
	})
	require.NoError(t, err)
	require.Len(t, routes, 1)

	// A signed Slack button click carrying the canonical command.
	body := `{"type":"interactive_message","user":{"id":"U_ALICE"},"channel":{"id":"C1"},` +
		`"actions":[{"value":"/levee approve run-e2e"}]}`
	ts := strconvFormatNow()
	mac := hmac.New(sha256.New, []byte("v0:"+secret))
	mac.Write([]byte("v0:" + ts + ":" + body))
	req := httptest.NewRequest(http.MethodPost, routes[0].Path, strings.NewReader(body))
	req.Header.Set("X-Slack-Request-Timestamp", ts)
	req.Header.Set("X-Slack-Signature", "v0="+hexEncode(mac.Sum(nil)))
	rec := httptest.NewRecorder()
	routes[0].Handler.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
	assert.Contains(t, bot.lastReply(), "已记录你对 run-e2e 的批准",
		"the click must reach the decision handler, not the stub")

	run, err := store.GetRun(ctx, "run-e2e")
	require.NoError(t, err)
	assert.Equal(t, "approved", run.Status, "the chat decision must settle the run")

	a, err := appr.ApprovalForRun(ctx, "run-e2e")
	require.NoError(t, err)
	require.Len(t, a.Decisions, 1)
	assert.Equal(t, "alice", a.Decisions[0].Approver,
		"the identity table maps the Slack member id to the LEVEE subject")
}

func TestChatOpsInbound_UnsignedCallbackNeverDecides(t *testing.T) {
	ctx := context.Background()
	store, err := state.NewSQLiteStore(ctx, t.TempDir()+"/e2e2.db")
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })

	require.NoError(t, store.CreateRun(ctx, &state.Run{
		ID: "run-e2e2", WorkflowName: "wf", Status: "draft", Creator: "alice",
	}))
	appr := approval.NewService(newApprovalStoreAdapter(store))
	_, err = appr.Create(ctx, approval.CreateRequest{
		RunID: "run-e2e2", Level: approval.LevelStandard,
		Approvers: []string{"alice"}, MinApprovers: 1,
		ExpiresAt: time.Now().UTC().Add(time.Hour),
	})
	require.NoError(t, err)

	mgr := chatops.NewBotManager()
	bot := &recordingBot{router: mgr.Router()}
	require.NoError(t, mgr.Register(bot))
	const secret = "slack-signing-secret"
	wireChatOpsDecisions(mgr, config.ChatOpsConfig{}, appr,
		grpc.NewChangeService(store, nil, appr, nil))
	routes, err := chatOpsCallbackRoutes(mgr, config.ChatOpsConfig{
		Bots: []config.ChatOpsBotConfig{{
			Platform: "slack", Name: "e2e", WebhookURL: "https://hooks.slack.invalid/x", Secret: secret,
		}},
	})
	require.NoError(t, err)
	require.Len(t, routes, 1)

	// No signature headers at all.
	body := `{"type":"interactive_message","user":{"id":"U_ALICE"},"channel":{"id":"C1"},` +
		`"actions":[{"value":"/levee approve run-e2e2"}]}`
	req := httptest.NewRequest(http.MethodPost, routes[0].Path, strings.NewReader(body))
	rec := httptest.NewRecorder()
	routes[0].Handler.ServeHTTP(rec, req)

	require.Equal(t, http.StatusUnauthorized, rec.Code)
	assert.Empty(t, bot.lastReply())

	run, err := store.GetRun(ctx, "run-e2e2")
	require.NoError(t, err)
	assert.Equal(t, "draft", run.Status, "an unsigned callback must not decide anything")
}
