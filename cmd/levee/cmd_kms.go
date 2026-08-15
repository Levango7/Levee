package main


import (
	"context"
	"fmt"
	"io"
	"os"

	"github.com/nexus/levee/internal/credential"
	"github.com/spf13/cobra"
)

// KMS command option variables. Prefixed to avoid collisions with other
// command packages in the same main package.
var (
	kmsTestOptName string // --name: credential name for kms test
)

func init() {
	RegisterCommand(newKMSCmd())
}

// newKMSCmd builds the `levee kms` sub-command with its children.
func newKMSCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "kms",
		Short: "Manage external KMS integration",
		Long: "Inspect and test the external Key Management System " +
			"integration (HashiCorp Vault, AWS KMS). Sub-commands " +
			"report provider status, configuration, and connectivity.",
	}

	cmd.AddCommand(newKMSStatusCmd())
	cmd.AddCommand(newKMSConfigCmd())
	cmd.AddCommand(newKMSTestCmd())

	return cmd
}

// newKMSStatusCmd builds the `levee kms status` sub-command. It reports
// the health of every registered KMS provider.
func newKMSStatusCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "status",
		Short: "Show KMS provider status",
		Long: "Display the health and reachability of every registered " +
			"KMS provider. When no providers are configured the command " +
			"reports that the local CredentialStore is in use.",
		Args: cobra.NoArgs,
		RunE: runKMSStatus,
	}
	return cmd
}

// newKMSConfigCmd builds the `levee kms config` sub-command. It shows the
// KMS configuration derived from the LEVEE config file and environment.
func newKMSConfigCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "config",
		Short: "Show KMS configuration",
		Long: "Display the KMS configuration: which providers are " +
			"enabled, the default provider, the routing table, and " +
			"whether local fallback is enabled.",
		Args: cobra.NoArgs,
		RunE: runKMSConfig,
	}
	return cmd
}

// newKMSTestCmd builds the `levee kms test` sub-command. It performs a
// connectivity test against each registered provider (HealthCheck) and,
// when --name is supplied, attempts a full GetSecret round-trip.
func newKMSTestCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "test",
		Short: "Test KMS connectivity",
		Long: "Perform a connectivity test against every registered KMS " +
			"provider. With --name the command also attempts a full " +
			"GetSecret round-trip for the named credential.",
		Args: cobra.NoArgs,
		RunE: runKMSTest,
	}
	cmd.Flags().StringVar(&kmsTestOptName, "name", "", "Credential name for a full GetSecret round-trip test")
	return cmd
}

// --- helpers ---------------------------------------------------------------

// kmsManagerFromConfig builds a KMSManager from the LEVEE configuration.
// When no KMS providers are configured it returns (nil, nil) so the caller
// can fall back to a status-only report.
func kmsManagerFromConfig(ctx context.Context) (*credential.KMSManager, error) {
	// Load the local CredentialStore to use as fallback.
	cs, store, err := credStoreWithCleanup(ctx)
	if err != nil {
		return nil, err
	}
	_ = cs
	// We keep the store open for the lifetime of the manager; the caller
	// is responsible for closing it via the returned closeStore closure.
	_ = store

	// In MVP we do not yet parse KMS provider configs from the YAML; the
	// manager is built with just the local fallback. Providers can be
	// registered at runtime via the gRPC service or future config keys.
	mgr, err := credential.NewKMSManager(cs, nil)
	if err != nil {
		store.Close()
		return nil, fmt.Errorf("create kms manager: %w", err)
	}
	return mgr, nil
}

// --- runKMSStatus ----------------------------------------------------------

// runKMSStatus executes the `levee kms status` command.
func runKMSStatus(cmd *cobra.Command, args []string) error {
	ctx := context.Background()

	mgr, err := kmsManagerFromConfig(ctx)
	if err != nil {
		return fmt.Errorf("open kms manager: %w", err)
	}

	statuses := mgr.Status(ctx)
	hasFallback := mgr.HasFallback()
	defaultProvider := mgr.DefaultProvider()

	output := map[string]any{
		"providers":       statuses,
		"fallback":        hasFallback,
		"default_provider": defaultProvider,
	}

	if optJSON {
		return PrintJSON(os.Stdout, map[string]any{
			"data":  output,
			"meta":  nil,
			"error": nil,
		})
	}

	printKMSStatusHuman(os.Stdout, output)
	return nil
}

// --- runKMSConfig ----------------------------------------------------------

// runKMSConfig executes the `levee kms config` command.
func runKMSConfig(cmd *cobra.Command, args []string) error {
	ctx := context.Background()

	mgr, err := kmsManagerFromConfig(ctx)
	if err != nil {
		return fmt.Errorf("open kms manager: %w", err)
	}

	output := map[string]any{
		"providers":        mgr.ProviderNames(),
		"default_provider": mgr.DefaultProvider(),
		"fallback":         mgr.HasFallback(),
	}

	if optJSON {
		return PrintJSON(os.Stdout, map[string]any{
			"data":  output,
			"meta":  nil,
			"error": nil,
		})
	}

	printKMSConfigHuman(os.Stdout, output)
	return nil
}

// --- runKMSTest ------------------------------------------------------------

// runKMSTest executes the `levee kms test` command. It runs HealthCheck
// against every provider and optionally a GetSecret round-trip when
// --name is supplied.
func runKMSTest(cmd *cobra.Command, args []string) error {
	ctx := context.Background()

	mgr, err := kmsManagerFromConfig(ctx)
	if err != nil {
		return fmt.Errorf("open kms manager: %w", err)
	}

	statuses := mgr.Status(ctx)

	// Build the result rows.
	rows := make([]map[string]any, 0, len(statuses))
	for _, s := range statuses {
		row := map[string]any{
			"provider": s.Name,
			"healthy":  s.Healthy,
			"error":    s.Error,
		}

		// Optional GetSecret round-trip.
		if kmsTestOptName != "" && s.Healthy {
			cred, getErr := mgr.GetCredential(ctx, kmsTestOptName)
			if getErr != nil {
				row["get_secret"] = "fail"
				row["get_secret_error"] = getErr.Error()
			} else {
				row["get_secret"] = "ok"
				row["source"] = cred.Source
				row["plaintext_len"] = len(cred.Plaintext)
				// Zero the plaintext immediately; we never print it.
				credential.ClearKMSCredential(cred)
			}
		}

		rows = append(rows, row)
	}

	output := map[string]any{
		"results": rows,
	}

	if optJSON {
		return PrintJSON(os.Stdout, map[string]any{
			"data":  output,
			"meta":  nil,
			"error": nil,
		})
	}

	printKMSTestHuman(os.Stdout, output)
	return nil
}

// --- human-readable output --------------------------------------------------

// printKMSStatusHuman renders the `kms status` output in human-readable form.
func printKMSStatusHuman(w io.Writer, output map[string]any) {
	fmt.Fprintln(w, "KMS Provider Status")
	fmt.Fprintln(w, "===================")

	statuses, _ := output["providers"].([]credential.ProviderStatus)
	if len(statuses) == 0 {
		fmt.Fprintln(w, "No KMS providers registered. Local CredentialStore (AES-256-GCM) is in use.")
	} else {
		fmt.Fprintf(w, "%-16s %-8s %s\n", "PROVIDER", "HEALTH", "ERROR")
		for _, s := range statuses {
			health := "ok"
			if !s.Healthy {
				health = "fail"
			}
			errMsg := s.Error
			if errMsg == "" {
				errMsg = "-"
			}
			fmt.Fprintf(w, "%-16s %-8s %s\n", s.Name, health, errMsg)
		}
	}

	fallback, _ := output["fallback"].(bool)
	defaultProvider, _ := output["default_provider"].(string)

	fmt.Fprintln(w)
	fmt.Fprintf(w, "Default provider: %s\n", strOrDefault(defaultProvider, "(none)"))
	fmt.Fprintf(w, "Local fallback:   %v\n", fallback)
}

// printKMSConfigHuman renders the `kms config` output.
func printKMSConfigHuman(w io.Writer, output map[string]any) {
	fmt.Fprintln(w, "KMS Configuration")
	fmt.Fprintln(w, "=================")

	providers, _ := output["providers"].([]string)
	if len(providers) == 0 {
		fmt.Fprintln(w, "Providers: (none)")
	} else {
		fmt.Fprintf(w, "Providers: %v\n", providers)
	}

	defaultProvider, _ := output["default_provider"].(string)
	fallback, _ := output["fallback"].(bool)

	fmt.Fprintf(w, "Default provider: %s\n", strOrDefault(defaultProvider, "(none)"))
	fmt.Fprintf(w, "Local fallback:   %v\n", fallback)
}

// printKMSTestHuman renders the `kms test` output.
func printKMSTestHuman(w io.Writer, output map[string]any) {
	fmt.Fprintln(w, "KMS Connectivity Test")
	fmt.Fprintln(w, "=====================")

	results, _ := output["results"].([]map[string]any)
	if len(results) == 0 {
		fmt.Fprintln(w, "No KMS providers registered.")
		return
	}

	fmt.Fprintf(w, "%-16s %-8s %-12s %s\n", "PROVIDER", "HEALTH", "GET_SECRET", "ERROR")
	for _, r := range results {
		provider, _ := r["provider"].(string)
		healthy, _ := r["healthy"].(bool)
		health := "ok"
		if !healthy {
			health = "fail"
		}
		getSecret, _ := r["get_secret"].(string)
		if getSecret == "" {
			getSecret = "-"
		}
		errMsg, _ := r["error"].(string)
		if errMsg == "" {
			errMsg = "-"
		}
		fmt.Fprintf(w, "%-16s %-8s %-12s %s\n", provider, health, getSecret, errMsg)
	}
}

// strOrDefault returns s when non-empty, otherwise dflt.
func strOrDefault(s, dflt string) string {
	if s == "" {
		return dflt
	}
	return s
}