// cmd_serve_chatops_test.go — the notify.chatops wiring helper. Validation
// must be fail-closed and name the offending bot (a bot silently skipped at
// boot would look like a working mirror that never delivers), and the
// happy path must register + start every configured bot.
package main

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nexus/levee/internal/config"
)

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
