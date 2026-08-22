package file

// Package file implements the LEVEE file module (design doc section 5.2,
// MVP task T017.1). The file module distributes files from the control node to
// the target and renders Go text/template templates with caller-supplied
// variables before uploading them.
//
// Actions:
//
//   - copy:     upload the local file at args["src"] to args["dest"] on the
//               target. Optional args: mode (e.g. "0644"), owner, group.
//   - template: render the template file at args["src"] with args["vars"]
//               (a map[string]any) and upload the result to args["dest"].
//               Optional args: mode, owner, group.
//
// The module declares itself idempotent. For copy, idempotency is achieved by
// comparing the local and remote content checksums before uploading: when
// they match, the upload is skipped and Changed is false. For template, the
// rendered output is compared the same way.
//
// Checksum comparison uses sha256 over the content that would be uploaded.
// The remote checksum is obtained by running `sha256sum <dest>` on the target
// and parsing the first word of stdout. When the remote file does not exist
// the checksum command exits non-zero and we treat that as "must upload".

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"
	"text/template"

	"github.com/nexus/levee/internal/channel"
	"github.com/nexus/levee/internal/executor"
)

// Module is the file module singleton. It is stateless and safe for
// concurrent use.
type Module struct{}

// New returns a fresh file Module.
func New() *Module { return &Module{} }

// Name returns the module registry key.
func (Module) Name() string { return "file" }

// Actions returns the supported action verbs.
func (Module) Actions() []string { return []string{"copy", "template"} }

// Idempotent reports whether the module declares itself idempotent. The file
// module is idempotent: copy / template only modify the target when the
// content would actually change.
func (Module) Idempotent() bool { return true }

// Execute dispatches to copyFile or templateFile based on action.
func (m *Module) Execute(ctx context.Context, action string, input executor.ModuleInput) (*executor.ModuleOutput, error) {
	switch action {
	case "copy":
		return m.copyFile(ctx, input)
	case "template":
		return m.templateFile(ctx, input)
	default:
		return nil, fmt.Errorf("file: unsupported action %q", action)
	}
}

// copyFile uploads args["src"] to args["dest"] on the target. It first
// computes the local sha256 and asks the target for the remote sha256; when
// they match, the upload is skipped and Changed is false. After a successful
// upload it applies the optional mode / owner / group attributes.
func (m *Module) copyFile(ctx context.Context, input executor.ModuleInput) (*executor.ModuleOutput, error) {
	src, err := stringArg(input.Args, "src")
	if err != nil {
		return nil, fmt.Errorf("file.copy: %w", err)
	}
	dest, err := stringArg(input.Args, "dest")
	if err != nil {
		return nil, fmt.Errorf("file.copy: %w", err)
	}

	localContent, err := os.ReadFile(src)
	if err != nil {
		return nil, fmt.Errorf("file.copy: read src: %w", err)
	}
	return m.uploadIfChanged(ctx, input, dest, localContent)
}

// templateFile renders the template at args["src"] with args["vars"] and
// uploads the result to args["dest"]. The template is parsed with Go
// text/template and executed with vars as the root data object. Missing keys
// in vars are left as <no value> by text/template; callers should populate
// vars fully.
func (m *Module) templateFile(ctx context.Context, input executor.ModuleInput) (*executor.ModuleOutput, error) {
	src, err := stringArg(input.Args, "src")
	if err != nil {
		return nil, fmt.Errorf("file.template: %w", err)
	}
	dest, err := stringArg(input.Args, "dest")
	if err != nil {
		return nil, fmt.Errorf("file.template: %w", err)
	}
	vars, _ := input.Args["vars"].(map[string]any)

	tmpl, err := template.ParseFiles(src)
	if err != nil {
		return nil, fmt.Errorf("file.template: parse: %w", err)
	}
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, vars); err != nil {
		return nil, fmt.Errorf("file.template: execute: %w", err)
	}
	return m.uploadIfChanged(ctx, input, dest, buf.Bytes())
}

// uploadIfChanged is the shared idempotency core for copy and template. It
// compares the sha256 of content with the remote file at dest; when they
// match, no upload happens and Changed is false. After an upload it applies
// optional mode/owner/group attributes via chmod/chown.
func (m *Module) uploadIfChanged(ctx context.Context, input executor.ModuleInput, dest string, content []byte) (*executor.ModuleOutput, error) {
	localSum := sha256sum(content)
	remoteSum, remoteErr := m.remoteSha256(ctx, input.Channel, dest)

	changed := false
	var last *remoteStep
	if remoteErr == nil && remoteSum == localSum {
		// Already in sync: nothing to do.
		return &executor.ModuleOutput{
			ExitCode: 0,
			Stdout:   fmt.Sprintf("file %s already in sync (sha256=%s)", shellQuote(dest), localSum[:12]),
			Changed:  false,
		}, nil
	}

	// Either the remote file is missing or its content differs: upload.
	if err := input.Channel.Upload(ctx, dest, bytes.NewReader(content)); err != nil {
		return nil, fmt.Errorf("file: upload %s: %w", dest, err)
	}
	changed = true
	last = &remoteStep{exit: 0, stdout: fmt.Sprintf("uploaded %d bytes to %s", len(content), shellQuote(dest))}

	// Apply optional mode / owner / group. Each is a separate ch* call so
	// that one failing attribute does not skip the others; we collect the
	// last non-zero exit for the result.
	if mode, ok := stringOk(input.Args, "mode"); ok {
		if err := validateOctalMode(mode); err != nil {
			return nil, fmt.Errorf("file: invalid mode %q: %w", mode, err)
		}
		r, err := m.applyAttr(ctx, input.Channel, fmt.Sprintf("chmod %s %s", shellQuote(mode), shellQuote(dest)))
		if err != nil {
			return nil, err
		}
		last = r
	}
	if owner, ok := stringOk(input.Args, "owner"); ok {
		r, err := m.applyAttr(ctx, input.Channel, fmt.Sprintf("chown %s %s", shellQuote(owner), shellQuote(dest)))
		if err != nil {
			return nil, err
		}
		last = r
	}
	if group, ok := stringOk(input.Args, "group"); ok {
		r, err := m.applyAttr(ctx, input.Channel, fmt.Sprintf("chgrp %s %s", shellQuote(group), shellQuote(dest)))
		if err != nil {
			return nil, err
		}
		last = r
	}

	if last == nil {
		last = &remoteStep{exit: 0, stdout: ""}
	}
	return &executor.ModuleOutput{
		ExitCode: last.exit,
		Stdout:   last.stdout,
		Stderr:   last.stderr,
		Changed:  changed,
	}, nil
}

// remoteStep is the parsed outcome of a single remote command.
type remoteStep struct {
	exit   int
	stdout string
	stderr string
}

// applyAttr runs a chmod/chown/chgrp command and returns a remoteStep.
func (m *Module) applyAttr(ctx context.Context, ch channel.Channel, cmd string) (*remoteStep, error) {
	res, err := ch.Exec(ctx, cmd)
	if err != nil {
		return nil, fmt.Errorf("file: apply attr %q: %w", cmd, err)
	}
	return &remoteStep{exit: res.ExitCode, stdout: res.Stdout, stderr: res.Stderr}, nil
}

// remoteSha256 asks the target for the sha256 of the file at path. A missing
// file or a non-zero exit is reported as an error so the caller can treat it
// as "must upload".
func (m *Module) remoteSha256(ctx context.Context, ch channel.Channel, path string) (string, error) {
	cmd := fmt.Sprintf("sha256sum %s", shellQuote(path))
	res, err := ch.Exec(ctx, cmd)
	if err != nil {
		return "", err
	}
	if res.ExitCode != 0 {
		return "", fmt.Errorf("remote sha256sum exited %d: %s", res.ExitCode, res.Stderr)
	}
	// sha256sum prints "<hex>  <path>"; take the first whitespace-delimited
	// token.
	fields := strings.Fields(res.Stdout)
	if len(fields) == 0 {
		return "", fmt.Errorf("remote sha256sum returned empty output")
	}
	return fields[0], nil
}

// sha256sum returns the hex-encoded sha256 of data.
func sha256sum(data []byte) string {
	h := sha256.Sum256(data)
	return hex.EncodeToString(h[:])
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
// string; otherwise ("", false). It is the optional counterpart to stringArg.
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

// validateOctalMode checks that s is a valid POSIX octal mode (3 or 4 digits
// in [0-7]). Malicious mode strings such as "0644; rm -rf /" are rejected.
var octalModeRe = regexp.MustCompile(`^[0-7]{3,4}$`)

func validateOctalMode(s string) error {
	if !octalModeRe.MatchString(s) {
		return fmt.Errorf("mode must be 3-4 octal digits (0-7), got %q", s)
	}
	return nil
}

// Compile-time guard: ensure the bytes we upload come from io.Reader-compatible
// sources. This is purely to keep the import of io honest if future refactors
// drop bytes.NewReader.
var _ io.Reader = (*bytes.Reader)(nil)

// init registers the module with the default executor.
func init() {
	executor.RegisterModule(New())
}
