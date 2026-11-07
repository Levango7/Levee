package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"github.com/spf13/cobra"

	"github.com/nexus/levee/internal/approval"
	"github.com/nexus/levee/internal/config"
	"github.com/nexus/levee/internal/log"
	"github.com/nexus/levee/internal/push"
)

// Push command option variables. Prefixed with "push" to avoid collisions
// with other command packages in the same main package.
var (
	pushOptUser     string
	pushOptToken    string
	pushOptPlatform string
	pushOptTitle    string
	pushOptBody     string
	pushOptDeepLink string

	pushOptAPNsKeyFile    string
	pushOptAPNsTeamID     string
	pushOptAPNsKeyID      string
	pushOptAPNsBundleID   string
	pushOptAPNsProduction bool

	pushOptFCMKeyFile   string
	pushOptFCMProjectID string

	pushOptBaseURL string
)

// pushManagerSingleton is the process-wide push manager used by the push
// subcommands. It is initialised lazily by getPushManager from the on-disk
// configuration. The mutex protects the lazy init.
var (
	pushManagerSingleton *push.PushManager
	pushManagerOnce      sync.Once
	pushManagerInitErr   error
)

func init() {
	RegisterCommand(newPushCmd())
}

// newPushCmd builds the `levee push` parent command and its subcommands.
func newPushCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "push",
		Short: "Manage mobile push notifications and device registration",
		Long: "Manage APNs / FCM push notifications for mobile approval.\n" +
			"Subcommands allow registering devices, sending notifications,\n" +
			"listing registered devices and configuring APNs / FCM credentials.",
		Args: cobra.NoArgs,
	}
	cmd.AddCommand(newPushRegisterCmd())
	cmd.AddCommand(newPushUnregisterCmd())
	cmd.AddCommand(newPushSendCmd())
	cmd.AddCommand(newPushDevicesCmd())
	cmd.AddCommand(newPushTestCmd())
	cmd.AddCommand(newPushConfigCmd())
	return cmd
}

// --- register --------------------------------------------------------------

// newPushRegisterCmd builds `levee push register`.
func newPushRegisterCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "register",
		Short: "Register a mobile device for push notifications",
		Long: "Associate a device token with a user so that subsequent " +
			"`levee push send` calls deliver to that device.",
		Args: cobra.NoArgs,
		RunE: runPushRegister,
	}
	cmd.Flags().StringVar(&pushOptUser, "user", "", "User ID (required)")
	cmd.Flags().StringVar(&pushOptToken, "token", "", "Device token (required)")
	cmd.Flags().StringVar(&pushOptPlatform, "platform", "ios", "Platform: ios | android")
	_ = cmd.MarkFlagRequired("user")
	_ = cmd.MarkFlagRequired("token")
	return cmd
}

// runPushRegister executes `levee push register`.
func runPushRegister(cmd *cobra.Command, args []string) error {
	pm, err := getPushManager()
	if err != nil {
		return err
	}
	if err := pm.RegisterDevice(pushOptUser, pushOptToken, pushOptPlatform); err != nil {
		return fmt.Errorf("register device: %w", err)
	}
	out := map[string]any{
		"user":     pushOptUser,
		"platform": pushOptPlatform,
		"status":   "registered",
	}
	if optJSON {
		return PrintJSON(os.Stdout, map[string]any{"data": out, "meta": nil, "error": nil})
	}
	if optQuiet {
		fmt.Fprintln(os.Stdout, pushOptToken)
		return nil
	}
	fmt.Fprintf(os.Stdout, "Device registered for user %s on %s\n", pushOptUser, pushOptPlatform)
	return nil
}

// --- unregister ------------------------------------------------------------

// newPushUnregisterCmd builds `levee push unregister`.
func newPushUnregisterCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "unregister",
		Short: "Unregister a mobile device",
		Args:  cobra.NoArgs,
		RunE:  runPushUnregister,
	}
	cmd.Flags().StringVar(&pushOptUser, "user", "", "User ID (required)")
	cmd.Flags().StringVar(&pushOptToken, "token", "", "Device token (required)")
	_ = cmd.MarkFlagRequired("user")
	_ = cmd.MarkFlagRequired("token")
	return cmd
}

// runPushUnregister executes `levee push unregister`.
func runPushUnregister(cmd *cobra.Command, args []string) error {
	pm, err := getPushManager()
	if err != nil {
		return err
	}
	if err := pm.UnregisterDevice(pushOptUser, pushOptToken); err != nil {
		return fmt.Errorf("unregister device: %w", err)
	}
	out := map[string]any{"user": pushOptUser, "status": "unregistered"}
	if optJSON {
		return PrintJSON(os.Stdout, map[string]any{"data": out, "meta": nil, "error": nil})
	}
	if optQuiet {
		fmt.Fprintln(os.Stdout, pushOptToken)
		return nil
	}
	fmt.Fprintf(os.Stdout, "Device unregistered for user %s\n", pushOptUser)
	return nil
}

// --- send ------------------------------------------------------------------

// newPushSendCmd builds `levee push send`.
func newPushSendCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "send",
		Short: "Send a push notification to a user's devices",
		Args:  cobra.NoArgs,
		RunE:  runPushSend,
	}
	cmd.Flags().StringVar(&pushOptUser, "user", "", "User ID (required)")
	cmd.Flags().StringVar(&pushOptTitle, "title", "", "Notification title (required)")
	cmd.Flags().StringVar(&pushOptBody, "body", "", "Notification body")
	cmd.Flags().StringVar(&pushOptDeepLink, "deep-link", "",
		"URL the mobile client opens on tap, e.g. levee://approval/run-123")
	_ = cmd.MarkFlagRequired("user")
	_ = cmd.MarkFlagRequired("title")
	return cmd
}

// runPushSend executes `levee push send`.
func runPushSend(cmd *cobra.Command, args []string) error {
	pm, err := getPushManager()
	if err != nil {
		return err
	}
	ctx := context.Background()
	if err := pm.SendToUser(ctx, pushOptUser, pushOptTitle, pushOptBody, pushSendData(pushOptDeepLink)); err != nil {
		return fmt.Errorf("send push: %w", err)
	}
	out := map[string]any{
		"user":   pushOptUser,
		"title":  pushOptTitle,
		"body":   pushOptBody,
		"status": "sent",
	}
	if pushOptDeepLink != "" {
		out["deep_link"] = pushOptDeepLink
	}
	if optJSON {
		return PrintJSON(os.Stdout, map[string]any{"data": out, "meta": nil, "error": nil})
	}
	if optQuiet {
		fmt.Fprintln(os.Stdout, "sent")
		return nil
	}
	fmt.Fprintf(os.Stdout, "Push sent to user %s: %s\n", pushOptUser, pushOptTitle)
	return nil
}

// pushSendData builds the notification payload for `levee push send`. The key
// is "deeplink" because that is what the approval service already puts on the
// wire (internal/approval/mobile.go): a hand-sent deep link must be
// indistinguishable from a system-sent one to the mobile client, and a second
// spelling of the same field is exactly how a client-side contract gets lost.
// An empty link yields a nil payload, preserving the pre-flag wire shape.
func pushSendData(deepLink string) map[string]string {
	if deepLink == "" {
		return nil
	}
	return map[string]string{"deeplink": deepLink}
}

// --- devices ---------------------------------------------------------------

// newPushDevicesCmd builds `levee push devices`.
func newPushDevicesCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "devices",
		Short: "List devices registered for a user",
		Args:  cobra.NoArgs,
		RunE:  runPushDevices,
	}
	cmd.Flags().StringVar(&pushOptUser, "user", "", "User ID (required)")
	_ = cmd.MarkFlagRequired("user")
	return cmd
}

// runPushDevices executes `levee push devices`.
func runPushDevices(cmd *cobra.Command, args []string) error {
	pm, err := getPushManager()
	if err != nil {
		return err
	}
	devices := pm.ListDevices(pushOptUser)
	if len(devices) == 0 {
		if optJSON {
			return PrintJSON(os.Stdout, map[string]any{"data": []any{}, "meta": nil, "error": nil})
		}
		fmt.Fprintf(os.Stdout, "No devices registered for user %s\n", pushOptUser)
		return nil
	}
	if optJSON {
		items := make([]map[string]any, 0, len(devices))
		for _, d := range devices {
			items = append(items, map[string]any{
				"token":         d.Token,
				"platform":      d.Platform,
				"registered_at": d.RegisteredAt,
			})
		}
		return PrintJSON(os.Stdout, map[string]any{"data": items, "meta": nil, "error": nil})
	}
	if optQuiet {
		for _, d := range devices {
			fmt.Fprintln(os.Stdout, d.Token)
		}
		return nil
	}
	fmt.Fprintf(os.Stdout, "Devices for user %s:\n", pushOptUser)
	for _, d := range devices {
		fmt.Fprintf(os.Stdout, "  %s  %s  registered=%s\n", d.Platform, d.Token, d.RegisteredAt.Format("2006-01-02 15:04:05"))
	}
	return nil
}

// --- test ------------------------------------------------------------------

// newPushTestCmd builds `levee push test`.
func newPushTestCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "test",
		Short: "Send a test push notification to a user",
		Args:  cobra.NoArgs,
		RunE:  runPushTest,
	}
	cmd.Flags().StringVar(&pushOptUser, "user", "", "User ID (required)")
	_ = cmd.MarkFlagRequired("user")
	return cmd
}

// runPushTest executes `levee push test`.
func runPushTest(cmd *cobra.Command, args []string) error {
	pm, err := getPushManager()
	if err != nil {
		return err
	}
	ctx := context.Background()
	title := "LEVEE 测试推送"
	body := "这是一条来自 levee push test 的测试消息"
	if err := pm.SendToUser(ctx, pushOptUser, title, body, map[string]string{"test": "true"}); err != nil {
		return fmt.Errorf("send test push: %w", err)
	}
	out := map[string]any{"user": pushOptUser, "status": "sent", "test": true}
	if optJSON {
		return PrintJSON(os.Stdout, map[string]any{"data": out, "meta": nil, "error": nil})
	}
	fmt.Fprintf(os.Stdout, "Test push sent to user %s\n", pushOptUser)
	return nil
}

// --- config ----------------------------------------------------------------

// newPushConfigCmd builds `levee push config`.
func newPushConfigCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "config",
		Short: "Configure APNs / FCM credentials",
		Long: "Persist APNs / FCM credentials to the LEVEE data directory. " +
			"Subsequent `levee push` commands load the saved configuration automatically.",
		Args: cobra.NoArgs,
		RunE: runPushConfig,
	}
	cmd.Flags().StringVar(&pushOptAPNsKeyFile, "apns-key-file", "", "Path to APNs .p8 key file")
	cmd.Flags().StringVar(&pushOptAPNsTeamID, "apns-team-id", "", "Apple Developer team ID")
	cmd.Flags().StringVar(&pushOptAPNsKeyID, "apns-key-id", "", "APNs key ID")
	cmd.Flags().StringVar(&pushOptAPNsBundleID, "apns-bundle-id", "", "iOS app bundle ID")
	cmd.Flags().BoolVar(&pushOptAPNsProduction, "apns-production", false, "Use APNs production endpoint")

	cmd.Flags().StringVar(&pushOptFCMKeyFile, "fcm-key-file", "", "Path to FCM service-account JSON")
	cmd.Flags().StringVar(&pushOptFCMProjectID, "fcm-project-id", "", "FCM / GCP project ID")
	cmd.Flags().StringVar(&pushOptBaseURL, "base-url", "",
		"Public origin of this deployment, used for web deep links (e.g. https://levee.example.com)")
	return cmd
}

// runPushConfig executes `levee push config`.
func runPushConfig(cmd *cobra.Command, args []string) error {
	cfg, err := loadPushConfig()
	if err != nil {
		return fmt.Errorf("load push config: %w", err)
	}

	changed := false
	if pushOptAPNsKeyFile != "" || pushOptAPNsTeamID != "" || pushOptAPNsKeyID != "" || pushOptAPNsBundleID != "" {
		keyBytes, err := os.ReadFile(pushOptAPNsKeyFile)
		if err != nil {
			return fmt.Errorf("read apns key file: %w", err)
		}
		cfg.APNs = pushConfigAPNs{
			PrivateKey: string(keyBytes),
			TeamID:     pushOptAPNsTeamID,
			KeyID:      pushOptAPNsKeyID,
			BundleID:   pushOptAPNsBundleID,
			Production: pushOptAPNsProduction,
		}
		changed = true
	}
	if pushOptFCMKeyFile != "" || pushOptFCMProjectID != "" {
		keyBytes, err := os.ReadFile(pushOptFCMKeyFile)
		if err != nil {
			return fmt.Errorf("read fcm key file: %w", err)
		}
		cfg.FCM = pushConfigFCM{
			ServiceAccountKey: string(keyBytes),
			ProjectID:         pushOptFCMProjectID,
		}
		changed = true
	}
	if pushOptBaseURL != "" {
		cfg.BaseURL = pushOptBaseURL
		changed = true
	}
	if !changed {
		return fmt.Errorf("no flags given; supply APNs and/or FCM configuration [exit=2]")
	}

	if err := savePushConfig(cfg); err != nil {
		return fmt.Errorf("save push config: %w", err)
	}
	out := map[string]any{"status": "saved", "apns": cfg.APNs != (pushConfigAPNs{}), "fcm": cfg.FCM != (pushConfigFCM{}), "base_url": cfg.BaseURL}
	if optJSON {
		return PrintJSON(os.Stdout, map[string]any{"data": out, "meta": nil, "error": nil})
	}
	fmt.Fprintln(os.Stdout, "Push configuration saved")
	return nil
}

// --- push config persistence -----------------------------------------------

// pushConfigFile is the on-disk JSON representation of the push configuration.
type pushConfigFile struct {
	APNs pushConfigAPNs `json:"apns,omitempty"`
	FCM  pushConfigFCM  `json:"fcm,omitempty"`
	// BaseURL is this deployment's public origin (e.g.
	// https://levee.example.com). It is the origin the web ("show") deep
	// links point at — the approve/reject links use the "levee://" scheme
	// and do not need it.
	//
	// It lives in the push configuration rather than the main server config
	// because that is where the rest of the mobile story already lives, and
	// because the CLI that writes this file and the daemon that reads it
	// must not disagree about it.
	BaseURL string `json:"base_url,omitempty"`
}

// pushConfigAPNs holds APNs credentials.
type pushConfigAPNs struct {
	PrivateKey string `json:"private_key"`
	TeamID     string `json:"team_id"`
	KeyID      string `json:"key_id"`
	BundleID   string `json:"bundle_id"`
	Production bool   `json:"production"`
}

// pushConfigFCM holds FCM credentials.
type pushConfigFCM struct {
	ServiceAccountKey string `json:"service_account_key"`
	ProjectID         string `json:"project_id"`
}

// defaultDeepLinkBaseURL is the origin web ("show") deep links point at when
// the deployment has not configured one. It is a placeholder, not a working
// address: a deployment that serves web deep links must set base_url.
const defaultDeepLinkBaseURL = "https://levee.local"

// pushDataDir returns the LEVEE data directory the push files live in.
func pushDataDir() (string, error) {
	cfg, err := config.Load(optConfigPath)
	if err != nil {
		return "", err
	}
	applySecurityConfig(cfg)
	return cfg.Server.DataDir, nil
}

// pushConfigPath returns the path to the push config JSON file. It lives in
// the LEVEE data directory: <dataDir>/push.json.
func pushConfigPath() (string, error) {
	dir, err := pushDataDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "push.json"), nil
}

// pushRuntime is what a deployment needs in order to actually push: the
// manager, the public base URL for web deep links, and whether any push
// channel is configured at all.
type pushRuntime struct {
	manager *push.PushManager
	baseURL string
	// configured reports whether at least one channel (APNs / FCM) has
	// credentials.
	//
	// The CLI enrolls devices regardless of this: a phone can be registered
	// before the deployment has push credentials. The daemon is the opposite
	// — it only pushes when a channel exists, because a manager without a
	// channel reports a successful send while delivering nothing.
	configured bool
}

// newPushRuntimeFromDataDir builds the push runtime from the push
// configuration and device registry in dataDir.
//
// The CLI and the daemon both go through here, so the credentials and the
// device registry they use cannot disagree.
func newPushRuntimeFromDataDir(dataDir string) (pushRuntime, error) {
	cfg, err := loadPushConfigFromDataDir(dataDir)
	if err != nil {
		return pushRuntime{}, err
	}
	var apnsClient *push.APNSClient
	if cfg.APNs.PrivateKey != "" {
		apnsClient, err = push.NewAPNSClient(
			cfg.APNs.TeamID, cfg.APNs.KeyID, cfg.APNs.BundleID,
			[]byte(cfg.APNs.PrivateKey), cfg.APNs.Production,
		)
		if err != nil {
			return pushRuntime{}, fmt.Errorf("init apns: %w", err)
		}
	}
	var fcmClient *push.FCMClient
	if cfg.FCM.ServiceAccountKey != "" {
		fcmClient = push.NewFCMClient(cfg.FCM.ProjectID, []byte(cfg.FCM.ServiceAccountKey))
	}
	pm, err := push.NewPushManagerWithStore(
		apnsClient, fcmClient, push.NewFileDeviceStore(filepath.Join(dataDir, "push_devices.json")))
	if err != nil {
		return pushRuntime{}, fmt.Errorf("load device registry: %w", err)
	}
	return pushRuntime{
		manager:    pm,
		baseURL:    cfg.BaseURL,
		configured: apnsClient != nil || fcmClient != nil,
	}, nil
}

// loadPushConfigFromDataDir reads the push configuration from
// <dataDir>/push.json. A missing file yields an empty configuration (no
// error): a deployment without push configured is a normal state, not a
// failure.
func loadPushConfigFromDataDir(dataDir string) (*pushConfigFile, error) {
	path := filepath.Join(dataDir, "push.json")
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return &pushConfigFile{}, nil
		}
		return nil, fmt.Errorf("read push config %s: %w", path, err)
	}
	var cfg pushConfigFile
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parse push config %s: %w", path, err)
	}
	return &cfg, nil
}

// loadPushConfig reads the push config from disk. A missing file yields a
// zero-value config (no error).
func loadPushConfig() (*pushConfigFile, error) {
	dir, err := pushDataDir()
	if err != nil {
		return nil, err
	}
	return loadPushConfigFromDataDir(dir)
}

// savePushConfig writes the push config to disk as JSON.
func savePushConfig(cfg *pushConfigFile) error {
	path, err := pushConfigPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return err
	}
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o600)
}

// getPushManager lazily builds the process-wide PushManager from the on-disk
// push configuration. When no configuration exists, a manager with nil APNs
// and FCM clients is returned, allowing device registration to proceed even
// before push credentials are configured.
func getPushManager() (*push.PushManager, error) {
	pushManagerOnce.Do(func() {
		dir, err := pushDataDir()
		if err != nil {
			pushManagerInitErr = err
			return
		}
		rt, err := newPushRuntimeFromDataDir(dir)
		if err != nil {
			pushManagerInitErr = err
			return
		}
		pushManagerSingleton = rt.manager
	})
	if pushManagerInitErr != nil {
		return nil, pushManagerInitErr
	}
	return pushManagerSingleton, nil
}

// resetPushManagerForTest clears the singleton so tests can re-initialise it
// with a fresh configuration. Intended only for unit tests.
func resetPushManagerForTest() {
	pushManagerSingleton = nil
	pushManagerOnce = sync.Once{}
	pushManagerInitErr = nil
}

// installServeMobileApproval wires the mobile approval service for the
// daemon: the deep-link approve/reject endpoints plus, when the deployment
// has a push channel, the push manager that actually delivers the approval
// notification.
//
// The returned push manager is nil when no APNs / FCM credentials are
// configured — and also when the configuration is unusable. That is
// deliberate: the mobile service treats a nil manager as "push disabled" and
// says so, whereas a manager without a channel would report a successful
// send while delivering nothing.
//
// The deployment's credentials live in <dataDir>/push.json and its device
// registry in <dataDir>/push_devices.json — the same files the `levee push`
// CLI writes, so the daemon and the CLI cannot disagree about who is
// enrolled or which channel to use.
func installServeMobileApproval(approvalSvc *approval.Service, dataDir string) (*approval.MobileApprovalService, *push.PushManager) {
	rt, err := newPushRuntimeFromDataDir(dataDir)
	if err != nil {
		// A broken push configuration must not take the control plane
		// down: push delivery is best-effort (the approval is recorded
		// either way), so a daemon that refused to start because of an
		// unparseable notification key would turn a notification problem
		// into an outage. Disable push and say why, loudly.
		log.Error("serve: push disabled; push configuration is unusable",
			"data_dir", dataDir, "err", err)
		rt = pushRuntime{}
	}
	var manager *push.PushManager
	if rt.configured {
		manager = rt.manager
	} else {
		log.Info("serve: push disabled; no APNs / FCM credentials configured",
			"data_dir", dataDir)
	}
	// The web ("show") deep links point at the deployment's public origin.
	// Approve/reject links use the "levee://" scheme and do not need it.
	base := rt.baseURL
	if base == "" {
		base = defaultDeepLinkBaseURL
	}
	service := approval.NewMobileApprovalService(
		approvalSvc, manager, push.NewDeepLinkGenerator("levee", base))
	return service, manager
}
