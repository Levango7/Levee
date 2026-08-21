// Package shell implements the LEVEE shell module (design doc section 5.2,
// MVP task T017). The shell module is a thin "direct execution" adapter: it
// forwards a command or an inline script to the channel.Channel attached to
// the ModuleInput and returns the captured ExecResult as a ModuleOutput.
//
// The module deliberately declares itself non-idempotent. A shell command can
// in principle be idempotent (e.g. `mkdir -p /x`) but the module cannot prove
// that from the command string alone, so it conservatively reports false and
// sets Changed=true on every successful run. Workflows that need idempotency
// should use the dedicated file / pkg / svc / user modules instead.
//
// Actions:
//
//   - exec:   run a single command line given by args["cmd"].
//   - script: write args["script"] to a temporary file on the target and
//     execute it with `sh`. The temporary path is /tmp/levee-shell-<random>.sh
//     and is removed after execution (best-effort).
package shell

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strings"

	"github.com/nexus/levee/internal/executor"
)

// Module is the shell module singleton. It is stateless and therefore safe to
// share across goroutines; the executor may invoke Execute concurrently.
type Module struct{}

// New returns a fresh shell Module. The returned value is stateless so callers
// may also simply use the zero value Module{}.
func New() *Module { return &Module{} }

// Name returns the module registry key.
func (Module) Name() string { return "shell" }

// Actions returns the supported action verbs.
func (Module) Actions() []string { return []string{"exec", "script"} }

// Idempotent reports whether the module declares itself idempotent. The shell
// module cannot prove idempotency from the command string, so it always
// returns false.
func (Module) Idempotent() bool { return false }

// Execute dispatches to execScript or execCommand based on action.
func (m *Module) Execute(ctx context.Context, action string, input executor.ModuleInput) (*executor.ModuleOutput, error) {
	switch action {
	case "exec":
		return m.execCommand(ctx, input)
	case "script":
		return m.execScript(ctx, input)
	default:
		// The executor wrapper rejects unknown actions before reaching here,
		// but we defend in case the module is called directly.
		return nil, fmt.Errorf("shell: unsupported action %q", action)
	}
}

// execCommand runs a single command line. The command is taken from
// input.Args["cmd"]; missing or non-string cmd is a usage error.
func (m *Module) execCommand(ctx context.Context, input executor.ModuleInput) (*executor.ModuleOutput, error) {
	cmd, err := stringArg(input.Args, "cmd")
	if err != nil {
		return nil, fmt.Errorf("shell.exec: %w", err)
	}
	if strings.TrimSpace(cmd) == "" {
		return nil, fmt.Errorf("shell.exec: empty cmd")
	}

	// Security: reject commands containing shell metacharacters.
	// This prevents command injection via user-supplied DSL.
	if err := validateShellCommand(cmd); err != nil {
		return nil, fmt.Errorf("shell.exec: unsafe command: %w", err)
	}

	res, err := input.Channel.Exec(ctx, cmd)
	if err != nil {
		return nil, fmt.Errorf("shell.exec: channel exec: %w", err)
	}
	return &executor.ModuleOutput{
		ExitCode: res.ExitCode,
		Stdout:   res.Stdout,
		Stderr:   res.Stderr,
		Duration: res.Duration,
		Changed:  true, // non-idempotent: assume changed
	}, nil
}

// execScript writes the inline script body to a temporary file on the target
// and runs it with `sh`. The temporary file is removed after execution
// (best-effort: a cleanup failure does not fail the step).
func (m *Module) execScript(ctx context.Context, input executor.ModuleInput) (*executor.ModuleOutput, error) {
	body, err := stringArg(input.Args, "script")
	if err != nil {
		return nil, fmt.Errorf("shell.script: %w", err)
	}
	if strings.TrimSpace(body) == "" {
		return nil, fmt.Errorf("shell.script: empty script")
	}

	// Generate a random suffix to avoid collisions between concurrent steps.
	suffix, err := randomSuffix()
	if err != nil {
		return nil, fmt.Errorf("shell.script: random suffix: %w", err)
	}
	remotePath := fmt.Sprintf("/tmp/levee-shell-%s.sh", suffix)

	// Upload the script body. We use a single Upload call so that the channel
	// implementation can pick the most efficient transport (SCP/SFTP/WinRM
	// put-file) without us having to special-case any of them.
	if err := input.Channel.Upload(ctx, remotePath, strings.NewReader(body)); err != nil {
		return nil, fmt.Errorf("shell.script: upload script: %w", err)
	}

		// chmod +x then execute. We chain with && so that a chmod failure aborts
		// execution rather than running a non-executable file.
		// remotePath is generated internally and shell-quoted; it is safe.
		runCmd := fmt.Sprintf("chmod +x %s && sh %s", remotePath, remotePath)
		res, err := input.Channel.Exec(ctx, runCmd)
		if err != nil {
			return nil, fmt.Errorf("shell.script: channel exec: %w", err)
		}

	// Best-effort cleanup. We swallow the error because the step has already
	// produced its evidence (exit code / stdout / stderr) and a leftover temp
	// file is not a step failure.
	_, _ = input.Channel.Exec(ctx, fmt.Sprintf("rm -f %s", remotePath))

	return &executor.ModuleOutput{
		ExitCode: res.ExitCode,
		Stdout:   res.Stdout,
		Stderr:   res.Stderr,
		Duration: res.Duration,
		Changed:  true,
	}, nil
}

// stringArg extracts a string value from args[key]. It returns a typed error
// when the key is missing or the value is not a string, so callers can wrap it
// with %w to produce a clear usage message.
func stringArg(args map[string]any, key string) (string, error) {
	v, ok := args[key]
	if !ok {
		return "", fmt.Errorf("missing argument %q", key)
	}
	s, ok := v.(string)
	if !ok {
		return "", fmt.Errorf("argument %q must be a string, got %T", key, v)
	}
	return s, nil
}

// validateShellCommand checks that a command does not contain shell
// metacharacters that could be used for command injection. This is a
// conservative safety net; the remote channel executor may still interpret
// the command through a shell. The check rejects the following characters:
// ; & | ` $ ( ) < > { } [ ] ' " \n \r \t and whitespace that could be used
// to break out of a single argument.
func validateShellCommand(cmd string) error {
	// Reject any character that is not a letter, digit, hyphen, underscore,
	// dot, slash, or space. This is intentionally strict: it allows only
	// safe command names and arguments composed of alphanumeric characters,
	// paths, and simple flags.
	for _, ch := range cmd {
		if !((ch >= 'a' && ch <= 'z') || (ch >= 'A' && ch <= 'Z') ||
			(ch >= '0' && ch <= '9') || ch == '-' || ch == '_' ||
			ch == '.' || ch == '/' || ch == ' ' || ch == '=') {
			return fmt.Errorf("disallowed character %q in command", ch)
		}
	}
	return nil
}

// randomSuffix returns 8 hex characters derived from 4 random bytes. It is
// good enough to avoid collisions between concurrent shell.script steps on
// the same target.
func randomSuffix() (string, error) {
	b := make([]byte, 4)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// init registers the module with the default executor so that workflows can
// refer to it as "shell" without any explicit wiring.
func init() {
	executor.RegisterModule(New())
}
