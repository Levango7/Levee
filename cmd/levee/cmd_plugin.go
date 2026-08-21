package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/nexus/levee/internal/config"
	"github.com/nexus/levee/internal/plugin"
)

// Plugin command option variables. Prefixed to avoid collisions with other
// command packages in the same main package.
var (
	pluginOptVerifySig   bool
	pluginOptNoVerifySig bool
)

func init() {
	RegisterCommand(newPluginCmd())
}

// newPluginCmd builds the `levee plugin` sub-command with its children.
func newPluginCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "plugin",
		Short: "Manage LEVEE plugins",
		Long: "Manage the LEVEE plugin system: list, install, enable, " +
			"disable, remove and inspect plugins. Plugins extend the " +
			"engine with custom channels, gates, modules and notifiers.",
	}

	cmd.AddCommand(newPluginListCmd())
	cmd.AddCommand(newPluginInstallCmd())
	cmd.AddCommand(newPluginEnableCmd())
	cmd.AddCommand(newPluginDisableCmd())
	cmd.AddCommand(newPluginRemoveCmd())
	cmd.AddCommand(newPluginInfoCmd())

	return cmd
}

// newPluginListCmd builds the `levee plugin list` sub-command.
func newPluginListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List installed plugins",
		Long:  "List all plugins registered in the plugin registry, sorted by name.",
		Args:  cobra.NoArgs,
		RunE:  runPluginList,
	}
}

// newPluginInstallCmd builds the `levee plugin install <path>` sub-command.
func newPluginInstallCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "install <path>",
		Short: "Install a plugin from a directory or binary path",
		Long: "Install a plugin by reading its manifest (plugin.yaml) " +
			"from the given path. The path may be a directory " +
			"containing the manifest and binary, or the binary file " +
			"itself (in which case the manifest is read from the " +
			"same directory).",
		Args: cobra.ExactArgs(1),
		RunE: runPluginInstall,
	}
	cmd.Flags().BoolVar(&pluginOptVerifySig, "verify-signature", false,
		"Verify the plugin binary's SHA-256 signature at install and enable time")
	return cmd
}

// newPluginEnableCmd builds the `levee plugin enable <name>` sub-command.
func newPluginEnableCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "enable <name>",
		Short: "Enable a plugin",
		Long:  "Start the plugin's sub-process and mark it as enabled.",
		Args:  cobra.ExactArgs(1),
		RunE:  runPluginEnable,
	}
}

// newPluginDisableCmd builds the `levee plugin disable <name>` sub-command.
func newPluginDisableCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "disable <name>",
		Short: "Disable a plugin",
		Long:  "Stop the plugin's sub-process and mark it as disabled.",
		Args:  cobra.ExactArgs(1),
		RunE:  runPluginDisable,
	}
}

// newPluginRemoveCmd builds the `levee plugin remove <name>` sub-command.
func newPluginRemoveCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "remove <name>",
		Short: "Remove a plugin",
		Long:  "Remove a plugin from the registry. The plugin is disabled first if enabled.",
		Args:  cobra.ExactArgs(1),
		RunE:  runPluginRemove,
	}
}

// newPluginInfoCmd builds the `levee plugin info <name>` sub-command.
func newPluginInfoCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "info <name>",
		Short: "Show plugin details",
		Long:  "Display the full registry record for a plugin.",
		Args:  cobra.ExactArgs(1),
		RunE:  runPluginInfo,
	}
}

// --- Command runners ---------------------------------------------------------

// openPluginManager creates a PluginManager from the configuration. The
// caller is responsible for closing the manager and registry when done.
func openPluginManager(ctx context.Context) (*plugin.PluginManager, func(), error) {
	cfg, err := config.Load(optConfigPath)
	if err != nil {
		return nil, nil, fmt.Errorf("load config: %w", err)
	}

	registryPath := pluginRegistryPath(cfg)
	registry, err := plugin.NewRegistry(ctx, registryPath)
	if err != nil {
		return nil, nil, fmt.Errorf("open plugin registry: %w", err)
	}

	mgrCfg := plugin.DefaultManagerConfig()
	mgrCfg.PluginsDir = filepath.Join(cfg.Server.DataDir, "plugins")
	mgrCfg.HostVersion = version
	mgrCfg.VerifySignatures = pluginOptVerifySig

	mgr := plugin.NewPluginManager(registry, mgrCfg)
	cleanup := func() {
		_ = mgr.Close(ctx)
		_ = registry.Close()
	}
	return mgr, cleanup, nil
}

// pluginRegistryPath returns the path to the plugin registry SQLite file.
// It lives in the data directory next to the main state database.
func pluginRegistryPath(cfg *config.Config) string {
	return filepath.Join(cfg.Server.DataDir, "plugin_registry.db")
}

// runPluginList executes the `levee plugin list` command.
func runPluginList(cmd *cobra.Command, args []string) error {
	ctx := context.Background()
	mgr, cleanup, err := openPluginManager(ctx)
	if err != nil {
		return err
	}
	defer cleanup()

	records, err := mgr.ListRecords(ctx)
	if err != nil {
		return fmt.Errorf("list plugins: %w", err)
	}

	rows := make([]map[string]any, 0, len(records))
	for _, rec := range records {
		rows = append(rows, pluginRecordToMap(rec))
	}

	if optJSON {
		return PrintJSON(os.Stdout, map[string]any{
			"data":  rows,
			"meta":  map[string]any{"count": len(rows)},
			"error": nil,
		})
	}

	if optQuiet {
		for _, rec := range records {
			fmt.Fprintln(os.Stdout, rec.Name)
		}
		return nil
	}

	printPluginListHuman(os.Stdout, rows)
	return nil
}

// runPluginInstall executes the `levee plugin install <path>` command.
func runPluginInstall(cmd *cobra.Command, args []string) error {
	ctx := context.Background()
	mgr, cleanup, err := openPluginManager(ctx)
	if err != nil {
		return err
	}
	defer cleanup()

	rec, err := mgr.Install(ctx, args[0])
	if err != nil {
		return fmt.Errorf("install plugin: %w", err)
	}

	output := pluginRecordToMap(rec)

	if optJSON {
		return PrintJSON(os.Stdout, map[string]any{
			"data":  output,
			"meta":  nil,
			"error": nil,
		})
	}

	if optQuiet {
		fmt.Fprintln(os.Stdout, rec.Name)
		return nil
	}

	printPluginInstallHuman(os.Stdout, output)
	return nil
}

// runPluginEnable executes the `levee plugin enable <name>` command.
func runPluginEnable(cmd *cobra.Command, args []string) error {
	ctx := context.Background()
	mgr, cleanup, err := openPluginManager(ctx)
	if err != nil {
		return err
	}
	defer cleanup()

	if err := mgr.Enable(ctx, args[0]); err != nil {
		return fmt.Errorf("enable plugin: %w", err)
	}

	output := map[string]any{
		"name":   args[0],
		"status": "enabled",
	}

	if optJSON {
		return PrintJSON(os.Stdout, map[string]any{
			"data":  output,
			"meta":  nil,
			"error": nil,
		})
	}

	if optQuiet {
		fmt.Fprintln(os.Stdout, args[0])
		return nil
	}

	fmt.Fprintf(os.Stdout, "Plugin %q enabled.\n", args[0])
	return nil
}

// runPluginDisable executes the `levee plugin disable <name>` command.
func runPluginDisable(cmd *cobra.Command, args []string) error {
	ctx := context.Background()
	mgr, cleanup, err := openPluginManager(ctx)
	if err != nil {
		return err
	}
	defer cleanup()

	if err := mgr.Disable(ctx, args[0]); err != nil {
		return fmt.Errorf("disable plugin: %w", err)
	}

	output := map[string]any{
		"name":   args[0],
		"status": "disabled",
	}

	if optJSON {
		return PrintJSON(os.Stdout, map[string]any{
			"data":  output,
			"meta":  nil,
			"error": nil,
		})
	}

	if optQuiet {
		fmt.Fprintln(os.Stdout, args[0])
		return nil
	}

	fmt.Fprintf(os.Stdout, "Plugin %q disabled.\n", args[0])
	return nil
}

// runPluginRemove executes the `levee plugin remove <name>` command.
func runPluginRemove(cmd *cobra.Command, args []string) error {
	ctx := context.Background()
	mgr, cleanup, err := openPluginManager(ctx)
	if err != nil {
		return err
	}
	defer cleanup()

	if err := mgr.Remove(ctx, args[0]); err != nil {
		return fmt.Errorf("remove plugin: %w", err)
	}

	output := map[string]any{
		"name":    args[0],
		"removed": true,
	}

	if optJSON {
		return PrintJSON(os.Stdout, map[string]any{
			"data":  output,
			"meta":  nil,
			"error": nil,
		})
	}

	if optQuiet {
		fmt.Fprintln(os.Stdout, args[0])
		return nil
	}

	fmt.Fprintf(os.Stdout, "Plugin %q removed.\n", args[0])
	return nil
}

// runPluginInfo executes the `levee plugin info <name>` command.
func runPluginInfo(cmd *cobra.Command, args []string) error {
	ctx := context.Background()
	mgr, cleanup, err := openPluginManager(ctx)
	if err != nil {
		return err
	}
	defer cleanup()

	rec, err := mgr.Info(ctx, args[0])
	if err != nil {
		return fmt.Errorf("plugin info: %w", err)
	}

	output := pluginRecordToMap(rec)

	if optJSON {
		return PrintJSON(os.Stdout, map[string]any{
			"data":  output,
			"meta":  nil,
			"error": nil,
		})
	}

	if optQuiet {
		fmt.Fprintln(os.Stdout, rec.Name)
		return nil
	}

	printPluginInfoHuman(os.Stdout, output)
	return nil
}

// --- Output helpers ---------------------------------------------------------

// pluginRecordToMap converts a RegistryRecord to a map for JSON output.
func pluginRecordToMap(rec *plugin.RegistryRecord) map[string]any {
	m := map[string]any{
		"name":         rec.Name,
		"version":      rec.Version,
		"type":         string(rec.Type),
		"author":       rec.Author,
		"description":  rec.Description,
		"entry_point":  rec.EntryPoint,
		"state":        string(rec.State),
		"binary_path":  rec.BinaryPath,
		"installed_at": rec.InstalledAt,
		"updated_at":   rec.UpdatedAt,
	}
	if rec.MinHostVersion != "" {
		m["min_host_version"] = rec.MinHostVersion
	}
	if rec.MaxHostVersion != "" {
		m["max_host_version"] = rec.MaxHostVersion
	}
	if rec.ConfigYAML != "" {
		m["config_yaml"] = rec.ConfigYAML
	}
	if rec.Signature != "" {
		m["signature"] = rec.Signature
	}
	if rec.ErrorMsg != "" {
		m["error_msg"] = rec.ErrorMsg
	}
	return m
}

// printPluginListHuman renders the plugin list in a human-readable table.
func printPluginListHuman(w io.Writer, rows []map[string]any) {
	if len(rows) == 0 {
		fmt.Fprintln(w, "No plugins installed.")
		return
	}
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintf(tw, "%-20s\t%-10s\t%-10s\t%-10s\t%s\n", "NAME", "VERSION", "TYPE", "STATE", "DESCRIPTION")
	for _, row := range rows {
		name, _ := row["name"].(string)
		ver, _ := row["version"].(string)
		typ, _ := row["type"].(string)
		state, _ := row["state"].(string)
		desc, _ := row["description"].(string)
		fmt.Fprintf(tw, "%-20s\t%-10s\t%-10s\t%-10s\t%s\n", name, ver, typ, state, desc)
	}
	_ = tw.Flush()
}

// printPluginInstallHuman renders the install confirmation.
func printPluginInstallHuman(w io.Writer, output map[string]any) {
	fmt.Fprintf(w, "Plugin installed: %v (version %v, type %v)\n",
		output["name"], output["version"], output["type"])
}

// printPluginInfoHuman renders the plugin details.
func printPluginInfoHuman(w io.Writer, output map[string]any) {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintf(tw, "Name:\t%v\n", output["name"])
	fmt.Fprintf(tw, "Version:\t%v\n", output["version"])
	fmt.Fprintf(tw, "Type:\t%v\n", output["type"])
	fmt.Fprintf(tw, "Author:\t%v\n", output["author"])
	fmt.Fprintf(tw, "Description:\t%v\n", output["description"])
	fmt.Fprintf(tw, "State:\t%v\n", output["state"])
	fmt.Fprintf(tw, "Entry point:\t%v\n", output["entry_point"])
	fmt.Fprintf(tw, "Binary path:\t%v\n", output["binary_path"])
	fmt.Fprintf(tw, "Installed at:\t%v\n", output["installed_at"])
	fmt.Fprintf(tw, "Updated at:\t%v\n", output["updated_at"])
	if v, ok := output["min_host_version"]; ok {
		fmt.Fprintf(tw, "Min host version:\t%v\n", v)
	}
	if v, ok := output["max_host_version"]; ok {
		fmt.Fprintf(tw, "Max host version:\t%v\n", v)
	}
	if v, ok := output["config_yaml"]; ok && v != "" {
		fmt.Fprintf(tw, "Config:\t%v\n", v)
	}
	if v, ok := output["signature"]; ok && v != "" {
		fmt.Fprintf(tw, "Signature:\t%v\n", v)
	}
	if v, ok := output["error_msg"]; ok && v != "" {
		fmt.Fprintf(tw, "Error:\t%v\n", v)
	}
	_ = tw.Flush()
}
