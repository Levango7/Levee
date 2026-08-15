package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/nexus/levee/internal/permission"
	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"
)

// Team command option variables. Prefixed with "team" to avoid collisions
// with other command packages in the same main package.
var (
	teamListOptFormat string
	teamAddOptName    string
	teamAddOptEnv     string
)

func init() {
	RegisterCommand(newTeamCmd())
}

// newTeamCmd builds the `levee team` sub-command with its children.
func newTeamCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "team",
		Short: "Manage teams in the permission matrix",
		Long: "Manage teams and their environment permissions within the " +
			"LEVEE permission matrix. Teams define groups of users that " +
			"share the same set of allowed actions per environment.",
	}

	cmd.AddCommand(newTeamListCmd())
	cmd.AddCommand(newTeamAddCmd())

	return cmd
}

// newTeamListCmd builds the `levee team list` sub-command.
func newTeamListCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List teams in the permission matrix",
		Long:  "List all teams and their environment permissions from the permission matrix.",
		Args:  cobra.NoArgs,
		RunE:  runTeamList,
	}
	cmd.Flags().StringVar(&teamListOptFormat, "format", "", "Output format (text)")
	return cmd
}

// newTeamAddCmd builds the `levee team add` sub-command.
func newTeamAddCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "add",
		Short: "Add a team with environment permissions",
		Long: "Add a team to the permission matrix with an environment and " +
			"default actions. The team is granted plan, apply, and view " +
			"actions on the specified environment by default.",
		Args: cobra.NoArgs,
		RunE: runTeamAdd,
	}
	cmd.Flags().StringVar(&teamAddOptName, "name", "", "Team name (required)")
	cmd.Flags().StringVar(&teamAddOptEnv, "env", "", "Environment name (required)")
	cmd.MarkFlagRequired("name")
	cmd.MarkFlagRequired("env")
	return cmd
}

// --- Command runners ---------------------------------------------------------

// runTeamList executes the `levee team list` command.
func runTeamList(cmd *cobra.Command, args []string) error {
	cfg, err := loadConfigForCmd()
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	matrix, err := loadPermissionMatrix(cfg)
	if err != nil {
		return fmt.Errorf("load permission matrix: %w", err)
	}

	teams := matrix.Teams()
	envs := matrix.Environments()

	rows := make([]map[string]any, 0, len(teams))
	for _, team := range teams {
		// Collect environments and actions for this team.
		var teamEnvs []map[string]any
		for _, env := range envs {
			actions := matrix.ActionsFor(team, env)
			if len(actions) > 0 {
				teamEnvs = append(teamEnvs, map[string]any{
					"env":     env,
					"actions": actions,
				})
			}
		}
		rows = append(rows, map[string]any{
			"team":         team,
			"environments": teamEnvs,
		})
	}

	if optJSON {
		return PrintJSON(os.Stdout, map[string]any{
			"data":  rows,
			"meta":  nil,
			"error": nil,
		})
	}

	if optQuiet {
		for _, team := range teams {
			fmt.Fprintln(os.Stdout, team)
		}
		return nil
	}

	printTeamListHuman(os.Stdout, rows)
	return nil
}

// runTeamAdd executes the `levee team add` command.
func runTeamAdd(cmd *cobra.Command, args []string) error {
	cfg, err := loadConfigForCmd()
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	if teamAddOptName == "" {
		return fmt.Errorf("team name is required")
	}
	if teamAddOptEnv == "" {
		return fmt.Errorf("environment is required")
	}

	// Load existing permission config, add the new team+env, and save.
	permPath := filepath.Join(cfg.Server.DataDir, "permissions.yaml")
	permCfg, err := loadPermissionConfig(permPath)
	if err != nil {
		return fmt.Errorf("load permission config: %w", err)
	}

	// Check if team already exists with the same env.
	for i, team := range permCfg.Teams {
		if team.Name == teamAddOptName {
			for _, env := range team.Environments {
				if env.Name == teamAddOptEnv {
					return fmt.Errorf("team %q already has environment %q", teamAddOptName, teamAddOptEnv)
				}
			}
			// Add env to existing team.
			permCfg.Teams[i].Environments = append(permCfg.Teams[i].Environments,
				permission.EnvPermission{
					Name:    teamAddOptEnv,
					Actions: defaultTeamActions(),
				})
			if err := savePermissionConfig(permPath, permCfg); err != nil {
				return fmt.Errorf("save permission config: %w", err)
			}
			return printTeamAddResult(teamAddOptName, teamAddOptEnv, defaultTeamActions())
		}
	}

	// New team.
	permCfg.Teams = append(permCfg.Teams, permission.TeamRule{
		Name: teamAddOptName,
		Environments: []permission.EnvPermission{
			{
				Name:    teamAddOptEnv,
				Actions: defaultTeamActions(),
			},
		},
	})

	if err := savePermissionConfig(permPath, permCfg); err != nil {
		return fmt.Errorf("save permission config: %w", err)
	}

	return printTeamAddResult(teamAddOptName, teamAddOptEnv, defaultTeamActions())
}

// --- Permission config file helpers ------------------------------------------

// loadPermissionConfig reads the permission configuration from the YAML file
// at path. If the file does not exist, an empty config is returned.
func loadPermissionConfig(path string) (*permission.PermissionConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return &permission.PermissionConfig{}, nil
		}
		return nil, fmt.Errorf("read permission config: %w", err)
	}
	var cfg permission.PermissionConfig
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("unmarshal permission config: %w", err)
	}
	return &cfg, nil
}

// savePermissionConfig writes the permission configuration to the YAML file
// at path. The parent directory is created if it does not exist.
func savePermissionConfig(path string, cfg *permission.PermissionConfig) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create permission config dir: %w", err)
	}
	data, err := yaml.Marshal(cfg)
	if err != nil {
		return fmt.Errorf("marshal permission config: %w", err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return fmt.Errorf("write permission config: %w", err)
	}
	return nil
}

// defaultTeamActions returns the default actions granted when adding a new
// team+env pair.
func defaultTeamActions() []string {
	return []string{permission.ActionPlan, permission.ActionApply, permission.ActionView}
}

// printTeamAddResult outputs the result of adding a team+env pair.
func printTeamAddResult(teamName, envName string, actions []string) error {
	output := map[string]any{
		"team":    teamName,
		"env":     envName,
		"actions": actions,
	}

	if optJSON {
		return PrintJSON(os.Stdout, map[string]any{
			"data":  output,
			"meta":  nil,
			"error": nil,
		})
	}

	if optQuiet {
		fmt.Fprintln(os.Stdout, teamName)
		return nil
	}

	printTeamAddHuman(os.Stdout, output)
	return nil
}

// --- Human-readable printers -------------------------------------------------

// printTeamListHuman renders the team list in a human-readable format.
func printTeamListHuman(w io.Writer, rows []map[string]any) {
	if len(rows) == 0 {
		fmt.Fprintln(w, "No teams found.")
		return
	}
	for _, row := range rows {
		team, _ := row["team"].(string)
		fmt.Fprintf(w, "Team: %s\n", team)
		envs, _ := row["environments"].([]map[string]any)
		if len(envs) == 0 {
			fmt.Fprintln(w, "  (no environments)")
		}
		for _, envRow := range envs {
			env, _ := envRow["env"].(string)
			actions, _ := envRow["actions"].([]string)
			fmt.Fprintf(w, "  %-15s %v\n", env, actions)
		}
	}
}

// printTeamAddHuman renders the team add output.
func printTeamAddHuman(w io.Writer, output map[string]any) {
	fmt.Fprintf(w, "Added team %q with environment %q (actions: %v)\n",
		output["team"], output["env"], output["actions"])
}
