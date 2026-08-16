package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/nexus/levee/internal/approval"
	"github.com/nexus/levee/internal/chatops"
	"github.com/nexus/levee/internal/state"
	"github.com/spf13/cobra"
)

// chatops 子命令选项。
var (
	chatopsOptPlatform string
	chatopsOptConfig   string
	chatopsOptChannel  string
	chatopsOptMessage  string
	chatopsOptReason   string
	chatopsOptTimeout  time.Duration
	chatopsOptChangeID string
)

func init() {
	RegisterCommand(newChatopsCmd())
}

// newChatopsCmd 构建 `levee chatops` 父命令及其四个子命令。
func newChatopsCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "chatops",
		Short: "ChatOps 集成：飞书 / 钉钉 / Slack 机器人管理",
		Long: "ChatOps 子命令用于管理 LEVEE 的 IM 平台机器人，支持\n" +
			"飞书 / 钉钉 / Slack 三种适配。可启动机器人、发送消息、\n" +
			"通过 CLI 触发审批通过 / 驳回。",
		Args: cobra.NoArgs,
	}
	cmd.AddCommand(newChatopsStartCmd())
	cmd.AddCommand(newChatopsSendCmd())
	cmd.AddCommand(newChatopsApproveCmd())
	cmd.AddCommand(newChatopsRejectCmd())
	return cmd
}

// newChatopsStartCmd 构建 `levee chatops start` 子命令。
func newChatopsStartCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "start",
		Short: "启动 ChatOps 机器人",
		Long: "根据 --platform 与 --config 启动对应平台的机器人，\n" +
			"开始监听 LEVEE 事件并向 IM 群推送卡片消息。",
		Args: cobra.NoArgs,
		RunE: runChatopsStart,
	}
	cmd.Flags().StringVar(&chatopsOptPlatform, "platform", "feishu", "平台：feishu / dingtalk / slack")
	cmd.Flags().StringVarP(&chatopsOptConfig, "config", "c", "", "机器人配置文件路径（JSON）")
	cmd.Flags().DurationVar(&chatopsOptTimeout, "timeout", 0, "运行时长；0 表示持续运行直到收到信号")
	return cmd
}

// newChatopsSendCmd 构建 `levee chatops send` 子命令。
func newChatopsSendCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "send",
		Short: "通过 ChatOps 机器人发送消息",
		Long: "通过 --platform 指定的机器人向 --channel 发送一条文本消息。",
		Args:  cobra.NoArgs,
		RunE:  runChatopsSend,
	}
	cmd.Flags().StringVar(&chatopsOptPlatform, "platform", "feishu", "平台：feishu / dingtalk / slack")
	cmd.Flags().StringVarP(&chatopsOptConfig, "config", "c", "", "机器人配置文件路径（JSON）")
	cmd.Flags().StringVarP(&chatopsOptChannel, "channel", "C", "", "目标 channel / 群 ID")
	cmd.Flags().StringVarP(&chatopsOptMessage, "message", "m", "", "消息内容")
	return cmd
}

// newChatopsApproveCmd 构建 `levee chatops approve` 子命令。
func newChatopsApproveCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "approve",
		Short: "通过 ChatOps 触发审批通过",
		Long: "通过 --id 指定的变更 ID 触发审批通过，复用 approval 服务。",
		Args:  cobra.MaximumNArgs(1),
		RunE:  runChatopsApprove,
	}
	cmd.Flags().StringVarP(&chatopsOptPlatform, "platform", "p", "feishu", "平台：feishu / dingtalk / slack")
	cmd.Flags().StringVarP(&chatopsOptConfig, "config", "c", "", "机器人配置文件路径（JSON，可选）")
	cmd.Flags().StringVar(&chatopsOptChangeID, "id", "", "变更 ID（等价于位置参数）")
	return cmd
}

// newChatopsRejectCmd 构建 `levee chatops reject` 子命令。
func newChatopsRejectCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "reject",
		Short: "通过 ChatOps 触发审批驳回",
		Long: "通过 --id 指定的变更 ID 触发审批驳回，需要 --reason。",
		Args:  cobra.MaximumNArgs(1),
		RunE:  runChatopsReject,
	}
	cmd.Flags().StringVarP(&chatopsOptPlatform, "platform", "p", "feishu", "平台：feishu / dingtalk / slack")
	cmd.Flags().StringVarP(&chatopsOptConfig, "config", "c", "", "机器人配置文件路径（JSON，可选）")
	cmd.Flags().StringVar(&chatopsOptChangeID, "id", "", "变更 ID（等价于位置参数）")
	cmd.Flags().StringVarP(&chatopsOptReason, "reason", "r", "", "驳回原因（必填）")
	return cmd
}

// --- run handlers ----------------------------------------------------------

// runChatopsStart 启动指定平台的机器人。在 CLI 形态下，启动后阻塞直到
// 超时或收到中断信号；在测试中可通过 --timeout 短时间运行。
func runChatopsStart(cmd *cobra.Command, args []string) error {
	platform, err := chatops.ParsePlatform(chatopsOptPlatform)
	if err != nil {
		return err
	}

	bot, err := buildBotFromConfig(platform, chatopsOptConfig)
	if err != nil {
		return err
	}

	mgr := chatops.NewBotManager()
	if err := mgr.Register(bot); err != nil {
		return err
	}

	ctx := context.Background()
	if err := bot.Start(ctx); err != nil {
		return fmt.Errorf("start bot: %w", err)
	}
	defer func() { _ = bot.Stop() }()

	out := map[string]any{
		"platform": string(platform),
		"bot":      bot.Name(),
		"status":   "started",
	}

	if optJSON {
		return PrintJSON(os.Stdout, map[string]any{"data": out, "meta": nil, "error": nil})
	}
	if optQuiet {
		fmt.Fprintln(os.Stdout, bot.Name())
		return nil
	}
	fmt.Fprintf(os.Stdout, "ChatOps bot %q (%s) started\n", bot.Name(), platform)

	if chatopsOptTimeout > 0 {
		time.Sleep(chatopsOptTimeout)
	} else {
		// 持续运行直到进程被外部信号终止。CLI 形态下我们简单地等待。
		select {}
	}
	return nil
}

// runChatopsSend 通过指定平台的机器人发送一条文本消息。
func runChatopsSend(cmd *cobra.Command, args []string) error {
	if chatopsOptChannel == "" {
		return fmt.Errorf("--channel is required [exit=2]")
	}
	if chatopsOptMessage == "" {
		return fmt.Errorf("--message is required [exit=2]")
	}

	platform, err := chatops.ParsePlatform(chatopsOptPlatform)
	if err != nil {
		return err
	}

	bot, err := buildBotFromConfig(platform, chatopsOptConfig)
	if err != nil {
		return err
	}

	ctx := context.Background()
	if err := bot.SendMessage(ctx, chatopsOptChannel, chatopsOptMessage); err != nil {
		return fmt.Errorf("send message: %w", err)
	}

	out := map[string]any{
		"platform": string(platform),
		"bot":      bot.Name(),
		"channel":  chatopsOptChannel,
		"message":  chatopsOptMessage,
		"status":   "sent",
	}

	if optJSON {
		return PrintJSON(os.Stdout, map[string]any{"data": out, "meta": nil, "error": nil})
	}
	if optQuiet {
		fmt.Fprintln(os.Stdout, "sent")
		return nil
	}
	fmt.Fprintf(os.Stdout, "Message sent to %s via %s\n", chatopsOptChannel, platform)
	return nil
}

// runChatopsApprove 通过 ChatOps 触发审批通过。它复用 approval 服务，
// 与 `levee approve` 命令等价，但记录执行来源为 chatops。
func runChatopsApprove(cmd *cobra.Command, args []string) error {
	changeID, err := readChangeIDArg(cmd, args)
	if err != nil {
		return err
	}

	approver := currentActor()
	if v := os.Getenv("LEVEE_CHATOPS_APPROVER"); v != "" {
		approver = v
	}

	ctx := context.Background()
	store, err := openStore(ctx)
	if err != nil {
		return fmt.Errorf("open store: %w", err)
	}
	defer store.Close()

	approvalID, err := findPendingApprovalID(ctx, store, changeID, "")
	if err != nil {
		return err
	}

	svc := approval.NewService(newApprovalStoreAdapter(store))
	if err := svc.Approve(ctx, approvalID, approver); err != nil {
		return mapApprovalError(err)
	}

	out := map[string]any{
		"change_id":   changeID,
		"approval_id": approvalID,
		"action":      "approved",
		"approver":    approver,
		"source":      "chatops",
		"platform":    chatopsOptPlatform,
	}

	if optJSON {
		return PrintJSON(os.Stdout, map[string]any{"data": out, "meta": nil, "error": nil})
	}
	if optQuiet {
		fmt.Fprintln(os.Stdout, approvalID)
		return nil
	}
	fmt.Fprintf(os.Stdout, "Change %s approved by %s via ChatOps (%s)\n",
		changeID, approver, chatopsOptPlatform)
	return nil
}

// runChatopsReject 通过 ChatOps 触发审批驳回。
func runChatopsReject(cmd *cobra.Command, args []string) error {
	changeID, err := readChangeIDArg(cmd, args)
	if err != nil {
		return err
	}
	if chatopsOptReason == "" {
		return fmt.Errorf("--reason is required for reject [exit=2]")
	}

	approver := currentActor()
	if v := os.Getenv("LEVEE_CHATOPS_APPROVER"); v != "" {
		approver = v
	}

	ctx := context.Background()
	store, err := openStore(ctx)
	if err != nil {
		return fmt.Errorf("open store: %w", err)
	}
	defer store.Close()

	approvalID, err := findPendingApprovalID(ctx, store, changeID, "")
	if err != nil {
		return err
	}

	svc := approval.NewService(newApprovalStoreAdapter(store))
	if err := svc.Reject(ctx, approvalID, approver, chatopsOptReason); err != nil {
		return mapApprovalError(err)
	}

	out := map[string]any{
		"change_id":   changeID,
		"approval_id": approvalID,
		"action":      "rejected",
		"approver":    approver,
		"reason":      chatopsOptReason,
		"source":      "chatops",
		"platform":    chatopsOptPlatform,
	}

	if optJSON {
		return PrintJSON(os.Stdout, map[string]any{"data": out, "meta": nil, "error": nil})
	}
	if optQuiet {
		fmt.Fprintln(os.Stdout, approvalID)
		return nil
	}
	fmt.Fprintf(os.Stdout, "Change %s rejected by %s via ChatOps (%s): %s\n",
		changeID, approver, chatopsOptPlatform, chatopsOptReason)
	return nil
}

// --- helpers --------------------------------------------------------------

// readChangeIDArg 从位置参数或 --id 读取变更 ID。两者都支持以保持 CLI
// 兼容性：`levee chatops approve --id run-1` 与 `levee chatops approve run-1`。
func readChangeIDArg(cmd *cobra.Command, args []string) (string, error) {
	if len(args) >= 1 {
		return args[0], nil
	}
	if chatopsOptChangeID != "" {
		return chatopsOptChangeID, nil
	}
	return "", fmt.Errorf("change id is required (positional arg or --id) [exit=2]")
}

// chatopsBotConfig 是从 JSON 配置文件加载的机器人配置。它包含三个平台
// 的字段，按 platform 取用对应的一组。
type chatopsBotConfig struct {
	Name        string `json:"name"`
	WebhookURL  string `json:"webhook_url"`
	Secret      string `json:"secret,omitempty"`
	BotToken    string `json:"bot_token,omitempty"`
	Timeout     string `json:"timeout,omitempty"`
	MaxRetries  int    `json:"max_retries,omitempty"`
	RetryDelay  string `json:"retry_delay,omitempty"`
	EventBuffer int    `json:"event_buffer,omitempty"`
}

// loadChatopsConfig 从路径加载 JSON 配置。空路径返回零值配置。
func loadChatopsConfig(path string) (chatopsBotConfig, error) {
	if path == "" {
		return chatopsBotConfig{}, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return chatopsBotConfig{}, fmt.Errorf("read chatops config: %w", err)
	}
	var cfg chatopsBotConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		return chatopsBotConfig{}, fmt.Errorf("parse chatops config: %w", err)
	}
	return cfg, nil
}

// parseDurationOrZero 解析时长字符串，空串返回 0。
func parseDurationOrZero(s string) (time.Duration, error) {
	if s == "" {
		return 0, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0, fmt.Errorf("parse duration %q: %w", s, err)
	}
	return d, nil
}

// buildBotFromConfig 根据平台与配置文件构造对应的 Bot 实例。
func buildBotFromConfig(platform chatops.Platform, configPath string) (chatops.Bot, error) {
	cfg, err := loadChatopsConfig(configPath)
	if err != nil {
		return nil, err
	}
	timeout, err := parseDurationOrZero(cfg.Timeout)
	if err != nil {
		return nil, err
	}
	retryDelay, err := parseDurationOrZero(cfg.RetryDelay)
	if err != nil {
		return nil, err
	}

	// 当配置中没有 name / webhook_url 时，回退到环境变量，便于在 CI / 容器
	// 中通过环境注入。
	if cfg.Name == "" {
		cfg.Name = fmt.Sprintf("%s-default", platform)
	}
	if cfg.WebhookURL == "" {
		cfg.WebhookURL = os.Getenv(strings.ToUpper(string(platform)) + "_WEBHOOK_URL")
	}

	switch platform {
	case chatops.PlatformFeishu:
		return chatops.NewFeishuBot(chatops.FeishuConfig{
			Name:        cfg.Name,
			WebhookURL:  cfg.WebhookURL,
			Secret:      cfg.Secret,
			Timeout:     timeout,
			MaxRetries:  cfg.MaxRetries,
			RetryDelay:  retryDelay,
			EventBuffer: cfg.EventBuffer,
		}, nil)
	case chatops.PlatformDingtalk:
		return chatops.NewDingtalkBot(chatops.DingtalkConfig{
			Name:        cfg.Name,
			WebhookURL:  cfg.WebhookURL,
			Secret:      cfg.Secret,
			Timeout:     timeout,
			MaxRetries:  cfg.MaxRetries,
			RetryDelay:  retryDelay,
			EventBuffer: cfg.EventBuffer,
		}, nil)
	case chatops.PlatformSlack:
		return chatops.NewSlackBot(chatops.SlackConfig{
			Name:        cfg.Name,
			WebhookURL:  cfg.WebhookURL,
			BotToken:    cfg.BotToken,
			Timeout:     timeout,
			MaxRetries:  cfg.MaxRetries,
			RetryDelay:  retryDelay,
			EventBuffer: cfg.EventBuffer,
		}, nil)
	default:
		return nil, fmt.Errorf("unsupported platform %q: %w", platform, chatops.ErrUnknownPlatform)
	}
}

// 兼容性：确保 state 包被使用（用于 openStore / findPendingApprovalID）。
var _ = state.RunFilter{}