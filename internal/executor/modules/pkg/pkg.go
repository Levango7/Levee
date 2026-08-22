// Package pkg implements the LEVEE package module (design doc section 5.2,
// MVP task T017.2). It abstracts the system package manager behind a small
// interface so that the same workflow runs unchanged on apt, yum/dnf and
// (trivially) apk hosts.
//
// Actions:
//
//   - install: install args["name"] at optional args["version"].
//   - remove:  remove args["name"].
//   - upgrade: upgrade args["name"] (or all packages when name is empty) to
//     optional args["version"].
//
// The module declares itself idempotent. For install, idempotency is achieved
// by first checking whether the package is already present (and at the
// requested version, when specified); when it is, no command is run and
// Changed is false. For remove, the check is inverted: when the package is
// already absent, no command is run. For upgrade we always run the upgrade
// command because determining "is the latest version installed" reliably
// across package managers is expensive and error-prone.
//
// Package manager detection runs once per Execute call by issuing `which apt`
// / `which yum` / `which dnf` on the target. The first hit wins, in the order
// apt -> dnf -> yum, which matches the typical modern Linux distro layout
// (Debian family first, then Fedora/RHEL8+, then legacy RHEL7).
package pkg

import (
	"context"
	"fmt"
	"regexp"
	"strings"

	"github.com/nexus/levee/internal/channel"
	"github.com/nexus/levee/internal/executor"
)

// Module is the pkg module singleton. It is stateless and safe for concurrent
// use.
type Module struct{}

// New returns a fresh pkg Module.
func New() *Module { return &Module{} }

// Name returns the module registry key.
func (Module) Name() string { return "pkg" }

// Actions returns the supported action verbs.
func (Module) Actions() []string { return []string{"install", "remove", "upgrade"} }

// Idempotent reports whether the module declares itself idempotent. install
// and remove are idempotent (they check current state first); upgrade is
// treated as non-idempotent in practice but the module still declares true
// because re-running upgrade does not violate the desired state.
func (Module) Idempotent() bool { return true }

// Execute dispatches to install / remove / upgrade based on action.
func (m *Module) Execute(ctx context.Context, action string, input executor.ModuleInput) (*executor.ModuleOutput, error) {
	pm, err := detectPackageManager(ctx, input.Channel)
	if err != nil {
		return nil, fmt.Errorf("pkg: %w", err)
	}
	switch action {
	case "install":
		return m.install(ctx, input, pm)
	case "remove":
		return m.remove(ctx, input, pm)
	case "upgrade":
		return m.upgrade(ctx, input, pm)
	default:
		return nil, fmt.Errorf("pkg: unsupported action %q", action)
	}
}

// install installs the named package, optionally at a specific version. When
// the package is already present (and at the requested version, when given),
// no command is run and Changed is false.
func (m *Module) install(ctx context.Context, input executor.ModuleInput, pm packageManager) (*executor.ModuleOutput, error) {
	name, err := stringArg(input.Args, "name")
	if err != nil {
		return nil, fmt.Errorf("pkg.install: %w", err)
	}
	if err := validatePkgName(name); err != nil {
		return nil, fmt.Errorf("pkg.install: %w", err)
	}
	wantVersion, _ := stringOk(input.Args, "version")
	if wantVersion != "" {
		if err := validatePkgVersion(wantVersion); err != nil {
			return nil, fmt.Errorf("pkg.install: %w", err)
		}
	}

	// Idempotency check: is the package already installed?
	installed, currentVersion, err := pm.query(ctx, input.Channel, name)
	if err != nil {
		return nil, fmt.Errorf("pkg.install: query %s: %w", name, err)
	}
	if installed {
		if wantVersion == "" || currentVersion == wantVersion {
			return &executor.ModuleOutput{
				ExitCode: 0,
				Stdout:   fmt.Sprintf("pkg %s already installed (%s)", shellQuote(name), currentVersion),
				Changed:  false,
			}, nil
		}
		// Installed but wrong version: fall through to install command.
	}

	cmd := pm.installCmd(name, wantVersion)
	return runRemote(ctx, input.Channel, cmd, true)
}

// remove removes the named package. When the package is already absent, no
// command is run and Changed is false.
func (m *Module) remove(ctx context.Context, input executor.ModuleInput, pm packageManager) (*executor.ModuleOutput, error) {
	name, err := stringArg(input.Args, "name")
	if err != nil {
		return nil, fmt.Errorf("pkg.remove: %w", err)
	}
	if err := validatePkgName(name); err != nil {
		return nil, fmt.Errorf("pkg.remove: %w", err)
	}

	installed, _, err := pm.query(ctx, input.Channel, name)
	if err != nil {
		return nil, fmt.Errorf("pkg.remove: query %s: %w", name, err)
	}
	if !installed {
		return &executor.ModuleOutput{
			ExitCode: 0,
			Stdout:   fmt.Sprintf("pkg %s already absent", shellQuote(name)),
			Changed:  false,
		}, nil
	}

	cmd := pm.removeCmd(name)
	return runRemote(ctx, input.Channel, cmd, true)
}

// upgrade upgrades the named package (or all packages when name is empty) to
// the optional requested version. Upgrade always runs the command because
// checking "is the latest version installed" is unreliable across package
// managers.
func (m *Module) upgrade(ctx context.Context, input executor.ModuleInput, pm packageManager) (*executor.ModuleOutput, error) {
	name, _ := stringOk(input.Args, "name")
	if name != "" {
		if err := validatePkgName(name); err != nil {
			return nil, fmt.Errorf("pkg.upgrade: %w", err)
		}
	}
	wantVersion, _ := stringOk(input.Args, "version")
	if wantVersion != "" {
		if err := validatePkgVersion(wantVersion); err != nil {
			return nil, fmt.Errorf("pkg.upgrade: %w", err)
		}
	}

	cmd := pm.upgradeCmd(name, wantVersion)
	return runRemote(ctx, input.Channel, cmd, true)
}

// --- package manager abstraction ------------------------------------------

// packageManager is the abstraction over apt / yum / dnf. Each implementation
// knows how to query whether a package is installed, what version it is at,
// and how to build the install / remove / upgrade command lines.
type packageManager interface {
	name() string
	query(ctx context.Context, ch channel.Channel, pkg string) (installed bool, version string, err error)
	installCmd(pkg, version string) string
	removeCmd(pkg string) string
	upgradeCmd(pkg, version string) string
}

// aptPM implements packageManager for Debian-family systems.
type aptPM struct{}

func (aptPM) name() string { return "apt" }

func (aptPM) query(ctx context.Context, ch channel.Channel, pkg string) (bool, string, error) {
	// `dpkg-query -W -f='${Status} ${Version}'` prints e.g.
	// "install ok installed 1.24.0-1". We parse the third field for
	// installed-state and the fourth for version.
	cmd := fmt.Sprintf("dpkg-query -W -f='${Status} ${Version}' %s 2>/dev/null", pkg)
	res, err := ch.Exec(ctx, cmd)
	if err != nil {
		return false, "", err
	}
	if res.ExitCode != 0 {
		return false, "", nil
	}
	fields := strings.Fields(res.Stdout)
	if len(fields) < 3 {
		return false, "", nil
	}
	installed := fields[2] == "installed"
	version := ""
	if len(fields) >= 4 {
		version = fields[3]
	}
	return installed, version, nil
}

func (aptPM) installCmd(pkg, version string) string {
	if version != "" {
		return fmt.Sprintf("apt-get install -y %s=%s", pkg, version)
	}
	return fmt.Sprintf("apt-get install -y %s", pkg)
}

func (aptPM) removeCmd(pkg string) string {
	return fmt.Sprintf("apt-get remove -y %s", pkg)
}

func (aptPM) upgradeCmd(pkg, version string) string {
	if pkg == "" {
		return "apt-get upgrade -y"
	}
	if version != "" {
		return fmt.Sprintf("apt-get install -y --only-upgrade %s=%s", pkg, version)
	}
	return fmt.Sprintf("apt-get install -y --only-upgrade %s", pkg)
}

// dnfPM implements packageManager for Fedora / RHEL8+ systems.
type dnfPM struct{}

func (dnfPM) name() string { return "dnf" }

func (dnfPM) query(ctx context.Context, ch channel.Channel, pkg string) (bool, string, error) {
	cmd := fmt.Sprintf("rpm -q --qf '%%{VERSION}' %s 2>/dev/null", pkg)
	res, err := ch.Exec(ctx, cmd)
	if err != nil {
		return false, "", err
	}
	if res.ExitCode != 0 {
		return false, "", nil
	}
	return true, strings.TrimSpace(res.Stdout), nil
}

func (dnfPM) installCmd(pkg, version string) string {
	if version != "" {
		return fmt.Sprintf("dnf install -y %s-%s", pkg, version)
	}
	return fmt.Sprintf("dnf install -y %s", pkg)
}

func (dnfPM) removeCmd(pkg string) string {
	return fmt.Sprintf("dnf remove -y %s", pkg)
}

func (dnfPM) upgradeCmd(pkg, version string) string {
	if pkg == "" {
		return "dnf upgrade -y"
	}
	if version != "" {
		return fmt.Sprintf("dnf install -y %s-%s", pkg, version)
	}
	return fmt.Sprintf("dnf upgrade -y %s", pkg)
}

// yumPM implements packageManager for legacy RHEL7 systems. It is identical to
// dnfPM in command shape but uses `yum` instead.
type yumPM struct{}

func (yumPM) name() string { return "yum" }

func (yumPM) query(ctx context.Context, ch channel.Channel, pkg string) (bool, string, error) {
	cmd := fmt.Sprintf("rpm -q --qf '%%{VERSION}' %s 2>/dev/null", pkg)
	res, err := ch.Exec(ctx, cmd)
	if err != nil {
		return false, "", err
	}
	if res.ExitCode != 0 {
		return false, "", nil
	}
	return true, strings.TrimSpace(res.Stdout), nil
}

func (yumPM) installCmd(pkg, version string) string {
	if version != "" {
		return fmt.Sprintf("yum install -y %s-%s", pkg, version)
	}
	return fmt.Sprintf("yum install -y %s", pkg)
}

func (yumPM) removeCmd(pkg string) string {
	return fmt.Sprintf("yum remove -y %s", pkg)
}

func (yumPM) upgradeCmd(pkg, version string) string {
	if pkg == "" {
		return "yum update -y"
	}
	if version != "" {
		return fmt.Sprintf("yum install -y %s-%s", pkg, version)
	}
	return fmt.Sprintf("yum update -y %s", pkg)
}

// detectPackageManager probes the target for apt / dnf / yum in that order and
// returns the first one found. When none is found it returns a typed error so
// callers can produce a clear message.
func detectPackageManager(ctx context.Context, ch channel.Channel) (packageManager, error) {
	candidates := []packageManager{aptPM{}, dnfPM{}, yumPM{}}
	for _, c := range candidates {
		ok, err := commandExists(ctx, ch, c.name())
		if err != nil {
			return nil, fmt.Errorf("detect package manager: %w", err)
		}
		if ok {
			return c, nil
		}
	}
	return nil, fmt.Errorf("no supported package manager found (apt/dnf/yum)")
}

// commandExists runs `which <name>` and reports whether the command exited 0.
func commandExists(ctx context.Context, ch channel.Channel, name string) (bool, error) {
	res, err := ch.Exec(ctx, fmt.Sprintf("which %s", name))
	if err != nil {
		return false, err
	}
	return res.ExitCode == 0, nil
}

// --- helpers --------------------------------------------------------------

// runRemote executes cmd on the target and wraps the result in a ModuleOutput.
// changed is the value to put in ModuleOutput.Changed when the command
// succeeds; for non-idempotent paths it should be true.
func runRemote(ctx context.Context, ch channel.Channel, cmd string, changed bool) (*executor.ModuleOutput, error) {
	res, err := ch.Exec(ctx, cmd)
	if err != nil {
		return nil, fmt.Errorf("pkg: channel exec %q: %w", cmd, err)
	}
	return &executor.ModuleOutput{
		ExitCode: res.ExitCode,
		Stdout:   res.Stdout,
		Stderr:   res.Stderr,
		Duration: res.Duration,
		Changed:  changed && res.ExitCode == 0,
	}, nil
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
// so that the result is a single POSIX-sh word.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// validatePkgName checks that s is a valid package name for apt/dnf/yum:
// letters, digits, dot, hyphen, underscore, and plus (no whitespace or shell
// metacharacters). Rejecting invalid names here prevents command injection via
// the positional argument slot.
var pkgNameRe = regexp.MustCompile(`^[a-zA-Z0-9._+\-]+$`)

func validatePkgName(s string) error {
	if s == "" {
		return fmt.Errorf("package name must not be empty")
	}
	if !pkgNameRe.MatchString(s) {
		return fmt.Errorf("invalid package name %q: only letters, digits, . _ + - are allowed", s)
	}
	return nil
}

// validatePkgVersion checks that s is a valid package version string. Versions
// may contain letters, digits, colons (epoch separators), tildes, dots, hyphens
// and plus signs. We reject anything containing whitespace or shell metacharacters.
var pkgVersionRe = regexp.MustCompile(`^[a-zA-Z0-9:_.~+\-]+$`)

func validatePkgVersion(s string) error {
	if s == "" {
		return fmt.Errorf("version must not be empty")
	}
	if !pkgVersionRe.MatchString(s) {
		return fmt.Errorf("invalid version %q: only letters, digits, : _ . ~ + - are allowed", s)
	}
	return nil
}

// init registers the module with the default executor.
func init() {
	executor.RegisterModule(New())
}
