package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/nexus/levee/internal/config"

	"github.com/spf13/cobra"

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
			"toolchain version. With --json the output is a structured document.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			info := versionInfo{
				Version:    version,
				BuildTime:  buildTime,
				GoVersion:  goVersion,
				CommitHash: commitHash,
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

// newSystemStatusCmd builds the `levee system status` sub-command.
func newSystemStatusCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "status",
		Short: "Show system status",
		Long: "Display system status including database connectivity, " +
			"configuration path, and version information.",
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
	cfg, err := loadConfigForCmd()
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	// Check DB connectivity.
	dbStatus := "ok"
	dbErr := ""
	ctx := context.Background()
	store, err := state.NewSQLiteStore(ctx, cfg.Database.Path)
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
		"db_path":     cfg.Database.Path,
	}
	if dbErr != "" {
		output["db_error"] = dbErr
	}

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

	// Check 2: Database reachable.
	if cfg != nil {
		ctx := context.Background()
		store, dbErr := state.NewSQLiteStore(ctx, cfg.Database.Path)
		if dbErr != nil {
			checks = append(checks, map[string]any{
				"check":  "database",
				"status": "FAIL",
				"error":  dbErr.Error(),
			})
		} else {
			_ = store.Close()
			checks = append(checks, map[string]any{
				"check":  "database",
				"status": "OK",
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
	dir := cfgPath[:strings.LastIndex(cfgPath, "/")]
	if dir == "" {
		dir = "."
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
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

	if err := os.WriteFile(cfgPath, []byte(content), 0o644); err != nil {
		return fmt.Errorf("write config: %w", err)
	}
	return nil
}

// --- Human-readable printers -------------------------------------------------

// printSystemStatusHuman renders the system status in a human-readable format.
func printSystemStatusHuman(w io.Writer, output map[string]any) {
	fmt.Fprintf(w, "levee %s\n", output["version"])
	fmt.Fprintf(w, "  config:  %s\n", output["config_path"])
	fmt.Fprintf(w, "  db:      %s (%s)\n", output["db_status"], output["db_path"])
	if dbErr, ok := output["db_error"].(string); ok && dbErr != "" {
		fmt.Fprintf(w, "  db_err:  %s\n", dbErr)
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
