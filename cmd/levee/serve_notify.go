package main

// serve_notify.go — 装配出站通知传输层（notify.webhook.*），供执行引擎投递
// 回滚分级结果使用。
//
// 这个文件为什么存在：渠道实现、配置键、启动校验三样都齐了，唯独没人构造它，
// 于是 notify.webhook.enabled=true 是一个背后没接线的开关。引擎侧的分级路径在
// 没有 manager 时刻意降级为"只记日志"（见 internal/wiring/rollback_grade.go），
// 那是诚实的行为——不诚实的是配置。

import (
	"fmt"
	"os"

	"github.com/nexus/levee/internal/config"
	"github.com/nexus/levee/internal/log"
	"github.com/nexus/levee/internal/notify"
)

const (
	// webhookChannelName 是通用 webhook 渠道在管理器里的注册名。下游按名字过滤，
	// 所以它是固定字符串，不从 URL 推导。
	webhookChannelName = "webhook"

	// envWebhookSigningKey 提供 HMAC-SHA256 签名密钥。和 Jira token 一样走环境变量
	// 而不是配置文件：配置文件会被贴进工单和评审。
	envWebhookSigningKey = "LEVEE_WEBHOOK_SECRET"
)

// buildServeNotifyManager 按配置构造通知管理器；一个渠道都没启用时返回
// (nil, nil)。
//
// 启用了却不可用的渠道会拒绝启动，而不是被跳过——操作者配置写着 enabled，
// 服务端却静默不投递，比一个当场说明为何投递不了的启动更坏。notify.chatops.*
// 与 notify.jira.* 用的是同一条规则。
func buildServeNotifyManager(cfg config.NotifyConfig) (*notify.NotificationManager, error) {
	if !cfg.Webhook.Enabled {
		return nil, nil
	}
	secret := os.Getenv(envWebhookSigningKey)
	if secret == "" {
		log.Warn("notify.webhook is enabled without " + envWebhookSigningKey +
			"; payloads will be delivered unsigned, so the receiver cannot verify they came from LEVEE")
	}

	n, err := notify.NewWebhookNotifier(notify.WebhookConfig{
		Name:   webhookChannelName,
		URL:    cfg.Webhook.URL,
		Secret: secret,
		// Retry==0 沿用 notify 包文档化的默认（3 次重试）；config.example.yaml
		// 写的默认值也是 3，两边口径一致。
		MaxRetries: cfg.Webhook.Retry,
		Timeout:    cfg.Webhook.Timeout,
	})
	if err != nil {
		return nil, fmt.Errorf("serve: notify.webhook: %w", err)
	}

	mgr := notify.NewNotificationManager()
	if err := mgr.Register(n); err != nil {
		return nil, fmt.Errorf("serve: notify.webhook register: %w", err)
	}
	log.Info("serve: webhook notification channel installed",
		"name", webhookChannelName,
		"signed", secret != "",
		"retry", cfg.Webhook.Retry)
	return mgr, nil
}

// warnNotifyTransportWithoutEngine says the quiet part out loud: rollback
// grading is today's only producer of notifications, and it lives inside the
// engine. An enabled webhook with the engine switched off would sit configured
// and deliver nothing forever, so the boot states that instead of letting the
// switch read as "we will be notified".
func warnNotifyTransportWithoutEngine(cfg *config.Config) {
	if cfg != nil && cfg.Notify.Webhook.Enabled {
		log.Warn("serve: notify.webhook is enabled but the execution engine is off (--engine-enabled=false); no notification will ever be sent")
	}
}
