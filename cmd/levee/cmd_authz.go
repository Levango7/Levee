package main

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/nexus/levee/internal/authz"
	"github.com/nexus/levee/internal/config"
)

var (
	authzExplainOptSubject string
	authzExplainOptEnv     string
	authzExplainOptAction  string
)

func init() {
	RegisterCommand(newAuthzCmd())
}

// newAuthzCmd builds the `levee authz` command group: the diagnostic surface
// for the policy layer. Without it, "why was I denied" is answerable only by
// reading three YAML files and the matrix code by hand — which is how RBAC
// deployments end up with permissions nobody can debug.
func newAuthzCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "authz",
		Short: "Inspect the authorization policy and explain decisions",
		Long: "Inspect the permission matrix / role tree / user registry that govern " +
			"change actions (plan, apply, rollback, approve, reject), the read scope " +
			"(view) and the fleet surfaces (inventory, template library, audit trail, " +
			"system config, agent registry — admin), and explain a " +
			"single decision: which team and role a subject resolves to, which axis " +
			"granted or refused, and why.",
	}
	cmd.AddCommand(newAuthzStatusCmd(), newAuthzExplainCmd())
	return cmd
}

func newAuthzStatusCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Show whether policy enforcement is active and what is loaded",
		Args:  cobra.NoArgs,
		RunE:  runAuthzStatus,
	}
}

func newAuthzExplainCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "explain",
		Short: "Explain one authorization decision",
		Long: "Print how LEVEE would decide a single (subject, env, action) question: " +
			"the resolved team and role, the axis that decided it, and the reason. " +
			"Use it before and after configuring a matrix — the answer is the same " +
			"one the RPCs act on.",
		Args: cobra.NoArgs,
		RunE: runAuthzExplain,
	}
	cmd.Flags().StringVar(&authzExplainOptSubject, "subject", "", "Authenticated subject (as a named token or SSO login presents it)")
	cmd.Flags().StringVar(&authzExplainOptEnv, "env", "", "Environment to judge; defaults to permission.default_env")
	cmd.Flags().StringVar(&authzExplainOptAction, "action", "", "Action to judge: plan|apply|approve|rollback|pause|resume|view")
	_ = cmd.MarkFlagRequired("subject")
	_ = cmd.MarkFlagRequired("action")
	return cmd
}

func loadAuthorizerForCmd() (*authz.Authorizer, *config.Config, error) {
	cfg, err := loadConfigForCmd()
	if err != nil {
		return nil, nil, fmt.Errorf("load config: %w", err)
	}
	a, err := authz.Load(cfg.Server.DataDir, cfg.Permission.DefaultEnv)
	if err != nil {
		return nil, nil, err
	}
	return a, cfg, nil
}

func runAuthzStatus(cmd *cobra.Command, args []string) error {
	a, cfg, err := loadAuthorizerForCmd()
	if err != nil {
		return err
	}

	enforced := a.Enforced()
	registered := a.Registered()
	if optJSON {
		return PrintJSON(os.Stdout, map[string]any{
			"data": map[string]any{
				"enforced":            enforced,
				"data_dir":            cfg.Server.DataDir,
				"default_env":         cfg.Permission.DefaultEnv,
				"registered_subjects": registered,
				"matrix_file":         authz.MatrixFileName,
				"role_tree_file":      authz.RoleTreeFileName,
			},
			"meta":  nil,
			"error": nil,
		})
	}
	if optQuiet {
		fmt.Fprintln(os.Stdout, enforced)
		return nil
	}

	fmt.Fprintf(os.Stdout, "data dir   : %s\n", cfg.Server.DataDir)
	fmt.Fprintf(os.Stdout, "default env: %s\n", cfg.Permission.DefaultEnv)
	fmt.Fprintf(os.Stdout, "enforced   : %v\n", enforced)
	if enforced {
		fmt.Fprintf(os.Stdout, "subjects   : %d registered\n", len(registered))
		for _, s := range registered {
			fmt.Fprintf(os.Stdout, "  - %s\n", s)
		}
	} else {
		fmt.Fprintln(os.Stdout, "note       : no permission matrix found — every RPC (changes,")
		fmt.Fprintln(os.Stdout, "             inventory, templates, audit reads, system config,")
		fmt.Fprintln(os.Stdout, "             agent registry) is limited to authentication alone.")
		fmt.Fprintln(os.Stdout, "             Configure teams with `levee team add`, roles with")
		fmt.Fprintln(os.Stdout, "             `levee rbac`, and members with `levee user add`.")
	}
	return nil
}

func runAuthzExplain(cmd *cobra.Command, args []string) error {
	a, _, err := loadAuthorizerForCmd()
	if err != nil {
		return err
	}
	env := authzExplainOptEnv
	d := a.Decide(authzExplainOptSubject, env, authzExplainOptAction)

	if optJSON {
		return PrintJSON(os.Stdout, map[string]any{
			"data": map[string]any{
				"subject":  d.Subject,
				"env":      d.Env,
				"action":   d.Action,
				"enforced": d.Enforced,
				"allowed":  d.Allowed,
				"via":      d.Via,
				"team":     d.Team,
				"role":     d.Role,
				"reason":   d.Reason,
			},
			"meta":  nil,
			"error": nil,
		})
	}
	fmt.Fprint(os.Stdout, a.Explain(authzExplainOptSubject, env, authzExplainOptAction))
	return nil
}
