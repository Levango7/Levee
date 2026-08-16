package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"github.com/nexus/levee/internal/config"
	"github.com/nexus/levee/internal/push"
	"github.com/spf13/cobra"
)

// Push command option variables. Prefixed with "push" to avoid collisions
// with other command packages in the same main package.
var (
	pushOptUser     string
	pushOptToken    string
	pushOptPlatform string
	pushOptTitle    string
	pushOptBody     string

	pushOptAPNsKeyFile    string
	pushOptAPNsTeamID     string
	pushOptAPNsKeyID      string
	pushOptAPNsBundleID   string
	pushOptAPNsProduction bool

	pushOptFCMKeyFile   string
	pushOptFCMProjectID string
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
	cmd.MarkFlagRequired("user")
	cmd.MarkFlagRequired("token")
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
	cmd.MarkFlagRequired("user")
	cmd.MarkFlagRequired("token")
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
	cmd.MarkFlagRequired("user")
	cmd.MarkFlagRequired("title")
	return cmd
}

// runPushSend executes `levee push send`.
func runPushSend(cmd *cobra.Command, args []string) error {
	pm, err := getPushManager()
	if err != nil {
		return err
	}
	ctx := context.Background()
	if err := pm.SendToUser(ctx, pushOptUser, pushOptTitle, pushOptBody, nil); err != nil {
		return fmt.Errorf("send push: %w", err)
	}
	out := map[string]any{
		"user":   pushOptUser,
		"title":  pushOptTitle,
		"body":   pushOptBody,
		"status": "sent",
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
	cmd.MarkFlagRequired("user")
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
	cmd.MarkFlagRequired("user")
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
	if !changed {
		return fmt.Errorf("no flags given; supply APNs and/or FCM configuration [exit=2]")
	}

	if err := savePushConfig(cfg); err != nil {
		return fmt.Errorf("save push config: %w", err)
	}
	out := map[string]any{"status": "saved", "apns": cfg.APNs != (pushConfigAPNs{}), "fcm": cfg.FCM != (pushConfigFCM{})}
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

// pushConfigPath returns the path to the push config JSON file. It lives in
// the LEVEE data directory: <dataDir>/push.json.
func pushConfigPath() (string, error) {
	cfg, err := config.Load(optConfigPath)
	if err != nil {
		return "", err
	}
	return filepath.Join(cfg.Server.DataDir, "push.json"), nil
}

// loadPushConfig reads the push config from disk. A missing file yields a
// zero-value config (no error).
func loadPushConfig() (*pushConfigFile, error) {
	path, err := pushConfigPath()
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return &pushConfigFile{}, nil
		}
		return nil, err
	}
	var cfg pushConfigFile
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, err
	}
	return &cfg, nil
}

// savePushConfig writes the push config to disk as JSON.
func savePushConfig(cfg *pushConfigFile) error {
	path, err := pushConfigPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
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
		cfg, err := loadPushConfig()
		if err != nil {
			pushManagerInitErr = err
			return
		}
		var apnsClient *push.APNSClient
		if cfg.APNs.PrivateKey != "" {
			apnsClient, err = push.NewAPNSClient(
				cfg.APNs.TeamID, cfg.APNs.KeyID, cfg.APNs.BundleID,
				[]byte(cfg.APNs.PrivateKey), cfg.APNs.Production,
			)
			if err != nil {
				pushManagerInitErr = fmt.Errorf("init apns: %w", err)
				return
			}
		}
		var fcmClient *push.FCMClient
		if cfg.FCM.ServiceAccountKey != "" {
			fcmClient = push.NewFCMClient(cfg.FCM.ProjectID, []byte(cfg.FCM.ServiceAccountKey))
		}
		pushManagerSingleton = push.NewPushManager(apnsClient, fcmClient)
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
