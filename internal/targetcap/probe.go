// Package targetcap performs read-only target capability probes. It reports
// what this host can support NOW; it does not turn observation into a support
// or production-acceptance claim.
package targetcap

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/nexus/levee/internal/channel"
	"github.com/nexus/levee/internal/executor/modules/pkg"
)

const (
	SchemaVersion = "levee.capabilities/v1"
	Supported     = "supported"
	Unsupported   = "unsupported"
	Unknown       = "unknown"
)

// Report is the stable JSON contract emitted by `levee capabilities`.
type Report struct {
	SchemaVersion  string            `json:"schema_version"`
	Evidence       string            `json:"evidence"`
	Target         string            `json:"target"`
	ObservedAt     time.Time         `json:"observed_at"`
	Kernel         string            `json:"kernel,omitempty"`
	OS             map[string]string `json:"os,omitempty"`
	PackageManager string            `json:"package_manager,omitempty"`
	InitSystem     string            `json:"init_system,omitempty"`
	Tools          map[string]string `json:"tools,omitempty"`
	Actions        []Action          `json:"actions"`
	Summary        Summary           `json:"summary"`
}

// Action describes one executable action family or an action with additional
// topology requirements. Reason is always present for unsupported/unknown.
type Action struct {
	Name   string `json:"name"`
	Status string `json:"status"`
	Reason string `json:"reason"`
}

// Summary counts statuses without treating unknown as support.
type Summary struct {
	Supported   int `json:"supported"`
	Unsupported int `json:"unsupported"`
	Unknown     int `json:"unknown"`
}

// Probe runs only read-only commands (which, --version, --help, uname, cat and
// test). No package, service, database or cluster is mutated.
func Probe(ctx context.Context, ch channel.Channel, target string) (*Report, error) {
	if ch == nil {
		return nil, fmt.Errorf("targetcap: nil channel")
	}
	r := &Report{
		SchemaVersion: SchemaVersion,
		Evidence:      "live-probe; observation only, not an acceptance verdict",
		Target:        target,
		ObservedAt:    time.Now().UTC(),
		OS:            map[string]string{},
		Tools:         map[string]string{},
		Actions:       []Action{},
	}
	r.Kernel = strings.TrimSpace(commandOutput(ctx, ch, "uname -s"))
	for _, line := range strings.Split(commandOutput(ctx, ch, "cat /etc/os-release"), "\n") {
		key, value, ok := strings.Cut(line, "=")
		if ok {
			r.OS[key] = strings.Trim(value, `"`)
		}
	}
	if name, err := pkg.DetectPackageManagerName(ctx, ch); err == nil {
		r.PackageManager = name
	}
	r.InitSystem = detectInitSystem(ctx, ch)
	for _, tool := range []string{"psql", "pg_dump", "mysql", "pt-online-schema-change"} {
		if exists(ctx, ch, tool) {
			r.Tools[tool] = firstLine(commandOutput(ctx, ch, tool+" --version"))
		}
	}
	r.Actions = buildActions(ctx, ch, r)
	for _, action := range r.Actions {
		switch action.Status {
		case Supported:
			r.Summary.Supported++
		case Unsupported:
			r.Summary.Unsupported++
		default:
			r.Summary.Unknown++
		}
	}
	return r, nil
}

func buildActions(ctx context.Context, ch channel.Channel, r *Report) []Action {
	var out []Action
	add := func(status, reason string, names ...string) {
		for _, name := range names {
			out = append(out, Action{Name: name, Status: status, Reason: reason})
		}
	}

	pkgReason := "no apt/dnf/yum/apk package manager detected"
	pkgStatus := Unsupported
	if r.PackageManager != "" {
		pkgStatus, pkgReason = Supported, r.PackageManager+" detected"
	}
	add(pkgStatus, pkgReason, "pkg.install", "pkg.remove", "pkg.upgrade")

	initReason := "neither systemctl nor service detected"
	initStatus := Unsupported
	if r.InitSystem != "" {
		initStatus, initReason = Supported, r.InitSystem+" detected"
	}
	add(initStatus, initReason, "svc.start", "svc.stop", "svc.restart", "svc.enable", "svc.disable", "svc.reload")

	shellReason := "POSIX sh and a writable /tmp are required"
	shellStatus := Unsupported
	if exists(ctx, ch, "sh") && commandSucceeds(ctx, ch, "test -d /tmp && test -w /tmp") {
		shellStatus, shellReason = Supported, "sh and writable /tmp detected"
	}
	add(shellStatus, shellReason, "shell.exec", "shell.script")

	fileReason := "sha256sum, chmod, chown and rm are required"
	fileStatus := Unsupported
	if exists(ctx, ch, "sha256sum") && exists(ctx, ch, "chmod") && exists(ctx, ch, "chown") && exists(ctx, ch, "rm") {
		fileStatus, fileReason = Supported, "POSIX file utilities detected"
	}
	add(fileStatus, fileReason, "file.copy", "file.template", "file.exists", "file.delete")

	userReason := "useradd, usermod and userdel are required"
	userStatus := Unsupported
	if exists(ctx, ch, "useradd") && exists(ctx, ch, "usermod") && exists(ctx, ch, "userdel") {
		userStatus, userReason = Supported, "POSIX account tools detected"
	}
	add(userStatus, userReason, "user.add", "user.remove", "user.modify")

	mysqlStatus, mysqlReason := toolStatus(r.Tools["mysql"] != "", "mysql client not detected", "mysql client detected")
	add(mysqlStatus, mysqlReason, "mysql.query", "mysql.schema_snapshot", "mysql.schema_restore", "mysql.preflight")
	oscStatus, oscReason := toolStatus(r.Tools["pt-online-schema-change"] != "", "pt-online-schema-change not detected", "pt-online-schema-change detected")
	add(oscStatus, oscReason, "mysql.pt_osc")
	add(Unknown, "replica_switch requires a live replica topology; this probe cannot validate it", "mysql.replica_switch")

	psqlStatus, psqlReason := toolStatus(r.Tools["psql"] != "", "psql not detected", "psql detected")
	add(psqlStatus, psqlReason, "pg.query", "pg.schema_snapshot", "pg.preflight")
	restrict := commandOutput(ctx, ch, "pg_dump --help")
	ddlStatus, ddlReason := Unsupported, "pg_dump with --restrict-key support is required"
	if r.Tools["pg_dump"] != "" && strings.Contains(restrict, "--restrict-key") {
		ddlStatus, ddlReason = Supported, "psql and pg_dump --restrict-key detected"
	}
	add(ddlStatus, ddlReason, "pg.schema_snapshot_ddl", "pg.schema_restore")

	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func detectInitSystem(ctx context.Context, ch channel.Channel) string {
	if exists(ctx, ch, "systemctl") {
		return "systemd"
	}
	if exists(ctx, ch, "service") {
		return "sysvinit"
	}
	return ""
}

func toolStatus(ok bool, missing, present string) (string, string) {
	if ok {
		return Supported, present
	}
	return Unsupported, missing
}

func exists(ctx context.Context, ch channel.Channel, name string) bool {
	return commandSucceeds(ctx, ch, "which "+name)
}

func commandSucceeds(ctx context.Context, ch channel.Channel, command string) bool {
	res, err := ch.Exec(ctx, command)
	return err == nil && res.ExitCode == 0
}

func commandOutput(ctx context.Context, ch channel.Channel, command string) string {
	res, err := ch.Exec(ctx, command)
	if err != nil || res.ExitCode != 0 {
		return ""
	}
	return strings.TrimSpace(res.Stdout)
}

func firstLine(value string) string {
	line, _, _ := strings.Cut(value, "\n")
	return strings.TrimSpace(line)
}
