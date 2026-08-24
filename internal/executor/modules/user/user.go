// Package user implements the LEVEE user module (design doc section 5.2,
// MVP task T017.4). It manages local OS users, their attributes and SSH
// authorized_keys on the target host.
//
// Actions:
//
//   - add:    create a local user args["name"] with optional uid / shell /
//     home / groups. When args["ssh_key"] is provided the key is
//     appended to ~name/.ssh/authorized_keys.
//   - remove: remove the user and its home directory (`userdel -r`).
//   - modify: modify an existing user's attributes (shell / home / group /
//     uid). When args["ssh_key"] is provided the key is (re)written
//     to authorized_keys.
//
// The module declares itself idempotent. For add, idempotency is achieved by
// first checking whether the user already exists; when it does, no useradd
// command is run (but ssh_key is still reconciled). For remove, the check is
// inverted: when the user is already absent, no command is run. For modify we
// always run usermod because diffing individual attributes across distros is
// fragile.
//
// SSH key distribution uses a small shell snippet that ensures ~/.ssh exists,
// appends the key (deduplicated by the key body) and sets correct ownership
// and mode. The snippet is sent as a single Exec call so that the channel
// implementation can run it atomically on the target.
package user

import (
	"context"
	crand "crypto/rand"
	"encoding/hex"
	"fmt"
	"regexp"
	"strings"

	"github.com/nexus/levee/internal/channel"
	"github.com/nexus/levee/internal/executor"
)

// Module is the user module singleton. It is stateless and safe for concurrent
// use.
type Module struct{}

// New returns a fresh user Module.
func New() *Module { return &Module{} }

// Name returns the module registry key.
func (Module) Name() string { return "user" }

// Actions returns the supported action verbs.
func (Module) Actions() []string { return []string{"add", "remove", "modify"} }

// Idempotent reports whether the module declares itself idempotent. add and
// remove are idempotent (they check current state first); modify is treated
// as idempotent in the declarative sense.
func (Module) Idempotent() bool { return true }

// Execute dispatches to add / remove / modify based on action.
func (m *Module) Execute(ctx context.Context, action string, input executor.ModuleInput) (*executor.ModuleOutput, error) {
	switch action {
	case "add":
		return m.add(ctx, input)
	case "remove":
		return m.remove(ctx, input)
	case "modify":
		return m.modify(ctx, input)
	default:
		return nil, fmt.Errorf("user: unsupported action %q", action)
	}
}

// add creates a local user. Idempotent: when the user already exists, useradd
// is skipped (but ssh_key is still reconciled). Returns Changed=true when
// either the user was created or the ssh key was added.
func (m *Module) add(ctx context.Context, input executor.ModuleInput) (*executor.ModuleOutput, error) {
	name, err := stringArg(input.Args, "name")
	if err != nil {
		return nil, fmt.Errorf("user.add: %w", err)
	}
	if err := validateUserName(name); err != nil {
		return nil, fmt.Errorf("user.add: %w", err)
	}

	changed := false
	var last *remoteStep

	exists, err := userExists(ctx, input.Channel, name)
	if err != nil {
		return nil, fmt.Errorf("user.add: check %s: %w", name, err)
	}
	if !exists {
		cmd := buildUseraddCmd(input.Args)
		r, err := runRemoteStep(ctx, input.Channel, cmd)
		if err != nil {
			return nil, fmt.Errorf("user.add: useradd: %w", err)
		}
		last = r
		changed = true
	}

	// Optional password. The credential is delivered via a temporary file
	// uploaded over the channel (SFTP/SCP) rather than embedded in the
	// command string, so the plaintext never appears in process argv,
	// sshd forced-command logs or the executor's audit trail. The file is
	// removed in the same Exec invocation regardless of chpasswd's exit
	// status.
	if pw, ok := stringOk(input.Args, "password"); ok {
		r, err := setPassword(ctx, input.Channel, name, pw)
		if err != nil {
			return nil, fmt.Errorf("user.add: chpasswd: %w", err)
		}
		last = r
		changed = true
	}

	// Optional SSH public key.
	if key, ok := stringOk(input.Args, "ssh_key"); ok {
		r, err := m.installSSHKey(ctx, input.Channel, name, key)
		if err != nil {
			return nil, fmt.Errorf("user.add: ssh key: %w", err)
		}
		if r != nil {
			last = r
			if r.exit == 0 {
				changed = true
			}
		}
	}

	if last == nil {
		last = &remoteStep{exit: 0, stdout: fmt.Sprintf("user %s already exists", shellQuote(name))}
	}
	return &executor.ModuleOutput{
		ExitCode: last.exit,
		Stdout:   last.stdout,
		Stderr:   last.stderr,
		Changed:  changed,
	}, nil
}

// remove deletes the user and its home directory. Idempotent: when the user
// is already absent, no command is run and Changed is false.
func (m *Module) remove(ctx context.Context, input executor.ModuleInput) (*executor.ModuleOutput, error) {
	name, err := stringArg(input.Args, "name")
	if err != nil {
		return nil, fmt.Errorf("user.remove: %w", err)
	}
	if err := validateUserName(name); err != nil {
		return nil, fmt.Errorf("user.remove: %w", err)
	}

	exists, err := userExists(ctx, input.Channel, name)
	if err != nil {
		return nil, fmt.Errorf("user.remove: check %s: %w", name, err)
	}
	if !exists {
		return &executor.ModuleOutput{
			ExitCode: 0,
			Stdout:   fmt.Sprintf("user %s already absent", shellQuote(name)),
			Changed:  false,
		}, nil
	}
	return runRemote(ctx, input.Channel, fmt.Sprintf("userdel -r %s", shellQuote(name)), true)
}

// modify changes an existing user's attributes. Always runs usermod because
// diffing individual attributes across distros is fragile. When ssh_key is
// provided it is (re)installed.
func (m *Module) modify(ctx context.Context, input executor.ModuleInput) (*executor.ModuleOutput, error) {
	name, err := stringArg(input.Args, "name")
	if err != nil {
		return nil, fmt.Errorf("user.modify: %w", err)
	}
	if err := validateUserName(name); err != nil {
		return nil, fmt.Errorf("user.modify: %w", err)
	}

	cmd := buildUsermodCmd(input.Args)
	out, err := runRemote(ctx, input.Channel, cmd, true)
	if err != nil {
		return nil, fmt.Errorf("user.modify: usermod: %w", err)
	}

	if key, ok := stringOk(input.Args, "ssh_key"); ok {
		r, err := m.installSSHKey(ctx, input.Channel, name, key)
		if err != nil {
			return nil, fmt.Errorf("user.modify: ssh key: %w", err)
		}
		if r != nil {
			out.ExitCode = r.exit
			out.Stdout = r.stdout
			out.Stderr = r.stderr
		}
	}
	return out, nil
}

// installSSHKey appends the given public key to ~name/.ssh/authorized_keys,
// deduplicating by the key body. The snippet ensures ~/.ssh exists with the
// right mode and ownership. Returns a remoteStep describing the outcome; a
// nil result means "no work done" (key already present).
func (m *Module) installSSHKey(ctx context.Context, ch channel.Channel, name, key string) (*remoteStep, error) {
	// Trim trailing whitespace for stable comparison and storage.
	key = strings.TrimSpace(key)
	if key == "" {
		return nil, nil
	}
	// We use a small idempotent shell snippet: create ~/.ssh, append the key
	// only when it is not already present, fix ownership and mode.
	//
	// The snippet is intentionally POSIX-sh compatible so it works on both
	// Debian and RHEL family minimal images (which may not have bash).
	snippet := fmt.Sprintf(
		`set -e
home=$(getent passwd %s | cut -d: -f6)
[ -n "$home" ] || { echo "user %s has no home" >&2; exit 1; }
mkdir -p "$home/.ssh"
chmod 700 "$home/.ssh"
touch "$home/.ssh/authorized_keys"
chmod 600 "$home/.ssh/authorized_keys"
if ! grep -qF -- %s "$home/.ssh/authorized_keys"; then
  printf '%%s\n' %s >> "$home/.ssh/authorized_keys"
fi
chown -R %s "$home/.ssh"`,
		shellQuote(name), shellQuote(name),
		shellQuote(key), shellQuote(key),
		shellQuote(name),
	)
	return runRemoteStep(ctx, ch, snippet)
}

// --- helpers --------------------------------------------------------------

// remoteStep is the parsed outcome of a single remote command.
type remoteStep struct {
	exit   int
	stdout string
	stderr string
}

// runRemoteStep runs cmd and returns a remoteStep.
func runRemoteStep(ctx context.Context, ch channel.Channel, cmd string) (*remoteStep, error) {
	res, err := ch.Exec(ctx, cmd)
	if err != nil {
		return nil, err
	}
	return &remoteStep{exit: res.ExitCode, stdout: res.Stdout, stderr: res.Stderr}, nil
}

// setPassword sets name's password by uploading "name:password\n" to a
// randomly-named temp file over the channel's file-transfer path and running
// `chpasswd < file`. The plaintext credential therefore never travels inside
// a command string. The same Exec removes the file before exiting so no
// credential material is left on disk, even when chpasswd fails.
func setPassword(ctx context.Context, ch channel.Channel, name, password string) (*remoteStep, error) {
	var suffix [4]byte
	if _, err := crand.Read(suffix[:]); err != nil {
		return nil, fmt.Errorf("generate temp suffix: %w", err)
	}
	tmpPath := fmt.Sprintf("/tmp/.levee-chpasswd-%s", hex.EncodeToString(suffix[:]))

	content := fmt.Sprintf("%s:%s\n", name, password)
	if err := ch.Upload(ctx, tmpPath, strings.NewReader(content)); err != nil {
		return nil, fmt.Errorf("upload credentials: %w", err)
	}

	cmd := fmt.Sprintf("chpasswd < %s; rc=$?; rm -f %s; exit $rc", shellQuote(tmpPath), shellQuote(tmpPath))
	return runRemoteStep(ctx, ch, cmd)
}

// runRemote runs cmd and returns a ModuleOutput.
func runRemote(ctx context.Context, ch channel.Channel, cmd string, changed bool) (*executor.ModuleOutput, error) {
	res, err := ch.Exec(ctx, cmd)
	if err != nil {
		return nil, fmt.Errorf("channel exec %q: %w", cmd, err)
	}
	return &executor.ModuleOutput{
		ExitCode: res.ExitCode,
		Stdout:   res.Stdout,
		Stderr:   res.Stderr,
		Duration: res.Duration,
		Changed:  changed && res.ExitCode == 0,
	}, nil
}

// userExists reports whether the named user exists on the target. Uses `id -u
// <name>` which is universally available on Linux.
func userExists(ctx context.Context, ch channel.Channel, name string) (bool, error) {
	res, err := ch.Exec(ctx, fmt.Sprintf("id -u %s", shellQuote(name)))
	if err != nil {
		return false, err
	}
	return res.ExitCode == 0, nil
}

// buildUseraddCmd builds a useradd command line from the args map. Supported
// optional keys: uid (int or numeric string), shell, home, groups (comma-
// separated string or []string). All interpolated values are shell-quoted:
// they originate in workflow DSL input and are executed by a remote shell.
func buildUseraddCmd(args map[string]any) string {
	name, _ := stringOk(args, "name")
	parts := []string{"useradd"}
	if v, ok := args["uid"]; ok {
		if s := toIntString(v); s != "" {
			parts = append(parts, "-u", shellQuote(s))
		}
	}
	if s, ok := stringOk(args, "shell"); ok {
		parts = append(parts, "-s", shellQuote(s))
	}
	if s, ok := stringOk(args, "home"); ok {
		parts = append(parts, "-d", shellQuote(s))
	}
	if s, ok := stringOk(args, "groups"); ok {
		parts = append(parts, "-G", shellQuote(s))
	} else if gs, ok := args["groups"].([]string); ok && len(gs) > 0 {
		parts = append(parts, "-G", shellQuote(strings.Join(gs, ",")))
	}
	parts = append(parts, shellQuote(name))
	return strings.Join(parts, " ")
}

// buildUsermodCmd builds a usermod command line from the args map. Same
// optional keys as useradd; missing keys are simply skipped. All
// interpolated values are shell-quoted for the same reason.
func buildUsermodCmd(args map[string]any) string {
	name, _ := stringOk(args, "name")
	parts := []string{"usermod"}
	if v, ok := args["uid"]; ok {
		if s := toIntString(v); s != "" {
			parts = append(parts, "-u", shellQuote(s))
		}
	}
	if s, ok := stringOk(args, "shell"); ok {
		parts = append(parts, "-s", shellQuote(s))
	}
	if s, ok := stringOk(args, "home"); ok {
		parts = append(parts, "-d", shellQuote(s))
	}
	if s, ok := stringOk(args, "group"); ok {
		parts = append(parts, "-g", shellQuote(s))
	}
	if s, ok := stringOk(args, "groups"); ok {
		parts = append(parts, "-G", shellQuote(s))
	}
	parts = append(parts, shellQuote(name))
	return strings.Join(parts, " ")
}

// toIntString converts an int, int64, float64 or numeric string to its string
// representation. Returns "" for unrecognised inputs.
func toIntString(v any) string {
	switch x := v.(type) {
	case int:
		return fmt.Sprintf("%d", x)
	case int64:
		return fmt.Sprintf("%d", x)
	case float64:
		return fmt.Sprintf("%d", int64(x))
	case string:
		return x
	default:
		return ""
	}
}

// stringArg extracts a required string from args[key].
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

// stringOk returns the string value of args[key] and true when present and a
// string; otherwise ("", false).
func stringOk(args map[string]any, key string) (string, bool) {
	v, ok := args[key]
	if !ok {
		return "", false
	}
	s, ok := v.(string)
	if !ok {
		return "", false
	}
	return s, true
}

// shellQuote wraps s in single quotes and escapes any embedded single quotes
// so that the result is a single POSIX-sh word. This is the standard
// '...'\”...' idiom.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// validateUserName checks that s is a valid POSIX username. User names must
// not contain whitespace or shell metacharacters; we restrict to the set
// allowed by Linux useradd (letters, digits, underscore, hyphen, dot).
var userNameRe = regexp.MustCompile(`^[a-zA-Z0-9._\-]+$`)

func validateUserName(s string) error {
	if s == "" {
		return fmt.Errorf("user name must not be empty")
	}
	if !userNameRe.MatchString(s) {
		return fmt.Errorf("invalid user name %q: only letters, digits, _ . - are allowed", s)
	}
	return nil
}

// init registers the module with the default executor.
func init() {
	executor.RegisterModule(New())
}
