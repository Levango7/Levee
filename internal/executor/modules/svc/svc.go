// Package svc implements the LEVEE service module (design doc section 5.2,
// MVP task T017.3). It abstracts the init system behind a small interface so
// that the same workflow runs unChanged on systemd and sysvinit hosts.
//
// Actions:
//
//   - start:   start the service args["name"].
//   - stop:    stop the service.
//   - restart: restart the service.
//   - reload:  reload the service configuration without restarting the
//     process. On sysvinit, where reload is not universally
//     supported, the module falls back to restart when the reload
//     command exits non-zero.
//   - enable:  enable the service to start at boot.
//   - disable: disable the service from starting at boot.
//
// The module declares itself idempotent. For start and enable, idempotency is
// achieved by first checking the current state and skipping the command when
// the desired state is already present. For stop, restart, reload, disable we
// always run the command because the resulting state is not easily queryable
// across init systems (and restart / reload are inherently mutating).
//
// Init system detection runs once per Execute call by issuing `which
// systemctl` on the target. When systemctl is present we use systemd;
// otherwise we fall back to sysvinit (`service <name> <action>`).
package svc

import (
	"context"
	"fmt"
	"strings"

	"github.com/nexus/levee/internal/channel"
	"github.com/nexus/levee/internal/executor"
)

// Module is the svc module singleton. It is stateless and safe for concurrent
// use.
type Module struct{}

// New returns a fresh svc Module.
func New() *Module { return &Module{} }

// Name returns the module registry key.
func (Module) Name() string { return "svc" }

// Actions returns the supported action verbs.
func (Module) Actions() []string {
	return []string{"start", "stop", "restart", "enable", "disable", "reload"}
}

// Idempotent reports whether the module declares itself idempotent. start and
// enable are idempotent (they check current state first); the others are
// treated as idempotent in the declarative sense (re-running them does not
// violate the desired state).
func (Module) Idempotent() bool { return true }

// Execute dispatches to the per-action handler.
func (m *Module) Execute(ctx context.Context, action string, input executor.ModuleInput) (*executor.ModuleOutput, error) {
	name, err := stringArg(input.Args, "name")
	if err != nil {
		return nil, fmt.Errorf("svc: %w", err)
	}

	init, err := detectInitSystem(ctx, input.Channel)
	if err != nil {
		return nil, fmt.Errorf("svc: %w", err)
	}

	switch action {
	case "start":
		return m.start(ctx, input, init, name)
	case "stop":
		return m.stop(ctx, input, init, name)
	case "restart":
		return m.restart(ctx, input, init, name)
	case "reload":
		return m.reload(ctx, input, init, name)
	case "enable":
		return m.enable(ctx, input, init, name)
	case "disable":
		return m.disable(ctx, input, init, name)
	default:
		return nil, fmt.Errorf("svc: unsupported action %q", action)
	}
}

// start starts the service. Idempotent: when already active, no command is
// run and Changed is false.
func (m *Module) start(ctx context.Context, input executor.ModuleInput, init initSystem, name string) (*executor.ModuleOutput, error) {
	active, err := init.isActive(ctx, input.Channel, name)
	if err != nil {
		return nil, fmt.Errorf("svc.start: query %s: %w", name, err)
	}
	if active {
		return &executor.ModuleOutput{
			ExitCode: 0,
			Stdout:   fmt.Sprintf("svc %s already active", name),
			Changed:  false,
		}, nil
	}
	return runRemote(ctx, input.Channel, init.startCmd(name), true)
}

// stop stops the service. Idempotent: when already inactive, no command is
// run and Changed is false.
func (m *Module) stop(ctx context.Context, input executor.ModuleInput, init initSystem, name string) (*executor.ModuleOutput, error) {
	active, err := init.isActive(ctx, input.Channel, name)
	if err != nil {
		return nil, fmt.Errorf("svc.stop: query %s: %w", name, err)
	}
	if !active {
		return &executor.ModuleOutput{
			ExitCode: 0,
			Stdout:   fmt.Sprintf("svc %s already inactive", name),
			Changed:  false,
		}, nil
	}
	return runRemote(ctx, input.Channel, init.stopCmd(name), true)
}

// restart restarts the service. Always runs the command.
func (m *Module) restart(ctx context.Context, input executor.ModuleInput, init initSystem, name string) (*executor.ModuleOutput, error) {
	return runRemote(ctx, input.Channel, init.restartCmd(name), true)
}

// reload reloads the service configuration. On systemd this maps to
// `systemctl reload`. On sysvinit, where reload is not universally supported,
// we try `service <name> reload` and fall back to restart when it exits
// non-zero.
func (m *Module) reload(ctx context.Context, input executor.ModuleInput, init initSystem, name string) (*executor.ModuleOutput, error) {
	if init.name() == "systemd" {
		return runRemote(ctx, input.Channel, init.reloadCmd(name), true)
	}
	// sysvinit: try reload, fall back to restart on failure.
	res, err := input.Channel.Exec(ctx, init.reloadCmd(name))
	if err != nil {
		return nil, fmt.Errorf("svc.reload: channel exec: %w", err)
	}
	if res.ExitCode == 0 {
		return &executor.ModuleOutput{
			ExitCode: 0,
			Stdout:   res.Stdout,
			Stderr:   res.Stderr,
			Duration: res.Duration,
			Changed:  true,
		}, nil
	}
	// Fallback: restart.
	out, err := runRemote(ctx, input.Channel, init.restartCmd(name), true)
	if err != nil {
		return nil, err
	}
	if out.Stderr == "" {
		out.Stderr = fmt.Sprintf("reload failed (exit %d), fell back to restart", res.ExitCode)
	} else {
		out.Stderr += fmt.Sprintf("\nreload failed (exit %d), fell back to restart", res.ExitCode)
	}
	return out, nil
}

// enable enables the service to start at boot. Idempotent: when already
// enabled, no command is run and Changed is false.
func (m *Module) enable(ctx context.Context, input executor.ModuleInput, init initSystem, name string) (*executor.ModuleOutput, error) {
	enabled, err := init.isEnabled(ctx, input.Channel, name)
	if err != nil {
		return nil, fmt.Errorf("svc.enable: query %s: %w", name, err)
	}
	if enabled {
		return &executor.ModuleOutput{
			ExitCode: 0,
			Stdout:   fmt.Sprintf("svc %s already enabled", name),
			Changed:  false,
		}, nil
	}
	return runRemote(ctx, input.Channel, init.enableCmd(name), true)
}

// disable disables the service from starting at boot. Idempotent: when already
// disabled, no command is run and Changed is false.
func (m *Module) disable(ctx context.Context, input executor.ModuleInput, init initSystem, name string) (*executor.ModuleOutput, error) {
	enabled, err := init.isEnabled(ctx, input.Channel, name)
	if err != nil {
		return nil, fmt.Errorf("svc.disable: query %s: %w", name, err)
	}
	if !enabled {
		return &executor.ModuleOutput{
			ExitCode: 0,
			Stdout:   fmt.Sprintf("svc %s already disabled", name),
			Changed:  false,
		}, nil
	}
	return runRemote(ctx, input.Channel, init.disableCmd(name), true)
}

// --- init system abstraction ---------------------------------------------

// initSystem is the abstraction over systemd and sysvinit.
type initSystem interface {
	name() string
	isActive(ctx context.Context, ch channel.Channel, svc string) (bool, error)
	isEnabled(ctx context.Context, ch channel.Channel, svc string) (bool, error)
	startCmd(svc string) string
	stopCmd(svc string) string
	restartCmd(svc string) string
	reloadCmd(svc string) string
	enableCmd(svc string) string
	disableCmd(svc string) string
}

// systemdInit implements initSystem for systemd hosts.
type systemdInit struct{}

func (systemdInit) name() string { return "systemd" }

func (systemdInit) isActive(ctx context.Context, ch channel.Channel, svc string) (bool, error) {
	res, err := ch.Exec(ctx, fmt.Sprintf("systemctl is-active --quiet %s", svc))
	if err != nil {
		return false, err
	}
	return res.ExitCode == 0, nil
}

func (systemdInit) isEnabled(ctx context.Context, ch channel.Channel, svc string) (bool, error) {
	res, err := ch.Exec(ctx, fmt.Sprintf("systemctl is-enabled --quiet %s", svc))
	if err != nil {
		return false, err
	}
	return res.ExitCode == 0, nil
}

func (systemdInit) startCmd(svc string) string   { return fmt.Sprintf("systemctl start %s", svc) }
func (systemdInit) stopCmd(svc string) string    { return fmt.Sprintf("systemctl stop %s", svc) }
func (systemdInit) restartCmd(svc string) string { return fmt.Sprintf("systemctl restart %s", svc) }
func (systemdInit) reloadCmd(svc string) string  { return fmt.Sprintf("systemctl reload %s", svc) }
func (systemdInit) enableCmd(svc string) string  { return fmt.Sprintf("systemctl enable %s", svc) }
func (systemdInit) disableCmd(svc string) string { return fmt.Sprintf("systemctl disable %s", svc) }

// sysvinitInit implements initSystem for legacy sysvinit hosts.
type sysvinitInit struct{}

func (sysvinitInit) name() string { return "sysvinit" }

func (sysvinitInit) isActive(ctx context.Context, ch channel.Channel, svc string) (bool, error) {
	// `service <name> status` exit code is 0 when running, non-zero otherwise.
	res, err := ch.Exec(ctx, fmt.Sprintf("service %s status", svc))
	if err != nil {
		return false, err
	}
	return res.ExitCode == 0, nil
}

func (sysvinitInit) isEnabled(ctx context.Context, ch channel.Channel, svc string) (bool, error) {
	// sysvinit does not have a uniform "is-enabled" query; we check for the
	// presence of an S-symlink in /etc/rc2.d/ as a best-effort heuristic.
	res, err := ch.Exec(ctx, fmt.Sprintf("ls /etc/rc2.d/S*%s >/dev/null 2>&1", svc))
	if err != nil {
		return false, err
	}
	return res.ExitCode == 0, nil
}

func (sysvinitInit) startCmd(svc string) string   { return fmt.Sprintf("service %s start", svc) }
func (sysvinitInit) stopCmd(svc string) string    { return fmt.Sprintf("service %s stop", svc) }
func (sysvinitInit) restartCmd(svc string) string { return fmt.Sprintf("service %s restart", svc) }
func (sysvinitInit) reloadCmd(svc string) string  { return fmt.Sprintf("service %s reload", svc) }
func (sysvinitInit) enableCmd(svc string) string  { return fmt.Sprintf("update-rc.d %s defaults", svc) }
func (sysvinitInit) disableCmd(svc string) string {
	return fmt.Sprintf("update-rc.d -f %s remove", svc)
}

// detectInitSystem probes the target for systemctl and returns systemdInit
// when found, sysvinitInit otherwise.
func detectInitSystem(ctx context.Context, ch channel.Channel) (initSystem, error) {
	res, err := ch.Exec(ctx, "which systemctl")
	if err != nil {
		return nil, fmt.Errorf("detect init system: %w", err)
	}
	if res.ExitCode == 0 {
		return systemdInit{}, nil
	}
	return sysvinitInit{}, nil
}

// --- helpers --------------------------------------------------------------

// runRemote executes cmd on the target and wraps the result in a ModuleOutput.
func runRemote(ctx context.Context, ch channel.Channel, cmd string, Changed bool) (*executor.ModuleOutput, error) {
	res, err := ch.Exec(ctx, cmd)
	if err != nil {
		return nil, fmt.Errorf("svc: channel exec %q: %w", cmd, err)
	}
	return &executor.ModuleOutput{
		ExitCode: res.ExitCode,
		Stdout:   res.Stdout,
		Stderr:   res.Stderr,
		Duration: res.Duration,
		Changed:  Changed && res.ExitCode == 0,
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

// keep strings import honest for future refactors that may drop direct uses.
var _ = strings.TrimSpace

// init registers the module with the default executor.
func init() {
	executor.RegisterModule(New())
}
