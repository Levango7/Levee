package dsl

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fullWorkflowYAML is a complete workflow exercising every supported field,
// derived from the LEVEELang spec MVP YAML subset example.
const fullWorkflowYAML = `
name: patch-rolling-mvp
version: "1.0"
description: "Rolling OS patch upgrade"
input:
  - name: pkg_name
    type: string
    required: true
    validate: "min=1,max=64"
  - name: grace_period
    type: duration
    default: 5m
target:
  type: host
  query: "os=linux AND env=prod"
  min_count: 1
  max_count: 100
window:
  start: "02:00"
  end: "06:00"
  timezone: "Asia/Shanghai"
  max_concurrency: 50
batches:
  strategy: percent
  steps: [1, 10, 50, 100]
  max_concurrency: 100
approval:
  level: high
  min_approvers: 2
  exclude_initiator: true
  timeout: 4h
  approvers: ["alice", "bob"]
steps:
  - name: upgrade
    action: pkg.upgrade
    args:
      name: kernel
      version: 5.15.0-91
    requires_reboot: true
  - name: health_check
    action: shell.exec
    args:
      cmd: "uname -r"
    depends_on: ["upgrade"]
    verify:
      cmd:
        run: "uname -r | grep -q 5.15"
        expect_exit: 0
gates:
  - position: post_batch
    cmd:
      run: "systemctl is-active sshd"
      expect_exit: 0
  - position: post_apply
    slo:
      query: "rate(node_load1[5m]) < 4"
      source: prometheus
rollback:
  strategy: snapshot
  on_failure: auto
  verify_after: true
`

// TestParseFullWorkflow verifies that a complete workflow with all supported
// fields is parsed correctly into the AST.
func TestParseFullWorkflow(t *testing.T) {
	p := NewParser()
	wf, err := p.ParseBytes([]byte(fullWorkflowYAML))
	require.NoError(t, err)
	require.NotNil(t, wf)

	// Meta
	assert.Equal(t, "patch-rolling-mvp", wf.Meta.Name)
	assert.Equal(t, "1.0", wf.Meta.Version)
	assert.Equal(t, "Rolling OS patch upgrade", wf.Meta.Description)

	// Inputs
	require.Len(t, wf.Inputs, 2)
	assert.Equal(t, "pkg_name", wf.Inputs[0].Name)
	assert.Equal(t, "string", wf.Inputs[0].Type)
	assert.True(t, wf.Inputs[0].Required)
	assert.Equal(t, "min=1,max=64", wf.Inputs[0].Validate)
	assert.Equal(t, "grace_period", wf.Inputs[1].Name)
	assert.Equal(t, "duration", wf.Inputs[1].Type)
	assert.Equal(t, "5m", wf.Inputs[1].Default)

	// Targets
	require.Len(t, wf.Targets, 1)
	assert.Equal(t, "host", wf.Targets[0].Type)
	assert.Equal(t, "os=linux AND env=prod", wf.Targets[0].Query)
	assert.Equal(t, 1, wf.Targets[0].MinCount)
	assert.Equal(t, 100, wf.Targets[0].MaxCount)

	// Window
	assert.Equal(t, "02:00", wf.Window.Start)
	assert.Equal(t, "06:00", wf.Window.End)
	assert.Equal(t, "Asia/Shanghai", wf.Window.Timezone)
	assert.Equal(t, 50, wf.Window.MaxConcurrency)

	// Batches
	assert.Equal(t, "percent", wf.Batches.Strategy)
	assert.Equal(t, []int{1, 10, 50, 100}, wf.Batches.Steps)
	assert.Equal(t, 100, wf.Batches.MaxConcurrency)

	// Approval
	require.NotNil(t, wf.Approval)
	assert.Equal(t, "high", wf.Approval.Level)
	assert.Equal(t, 2, wf.Approval.MinApprovers)
	assert.True(t, wf.Approval.ExcludeInitiator)
	assert.Equal(t, "4h", wf.Approval.Timeout)
	assert.Equal(t, []string{"alice", "bob"}, wf.Approval.Approvers)

	// Steps
	require.Len(t, wf.Steps, 2)
	assert.Equal(t, "upgrade", wf.Steps[0].Name)
	assert.Equal(t, "pkg", wf.Steps[0].Module)
	assert.Equal(t, "upgrade", wf.Steps[0].Action)
	assert.True(t, wf.Steps[0].RequiresReboot)
	assert.Equal(t, "kernel", wf.Steps[0].Args["name"])

	assert.Equal(t, "health_check", wf.Steps[1].Name)
	assert.Equal(t, "shell", wf.Steps[1].Module)
	assert.Equal(t, "exec", wf.Steps[1].Action)
	assert.Equal(t, []string{"upgrade"}, wf.Steps[1].DependsOn)
	// Step-level verify gate
	require.NotNil(t, wf.Steps[1].Gate)
	require.Len(t, wf.Steps[1].Gate.Post, 1)
	assert.Equal(t, "cmd", wf.Steps[1].Gate.Post[0].Type)
	assert.Equal(t, "uname -r | grep -q 5.15", wf.Steps[1].Gate.Post[0].Command)
	assert.Equal(t, 0, wf.Steps[1].Gate.Post[0].ExpectExit)

	// Workflow-level gates
	require.NotNil(t, wf.Gate)
	require.Len(t, wf.Gate.Post, 2)
	assert.Equal(t, "cmd", wf.Gate.Post[0].Type)
	assert.Equal(t, "systemctl is-active sshd", wf.Gate.Post[0].Command)
	assert.Equal(t, "slo", wf.Gate.Post[1].Type)
	assert.Equal(t, "rate(node_load1[5m]) < 4", wf.Gate.Post[1].Command)
	assert.Equal(t, "prometheus", wf.Gate.Post[1].Source)

	// Rollback
	require.NotNil(t, wf.Rollback)
	assert.Equal(t, "snapshot", wf.Rollback.Strategy)
	assert.Equal(t, "auto", wf.Rollback.OnFailure)
	assert.True(t, wf.Rollback.VerifyAfter)
}

// TestParseInputOnly verifies that a workflow with only an input declaration
// (and a name) fails validation because target and steps are required.
func TestParseInputOnly(t *testing.T) {
	p := NewParser()
	yaml := `
name: input-only
input:
  - name: table
    type: string
    required: true
`
	wf, err := p.ParseBytes([]byte(yaml))
	require.Error(t, err)
	require.Nil(t, wf)

	var pe *ParseError
	require.ErrorAs(t, err, &pe)
	assert.Equal(t, "LE092", pe.Code)
	assert.Contains(t, pe.Field, "target")
}

// TestParseInputForms verifies the three supported input declaration forms:
// list, detailed map and shorthand map.
func TestParseInputForms(t *testing.T) {
	t.Run("list_form", func(t *testing.T) {
		p := NewParser()
		yaml := `
name: wf-list
target: {type: host, query: "env=prod"}
steps:
  - name: s1
    action: shell.exec
input:
  - name: pkg
    type: string
    required: true
  - name: wait
    type: duration
    default: 5m
`
		wf, err := p.ParseBytes([]byte(yaml))
		require.NoError(t, err)
		require.Len(t, wf.Inputs, 2)
		assert.Equal(t, "pkg", wf.Inputs[0].Name)
		assert.Equal(t, "string", wf.Inputs[0].Type)
		assert.True(t, wf.Inputs[0].Required)
		assert.Equal(t, "wait", wf.Inputs[1].Name)
		assert.Equal(t, "5m", wf.Inputs[1].Default)
	})

	t.Run("detailed_map_form", func(t *testing.T) {
		p := NewParser()
		yaml := `
name: wf-map
target: {type: host, query: "env=prod"}
steps:
  - name: s1
    action: shell.exec
input:
  pkg:
    type: string
    required: true
    validate: "min=1"
  wait:
    type: duration
    default: 5m
`
		wf, err := p.ParseBytes([]byte(yaml))
		require.NoError(t, err)
		require.Len(t, wf.Inputs, 2)
		// Map order is not guaranteed; find by name.
		byName := make(map[string]InputParam, len(wf.Inputs))
		for _, p := range wf.Inputs {
			byName[p.Name] = p
		}
		assert.Equal(t, "string", byName["pkg"].Type)
		assert.True(t, byName["pkg"].Required)
		assert.Equal(t, "min=1", byName["pkg"].Validate)
		assert.Equal(t, "duration", byName["wait"].Type)
		assert.Equal(t, "5m", byName["wait"].Default)
	})

	t.Run("shorthand_map_form", func(t *testing.T) {
		p := NewParser()
		yaml := `
name: wf-short
target: {type: host, query: "env=prod"}
steps:
  - name: s1
    action: shell.exec
input:
  pkg: string
  count: int
`
		wf, err := p.ParseBytes([]byte(yaml))
		require.NoError(t, err)
		require.Len(t, wf.Inputs, 2)
		byName := make(map[string]InputParam, len(wf.Inputs))
		for _, p := range wf.Inputs {
			byName[p.Name] = p
		}
		assert.Equal(t, "string", byName["pkg"].Type)
		assert.Equal(t, "int", byName["count"].Type)
	})
}

// TestParseStaticTarget verifies parsing of a static target host list.
func TestParseStaticTarget(t *testing.T) {
	p := NewParser()
	yaml := `
name: static-target
target:
  type: host
  hosts:
    - web-01
    - web-02
    - web-03
steps:
  - name: ping
    action: shell.exec
    args:
      cmd: "ping -c1 {{target.host}}"
`
	wf, err := p.ParseBytes([]byte(yaml))
	require.NoError(t, err)
	require.Len(t, wf.Targets, 1)
	assert.Equal(t, "host", wf.Targets[0].Type)
	assert.Equal(t, []string{"web-01", "web-02", "web-03"}, wf.Targets[0].Hosts)
}

// TestParseDynamicTarget verifies parsing of a dynamic target query.
func TestParseDynamicTarget(t *testing.T) {
	p := NewParser()
	yaml := `
name: dynamic-target
target:
  type: mysql
  query: "role=primary AND cluster=orders-db"
  min_count: 1
  max_count: 10
steps:
  - name: migrate
    action: mysql.pt-online-schema-change
`
	wf, err := p.ParseBytes([]byte(yaml))
	require.NoError(t, err)
	require.Len(t, wf.Targets, 1)
	assert.Equal(t, "mysql", wf.Targets[0].Type)
	assert.Equal(t, "role=primary AND cluster=orders-db", wf.Targets[0].Query)
	assert.Equal(t, 1, wf.Targets[0].MinCount)
	assert.Equal(t, 10, wf.Targets[0].MaxCount)
	assert.Empty(t, wf.Targets[0].Hosts)
}

// TestParseMultipleSteps verifies parsing of a workflow with multiple steps
// including dependencies and action splitting.
func TestParseMultipleSteps(t *testing.T) {
	p := NewParser()
	yaml := `
name: multi-step
target: {type: host, query: "env=prod"}
steps:
  - name: scan
    action: patch.scan
    args:
      host: "{{target.host}}"
  - name: upgrade
    action: pkg.upgrade
    args:
      name: kernel
    depends_on: ["scan"]
    requires_reboot: true
  - name: verify
    action: shell.exec
    args:
      cmd: "uname -r"
    depends_on: ["upgrade"]
    idempotent: true
`
	wf, err := p.ParseBytes([]byte(yaml))
	require.NoError(t, err)
	require.Len(t, wf.Steps, 3)

	assert.Equal(t, "scan", wf.Steps[0].Name)
	assert.Equal(t, "patch", wf.Steps[0].Module)
	assert.Equal(t, "scan", wf.Steps[0].Action)

	assert.Equal(t, "upgrade", wf.Steps[1].Name)
	assert.Equal(t, "pkg", wf.Steps[1].Module)
	assert.Equal(t, "upgrade", wf.Steps[1].Action)
	assert.Equal(t, []string{"scan"}, wf.Steps[1].DependsOn)
	assert.True(t, wf.Steps[1].RequiresReboot)

	assert.Equal(t, "verify", wf.Steps[2].Name)
	assert.Equal(t, "shell", wf.Steps[2].Module)
	assert.Equal(t, "exec", wf.Steps[2].Action)
	assert.True(t, wf.Steps[2].Idempotent)
}

// TestParseRollbackNested verifies parsing of a rollback block with nested
// undo steps.
func TestParseRollbackNested(t *testing.T) {
	p := NewParser()
	yaml := `
name: rollback-nested
target: {type: mysql, query: "role=primary"}
steps:
  - name: migrate
    action: mysql.pt-online-schema-change
rollback:
  strategy: undo-action
  on_failure: auto
  verify_after: true
  step:
    name: undo_migrate
    action: mysql.pt-online-schema-change
    args:
      alter: "DROP COLUMN status"
  steps:
    - name: reload
      action: svc.reload
      args:
        name: nginx
`
	wf, err := p.ParseBytes([]byte(yaml))
	require.NoError(t, err)
	require.NotNil(t, wf.Rollback)
	assert.Equal(t, "undo-action", wf.Rollback.Strategy)
	assert.Equal(t, "auto", wf.Rollback.OnFailure)
	assert.True(t, wf.Rollback.VerifyAfter)
	// One singular step + one plural step = 2.
	require.Len(t, wf.Rollback.Steps, 2)
	assert.Equal(t, "undo_migrate", wf.Rollback.Steps[0].Name)
	assert.Equal(t, "mysql", wf.Rollback.Steps[0].Module)
	assert.Equal(t, "pt-online-schema-change", wf.Rollback.Steps[0].Action)
	assert.Equal(t, "reload", wf.Rollback.Steps[1].Name)
	assert.Equal(t, "svc", wf.Rollback.Steps[1].Module)
	assert.Equal(t, "reload", wf.Rollback.Steps[1].Action)
}

// TestParseApprovalNested verifies parsing of an approval block nested at both
// workflow level and step level.
func TestParseApprovalNested(t *testing.T) {
	p := NewParser()
	yaml := `
name: approval-nested
target: {type: host, query: "env=prod"}
approval:
  level: high
  min_approvers: 2
  exclude_initiator: true
  timeout: 4h
  approvers: ["alice", "bob"]
steps:
  - name: risky
    action: file.delete
    args:
      path: /tmp/old
    irreversible: true
    approval:
      level: emergency
      timeout: 15m
`
	wf, err := p.ParseBytes([]byte(yaml))
	require.NoError(t, err)

	// Workflow-level approval
	require.NotNil(t, wf.Approval)
	assert.Equal(t, "high", wf.Approval.Level)
	assert.Equal(t, 2, wf.Approval.MinApprovers)
	assert.True(t, wf.Approval.ExcludeInitiator)
	assert.Equal(t, "4h", wf.Approval.Timeout)
	assert.Equal(t, []string{"alice", "bob"}, wf.Approval.Approvers)

	// Step-level approval override
	require.Len(t, wf.Steps, 1)
	require.NotNil(t, wf.Steps[0].Approval)
	assert.Equal(t, "emergency", wf.Steps[0].Approval.Level)
	assert.Equal(t, "15m", wf.Steps[0].Approval.Timeout)
	assert.True(t, wf.Steps[0].Irreversible)
}

// TestParseErrorMissingName verifies that a missing workflow name produces a
// structured error with the correct code and field.
func TestParseErrorMissingName(t *testing.T) {
	p := NewParser()
	yaml := `
target: {type: host, query: "env=prod"}
steps:
  - name: s1
    action: shell.exec
`
	wf, err := p.ParseBytes([]byte(yaml))
	require.Error(t, err)
	require.Nil(t, wf)
	var pe *ParseError
	require.ErrorAs(t, err, &pe)
	assert.Equal(t, "LE002", pe.Code)
	assert.Equal(t, "name", pe.Field)
}

// TestParseErrorMissingTarget verifies that a missing target block produces
// error LE092.
func TestParseErrorMissingTarget(t *testing.T) {
	p := NewParser()
	yaml := `
name: no-target
steps:
  - name: s1
    action: shell.exec
`
	wf, err := p.ParseBytes([]byte(yaml))
	require.Error(t, err)
	require.Nil(t, wf)
	var pe *ParseError
	require.ErrorAs(t, err, &pe)
	assert.Equal(t, "LE092", pe.Code)
}

// TestParseErrorMissingSteps verifies that a missing steps block produces
// error LE093.
func TestParseErrorMissingSteps(t *testing.T) {
	p := NewParser()
	yaml := `
name: no-steps
target: {type: host, query: "env=prod"}
`
	wf, err := p.ParseBytes([]byte(yaml))
	require.Error(t, err)
	require.Nil(t, wf)
	var pe *ParseError
	require.ErrorAs(t, err, &pe)
	assert.Equal(t, "LE093", pe.Code)
}

// TestParseErrorInvalidApprovalLevel verifies that an invalid approval level
// produces error LE044.
func TestParseErrorInvalidApprovalLevel(t *testing.T) {
	p := NewParser()
	yaml := `
name: bad-approval
target: {type: host, query: "env=prod"}
steps:
  - name: s1
    action: shell.exec
approval:
  level: super-high
`
	wf, err := p.ParseBytes([]byte(yaml))
	require.Error(t, err)
	require.Nil(t, wf)
	var pe *ParseError
	require.ErrorAs(t, err, &pe)
	assert.Equal(t, "LE044", pe.Code)
	assert.Contains(t, pe.Field, "approval.level")
}

// TestParseErrorPercentSteps verifies that an invalid percentage array
// (non-increasing or not ending at 100) produces the correct error.
func TestParseErrorPercentSteps(t *testing.T) {
	t.Run("not_increasing", func(t *testing.T) {
		p := NewParser()
		yaml := `
name: bad-percent
target: {type: host, query: "env=prod"}
steps:
  - name: s1
    action: shell.exec
batches:
  strategy: percent
  steps: [10, 5, 100]
`
		wf, err := p.ParseBytes([]byte(yaml))
		require.Error(t, err)
		require.Nil(t, wf)
		var pe *ParseError
		require.ErrorAs(t, err, &pe)
		assert.Equal(t, "LE031", pe.Code)
	})

	t.Run("not_ending_at_100", func(t *testing.T) {
		p := NewParser()
		yaml := `
name: bad-percent
target: {type: host, query: "env=prod"}
steps:
  - name: s1
    action: shell.exec
batches:
  strategy: percent
  steps: [1, 10, 50, 90]
`
		wf, err := p.ParseBytes([]byte(yaml))
		require.Error(t, err)
		require.Nil(t, wf)
		var pe *ParseError
		require.ErrorAs(t, err, &pe)
		assert.Equal(t, "LE032", pe.Code)
	})
}

// TestParseErrorYAMLSyntax verifies that malformed YAML produces a parse error.
func TestParseErrorYAMLSyntax(t *testing.T) {
	p := NewParser()
	yaml := `
name: bad-yaml
target: {type: host, query: "env=prod"
steps:
  - name: s1
    action: shell.exec
`
	wf, err := p.ParseBytes([]byte(yaml))
	require.Error(t, err)
	require.Nil(t, wf)
	var pe *ParseError
	require.ErrorAs(t, err, &pe)
}

// TestParseErrorEmptyDocument verifies that an empty YAML document produces an
// error.
func TestParseErrorEmptyDocument(t *testing.T) {
	p := NewParser()
	wf, err := p.ParseBytes([]byte(""))
	require.Error(t, err)
	require.Nil(t, wf)
	var pe *ParseError
	require.ErrorAs(t, err, &pe)
	assert.Equal(t, "LE002", pe.Code)
}

// TestParseFile verifies parsing from a file path.
func TestParseFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "workflow.yaml")
	require.NoError(t, os.WriteFile(path, []byte(fullWorkflowYAML), 0o644))

	p := NewParser()
	wf, err := p.ParseFile(path)
	require.NoError(t, err)
	assert.Equal(t, "patch-rolling-mvp", wf.Meta.Name)
}

// TestParseFileNotFound verifies that a non-existent file produces an error.
func TestParseFileNotFound(t *testing.T) {
	p := NewParser()
	wf, err := p.ParseFile("/nonexistent/path/workflow.yaml")
	require.Error(t, err)
	require.Nil(t, wf)
	var pe *ParseError
	require.ErrorAs(t, err, &pe)
}

// TestParseReader verifies parsing from an io.Reader.
func TestParseReader(t *testing.T) {
	p := NewParser()
	wf, err := p.ParseReader(strings.NewReader(fullWorkflowYAML))
	require.NoError(t, err)
	assert.Equal(t, "patch-rolling-mvp", wf.Meta.Name)
}

// TestParseActionSplit verifies that dotted action references are split into
// module and action correctly.
func TestParseActionSplit(t *testing.T) {
	tests := []struct {
		action string
		module string
		name   string
	}{
		{"pkg.upgrade", "pkg", "upgrade"},
		{"mysql.pt-online-schema-change", "mysql", "pt-online-schema-change"},
		{"shell.exec", "shell", "exec"},
		{"noslash", "", "noslash"},
		{"", "", ""},
	}
	for _, tt := range tests {
		m, n := splitAction(tt.action)
		assert.Equal(t, tt.module, m, "module for %q", tt.action)
		assert.Equal(t, tt.name, n, "name for %q", tt.action)
	}
}

// TestParseTargetsPlural verifies that the plural "targets" key is accepted
// for multiple target groups.
func TestParseTargetsPlural(t *testing.T) {
	p := NewParser()
	yaml := `
name: multi-target
targets:
  - type: host
    hosts: ["web-01", "web-02"]
  - type: mysql
    query: "role=primary"
steps:
  - name: s1
    action: shell.exec
`
	wf, err := p.ParseBytes([]byte(yaml))
	require.NoError(t, err)
	require.Len(t, wf.Targets, 2)
	assert.Equal(t, "host", wf.Targets[0].Type)
	assert.Equal(t, []string{"web-01", "web-02"}, wf.Targets[0].Hosts)
	assert.Equal(t, "mysql", wf.Targets[1].Type)
	assert.Equal(t, "role=primary", wf.Targets[1].Query)
}

// TestParseErrorParseErrorFormat verifies the ParseError.Error() formatting.
func TestParseErrorParseErrorFormat(t *testing.T) {
	pe := &ParseError{
		Code:    "LE002",
		Line:    5,
		Field:   "name",
		Message: "field is required",
	}
	s := pe.Error()
	assert.Contains(t, s, "LE002")
	assert.Contains(t, s, "field is required")
	assert.Contains(t, s, "field=name")
	assert.Contains(t, s, "line=5")
}

// TestParseGateParamsPassthrough verifies that the free-form params mapping
// on a gate declaration is carried verbatim into GateCheck.Params for every
// check type.
func TestParseGateParamsPassthrough(t *testing.T) {
	src := `
name: gate-params
target:
  hosts: [10.0.0.1]
steps:
  - name: deploy
    action: shell.run
    verify:
      probe: {}
      params:
        kind: http
        mode: direct
        url: "http://{target}/"
        expect_status: "200-299"
        timeout_seconds: 10
gates:
  - position: post_apply
    human:
      message: "confirm rollout"
    params:
      reason: "operator sign-off"
      timeout_seconds: 60
`
	wf, err := NewParser().ParseBytes([]byte(src))
	require.NoError(t, err)

	// Step-level probe gate keeps its params.
	require.Len(t, wf.Steps, 1)
	g := wf.Steps[0].Gate
	require.NotNil(t, g)
	require.Len(t, g.Post, 1)
	assert.Equal(t, "probe", g.Post[0].Type)
	assert.Equal(t, map[string]any{
		"kind":            "http",
		"mode":            "direct",
		"url":             "http://{target}/",
		"expect_status":   "200-299",
		"timeout_seconds": 10,
	}, g.Post[0].Params)

	// Workflow-level human gate keeps its params too.
	require.NotNil(t, wf.Gate)
	require.Len(t, wf.Gate.Post, 1)
	assert.Equal(t, "human", wf.Gate.Post[0].Type)
	assert.Equal(t, "operator sign-off", wf.Gate.Post[0].Params["reason"])
	assert.Equal(t, 60, wf.Gate.Post[0].Params["timeout_seconds"])
}

// TestParseGateTemplates ensures the shipped gate templates under
// examples/gate-templates stay valid against the parser.
func TestParseGateTemplates(t *testing.T) {
	matches, err := filepath.Glob(filepath.Join("..", "..", "examples", "gate-templates", "*.yaml"))
	require.NoError(t, err)
	require.NotEmpty(t, matches, "expected at least one gate template")

	p := NewParser()
	for _, path := range matches {
		t.Run(filepath.Base(path), func(t *testing.T) {
			wf, err := p.ParseFile(path)
			require.NoError(t, err, "template %s must parse", path)
			assert.NotEmpty(t, wf.Steps, "template %s must declare steps", path)
		})
	}
}
