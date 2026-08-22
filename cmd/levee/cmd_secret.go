package main

import (
	"context"
	"fmt"
	"io"
	"os"

	"github.com/spf13/cobra"

	"github.com/nexus/levee/internal/credential"
	"github.com/nexus/levee/internal/state"
)

// Secret command option variables.
var (
	secretAddOptName    string
	secretAddOptType    string
	secretAddOptValue   string
	secretRotateOptName string
	secretRevokeOptName string
	secretShowOptName   string
)

func init() {
	RegisterCommand(newSecretCmd())
}

// newSecretCmd builds the `levee secret` sub-command with its children.
func newSecretCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "secret",
		Short: "Manage credentials",
		Long: "Manage encrypted credentials: add, list, rotate, revoke, " +
			"and show credential metadata. Plaintext values are never " +
			"displayed by any sub-command.",
	}

	cmd.AddCommand(newSecretListCmd())
	cmd.AddCommand(newSecretAddCmd())
	cmd.AddCommand(newSecretRotateCmd())
	cmd.AddCommand(newSecretRevokeCmd())
	cmd.AddCommand(newSecretShowCmd())

	return cmd
}

// newSecretListCmd builds the `levee secret list` sub-command.
func newSecretListCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List all credentials (metadata only)",
		Long:  "List all stored credentials. Only metadata is shown; plaintext values are never displayed.",
		Args:  cobra.NoArgs,
		RunE:  runSecretList,
	}
	return cmd
}

// newSecretAddCmd builds the `levee secret add` sub-command.
func newSecretAddCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "add",
		Short: "Add a new credential",
		Long:  "Add a new encrypted credential to the store. The value is encrypted at rest.",
		Args:  cobra.NoArgs,
		RunE:  runSecretAdd,
	}
	cmd.Flags().StringVar(&secretAddOptName, "name", "", "Credential name (required)")
	cmd.Flags().StringVar(&secretAddOptType, "type", "ssh_password", "Credential type (ssh_key, ssh_password, winrm_password, api_token)")
	cmd.Flags().StringVar(&secretAddOptValue, "value", "", "Credential plaintext value (required)")
	_ = cmd.MarkFlagRequired("name")
	_ = cmd.MarkFlagRequired("value")
	return cmd
}

// newSecretRotateCmd builds the `levee secret rotate` sub-command.
func newSecretRotateCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "rotate",
		Short: "Rotate a credential's value",
		Long:  "Replace the encrypted value of an existing credential with a new one.",
		Args:  cobra.NoArgs,
		RunE:  runSecretRotate,
	}
	cmd.Flags().StringVar(&secretRotateOptName, "name", "", "Credential name to rotate (required)")
	_ = cmd.MarkFlagRequired("name")
	return cmd
}

// newSecretRevokeCmd builds the `levee secret revoke` sub-command.
func newSecretRevokeCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "revoke",
		Short: "Revoke (delete) a credential",
		Long:  "Permanently remove a credential from the store.",
		Args:  cobra.NoArgs,
		RunE:  runSecretRevoke,
	}
	cmd.Flags().StringVar(&secretRevokeOptName, "name", "", "Credential name to revoke (required)")
	_ = cmd.MarkFlagRequired("name")
	return cmd
}

// newSecretShowCmd builds the `levee secret show` sub-command.
func newSecretShowCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "show",
		Short: "Show credential metadata",
		Long:  "Display metadata for a credential. The plaintext value is never shown.",
		Args:  cobra.NoArgs,
		RunE:  runSecretShow,
	}
	cmd.Flags().StringVar(&secretShowOptName, "name", "", "Credential name to show (required)")
	_ = cmd.MarkFlagRequired("name")
	return cmd
}

// masterPassword returns the master password for credential encryption.
// It reads from the LEVEE_MASTER_PASSWORD environment variable.
func masterPassword() (string, error) {
	pw := os.Getenv("LEVEE_MASTER_PASSWORD")
	if pw == "" {
		return "", fmt.Errorf("LEVEE_MASTER_PASSWORD environment variable is not set")
	}
	return pw, nil
}

// openCredentialStore creates a CredentialStore from the state store and
// master password.
//
//nolint:unused // internal API reserved for future CLI subcommand
func openCredentialStore(ctx context.Context) (*credential.CredentialStore, error) {
	store, err := openStore(ctx)
	if err != nil {
		return nil, err
	}

	mp, err := masterPassword()
	if err != nil {
		_ = store.Close()
		return nil, fmt.Errorf("get master password: %w", err)
	}

	cs, err := credential.NewCredentialStore(store, mp)
	if err != nil {
		_ = store.Close()
		return nil, fmt.Errorf("create credential store: %w", err)
	}

	return cs, nil
}

// credStoreWithCleanup opens a credential store and returns both the store
// and the underlying state store so the caller can close both.
func credStoreWithCleanup(ctx context.Context) (*credential.CredentialStore, *state.SQLiteStore, error) {
	s, err := openStore(ctx)
	if err != nil {
		return nil, nil, err
	}

	mp, err := masterPassword()
	if err != nil {
		_ = s.Close()
		return nil, nil, fmt.Errorf("get master password: %w", err)
	}

	cs, err := credential.NewCredentialStore(s, mp)
	if err != nil {
		_ = s.Close()
		return nil, nil, fmt.Errorf("create credential store: %w", err)
	}

	return cs, s, nil
}

// runSecretList executes the `levee secret list` command.
func runSecretList(cmd *cobra.Command, args []string) error {
	ctx := context.Background()

	cs, store, err := credStoreWithCleanup(ctx)
	if err != nil {
		return fmt.Errorf("open credential store: %w", err)
	}
	defer func() { _ = store.Close() }()

	creds, err := cs.List(ctx)
	if err != nil {
		return fmt.Errorf("list credentials: %w", err)
	}

	// Build output without ciphertext or plaintext.
	rows := make([]map[string]any, 0, len(creds))
	for _, c := range creds {
		rows = append(rows, map[string]any{
			"id":         c.ID,
			"name":       c.Name,
			"type":       c.Type,
			"created_at": c.CreatedAt,
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
		for _, c := range creds {
			fmt.Fprintln(os.Stdout, c.Name)
		}
		return nil
	}

	printSecretListHuman(os.Stdout, rows)
	return nil
}

// runSecretAdd executes the `levee secret add` command.
func runSecretAdd(cmd *cobra.Command, args []string) error {
	ctx := context.Background()

	cs, store, err := credStoreWithCleanup(ctx)
	if err != nil {
		return fmt.Errorf("open credential store: %w", err)
	}
	defer func() { _ = store.Close() }()

	spec := credential.CredentialSpec{
		Name:      secretAddOptName,
		Type:      secretAddOptType,
		Plaintext: []byte(secretAddOptValue),
	}

	cred, err := cs.Store(ctx, spec)
	if err != nil {
		return fmt.Errorf("add credential: %w", err)
	}

	output := map[string]any{
		"id":         cred.ID,
		"name":       cred.Name,
		"type":       cred.Type,
		"created_at": cred.CreatedAt,
	}

	if optJSON {
		return PrintJSON(os.Stdout, map[string]any{
			"data":  output,
			"meta":  nil,
			"error": nil,
		})
	}

	if optQuiet {
		fmt.Fprintln(os.Stdout, cred.ID)
		return nil
	}

	printSecretAddHuman(os.Stdout, output)
	return nil
}

// runSecretRotate executes the `levee secret rotate` command.
func runSecretRotate(cmd *cobra.Command, args []string) error {
	ctx := context.Background()

	cs, store, err := credStoreWithCleanup(ctx)
	if err != nil {
		return fmt.Errorf("open credential store: %w", err)
	}
	defer func() { _ = store.Close() }()

	// Prompt for new value from stdin.
	fmt.Fprint(os.Stderr, "Enter new value: ")
	var newValue string
	if _, err := fmt.Fscanln(os.Stdin, &newValue); err != nil {
		return fmt.Errorf("read new value: %w", err)
	}

	cred, err := cs.Rotate(ctx, secretRotateOptName, []byte(newValue))
	if err != nil {
		return fmt.Errorf("rotate credential: %w", err)
	}

	output := map[string]any{
		"id":         cred.ID,
		"name":       cred.Name,
		"type":       cred.Type,
		"rotated_at": cred.RotatedAt,
	}

	if optJSON {
		return PrintJSON(os.Stdout, map[string]any{
			"data":  output,
			"meta":  nil,
			"error": nil,
		})
	}

	if optQuiet {
		fmt.Fprintln(os.Stdout, cred.ID)
		return nil
	}

	printSecretRotateHuman(os.Stdout, output)
	return nil
}

// runSecretRevoke executes the `levee secret revoke` command.
func runSecretRevoke(cmd *cobra.Command, args []string) error {
	ctx := context.Background()

	cs, store, err := credStoreWithCleanup(ctx)
	if err != nil {
		return fmt.Errorf("open credential store: %w", err)
	}
	defer func() { _ = store.Close() }()

	if err := cs.Delete(ctx, secretRevokeOptName); err != nil {
		return fmt.Errorf("revoke credential: %w", err)
	}

	output := map[string]any{
		"name":    secretRevokeOptName,
		"revoked": true,
	}

	if optJSON {
		return PrintJSON(os.Stdout, map[string]any{
			"data":  output,
			"meta":  nil,
			"error": nil,
		})
	}

	if optQuiet {
		fmt.Fprintln(os.Stdout, secretRevokeOptName)
		return nil
	}

	printSecretRevokeHuman(os.Stdout, output)
	return nil
}

// runSecretShow executes the `levee secret show` command.
func runSecretShow(cmd *cobra.Command, args []string) error {
	ctx := context.Background()

	cs, store, err := credStoreWithCleanup(ctx)
	if err != nil {
		return fmt.Errorf("open credential store: %w", err)
	}
	defer func() { _ = store.Close() }()

	cred, err := cs.GetMetadata(ctx, secretShowOptName)
	if err != nil {
		return fmt.Errorf("show credential: %w", err)
	}

	output := map[string]any{
		"id":         cred.ID,
		"name":       cred.Name,
		"type":       cred.Type,
		"created_at": cred.CreatedAt,
		"rotated_at": cred.RotatedAt,
	}

	if optJSON {
		return PrintJSON(os.Stdout, map[string]any{
			"data":  output,
			"meta":  nil,
			"error": nil,
		})
	}

	if optQuiet {
		fmt.Fprintln(os.Stdout, cred.ID)
		return nil
	}

	printSecretShowHuman(os.Stdout, output)
	return nil
}

// printSecretListHuman renders the secret list output.
func printSecretListHuman(w io.Writer, rows []map[string]any) {
	if len(rows) == 0 {
		fmt.Fprintln(w, "No credentials found.")
		return
	}
	fmt.Fprintf(w, "%-16s %-20s %-16s %s\n", "ID", "NAME", "TYPE", "CREATED_AT")
	for _, row := range rows {
		id, _ := row["id"].(string)
		name, _ := row["name"].(string)
		typ, _ := row["type"].(string)
		createdAt := fmt.Sprintf("%v", row["created_at"])
		fmt.Fprintf(w, "%-16s %-20s %-16s %s\n", id, name, typ, createdAt)
	}
}

// printSecretAddHuman renders the secret add output.
func printSecretAddHuman(w io.Writer, output map[string]any) {
	fmt.Fprintf(w, "Credential added: %v (type=%v, id=%v)\n",
		output["name"], output["type"], output["id"])
}

// printSecretRotateHuman renders the secret rotate output.
func printSecretRotateHuman(w io.Writer, output map[string]any) {
	fmt.Fprintf(w, "Credential rotated: %v (id=%v)\n",
		output["name"], output["id"])
}

// printSecretRevokeHuman renders the secret revoke output.
func printSecretRevokeHuman(w io.Writer, output map[string]any) {
	fmt.Fprintf(w, "Credential revoked: %v\n", output["name"])
}

// printSecretShowHuman renders the secret show output.
func printSecretShowHuman(w io.Writer, output map[string]any) {
	fmt.Fprintf(w, "Credential: %v\n", output["name"])
	fmt.Fprintf(w, "  ID:         %v\n", output["id"])
	fmt.Fprintf(w, "  Type:       %v\n", output["type"])
	fmt.Fprintf(w, "  Created At: %v\n", output["created_at"])
	if output["rotated_at"] != nil {
		fmt.Fprintf(w, "  Rotated At: %v\n", output["rotated_at"])
	} else {
		fmt.Fprintf(w, "  Rotated At: -\n")
	}
}
