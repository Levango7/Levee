package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/nexus/levee/internal/config"
	"github.com/nexus/levee/internal/identity"
	"github.com/nexus/levee/internal/permission"
)

// User command option variables. Prefixed with "user" to avoid collisions
// with other command packages in the same main package.
var (
	userListOptFormat string
	userAddOptName    string
	userAddOptTeam    string
	userAddOptRole    string
)

func init() {
	RegisterCommand(newUserCmd())
}

// newUserCmd builds the `levee user` sub-command with its children.
func newUserCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "user",
		Short: "Manage users in the permission matrix",
		Long: "Manage users and their team/role assignments within the " +
			"LEVEE permission matrix. Users are mapped to teams with roles " +
			"that determine their allowed actions per environment.",
	}

	cmd.AddCommand(newUserListCmd())
	cmd.AddCommand(newUserAddCmd())

	return cmd
}

// newUserListCmd builds the `levee user list` sub-command.
func newUserListCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List users in the permission matrix",
		Long:  "List all users registered in the permission matrix with their team and role assignments.",
		Args:  cobra.NoArgs,
		RunE:  runUserList,
	}
	cmd.Flags().StringVar(&userListOptFormat, "format", "", "Output format (text)")
	return cmd
}

// newUserAddCmd builds the `levee user add` sub-command.
func newUserAddCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "add",
		Short: "Add a user to the permission matrix",
		Long: "Add a user to the permission matrix with a team and role. " +
			"The role determines which actions the user can perform on " +
			"environments assigned to their team.",
		Args: cobra.NoArgs,
		RunE: runUserAdd,
	}
	cmd.Flags().StringVar(&userAddOptName, "name", "", "User name (required)")
	cmd.Flags().StringVar(&userAddOptTeam, "team", "", "Team name (required)")
	cmd.Flags().StringVar(&userAddOptRole, "role", "", "User role: admin|operator|viewer (required)")
	_ = cmd.MarkFlagRequired("name")
	_ = cmd.MarkFlagRequired("team")
	_ = cmd.MarkFlagRequired("role")
	return cmd
}

// --- User registry types ----------------------------------------------------

// The registry implementation lives in internal/identity, because the
// serving process has to resolve a caller's team from the same file the CLI
// writes. These aliases keep the CLI's call sites, error text and tests
// unchanged across the move.
type (
	userRegistry = identity.Registry
	userEntry    = identity.User
)

func usersFilePath(dataDir string) string { return identity.FilePath(dataDir) }

func loadUserRegistry(path string) (*userRegistry, error) { return identity.Load(path) }

func saveUserRegistry(path string, reg *userRegistry) error { return identity.Save(path, reg) }

// --- Command runners ---------------------------------------------------------

// runUserList executes the `levee user list` command.
func runUserList(cmd *cobra.Command, args []string) error {
	cfg, err := loadConfigForCmd()
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	reg, err := loadUserRegistry(usersFilePath(cfg.Server.DataDir))
	if err != nil {
		return fmt.Errorf("load user registry: %w", err)
	}

	rows := make([]map[string]any, 0, len(reg.Users))
	for _, u := range reg.Users {
		rows = append(rows, map[string]any{
			"name": u.Name,
			"team": u.Team,
			"role": u.Role,
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
		for _, u := range reg.Users {
			fmt.Fprintln(os.Stdout, u.Name)
		}
		return nil
	}

	printUserListHuman(os.Stdout, rows)
	return nil
}

// runUserAdd executes the `levee user add` command.
func runUserAdd(cmd *cobra.Command, args []string) error {
	cfg, err := loadConfigForCmd()
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	if userAddOptName == "" {
		return fmt.Errorf("user name is required")
	}
	if userAddOptTeam == "" {
		return fmt.Errorf("team is required")
	}
	if userAddOptRole == "" {
		return fmt.Errorf("role is required")
	}

	// Validate role.
	switch userAddOptRole {
	case "admin", "operator", "viewer":
		// OK
	default:
		return fmt.Errorf("invalid role %q: must be admin, operator, or viewer", userAddOptRole)
	}

	// Validate team exists in the permission matrix.
	matrix, err := loadPermissionMatrix(cfg)
	if err != nil {
		return fmt.Errorf("load permission matrix: %w", err)
	}
	teamExists := false
	for _, t := range matrix.Teams() {
		if t == userAddOptTeam {
			teamExists = true
			break
		}
	}
	if !teamExists {
		return fmt.Errorf("team %q not found in permission matrix", userAddOptTeam)
	}

	path := usersFilePath(cfg.Server.DataDir)
	reg, err := loadUserRegistry(path)
	if err != nil {
		return fmt.Errorf("load user registry: %w", err)
	}

	// Check for duplicate user name.
	for _, u := range reg.Users {
		if u.Name == userAddOptName {
			return fmt.Errorf("user %q already exists", userAddOptName)
		}
	}

	entry := userEntry{
		Name: userAddOptName,
		Team: userAddOptTeam,
		Role: userAddOptRole,
	}
	reg.Users = append(reg.Users, entry)

	if err := saveUserRegistry(path, reg); err != nil {
		return fmt.Errorf("save user registry: %w", err)
	}

	output := map[string]any{
		"name": entry.Name,
		"team": entry.Team,
		"role": entry.Role,
	}

	if optJSON {
		return PrintJSON(os.Stdout, map[string]any{
			"data":  output,
			"meta":  nil,
			"error": nil,
		})
	}

	if optQuiet {
		fmt.Fprintln(os.Stdout, entry.Name)
		return nil
	}

	printUserAddHuman(os.Stdout, output)
	return nil
}

// --- Human-readable printers -------------------------------------------------

// printUserListHuman renders the user list in a human-readable format.
func printUserListHuman(w io.Writer, rows []map[string]any) {
	if len(rows) == 0 {
		fmt.Fprintln(w, "No users found.")
		return
	}
	fmt.Fprintf(w, "%-20s %-15s %-10s\n", "NAME", "TEAM", "ROLE")
	for _, row := range rows {
		name, _ := row["name"].(string)
		team, _ := row["team"].(string)
		role, _ := row["role"].(string)
		fmt.Fprintf(w, "%-20s %-15s %-10s\n", name, team, role)
	}
}

// printUserAddHuman renders the user add output.
func printUserAddHuman(w io.Writer, output map[string]any) {
	fmt.Fprintf(w, "Added user %q to team %q with role %q\n",
		output["name"], output["team"], output["role"])
}

// --- Shared helpers for user/team/system commands ----------------------------

// loadConfigForCmd loads the LEVEE configuration using the global config
// path flag. It returns the config or a wrapped error.
func loadConfigForCmd() (*config.Config, error) {
	cfg, err := config.Load(optConfigPath)
	if err != nil {
		return nil, err
	}
	applySecurityConfig(cfg)
	return cfg, nil
}

// loadPermissionMatrix loads the permission matrix from the default
// permissions file in the data directory. If the file does not exist,
// an empty matrix is returned.
func loadPermissionMatrix(cfg *config.Config) (*permission.PermissionMatrix, error) {
	path := filepath.Join(cfg.Server.DataDir, "permissions.yaml")
	matrix := permission.NewPermissionMatrix()
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return matrix, nil
	}
	if err := matrix.LoadFromYAML(path); err != nil {
		return nil, fmt.Errorf("load permission matrix from %s: %w", path, err)
	}
	return matrix, nil
}
