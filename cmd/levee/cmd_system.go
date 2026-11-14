package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/nexus/levee/internal/config"
	"github.com/nexus/levee/internal/partition"

	"github.com/spf13/cobra"
	"google.golang.org/protobuf/types/known/emptypb"

	"github.com/nexus/levee/internal/state"
)

// System command option variables. Prefixed to avoid collisions with other
// command packages in the same main package.
var (
	statusOptFormat string
)

func init() {
	RegisterCommand(newSystemCmd())
}

// newSystemCmd builds the `levee system` sub-command with its children.
func newSystemCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "system",
		Short: "System information and diagnostics",
		Long: "System-level commands for version info, status checks, " +
			"configuration management, and diagnostic tools.",
	}

	cmd.AddCommand(newSystemVersionCmd())
	cmd.AddCommand(newSystemStatusCmd())
	cmd.AddCommand(newSystemConfigCmd())
	cmd.AddCommand(newSystemDoctorCmd())

	return cmd
}

// newSystemVersionCmd builds the `levee system version` sub-command.
// It mirrors the root-level `levee version` command under the system group.
func newSystemVersionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print LEVEE version information",
		Long: "Print the LEVEE binary version, build time, commit hash and Go " +
			"toolchain version. With --json the output is a structured document. " +
			"With --remote it reports the SERVER's build instead of this binary's — " +
			"which is the usual question for a deployment.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			info, err := fetchVersionInfo(agentRunContext(cmd))
			if err != nil {
				return err
			}
			if optJSON {
				return PrintJSON(os.Stdout, map[string]any{
					"data":  info,
					"meta":  nil,
					"error": nil,
				})
			}
			printVersionHuman(os.Stdout, info)
			return nil
		},
	}
}

// fetchVersionInfo asks the ACTIVE MODE for build information.
//
// Local mode answers from the build variables linked into this binary; remote
// mode asks the server. Both go through the service factory, which is the one
// place that decides which side answers — that is what wiring it buys. Local
// mode needs no store: GetVersion never reads one, and the factory builds local
// services on demand precisely so that asking for a version cannot create a
// database as a side effect.
func fetchVersionInfo(ctx context.Context) (versionInfo, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	mode, err := resolveServiceMode()
	if err != nil {
		return versionInfo{}, err
	}
	f, err := newServiceFactory(mode, optServer, optAPIToken)
	if err != nil {
		return versionInfo{}, err
	}
	defer func() { _ = f.Close() }()

	svc, err := f.GetSystemService()
	if err != nil {
		return versionInfo{}, err
	}
	v, err := svc.GetVersion(ctx, &emptypb.Empty{})
	if err != nil {
		return versionInfo{}, fmt.Errorf("get version: %w", err)
	}
	info := versionInfo{
		Version:    v.GetVersion(),
		BuildTime:  v.GetBuildDate(),
		GoVersion:  v.GetGoVersion(),
		CommitHash: v.GetGitCommit(),
	}
	if mode == modeRemote {
		info.Source = "server " + optServer
	}
	return info, nil
}

// newSystemStatusCmd builds the `levee system status` sub-command.
func newSystemStatusCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "status",
		Short: "Show system status",
		Long: "Display system status. Local mode reports the configuration path and " +
			"the connectivity of the database this machine's config points at. " +
			"With --remote it reports the server's runtime health (status, run " +
			"counts, uptime, store type, warnings) and names the fields the " +
			"GetStatus RPC does not carry (config path and database location, " +
			"which GetConfig gates behind admin).",
		Args: cobra.NoArgs,
		RunE: runSystemStatus,
	}
	cmd.Flags().StringVar(&statusOptFormat, "format", "", "Output format (text)")
	return cmd
}

// newSystemConfigCmd builds the `levee system config` sub-command with
// get and set children.
func newSystemConfigCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "config",
		Short: "Manage LEVEE configuration",
		Long:  "Get and set LEVEE configuration values.",
	}

	cmd.AddCommand(newSystemConfigGetCmd())
	cmd.AddCommand(newSystemConfigSetCmd())

	return cmd
}

// newSystemConfigGetCmd builds the `levee system config get <key>` sub-command.
func newSystemConfigGetCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "get <key>",
		Short: "Get a configuration value",
		Long: "Get a configuration value by its dotted key path " +
			"(e.g. server.log_level, database.path).",
		Args: cobra.ExactArgs(1),
		RunE: runSystemConfigGet,
	}
}

// newSystemConfigSetCmd builds the `levee system config set <key> <value>` sub-command.
func newSystemConfigSetCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "set <key> <value>",
		Short: "Set a configuration value",
		Long: "Set a configuration value by its dotted key path. " +
			"The change is written to the active configuration file.",
		Args: cobra.ExactArgs(2),
		RunE: runSystemConfigSet,
	}
}

// newSystemDoctorCmd builds the `levee system doctor` sub-command.
func newSystemDoctorCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "doctor",
		Short: "Run diagnostic checks",
		Long: "Run a suite of diagnostic checks: database reachability, " +
			"configuration completeness, and permission matrix loading.",
		Args: cobra.NoArgs,
		RunE: runSystemDoctor,
	}
}

// --- Command runners ---------------------------------------------------------

// runSystemStatus executes the `levee system status` command.
func runSystemStatus(cmd *cobra.Command, args []string) error {
	mode, err := resolveServiceMode()
	if err != nil {
		return err
	}
	if mode == modeRemote {
		return runRemoteSystemStatus(cmd.Context())
	}
	return runLocalSystemStatus()
}

// runLocalSystemStatus probes the database this machine's configuration points
// at. It must not run under --remote: the CLI's database and the server's
// database are different facts, and reporting the former as the latter is how
// `system status` once stayed green for a PostgreSQL deployment.
func runLocalSystemStatus() error {
	cfg, err := loadConfigForCmd()
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	// Check DB connectivity through the same decision every other command uses.
	// Probing SQLite directly here used to make `system status` green for a
	// PostgreSQL deployment: it opened (and created) the local SQLite file while
	// the server ran against PostgreSQL, so the health check validated a
	// database that holds nothing.
	dbStatus := "ok"
	dbErr := ""
	ctx := context.Background()
	store, err := openStoreFromConfig(ctx, cfg)
	if err != nil {
		dbStatus = "unreachable"
		dbErr = err.Error()
	} else {
		_ = store.Close()
	}

	// Determine config path.
	configPath := optConfigPath
	if configPath == "" {
		configPath = "default (~/.levee/config.yaml)"
	}

	output := map[string]any{
		"version":     version,
		"config_path": configPath,
		"db_status":   dbStatus,
		"db_driver":   cfg.Database.Driver,
		"db_location": cfg.Database.StoreLocation(),
	}
	// db_path stays for the SQLite deployments that already parse it. It is
	// omitted rather than filled with a DSN-shaped string under postgres: a key
	// called "path" that sometimes holds a connection URI is how an operator
	// ends up scripting against the wrong field.
	if cfg.Database.Driver == state.DriverSQLite {
		output["db_path"] = cfg.Database.Path
	}
	if dbErr != "" {
		output["db_error"] = dbErr
	}
	// The disconnect-policy block (Q3): the control-plane state is already
	// reported as db_status above; this pairs it with the whitelist of commands
	// that stay safe during a partition. An operator reading this in an outage
	// gets both halves of the answer — "is the plane up?" and "what may I still
	// run?" — from one command instead of having to know the list by heart.
	output["partition"] = partitionStatusBlock(dbStatus)

	if optJSON {
		return PrintJSON(os.Stdout, map[string]any{
			"data":  output,
			"meta":  nil,
			"error": nil,
		})
	}

	if optQuiet {
		fmt.Fprintln(os.Stdout, dbStatus)
		return nil
	}

	printSystemStatusHuman(os.Stdout, output)
	return nil
}

// remoteLocalOnlyStatusFields names the keys local `system status` reports that
// the remote GetStatus RPC deliberately does not carry.
//
// GetStatus performs NO authorization check: the REST gateway routes
// /system/status through bearer authentication only (when a token is
// configured), and the gRPC method has no policy gate either. A config path and
// a database endpoint are deployment topology, not health; GetConfig gates
// exactly that class of data behind ActionAdmin. Widening SystemStatus with
// these fields would put topology on every monitoring scrape, so the remote
// surface stays counts-only and the CLI names the difference instead of
// silently omitting keys.
var remoteLocalOnlyStatusFields = []string{
	"config_path",
	"db_status",
	"db_driver",
	"db_location",
	"db_path",
}

// runRemoteSystemStatus asks the server for its build and runtime health.
// It never loads the local config or opens the local store: the whole point of
// --remote is that the answers describe the other machine.
func runRemoteSystemStatus(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	f, err := newServiceFactory(modeRemote, optServer, optAPIToken)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()

	svc, err := f.GetSystemService()
	if err != nil {
		return err
	}
	info, err := svc.GetVersion(ctx, &emptypb.Empty{})
	if err != nil {
		return fmt.Errorf("get version: %w", err)
	}
	status, err := svc.GetStatus(ctx, &emptypb.Empty{})
	if err != nil {
		return fmt.Errorf("get status: %w", err)
	}

	warnings := status.GetWarnings()
	if warnings == nil {
		warnings = []string{}
	}
	output := map[string]any{
		"mode":           "remote",
		"server":         optServer,
		"version":        info.GetVersion(),
		"status":         status.GetStatus(),
		"active_runs":    status.GetActiveRuns(),
		"paused_runs":    status.GetPausedRuns(),
		"uptime_seconds": status.GetUptimeSeconds(),
		"store_type":     status.GetStoreType(),
		"warnings":       warnings,
		"local_only":     remoteLocalOnlyStatusFields,
	}

	if optJSON {
		return PrintJSON(os.Stdout, map[string]any{
			"data":  output,
			"meta":  nil,
			"error": nil,
		})
	}

	if optQuiet {
		fmt.Fprintln(os.Stdout, status.GetStatus())
		return nil
	}

	printRemoteSystemStatusHuman(os.Stdout, output)
	return nil
}

// printRemoteSystemStatusHuman renders the reduced remote schema. The last line
// is not decoration: without it, an operator comparing local and remote output
// sees missing keys and cannot tell "the server does not report this" from
// "--remote was ignored".
func printRemoteSystemStatusHuman(w io.Writer, output map[string]any) {
	fmt.Fprintf(w, "levee %s (server %s)\n", output["version"], output["server"])
	fmt.Fprintf(w, "  status:    %v\n", output["status"])
	fmt.Fprintf(w, "  runs:      %v active, %v paused\n", output["active_runs"], output["paused_runs"])
	fmt.Fprintf(w, "  uptime:    %vs\n", output["uptime_seconds"])
	fmt.Fprintf(w, "  store:     %v\n", output["store_type"])
	warnings, _ := output["warnings"].([]string)
	if len(warnings) == 0 {
		fmt.Fprintln(w, "  warnings:  none")
	} else {
		fmt.Fprintf(w, "  warnings:  %d\n", len(warnings))
		for _, warning := range warnings {
			fmt.Fprintf(w, "    - %s\n", warning)
		}
	}
	fmt.Fprintf(w, "  local-only: %s (the GetStatus RPC does not report these)\n",
		strings.Join(remoteLocalOnlyStatusFields, ", "))
}

// runSystemConfigGet executes the `levee system config get <key>` command.
func runSystemConfigGet(cmd *cobra.Command, args []string) error {
	key := args[0]

	cfg, err := loadConfigForCmd()
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	value, err := getConfigValue(cfg, key)
	if err != nil {
		return err
	}

	output := map[string]any{
		"key":   key,
		"value": value,
	}

	if optJSON {
		return PrintJSON(os.Stdout, map[string]any{
			"data":  output,
			"meta":  nil,
			"error": nil,
		})
	}

	if optQuiet {
		fmt.Fprintln(os.Stdout, value)
		return nil
	}

	printConfigGetHuman(os.Stdout, output)
	return nil
}

// runSystemConfigSet executes the `levee system config set <key> <value>` command.
func runSystemConfigSet(cmd *cobra.Command, args []string) error {
	key := args[0]
	value := args[1]

	// Validate the key is a known config key.
	if err := validateConfigKey(key); err != nil {
		return err
	}

	// For MVP, config set writes to the config file if one exists.
	// If no config file is specified, we create one at the default location.
	cfgPath := optConfigPath
	if cfgPath == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return fmt.Errorf("resolve home dir: %w", err)
		}
		cfgPath = home + "/.levee/config.yaml"
	}

	if err := setConfigValue(cfgPath, key, value); err != nil {
		return fmt.Errorf("set config value: %w", err)
	}

	output := map[string]any{
		"key":   key,
		"value": value,
		"path":  cfgPath,
	}

	if optJSON {
		return PrintJSON(os.Stdout, map[string]any{
			"data":  output,
			"meta":  nil,
			"error": nil,
		})
	}

	if optQuiet {
		fmt.Fprintln(os.Stdout, value)
		return nil
	}

	printConfigSetHuman(os.Stdout, output)
	return nil
}

// runSystemDoctor executes the `levee system doctor` command.
func runSystemDoctor(cmd *cobra.Command, args []string) error {
	var checks []map[string]any

	// Check 1: Configuration loadable.
	cfg, cfgErr := loadConfigForCmd()
	if cfgErr != nil {
		checks = append(checks, map[string]any{
			"check":  "config",
			"status": "FAIL",
			"error":  cfgErr.Error(),
		})
	} else {
		checks = append(checks, map[string]any{
			"check":  "config",
			"status": "OK",
		})
	}

	// Check 2: Database reachable — through openStoreFromConfig, so `doctor`
	// probes the backend the deployment actually uses instead of always opening
	// a local SQLite file and calling that healthy.
	if cfg != nil {
		ctx := context.Background()
		store, dbErr := openStoreFromConfig(ctx, cfg)
		if dbErr != nil {
			checks = append(checks, map[string]any{
				"check":    "database",
				"status":   "FAIL",
				"driver":   cfg.Database.Driver,
				"location": cfg.Database.StoreLocation(),
				"error":    dbErr.Error(),
			})
		} else {
			_ = store.Close()
			checks = append(checks, map[string]any{
				"check":    "database",
				"status":   "OK",
				"driver":   cfg.Database.Driver,
				"location": cfg.Database.StoreLocation(),
			})
		}
	} else {
		checks = append(checks, map[string]any{
			"check":  "database",
			"status": "SKIP",
			"error":  "config not loaded",
		})
	}

	// Check 3: Permission matrix loadable.
	if cfg != nil {
		matrix, permErr := loadPermissionMatrix(cfg)
		if permErr != nil {
			checks = append(checks, map[string]any{
				"check":  "permission_matrix",
				"status": "FAIL",
				"error":  permErr.Error(),
			})
		} else {
			teams := matrix.Teams()
			envs := matrix.Environments()
			checks = append(checks, map[string]any{
				"check":  "permission_matrix",
				"status": "OK",
				"teams":  len(teams),
				"envs":   len(envs),
			})
		}
	} else {
		checks = append(checks, map[string]any{
			"check":  "permission_matrix",
			"status": "SKIP",
			"error":  "config not loaded",
		})
	}

	// Determine overall status.
	allOK := true
	for _, c := range checks {
		if c["status"] != "OK" {
			allOK = false
			break
		}
	}
	overall := "healthy"
	if !allOK {
		overall = "unhealthy"
	}

	if optJSON {
		return PrintJSON(os.Stdout, map[string]any{
			"data": map[string]any{
				"overall": overall,
				"checks":  checks,
			},
			"meta":  nil,
			"error": nil,
		})
	}

	printDoctorHuman(os.Stdout, overall, checks)
	return nil
}

// --- Config value helpers ----------------------------------------------------

// getConfigValue retrieves a configuration value by dotted key path.
func getConfigValue(cfg *config.Config, key string) (string, error) {
	switch key {
	case "server.data_dir":
		return cfg.Server.DataDir, nil
	case "server.log_level":
		return cfg.Server.LogLevel, nil
	case "server.log_format":
		return cfg.Server.LogFormat, nil
	case "database.driver":
		return cfg.Database.Driver, nil
	case "database.path":
		return cfg.Database.Path, nil
	// The DSN is answerable but never verbatim: `system config get` prints to
	// stdout, and this value is a connection string with a password in it. The
	// redacted form still answers what the operator is asking — which server and
	// database this profile points at — without shipping the credential to the
	// terminal, the scrollback and any log capture in between.
	case "database.dsn":
		return state.RedactDSN(cfg.Database.DSN), nil
	case "database.max_open_conns":
		return fmt.Sprintf("%d", cfg.Database.MaxOpenConns), nil
	case "database.max_idle_conns":
		return fmt.Sprintf("%d", cfg.Database.MaxIdleConns), nil
	case "log.level":
		return cfg.Log.Level, nil
	case "log.format":
		return cfg.Log.Format, nil
	case "log.output":
		return cfg.Log.Output, nil
	case "permission.default_team":
		return cfg.Permission.DefaultTeam, nil
	case "permission.default_env":
		return cfg.Permission.DefaultEnv, nil
	default:
		return "", fmt.Errorf("unknown config key %q", key)
	}
}

// validateConfigKey checks that a key is a valid, settable config key.
func validateConfigKey(key string) error {
	allowed := map[string]bool{
		"server.log_level":  true,
		"server.log_format": true,
		"log.level":         true,
		"log.format":        true,
		"log.output":        true,
	}
	if !allowed[key] {
		return fmt.Errorf("config key %q is not settable (allowed: %s)",
			key, strings.Join(sortedKeys(allowed), ", "))
	}
	return nil
}

// sortedKeys returns the keys of a map sorted alphabetically.
func sortedKeys(m map[string]bool) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	// Simple insertion sort for small maps.
	for i := 1; i < len(keys); i++ {
		for j := i; j > 0 && keys[j] < keys[j-1]; j-- {
			keys[j], keys[j-1] = keys[j-1], keys[j]
		}
	}
	return keys
}

// setConfigValue writes a key=value pair to the YAML config file. For MVP,
// this appends a comment and the key=value line. A full implementation would
// use viper's WriteConfig, but that requires the config to have been read
// from a file which may not always be the case.
func setConfigValue(cfgPath, key, value string) error {
	// filepath.Dir understands both separators; a "/"-only LastIndex would
	// return -1 on Windows paths and panic the slice expression.
	dir := filepath.Dir(cfgPath)
	if dir == "" {
		dir = "."
	}
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return fmt.Errorf("create config dir: %w", err)
	}

	// Read existing content.
	existing := ""
	data, err := os.ReadFile(cfgPath)
	if err == nil {
		existing = string(data)
	}

	// Convert dotted key to YAML nested format.
	parts := strings.SplitN(key, ".", 2)
	var yamlLine string
	if len(parts) == 2 {
		yamlLine = fmt.Sprintf("%s:\n  %s: %s", parts[0], parts[1], value)
	} else {
		yamlLine = fmt.Sprintf("%s: %s", key, value)
	}

	// Append the line.
	content := existing
	if content != "" && !strings.HasSuffix(content, "\n") {
		content += "\n"
	}
	content += yamlLine + "\n"

	// cfgPath comes from the operator's own --config flag or the $HOME
	// default; the only caller is this CLI command (no server-side path
	// reaches here), and writing to the config file the operator names is the
	// command's purpose. key is whitelist-validated by validateConfigKey.
	if err := os.WriteFile(cfgPath, []byte(content), 0o600); err != nil { // #nosec G703 -- operator-named config file via --config flag; no untrusted input reaches here
		return fmt.Errorf("write config: %w", err)
	}
	return nil
}

// partitionStatusBlock builds the Q3 disconnect-policy half of `system status`:
// the control-plane state (as probed by the same store open every command
// uses) paired with the curated whitelist of commands that remain safe during a
// partition.
//
// Kept as its own function so the pairing is testable without running the whole
// command: a caller that reports "reachable" next to a list that was silently
// dropped would be worse than one that omits the block entirely.
func partitionStatusBlock(dbStatus string) map[string]any {
	return map[string]any{
		"control_plane":     dbStatus,
		"readonly_commands": partition.ReadonlyCommands(),
	}
}

// --- Human-readable printers -------------------------------------------------

// printSystemStatusHuman renders the system status in a human-readable format.
func printSystemStatusHuman(w io.Writer, output map[string]any) {
	fmt.Fprintf(w, "levee %s\n", output["version"])
	fmt.Fprintf(w, "  config:  %s\n", output["config_path"])
	fmt.Fprintf(w, "  db:      %s %s (%v)\n",
		output["db_status"], output["db_driver"], output["db_location"])
	if dbErr, ok := output["db_error"].(string); ok && dbErr != "" {
		fmt.Fprintf(w, "  db_err:  %s\n", dbErr)
	}
	// The partition block mirrors the JSON. Only the count is printed in the
	// human form: the whitelist is long, and an operator in an outage wants the
	// answer "what may I run?" to be one line that points at --json for the
	// list, not thirty lines of noise above the db status they came for.
	if p, ok := output["partition"].(map[string]any); ok {
		if cmds, ok := p["readonly_commands"].([]string); ok {
			fmt.Fprintf(w, "  断网策略: 控制面 %s；分区期安全命令 %d 条（详见 --json）\n",
				p["control_plane"], len(cmds))
		}
	}
}

// printConfigGetHuman renders the config get output.
func printConfigGetHuman(w io.Writer, output map[string]any) {
	fmt.Fprintf(w, "%s = %v\n", output["key"], output["value"])
}

// printConfigSetHuman renders the config set output.
func printConfigSetHuman(w io.Writer, output map[string]any) {
	fmt.Fprintf(w, "Set %s = %v (written to %s)\n",
		output["key"], output["value"], output["path"])
}

// printDoctorHuman renders the doctor diagnostic output.
func printDoctorHuman(w io.Writer, overall string, checks []map[string]any) {
	fmt.Fprintf(w, "LEVEE Doctor: %s\n\n", overall)
	for _, c := range checks {
		fmt.Fprintf(w, "  %-20s %s\n", c["check"], c["status"])
		if errMsg, ok := c["error"].(string); ok && errMsg != "" {
			fmt.Fprintf(w, "    error: %s\n", errMsg)
		}
		if teams, ok := c["teams"].(int); ok && teams > 0 {
			fmt.Fprintf(w, "    teams: %d\n", teams)
		}
		if envs, ok := c["envs"].(int); ok && envs > 0 {
			fmt.Fprintf(w, "    environments: %d\n", envs)
		}
	}
}
