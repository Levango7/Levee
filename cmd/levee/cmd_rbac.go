package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"

	"github.com/nexus/levee/internal/permission"
)

// RBAC command option variables. Prefixed with "rbac" to avoid collisions
// with other command packages in the same main package.
var (
	// role add/remove
	rbacRoleAddOptName   string
	rbacRoleAddOptParent string
	rbacRoleRemoveOptID  string

	// policy add/remove
	rbacPolicyAddOptID        string
	rbacPolicyAddOptEffect    string
	rbacPolicyAddOptResource  string
	rbacPolicyAddOptAction    string
	rbacPolicyAddOptCondition string
	rbacPolicyAddOptDesc      string
	rbacPolicyRemoveOptID     string

	// check
	rbacCheckOptUser     string
	rbacCheckOptAction   string
	rbacCheckOptResource string
	rbacCheckOptLabels   []string
	rbacCheckOptVerbose  bool
)

func init() {
	RegisterCommand(newRBACCmd())
}

// newRBACCmd builds the `levee rbac` sub-command tree.
//
//	levee rbac role list/add/remove
//	levee rbac policy list/add/remove
//	levee rbac check --user <u> --action <a> --resource <r>
//	levee rbac tree
func newRBACCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "rbac",
		Short: "Manage RBAC roles, policies, and permission checks",
		Long: "Manage the role-based access control system: role " +
			"inheritance tree, fine-grained (Resource × Action × " +
			"Condition) policies, ABAC label-based access control, " +
			"and ad-hoc permission checks.",
	}
	cmd.AddCommand(newRBACRoleCmd())
	cmd.AddCommand(newRBACPolicyCmd())
	cmd.AddCommand(newRBACCheckCmd())
	cmd.AddCommand(newRBACTreeCmd())
	return cmd
}

// --- role sub-commands ------------------------------------------------------

func newRBACRoleCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "role",
		Short: "Manage roles in the inheritance tree",
	}
	cmd.AddCommand(newRBACRoleListCmd())
	cmd.AddCommand(newRBACRoleAddCmd())
	cmd.AddCommand(newRBACRoleRemoveCmd())
	return cmd
}

func newRBACRoleListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List roles and their parent relationships",
		Args:  cobra.NoArgs,
		RunE:  runRBACRoleList,
	}
}

func newRBACRoleAddCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "add",
		Short: "Add a role, optionally with a parent for inheritance",
		Args:  cobra.NoArgs,
		RunE:  runRBACRoleAdd,
	}
	cmd.Flags().StringVar(&rbacRoleAddOptName, "name", "", "Role name (required)")
	cmd.Flags().StringVar(&rbacRoleAddOptParent, "parent", "", "Parent role for inheritance (optional)")
	_ = cmd.MarkFlagRequired("name")
	return cmd
}

func newRBACRoleRemoveCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "remove",
		Short: "Remove a role from the inheritance tree",
		Args:  cobra.NoArgs,
		RunE:  runRBACRoleRemove,
	}
	cmd.Flags().StringVar(&rbacRoleRemoveOptID, "name", "", "Role name to remove (required)")
	_ = cmd.MarkFlagRequired("name")
	return cmd
}

// --- policy sub-commands ----------------------------------------------------

func newRBACPolicyCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "policy",
		Short: "Manage fine-grained permission policies",
	}
	cmd.AddCommand(newRBACPolicyListCmd())
	cmd.AddCommand(newRBACPolicyAddCmd())
	cmd.AddCommand(newRBACPolicyRemoveCmd())
	return cmd
}

func newRBACPolicyListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List all policies",
		Args:  cobra.NoArgs,
		RunE:  runRBACPolicyList,
	}
}

func newRBACPolicyAddCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "add",
		Short: "Add a (Resource × Action × Condition) policy",
		Args:  cobra.NoArgs,
		RunE:  runRBACPolicyAdd,
	}
	cmd.Flags().StringVar(&rbacPolicyAddOptID, "id", "", "Policy id (required)")
	cmd.Flags().StringVar(&rbacPolicyAddOptEffect, "effect", "allow", "Effect: allow or deny")
	cmd.Flags().StringVar(&rbacPolicyAddOptResource, "resource", "", "Resource pattern (required)")
	cmd.Flags().StringVar(&rbacPolicyAddOptAction, "action", "", "Action (required)")
	cmd.Flags().StringVar(&rbacPolicyAddOptCondition, "condition", "", "Label condition (optional)")
	cmd.Flags().StringVar(&rbacPolicyAddOptDesc, "description", "", "Human-readable description")
	_ = cmd.MarkFlagRequired("id")
	_ = cmd.MarkFlagRequired("resource")
	_ = cmd.MarkFlagRequired("action")
	return cmd
}

func newRBACPolicyRemoveCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "remove",
		Short: "Remove a policy by id",
		Args:  cobra.NoArgs,
		RunE:  runRBACPolicyRemove,
	}
	cmd.Flags().StringVar(&rbacPolicyRemoveOptID, "id", "", "Policy id to remove (required)")
	_ = cmd.MarkFlagRequired("id")
	return cmd
}

// --- check / tree sub-commands ---------------------------------------------

func newRBACCheckCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "check",
		Short: "Check whether a user may perform an action on a resource",
		Args:  cobra.NoArgs,
		RunE:  runRBACCheck,
	}
	cmd.Flags().StringVar(&rbacCheckOptUser, "user", "", "Subject / user (required)")
	cmd.Flags().StringVar(&rbacCheckOptAction, "action", "", "Action to check (required)")
	cmd.Flags().StringVar(&rbacCheckOptResource, "resource", "", "Resource to check (required)")
	cmd.Flags().StringSliceVar(&rbacCheckOptLabels, "label", nil, "Resource labels (key=value, repeatable)")
	cmd.Flags().BoolVar(&rbacCheckOptVerbose, "verbose", false, "Show detailed explanation")
	_ = cmd.MarkFlagRequired("user")
	_ = cmd.MarkFlagRequired("action")
	_ = cmd.MarkFlagRequired("resource")
	return cmd
}

func newRBACTreeCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "tree",
		Short: "Display the role inheritance tree",
		Args:  cobra.NoArgs,
		RunE:  runRBACTree,
	}
}

// --- Command runners --------------------------------------------------------

// runRBACRoleList executes `levee rbac role list`.
func runRBACRoleList(cmd *cobra.Command, args []string) error {
	tree, _, _, _, err := loadRBACState()
	if err != nil {
		return err
	}

	roles := tree.Roles()
	rows := make([]map[string]any, 0, len(roles))
	for _, r := range roles {
		parent, _ := tree.Parent(r)
		direct, _ := tree.DirectPermissions(r)
		effective, _ := tree.EffectivePermissions(r)
		rows = append(rows, map[string]any{
			"role":      r,
			"parent":    parent,
			"direct":    direct,
			"effective": effective,
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
		for _, r := range roles {
			fmt.Fprintln(os.Stdout, r)
		}
		return nil
	}
	printRBACRoleListHuman(os.Stdout, rows)
	return nil
}

// runRBACRoleAdd executes `levee rbac role add`.
func runRBACRoleAdd(cmd *cobra.Command, args []string) error {
	tree, _, treePath, _, err := loadRBACState()
	if err != nil {
		return err
	}
	if err := tree.AddRole(rbacRoleAddOptName, rbacRoleAddOptParent); err != nil {
		return fmt.Errorf("add role: %w", err)
	}
	if err := saveRBACRoleTree(treePath, tree); err != nil {
		return fmt.Errorf("save role tree: %w", err)
	}

	out := map[string]any{
		"role":   rbacRoleAddOptName,
		"parent": rbacRoleAddOptParent,
	}
	if optJSON {
		return PrintJSON(os.Stdout, map[string]any{"data": out, "meta": nil, "error": nil})
	}
	if optQuiet {
		fmt.Fprintln(os.Stdout, rbacRoleAddOptName)
		return nil
	}
	fmt.Fprintf(os.Stdout, "Added role %q", rbacRoleAddOptName)
	if rbacRoleAddOptParent != "" {
		fmt.Fprintf(os.Stdout, " with parent %q", rbacRoleAddOptParent)
	}
	fmt.Fprintln(os.Stdout)
	return nil
}

// runRBACRoleRemove executes `levee rbac role remove`.
func runRBACRoleRemove(cmd *cobra.Command, args []string) error {
	tree, _, treePath, _, err := loadRBACState()
	if err != nil {
		return err
	}
	if err := tree.RemoveRole(rbacRoleRemoveOptID); err != nil {
		return fmt.Errorf("remove role: %w", err)
	}
	if err := saveRBACRoleTree(treePath, tree); err != nil {
		return fmt.Errorf("save role tree: %w", err)
	}

	if optJSON {
		return PrintJSON(os.Stdout, map[string]any{
			"data":  map[string]any{"removed": rbacRoleRemoveOptID},
			"meta":  nil,
			"error": nil,
		})
	}
	fmt.Fprintf(os.Stdout, "Removed role %q\n", rbacRoleRemoveOptID)
	return nil
}

// runRBACPolicyList executes `levee rbac policy list`.
func runRBACPolicyList(cmd *cobra.Command, args []string) error {
	_, ps, _, _, err := loadRBACState()
	if err != nil {
		return err
	}
	policies := ps.List()
	rows := make([]map[string]any, 0, len(policies))
	for _, p := range policies {
		rows = append(rows, map[string]any{
			"id":          p.ID,
			"effect":      string(p.Effect),
			"resource":    p.Resource,
			"action":      p.Action,
			"condition":   p.Condition,
			"description": p.Description,
		})
	}

	if optJSON {
		return PrintJSON(os.Stdout, map[string]any{"data": rows, "meta": nil, "error": nil})
	}
	if optQuiet {
		for _, p := range policies {
			fmt.Fprintln(os.Stdout, p.ID)
		}
		return nil
	}
	printRBACPolicyListHuman(os.Stdout, rows)
	return nil
}

// runRBACPolicyAdd executes `levee rbac policy add`.
func runRBACPolicyAdd(cmd *cobra.Command, args []string) error {
	_, ps, _, policyPath, err := loadRBACState()
	if err != nil {
		return err
	}
	p := &permission.Policy{
		ID:          rbacPolicyAddOptID,
		Effect:      permission.PolicyEffect(rbacPolicyAddOptEffect),
		Resource:    rbacPolicyAddOptResource,
		Action:      rbacPolicyAddOptAction,
		Condition:   rbacPolicyAddOptCondition,
		Description: rbacPolicyAddOptDesc,
	}
	if err := ps.Add(p); err != nil {
		return fmt.Errorf("add policy: %w", err)
	}
	if err := saveRBACPolicySet(policyPath, ps); err != nil {
		return fmt.Errorf("save policies: %w", err)
	}

	out := map[string]any{
		"id":       p.ID,
		"effect":   string(p.Effect),
		"resource": p.Resource,
		"action":   p.Action,
	}
	if optJSON {
		return PrintJSON(os.Stdout, map[string]any{"data": out, "meta": nil, "error": nil})
	}
	fmt.Fprintf(os.Stdout, "Added policy %q (%s %s on %s)\n", p.ID, p.Effect, p.Action, p.Resource)
	return nil
}

// runRBACPolicyRemove executes `levee rbac policy remove`.
func runRBACPolicyRemove(cmd *cobra.Command, args []string) error {
	_, ps, _, policyPath, err := loadRBACState()
	if err != nil {
		return err
	}
	if !ps.Remove(rbacPolicyRemoveOptID) {
		return fmt.Errorf("policy %q not found", rbacPolicyRemoveOptID)
	}
	if err := saveRBACPolicySet(policyPath, ps); err != nil {
		return fmt.Errorf("save policies: %w", err)
	}

	if optJSON {
		return PrintJSON(os.Stdout, map[string]any{
			"data":  map[string]any{"removed": rbacPolicyRemoveOptID},
			"meta":  nil,
			"error": nil,
		})
	}
	fmt.Fprintf(os.Stdout, "Removed policy %q\n", rbacPolicyRemoveOptID)
	return nil
}

// runRBACCheck executes `levee rbac check`.
func runRBACCheck(cmd *cobra.Command, args []string) error {
	_, ps, _, _, err := loadRBACState()
	if err != nil {
		return err
	}
	labels := parseLabelFlags(rbacCheckOptLabels)
	engine := permission.NewABACEngine(ps)
	allowed, reason := engine.Evaluate(rbacCheckOptUser, rbacCheckOptAction, rbacCheckOptResource, labels)

	out := map[string]any{
		"allowed":  allowed,
		"reason":   reason,
		"user":     rbacCheckOptUser,
		"action":   rbacCheckOptAction,
		"resource": rbacCheckOptResource,
		"labels":   labels,
	}
	if optJSON {
		return PrintJSON(os.Stdout, map[string]any{"data": out, "meta": nil, "error": nil})
	}
	if rbacCheckOptVerbose {
		fmt.Fprint(os.Stdout, engine.Explain(rbacCheckOptUser, rbacCheckOptAction, rbacCheckOptResource, labels))
		return nil
	}
	decision := "DENY"
	if allowed {
		decision = "ALLOW"
	}
	fmt.Fprintf(os.Stdout, "%s — %s\n", decision, reason)
	return nil
}

// runRBACTree executes `levee rbac tree`.
func runRBACTree(cmd *cobra.Command, args []string) error {
	tree, _, _, _, err := loadRBACState()
	if err != nil {
		return err
	}
	if optJSON {
		roles := tree.Roles()
		nodes := make([]map[string]any, 0, len(roles))
		for _, r := range roles {
			parent, _ := tree.Parent(r)
			children, _ := tree.Children(r)
			effective, _ := tree.EffectivePermissions(r)
			nodes = append(nodes, map[string]any{
				"role":      r,
				"parent":    parent,
				"children":  children,
				"effective": effective,
			})
		}
		return PrintJSON(os.Stdout, map[string]any{"data": nodes, "meta": nil, "error": nil})
	}
	fmt.Fprint(os.Stdout, tree.Render())
	return nil
}

// --- RBAC state persistence helpers -----------------------------------------

// rbacRoleTreeConfig is the YAML representation of the role tree.
type rbacRoleTreeConfig struct {
	Roles []rbacRoleConfig `yaml:"roles"`
}

type rbacRoleConfig struct {
	Name        string   `yaml:"name"`
	Parent      string   `yaml:"parent,omitempty"`
	Permissions []string `yaml:"permissions,omitempty"`
}

// loadRBACState loads the role tree and policy set from the data
// directory. Missing files yield empty structures. The paths to the
// role tree and policy files are returned so callers can save updates.
func loadRBACState() (*permission.RoleTree, *permission.PolicySet, string, string, error) {
	cfg, err := loadConfigForCmd()
	if err != nil {
		return nil, nil, "", "", fmt.Errorf("load config: %w", err)
	}
	treePath := filepath.Join(cfg.Server.DataDir, "roles.yaml")
	policyPath := filepath.Join(cfg.Server.DataDir, "policies.yaml")

	tree, err := loadRBACRoleTree(treePath)
	if err != nil {
		return nil, nil, "", "", fmt.Errorf("load role tree: %w", err)
	}
	ps, err := loadRBACPolicySet(policyPath)
	if err != nil {
		return nil, nil, "", "", fmt.Errorf("load policies: %w", err)
	}
	return tree, ps, treePath, policyPath, nil
}

// loadRBACRoleTree reads the role tree from a YAML file. A missing file
// yields an empty tree.
func loadRBACRoleTree(path string) (*permission.RoleTree, error) {
	tree := permission.NewRoleTree()
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return tree, nil
		}
		return nil, fmt.Errorf("read role tree: %w", err)
	}
	var cfg rbacRoleTreeConfig
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("unmarshal role tree: %w", err)
	}
	// First pass: add roles so parents exist before children reference them.
	// We add parent-less roles first, then iterate until all are added.
	added := make(map[string]bool)
	for pass := 0; pass < len(cfg.Roles)+1; pass++ {
		progress := false
		for _, r := range cfg.Roles {
			if added[r.Name] {
				continue
			}
			if r.Parent != "" && !added[r.Parent] {
				continue
			}
			if err := tree.AddRole(r.Name, r.Parent); err != nil {
				return nil, fmt.Errorf("add role %q: %w", r.Name, err)
			}
			if len(r.Permissions) > 0 {
				if err := tree.GrantPermission(r.Name, r.Permissions...); err != nil {
					return nil, fmt.Errorf("grant permissions for %q: %w", r.Name, err)
				}
			}
			added[r.Name] = true
			progress = true
		}
		if !progress {
			break
		}
	}
	return tree, nil
}

// saveRBACRoleTree writes the role tree to a YAML file.
func saveRBACRoleTree(path string, tree *permission.RoleTree) error {
	roles := tree.Roles()
	cfg := rbacRoleTreeConfig{Roles: make([]rbacRoleConfig, 0, len(roles))}
	for _, r := range roles {
		parent, _ := tree.Parent(r)
		direct, _ := tree.DirectPermissions(r)
		cfg.Roles = append(cfg.Roles, rbacRoleConfig{
			Name:        r,
			Parent:      parent,
			Permissions: direct,
		})
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create role tree dir: %w", err)
	}
	data, err := yaml.Marshal(cfg)
	if err != nil {
		return fmt.Errorf("marshal role tree: %w", err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return fmt.Errorf("write role tree: %w", err)
	}
	return nil
}

// loadRBACPolicySet reads the policy set from a YAML file. A missing file
// yields an empty policy set.
func loadRBACPolicySet(path string) (*permission.PolicySet, error) {
	ps := permission.NewPolicySet()
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return ps, nil
		}
		return nil, fmt.Errorf("read policies: %w", err)
	}
	var cfg permission.PolicyConfig
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("unmarshal policies: %w", err)
	}
	if err := ps.LoadFromConfig(cfg); err != nil {
		return nil, fmt.Errorf("load policies: %w", err)
	}
	return ps, nil
}

// saveRBACPolicySet writes the policy set to a YAML file.
func saveRBACPolicySet(path string, ps *permission.PolicySet) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create policy dir: %w", err)
	}
	data, err := ps.MarshalYAML()
	if err != nil {
		return fmt.Errorf("marshal policies: %w", err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return fmt.Errorf("write policies: %w", err)
	}
	return nil
}

// parseLabelFlags parses a slice of "key=value" strings into a label
// map. Malformed entries are silently skipped so that a typo does not
// abort the whole check.
func parseLabelFlags(flags []string) map[string]string {
	out := make(map[string]string, len(flags))
	for _, f := range flags {
		k, v, ok := strings.Cut(f, "=")
		if !ok || k == "" {
			continue
		}
		out[k] = v
	}
	return out
}

// --- Human-readable printers ------------------------------------------------

func printRBACRoleListHuman(w io.Writer, rows []map[string]any) {
	if len(rows) == 0 {
		fmt.Fprintln(w, "No roles found.")
		return
	}
	for _, row := range rows {
		role, _ := row["role"].(string)
		parent, _ := row["parent"].(string)
		direct, _ := row["direct"].([]string)
		effective, _ := row["effective"].([]string)
		fmt.Fprintf(w, "Role: %s\n", role)
		if parent != "" {
			fmt.Fprintf(w, "  parent: %s\n", parent)
		}
		fmt.Fprintf(w, "  direct:    %v\n", direct)
		fmt.Fprintf(w, "  effective: %v\n", effective)
	}
}

func printRBACPolicyListHuman(w io.Writer, rows []map[string]any) {
	if len(rows) == 0 {
		fmt.Fprintln(w, "No policies found.")
		return
	}
	for _, row := range rows {
		fmt.Fprintf(w, "Policy %q\n", row["id"])
		fmt.Fprintf(w, "  effect:    %s\n", row["effect"])
		fmt.Fprintf(w, "  resource:  %s\n", row["resource"])
		fmt.Fprintf(w, "  action:    %s\n", row["action"])
		if cond, _ := row["condition"].(string); cond != "" {
			fmt.Fprintf(w, "  condition: %s\n", cond)
		}
		if desc, _ := row["description"].(string); desc != "" {
			fmt.Fprintf(w, "  desc:      %s\n", desc)
		}
	}
}
