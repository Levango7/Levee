// Package compat provides the compatibility layer framework that imports
// external automation formats (e.g. Ansible playbooks) into LEVEE's internal
// DSL AST (dsl.Workflow). The package is deliberately self-contained: it
// depends only on internal/dsl (for AST types) and the standard library,
// satisfying the R8 independence constraint — it never imports
// internal/executor, internal/channel, internal/engine or any other core
// runtime package. This keeps the compatibility layer a pure translation
// step that can be unit-tested in isolation.
//
// The primary abstraction is the CompatLayer interface. The reference
// implementation, AnsiblePlaybookImporter, translates a minimal Ansible
// playbook subset (shell, command, file, copy, template, apt, yum, service,
// user, group modules) into a dsl.Workflow that the LEVEE engine can execute.
package compat

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/nexus/levee/internal/dsl"
)

// --- sentinel errors -------------------------------------------------------

// Sentinel errors returned by the compatibility layer. Callers may use
// errors.Is to test against these so that error categorisation is robust to
// wrapping.
var (
	ErrInvalidPlaybook   = errors.New("compat: invalid playbook format")
	ErrUnsupportedModule = errors.New("compat: unsupported ansible module")
	ErrEmptyPlaybook     = errors.New("compat: empty playbook")
	ErrImportFailed      = errors.New("compat: import failed")
)

// --- module mapping --------------------------------------------------------

// ansibleModules is the RECOGNITION set: keys that name an Ansible module
// this layer knows about. Recognition is deliberately separate from
// RESOLUTION — a recognized module whose arguments cannot be translated
// faithfully is refused with an actionable error (see resolveAnsibleAction),
// never silently mapped to an action with different semantics.
var ansibleModules = map[string]bool{
	"shell":    true,
	"command":  true,
	"file":     true,
	"copy":     true,
	"template": true,
	"apt":      true,
	"yum":      true,
	"service":  true,
	"user":     true,
	"group":    true,
}

// resolveAnsibleAction translates a recognized module and its parsed
// arguments into a real LEVEE (module, action) pair.
//
// Fail-closed by construction: every branch either returns a pair that
// exists in the executor registry — pinned by
// TestMappingResolvesToRealExecutorActions against
// executor.DefaultExecutor() — or an error naming the unmappable construct.
// The previous flat table violated both halves: four modules mapped to
// actions that do not exist anywhere (file.manage / svc.manage / user.manage
// / user.group — a playbook importing fine and then failing at plan/execute
// time), and the mapping was state-blind (apt state=absent became
// pkg.install, the opposite of the declared intent).
func resolveAnsibleAction(module string, args map[string]any) (mod, action string, err error) {
	state, _ := args["state"].(string)
	switch module {
	case "shell", "command":
		return "shell", "exec", nil
	case "copy":
		return "file", "copy", nil
	case "template":
		return "file", "template", nil
	case "apt", "yum":
		switch state {
		case "", "present":
			return "pkg", "install", nil
		case "absent":
			return "pkg", "remove", nil
		case "latest":
			return "pkg", "upgrade", nil
		default:
			return "", "", fmt.Errorf("%w: %s state=%q (supported: present, absent, latest)",
				ErrUnsupportedModule, module, state)
		}
	case "service":
		enabled, hasEnabled := args["enabled"]
		if state != "" && hasEnabled {
			return "", "", fmt.Errorf("%w: service declares both state and enabled — LEVEE svc actions are single-purpose; split the task in two",
				ErrUnsupportedModule)
		}
		if state == "" {
			if !hasEnabled {
				return "", "", fmt.Errorf("%w: service requires state (started/stopped/restarted/reloaded) or enabled",
					ErrUnsupportedModule)
			}
			on, err := boolArgValue(enabled)
			if err != nil {
				return "", "", fmt.Errorf("%w: service enabled=%v", ErrUnsupportedModule, enabled)
			}
			if on {
				return "svc", "enable", nil
			}
			return "svc", "disable", nil
		}
		switch state {
		case "started":
			return "svc", "start", nil
		case "stopped":
			return "svc", "stop", nil
		case "restarted":
			return "svc", "restart", nil
		case "reloaded":
			return "svc", "reload", nil
		default:
			return "", "", fmt.Errorf("%w: service state=%q (supported: started, stopped, restarted, reloaded; or enabled)",
				ErrUnsupportedModule, state)
		}
	case "user":
		switch state {
		case "", "present":
			// user.add is idempotent (checks existence before creating), so
			// ansible's create-or-ensure semantics map faithfully.
			return "user", "add", nil
		case "absent":
			return "user", "remove", nil
		default:
			return "", "", fmt.Errorf("%w: user state=%q (supported: present, absent)",
				ErrUnsupportedModule, state)
		}
	case "file":
		return "", "", fmt.Errorf("%w: the ansible `file` module (state=touch/directory/absent/...) has no faithful LEVEE action; use `copy`/`template` for content or a `shell` task for the rest", ErrUnsupportedModule)
	case "group":
		return "", "", fmt.Errorf("%w: the ansible `group` module has no LEVEE executor action; LEVEE's user module covers users only", ErrUnsupportedModule)
	}
	return "", "", fmt.Errorf("%w: %q", ErrUnsupportedModule, module)
}

// boolArgValue normalizes an ansible boolean argument: both the YAML form
// (true/false) and the legacy key=value string form (yes/no/true/false).
func boolArgValue(v any) (bool, error) {
	switch t := v.(type) {
	case bool:
		return t, nil
	case string:
		switch strings.ToLower(t) {
		case "yes", "true":
			return true, nil
		case "no", "false":
			return false, nil
		}
	}
	return false, fmt.Errorf("not a boolean: %v", v)
}

// ansibleReservedKeys lists Ansible task-level directives that are not
// modules. When scanning a task mapping for the module key, these are
// skipped so that the remaining key is treated as the module name.
var ansibleReservedKeys = map[string]bool{
	"name":          true,
	"when":          true,
	"register":      true,
	"become":        true,
	"become_user":   true,
	"become_method": true,
	"tags":          true,
	"vars":          true,
	"with_items":    true,
	"with_dict":     true,
	"loop":          true,
	"loop_control":  true,
	"ignore_errors": true,
	"changed_when":  true,
	"failed_when":   true,
	"no_log":        true,
	"delegate_to":   true,
	"run_once":      true,
	"environment":   true,
	"retries":       true,
	"delay":         true,
	"until":         true,
	"block":         true,
	"rescue":        true,
	"always":        true,
}

// --- CompatLayer interface -------------------------------------------------

// CompatLayer is the abstraction for a compatibility layer that imports an
// external automation format (e.g. Ansible playbook) into LEVEE's internal
// DSL AST (dsl.Workflow). Implementations must be safe for concurrent use:
// the same importer may be shared across goroutines.
//
// The import is a pure translation step — it does not execute anything. The
// returned *dsl.Workflow can be fed into LEVEE's plan/apply pipeline exactly
// as a natively-authored workflow would.
type CompatLayer interface {
	// Name returns the compatibility layer identifier (e.g. "ansible").
	// The name is informational and may be used for logging and diagnostics.
	Name() string

	// Import reads the external format from the file at path and returns
	// the equivalent LEVEE DSL AST. A non-nil error indicates that the
	// file could not be read or that its contents are not a valid
	// representation of the source format.
	Import(path string) (*dsl.Workflow, error)

	// ImportBytes parses the external format from an in-memory byte slice
	// and returns the equivalent LEVEE DSL AST. This is the primary entry
	// point for programmatic use; Import is a thin wrapper that reads the
	// file and delegates here.
	ImportBytes(data []byte) (*dsl.Workflow, error)
}

// --- AnsiblePlaybookImporter -----------------------------------------------

// AnsiblePlaybookImporter imports an Ansible playbook (YAML) into a LEVEE
// DSL Workflow. It supports a minimal Ansible subset: the shell, command,
// file, copy, template, apt, yum, service, user and group modules. Other
// modules are rejected with ErrUnsupportedModule so that callers can detect
// unsupported constructs rather than silently dropping them.
//
// The importer maps Ansible constructs to LEVEE AST nodes as follows:
//   - play.hosts   -> Workflow.Targets (TargetGroup)
//   - play.become  -> Workflow.Meta (recorded as metadata)
//   - play.tasks   -> Workflow.Steps (Step per task)
//   - task.module  -> Step.Module + Step.Action via ansibleModuleMap
//
// The zero value is not usable; callers must use NewAnsiblePlaybookImporter.
type AnsiblePlaybookImporter struct {
	name string
}

// NewAnsiblePlaybookImporter returns a new AnsiblePlaybookImporter ready to
// import playbooks. The returned importer is safe for concurrent use.
func NewAnsiblePlaybookImporter() *AnsiblePlaybookImporter {
	return &AnsiblePlaybookImporter{name: "ansible"}
}

// Name returns "ansible".
func (a *AnsiblePlaybookImporter) Name() string {
	return a.name
}

// Import reads the playbook file at path and returns the LEVEE DSL AST.
// A read failure is wrapped with ErrImportFailed; a parse failure is wrapped
// with ErrInvalidPlaybook or ErrEmptyPlaybook as appropriate.
func (a *AnsiblePlaybookImporter) Import(path string) (*dsl.Workflow, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("%w: read %q: %v", ErrImportFailed, path, err)
	}
	return a.ImportBytes(data)
}

// ImportBytes parses the playbook YAML in data and returns the LEVEE DSL AST.
// The YAML must be an Ansible playbook: a list of play mappings. An empty
// list yields ErrEmptyPlaybook; malformed YAML yields ErrInvalidPlaybook;
// an unknown module yields ErrUnsupportedModule.
func (a *AnsiblePlaybookImporter) ImportBytes(data []byte) (*dsl.Workflow, error) {
	// An Ansible playbook is a YAML list of play mappings. Parse into a
	// generic []map[string]any so we can handle the dynamic task keys
	// (each task's module is an arbitrary key) without a fixed struct.
	var plays []map[string]any
	if err := yaml.Unmarshal(data, &plays); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidPlaybook, err)
	}
	if len(plays) == 0 {
		return nil, ErrEmptyPlaybook
	}

	wf := &dsl.Workflow{}
	for i, play := range plays {
		if err := applyPlay(wf, play, i); err != nil {
			return nil, err
		}
	}
	return wf, nil
}

// --- play / task translation -----------------------------------------------

// applyPlay applies a single Ansible play to the workflow. The playIndex
// argument is used for error attribution only.
func applyPlay(wf *dsl.Workflow, play map[string]any, playIndex int) error {
	// hosts -> targets. Ansible allows hosts to be a string (group name or
	// comma-separated list) or a list of hostnames. We normalise both into
	// a TargetGroup.Hosts slice.
	if hosts, ok := play["hosts"]; ok {
		tg := buildTargetGroup(hosts)
		if tg.Name == "" {
			tg.Name = fmt.Sprintf("play-%d", playIndex)
		}
		wf.Targets = append(wf.Targets, tg)
	}

	// become -> metadata. LEVEE's WorkflowMeta has no dedicated slot for
	// privilege escalation, so we record it in Meta.Description using a
	// stable convention. This preserves the information for downstream
	// consumers without extending the AST.
	if become, ok := play["become"]; ok {
		recordBecome(wf, become)
	}

	// tasks -> steps. A play with no tasks is valid (e.g. a vars-only play).
	tasks, ok := play["tasks"]
	if !ok {
		return nil
	}
	taskList, ok := tasks.([]any)
	if !ok {
		return fmt.Errorf("%w: play %d: tasks is not a list", ErrInvalidPlaybook, playIndex)
	}
	for i, t := range taskList {
		task, ok := t.(map[string]any)
		if !ok {
			return fmt.Errorf("%w: play %d: task %d is not a mapping", ErrInvalidPlaybook, playIndex, i)
		}
		step, err := convertTask(task, playIndex, i)
		if err != nil {
			return err
		}
		wf.Steps = append(wf.Steps, step)
	}
	return nil
}

// buildTargetGroup converts an Ansible hosts value into a TargetGroup.
// A string value may be a single host/group name or a comma-separated list;
// both are expanded into Hosts. A list value is copied element by element.
func buildTargetGroup(hosts any) dsl.TargetGroup {
	tg := dsl.TargetGroup{}
	switch h := hosts.(type) {
	case string:
		tg.Name = h
		// Ansible allows "host1,host2" comma-separated lists. Split and
		// trim each entry so downstream code receives individual hosts.
		for _, part := range strings.Split(h, ",") {
			part = strings.TrimSpace(part)
			if part != "" {
				tg.Hosts = append(tg.Hosts, part)
			}
		}
	case []any:
		for _, item := range h {
			if s, ok := item.(string); ok {
				s = strings.TrimSpace(s)
				if s != "" {
					tg.Hosts = append(tg.Hosts, s)
				}
			}
		}
	}
	return tg
}

// recordBecome records the become (privilege escalation) flag on the workflow
// metadata. The convention is to append "become=<value>" to Meta.Description,
// preserving any previously recorded information.
func recordBecome(wf *dsl.Workflow, become any) {
	var text string
	switch b := become.(type) {
	case bool:
		if b {
			text = "become=yes"
		} else {
			text = "become=no"
		}
	case string:
		text = "become=" + b
	default:
		return
	}
	if wf.Meta.Description == "" {
		wf.Meta.Description = text
	} else {
		wf.Meta.Description = wf.Meta.Description + "; " + text
	}
}

// convertTask converts an Ansible task mapping into a LEVEE Step. The
// playIndex and taskIndex arguments are used for error attribution only.
func convertTask(task map[string]any, playIndex, taskIndex int) (dsl.Step, error) {
	step := dsl.Step{}
	if name, ok := task["name"]; ok {
		if s, ok := name.(string); ok {
			step.Name = s
		}
	}

	// Locate the module key: the first key (other than reserved directives)
	// that is present in ansibleModuleMap. Ansible tasks have exactly one
	// module key; any extra non-reserved keys are a user error.
	var moduleName string
	var moduleValue any
	for key, val := range task {
		if ansibleReservedKeys[key] {
			continue
		}
		if ansibleModules[key] {
			moduleName = key
			moduleValue = val
			break
		}
	}

	if moduleName == "" {
		// No recognised module. Report the offending key if we can find
		// one, so the user knows which module is unsupported.
		for key := range task {
			if ansibleReservedKeys[key] {
				continue
			}
			return dsl.Step{}, fmt.Errorf("%w: %q", ErrUnsupportedModule, key)
		}
		// No module key at all — the task only has reserved directives.
		label := step.Name
		if label == "" {
			label = fmt.Sprintf("play-%d task-%d", playIndex, taskIndex)
		}
		return dsl.Step{}, fmt.Errorf("%w: task %q has no module", ErrInvalidPlaybook, label)
	}

	args := parseModuleArgs(moduleName, moduleValue)
	mod, action, rerr := resolveAnsibleAction(moduleName, args)
	if rerr != nil {
		label := step.Name
		if label == "" {
			label = fmt.Sprintf("play-%d task-%d", playIndex, taskIndex)
		}
		return dsl.Step{}, fmt.Errorf("task %q: %w", label, rerr)
	}
	step.Module = mod
	step.Action = action
	step.Args = args
	return step, nil
}

// parseModuleArgs parses an Ansible module's value into an args map. The
// value can take three forms:
//   - a free-form string for shell/command (the command to run)
//   - a "key=value key=value" string for other modules (Ansible legacy form)
//   - a mapping for any module (modern Ansible form)
//
// For shell and command the string is always stored under the "cmd" key so
// that the LEVEE shell.exec action receives a uniform argument shape.
func parseModuleArgs(moduleName string, value any) map[string]any {
	args := make(map[string]any)

	// shell and command always take a free-form string as the command.
	if moduleName == "shell" || moduleName == "command" {
		switch v := value.(type) {
		case string:
			args["cmd"] = v
		case map[string]any:
			// Modern form: shell: {cmd: "...", chdir: "..."}.
			for k, val := range v {
				args[k] = val
			}
		}
		return args
	}

	// Other modules: try key=value string, then mapping, then raw fallback.
	switch v := value.(type) {
	case string:
		if kv := parseKeyValueString(v); kv != nil {
			return kv
		}
		// Not key=value — store as a raw argument so no information is lost.
		args["raw"] = v
	case map[string]any:
		for k, val := range v {
			args[k] = val
		}
	}
	return args
}

// parseKeyValueString parses an Ansible legacy "key=value key=value" string
// into a map. Returns nil if the string does not look like key=value pairs
// (e.g. a free-form command), so the caller can fall back to a different
// interpretation. A valid key is a non-empty identifier ([A-Za-z0-9_]+).
func parseKeyValueString(s string) map[string]any {
	s = strings.TrimSpace(s)
	if s == "" || !strings.Contains(s, "=") {
		return nil
	}
	result := make(map[string]any)
	for _, field := range strings.Fields(s) {
		idx := strings.Index(field, "=")
		if idx <= 0 {
			// A field without '=' means this is not a key=value list.
			return nil
		}
		key := field[:idx]
		val := field[idx+1:]
		if !isValidKey(key) {
			return nil
		}
		result[key] = val
	}
	if len(result) == 0 {
		return nil
	}
	return result
}

// isValidKey reports whether k is a simple identifier suitable as a
// key=value key: non-empty and containing only letters, digits and
// underscores.
func isValidKey(k string) bool {
	if k == "" {
		return false
	}
	for _, r := range k {
		if r != '_' &&
			!(r >= 'a' && r <= 'z') &&
			!(r >= 'A' && r <= 'Z') &&
			!(r >= '0' && r <= '9') {
			return false
		}
	}
	return true
}
