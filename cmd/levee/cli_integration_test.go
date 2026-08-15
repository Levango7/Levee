package main

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// --- Test helpers -----------------------------------------------------------

// captureStdout executes fn while redirecting os.Stdout to a buffer, then
// returns the captured output. It also redirects rootCmd's output streams.
func captureStdout(fn func() error) (string, error) {
	resetRootFlags()

	// Save original stdout.
	origStdout := os.Stdout

	// Create a pipe: writes go to w, reads come from r.
	r, w, err := os.Pipe()
	if err != nil {
		return "", err
	}

	// Replace os.Stdout with the write end.
	os.Stdout = w

	// Also redirect cobra's output to the same writer.
	rootCmd.SetOut(w)
	rootCmd.SetErr(w)

	// Channel to collect the captured output.
	outCh := make(chan string, 1)
	go func() {
		var buf bytes.Buffer
		_, _ = io.Copy(&buf, r)
		outCh <- buf.String()
	}()

	// Run the function.
	fnErr := fn()

	// Close the write end to signal EOF to the reader.
	w.Close()

	// Restore original stdout.
	os.Stdout = origStdout

	// Read the captured output.
	out := <-outCh
	return out, fnErr
}

// executeCommand resets global flags, redirects os.Stdout, sets the given args,
// and executes rootCmd. It returns the captured stdout output and the error
// from rootCmd.Execute().
func executeCommand(args ...string) (string, error) {
	return captureStdout(func() error {
		rootCmd.SetArgs(args)
		return rootCmd.Execute()
	})
}

// executeCommandJSON is a convenience wrapper that runs executeCommand and
// parses the output as JSON into the provided envelope-shaped map.
func executeCommandJSON(args ...string) (map[string]any, string, error) {
	out, err := executeCommand(args...)
	var result map[string]any
	jsonErr := json.Unmarshal([]byte(out), &result)
	if jsonErr != nil {
		return nil, out, err
	}
	return result, out, err
}

// --- 1. TestCLI_VersionJSON -------------------------------------------------

func TestCLI_VersionJSON(t *testing.T) {
	result, _, err := executeCommandJSON("version", "--json")
	// version --json should succeed (no store access needed).
	require.NoError(t, err)
	require.NotNil(t, result)

	data, ok := result["data"].(map[string]any)
	require.True(t, ok, "expected data to be a map")

	// Verify the canonical fields exist.
	assert.Contains(t, data, "version")
	assert.Contains(t, data, "build_time")
	assert.Contains(t, data, "commit_hash")
}

// --- 2. TestCLI_VersionHuman ------------------------------------------------

func TestCLI_VersionHuman(t *testing.T) {
	out, err := executeCommand("version")
	require.NoError(t, err)
	assert.Contains(t, out, "levee")
}

// --- 3. TestCLI_HelpOutput --------------------------------------------------

func TestCLI_HelpOutput(t *testing.T) {
	out, err := executeCommand("--help")
	// --help returns nil error from cobra.
	require.NoError(t, err)

	// Verify all major command names appear in the help output.
	expectedCommands := []string{
		"version", "new", "show", "apply", "list",
		"approve", "reject", "pause", "resume",
		"rollback", "trace", "logs", "diff",
		"archive", "cancel", "retry", "clone", "link",
		"template", "target", "audit", "secret", "system",
		"user", "team",
	}
	for _, name := range expectedCommands {
		assert.Contains(t, out, name, "help should list command %q", name)
	}
}

// --- 4. TestCLI_HelpJSON ----------------------------------------------------

func TestCLI_HelpJSON(t *testing.T) {
	result, _, err := executeCommandJSON("--help", "--json")
	require.NoError(t, err)
	require.NotNil(t, result)

	// The JSON help is wrapped in an outputEnvelope, so subcommands live
	// inside result["data"].
	data, ok := result["data"].(map[string]any)
	require.True(t, ok, "expected data to be a map in JSON help")

	subs, ok := data["subcommands"]
	require.True(t, ok, "expected subcommands key in JSON help data")
	assert.NotZero(t, subs, "subcommands should not be empty")
}

// --- 5. TestCLI_UnknownCommand ----------------------------------------------

func TestCLI_UnknownCommand(t *testing.T) {
	_, err := executeCommand("unknown-cmd")
	require.Error(t, err, "unknown command should return an error")
}

// --- 6. TestCLI_NewRequiresTemplate -----------------------------------------

func TestCLI_NewRequiresTemplate(t *testing.T) {
	_, err := executeCommand("new")
	require.Error(t, err, "new without template arg should error")
}

// --- 7. TestCLI_ShowRequiresRunID -------------------------------------------

func TestCLI_ShowRequiresRunID(t *testing.T) {
	_, err := executeCommand("show")
	require.Error(t, err, "show without run-id should error")
}

// --- 8. TestCLI_ApplyRequiresRunID ------------------------------------------

func TestCLI_ApplyRequiresRunID(t *testing.T) {
	_, err := executeCommand("apply")
	require.Error(t, err, "apply without run-id should error")
}

// --- 9. TestCLI_ListNoArgs --------------------------------------------------

func TestCLI_ListNoArgs(t *testing.T) {
	// list accepts no positional args; it may fail due to missing store
	// but should not panic.
	_, err := executeCommand("list")
	// We only verify it does not panic; the error may be nil or non-nil
	// depending on store availability. The key invariant is: no crash.
	_ = err
}

// --- 10. TestCLI_ApproveRejectRequiresRunID ---------------------------------

func TestCLI_ApproveRejectRequiresRunID(t *testing.T) {
	_, err := executeCommand("approve")
	require.Error(t, err, "approve without run-id should error")

	_, err = executeCommand("reject")
	require.Error(t, err, "reject without run-id should error")
}

// --- 11. TestCLI_PauseResumeRequiresRunID -----------------------------------

func TestCLI_PauseResumeRequiresRunID(t *testing.T) {
	_, err := executeCommand("pause")
	require.Error(t, err, "pause without run-id should error")

	_, err = executeCommand("resume")
	require.Error(t, err, "resume without run-id should error")
}

// --- 12. TestCLI_RollbackRequiresRunID --------------------------------------

func TestCLI_RollbackRequiresRunID(t *testing.T) {
	_, err := executeCommand("rollback")
	require.Error(t, err, "rollback without run-id should error")
}

// --- 13. TestCLI_TraceRequiresRunID -----------------------------------------

func TestCLI_TraceRequiresRunID(t *testing.T) {
	_, err := executeCommand("trace")
	require.Error(t, err, "trace without run-id should error")
}

// --- 14. TestCLI_LogsRequiresRunID ------------------------------------------

func TestCLI_LogsRequiresRunID(t *testing.T) {
	_, err := executeCommand("logs")
	require.Error(t, err, "logs without run-id should error")
}

// --- 15. TestCLI_DiffRequiresTwoRunIDs --------------------------------------

func TestCLI_DiffRequiresTwoRunIDs(t *testing.T) {
	// No args at all.
	_, err := executeCommand("diff")
	require.Error(t, err, "diff without args should error")

	// Only one arg.
	_, err = executeCommand("diff", "run-1")
	require.Error(t, err, "diff with one arg should error")
}

// --- 16. TestCLI_ArchiveRequiresRunID ---------------------------------------

func TestCLI_ArchiveRequiresRunID(t *testing.T) {
	_, err := executeCommand("archive")
	require.Error(t, err, "archive without run-id should error")
}

// --- 17. TestCLI_TemplateSubcommands ----------------------------------------

func TestCLI_TemplateSubcommands(t *testing.T) {
	// Verify that `levee template list` and `levee template show` exist as
	// valid sub-commands. They may fail at runtime (no store/config), but
	// cobra should not reject them as unknown.
	t.Run("template_list_exists", func(t *testing.T) {
		_, err := executeCommand("template", "list")
		// May fail due to store, but should not be "unknown command".
		if err != nil {
			assert.False(t, strings.Contains(err.Error(), "unknown command"),
				"template list should be a known sub-command")
		}
	})

	t.Run("template_show_exists", func(t *testing.T) {
		// template show requires a <name> arg; without it cobra returns an
		// args-validation error, not "unknown command".
		_, err := executeCommand("template", "show")
		require.Error(t, err, "template show without name should error")
		assert.False(t, strings.Contains(err.Error(), "unknown command"),
			"template show should be a known sub-command")
	})
}

// --- 18. TestCLI_TargetSubcommands ------------------------------------------

func TestCLI_TargetSubcommands(t *testing.T) {
	t.Run("target_list_exists", func(t *testing.T) {
		_, err := executeCommand("target", "list")
		if err != nil {
			assert.False(t, strings.Contains(err.Error(), "unknown command"),
				"target list should be a known sub-command")
		}
	})

	t.Run("target_check_exists", func(t *testing.T) {
		// target check requires a <host> arg.
		_, err := executeCommand("target", "check")
		require.Error(t, err, "target check without host should error")
		assert.False(t, strings.Contains(err.Error(), "unknown command"),
			"target check should be a known sub-command")
	})
}

// --- 19. TestCLI_AuditSubcommands -------------------------------------------

func TestCLI_AuditSubcommands(t *testing.T) {
	t.Run("audit_verify_exists", func(t *testing.T) {
		// audit verify requires a <run-id> arg.
		_, err := executeCommand("audit", "verify")
		require.Error(t, err, "audit verify without run-id should error")
		assert.False(t, strings.Contains(err.Error(), "unknown command"),
			"audit verify should be a known sub-command")
	})

	t.Run("audit_list_exists", func(t *testing.T) {
		// audit list requires a <run-id> arg.
		_, err := executeCommand("audit", "list")
		require.Error(t, err, "audit list without run-id should error")
		assert.False(t, strings.Contains(err.Error(), "unknown command"),
			"audit list should be a known sub-command")
	})
}

// --- 20. TestCLI_SecretSubcommands ------------------------------------------

func TestCLI_SecretSubcommands(t *testing.T) {
	t.Run("secret_list_exists", func(t *testing.T) {
		_, err := executeCommand("secret", "list")
		if err != nil {
			assert.False(t, strings.Contains(err.Error(), "unknown command"),
				"secret list should be a known sub-command")
		}
	})

	t.Run("secret_add_exists", func(t *testing.T) {
		// secret add requires --name and --value flags.
		_, err := executeCommand("secret", "add")
		// Cobra will error because required flags are missing.
		require.Error(t, err, "secret add without required flags should error")
		assert.False(t, strings.Contains(err.Error(), "unknown command"),
			"secret add should be a known sub-command")
	})
}

// --- 21. TestCLI_SystemSubcommands ------------------------------------------

func TestCLI_SystemSubcommands(t *testing.T) {
	t.Run("system_status_exists", func(t *testing.T) {
		_, err := executeCommand("system", "status")
		if err != nil {
			assert.False(t, strings.Contains(err.Error(), "unknown command"),
				"system status should be a known sub-command")
		}
	})

	t.Run("system_doctor_exists", func(t *testing.T) {
		_, err := executeCommand("system", "doctor")
		if err != nil {
			assert.False(t, strings.Contains(err.Error(), "unknown command"),
				"system doctor should be a known sub-command")
		}
	})
}

// --- 22. TestCLI_UserTeamSubcommands ----------------------------------------

func TestCLI_UserTeamSubcommands(t *testing.T) {
	t.Run("user_list_exists", func(t *testing.T) {
		_, err := executeCommand("user", "list")
		if err != nil {
			assert.False(t, strings.Contains(err.Error(), "unknown command"),
				"user list should be a known sub-command")
		}
	})

	t.Run("team_list_exists", func(t *testing.T) {
		_, err := executeCommand("team", "list")
		if err != nil {
			assert.False(t, strings.Contains(err.Error(), "unknown command"),
				"team list should be a known sub-command")
		}
	})
}

// --- 23. TestCLI_CancelRequiresRunID ----------------------------------------

func TestCLI_CancelRequiresRunID(t *testing.T) {
	_, err := executeCommand("cancel")
	require.Error(t, err, "cancel without run-id should error")
}

// --- 24. TestCLI_RetryRequiresRunID -----------------------------------------

func TestCLI_RetryRequiresRunID(t *testing.T) {
	_, err := executeCommand("retry")
	require.Error(t, err, "retry without run-id should error")
}

// --- 25. TestCLI_LinkRequiresArgs -------------------------------------------

func TestCLI_LinkRequiresArgs(t *testing.T) {
	// link requires a <run-id> positional arg.
	_, err := executeCommand("link")
	require.Error(t, err, "link without run-id should error")

	// link also requires the --incident flag (marked required).
	_, err = executeCommand("link", "run-abc123")
	require.Error(t, err, "link without --incident flag should error")
}
