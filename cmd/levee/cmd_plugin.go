package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/nexus/levee/internal/config"
	"github.com/nexus/levee/internal/plugin"
)

// Plugin command option variables. Prefixed to avoid collisions with other
// command packages in the same main package.
var (
	pluginOptVerifySig bool
)

func init() {
	RegisterCommand(newPluginCmd())
}

// newPluginCmd builds the `levee plugin` sub-command with its children.
func newPluginCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "plugin",
		Short: "Manage the LEVEE plugin registry",
		// The boundary is stated here rather than implied: NOTIFIER, GATE and
		// MODULE plugins are a live execution surface (serve registers
		// enabled "notify" plugins into its notification manager, resolves
		// "gate" plugins at check time, and registers "module" plugins
		// into the process-wide executor), while channel plugins remain
		// registry-only. To extend the engine's channels today, write a
		// compile-time channel plugin instead (docs/channel-plugins.md).
		Long: "Manage the LEVEE plugin registry: list, install, enable, " +
			"disable, remove, inspect and test plugins (registry, sandbox " +
			"and signature verification). EXECUTION SURFACE: enabled " +
			"notifier plugins are consumed by serve — they join the " +
			"notification manager as delivery channels; gate plugins are " +
			"dispatched by a workflow's `plugin:` gates; module plugins " +
			"are registered into the executor, so a step whose `module:` " +
			"names the plugin runs it. Channel plugins are still " +
			"registry-only (no engine consumption); to extend the " +
			"engine's channels today, write a compile-time channel " +
			"plugin instead (see docs/channel-plugins.md).",
	}

	cmd.AddCommand(newPluginListCmd())
	cmd.AddCommand(newPluginInstallCmd())
	cmd.AddCommand(newPluginEnableCmd())
	cmd.AddCommand(newPluginDisableCmd())
	cmd.AddCommand(newPluginRemoveCmd())
	cmd.AddCommand(newPluginInfoCmd())
	cmd.AddCommand(newPluginTestCmd())

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
		Long: "Display the full registry record for a plugin. For a module " +
			"plugin this includes the manifest's destructive_actions and " +
			"reversible_actions declarations read from the registry — no " +
			"plugin binary is executed.",
		Args: cobra.ExactArgs(1),
		RunE: runPluginInfo,
	}
}

// newPluginTestCmd builds the `levee plugin test <name>` sub-command.
func newPluginTestCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "test <name>",
		Short: "Run one live handshake against an enabled plugin",
		Long: "Execute the plugin's binary once and speak the JSON-line " +
			"stdio protocol to it: send the meta (identity) request and the " +
			"close request, report what came back. For a module plugin the " +
			"answer is also cross-checked against the manifest's action " +
			"declarations, so the output names the verbs the manifest left " +
			"unclassified — the ones the plan-time gate will judge " +
			"irreversible. This is the smallest " +
			"TRUE execution of the install->enable->execute chain — it " +
			"proves the registry path resolves, the binary runs and the " +
			"plugin answers. It is still NOT engine consumption: see the " +
			"plugin command's boundary note.",
		Args: cobra.ExactArgs(1),
		RunE: runPluginTest,
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
	applySecurityConfig(cfg)

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
	// Only a module plugin has actions a workflow step can name, so the
	// classification is reported for that type alone — a gate/channel/notifier
	// record showing empty action lists would read as "declared nothing
	// destructive" when the truth is "has no actions at all".
	if rec.Type == plugin.TypeModule {
		m["destructive_actions"] = rec.DestructiveActions
		m["reversible_actions"] = rec.ReversibleActions
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

	// The classification lines follow the table instead of sitting inside it:
	// an empty declaration is the common case, and "declared nothing" only
	// means something for a module plugin (the only type whose actions a
	// workflow step can name).
	if typ, _ := output["type"].(string); typ == string(plugin.TypeModule) {
		printModuleClassificationHint(w, output)
	}
}

// printModuleClassificationHint reports what the registry says about this
// module's actions, and — the part a manifest alone cannot answer — which
// verbs are still unclassified. Unclassified means the plan-time gate judges
// them irreversible, so an operator who expects a read-only verb to pass
// without allow_irreversible has to see this before the first refused plan.
func printModuleClassificationHint(w io.Writer, output map[string]any) {
	fmt.Fprintf(w, "Destructive actions:\t%s\n", joinActions(output["destructive_actions"], "(none declared)"))
	fmt.Fprintf(w, "Reversible actions:\t%s\n", joinActions(output["reversible_actions"], "(none declared)"))
	fmt.Fprintln(w, "Actions in neither list are judged irreversible at plan time.")
	fmt.Fprintln(w, "Run `levee plugin test <name>` to list them against the plugin's declared verbs.")
}

// joinActions renders a declaration list for a human reader, substituting a
// placeholder when the manifest declared nothing.
func joinActions(v any, empty string) string {
	items, ok := v.([]string)
	if !ok || len(items) == 0 {
		return empty
	}
	return strings.Join(items, ", ")
}

// runPluginTest executes `levee plugin test <name>`: one live handshake
// against an enabled plugin (see newPluginTestCmd for what "live" means and
// where it stops short of engine consumption).
func runPluginTest(cmd *cobra.Command, args []string) error {
	ctx := context.Background()
	mgr, cleanup, err := openPluginManager(ctx)
	if err != nil {
		return err
	}
	defer cleanup()

	rec, err := mgr.Info(ctx, args[0])
	if err != nil {
		return fmt.Errorf("plugin test: %w", err)
	}
	if rec.State != plugin.StateEnabled {
		return fmt.Errorf("plugin test: %q is %s; enable it first (`levee plugin enable %s`)",
			rec.Name, rec.State, rec.Name)
	}

	start := time.Now()
	responses, probeErr := plugin.Probe(ctx, rec.BinaryPath, plugin.MetaHandshake())
	elapsed := time.Since(start)
	if probeErr != nil {
		return fmt.Errorf("plugin test: handshake with %q failed: %w", rec.Name, probeErr)
	}
	if len(responses) == 0 || !responses[0].OK {
		msg := "no answer"
		if len(responses) > 0 {
			msg = responses[0].Error
		}
		return fmt.Errorf("plugin test: %q answered but refused the meta request: %s", rec.Name, msg)
	}

	identity := map[string]any{"message": ""}
	if responses[0].Result != nil {
		identity = responses[0].Result.Details
		if identity == nil {
			identity = map[string]any{"message": responses[0].Result.Message}
		}
	}

	output := map[string]any{
		"name":     rec.Name,
		"ok":       true,
		"identity": identity,
		"rounds":   len(responses),
		"elapsed":  elapsed.String(),
	}

	// A module plugin's handshake verb list is the only place the full action
	// surface exists, so this is where an operator can see which verbs the
	// manifest left unclassified — i.e. which ones the plan-time gate will
	// judge irreversible. A refusal here is the same refusal `serve` makes when
	// it builds the module, so `plugin test` cannot report a plugin as usable
	// that the engine will not load.
	var surface plugin.ModuleSurface
	if rec.Type == plugin.TypeModule {
		s, err := plugin.ClassifyModuleSurface(rec, responses[0])
		if err != nil {
			return fmt.Errorf("plugin test: %w", err)
		}
		surface = s
		output["classification"] = map[string]any{
			"actions":      surface.Actions,
			"destructive":  surface.Destructive,
			"reversible":   surface.Reversible,
			"unclassified": surface.Unclassified,
		}
	}

	if optJSON {
		return PrintJSON(os.Stdout, map[string]any{
			"data":  output,
			"meta":  nil,
			"error": nil,
		})
	}

	fmt.Fprintf(os.Stdout, "Plugin %s answered the handshake in %s\n", rec.Name, elapsed)
	for k, v := range identity {
		fmt.Fprintf(os.Stdout, "  %s: %v\n", k, v)
	}
	if rec.Type == plugin.TypeModule {
		printModuleSurface(os.Stdout, surface)
	}
	return nil
}

// printModuleSurface renders the classification block of `plugin test`. The
// unclassified line is printed even when empty: "none" is the answer the
// operator is looking for, and omitting it would leave "did you check?" open.
func printModuleSurface(w io.Writer, s plugin.ModuleSurface) {
	fmt.Fprintf(w, "  destructive: %s\n", joinActions(s.Destructive, "(none)"))
	fmt.Fprintf(w, "  reversible: %s\n", joinActions(s.Reversible, "(none)"))
	fmt.Fprintf(w, "  unclassified (judged irreversible at plan time): %s\n", joinActions(s.Unclassified, "(none)"))
}
