// cli_template_new_test.go drives the template → run → plan chain through the
// real CLI against real SQLite persistence.
//
// The chain was broken end to end: `levee new` stored the template *name* in
// run.WorkflowName, which is the field wiring.resolveWorkflow parses as the
// run's workflow source, so every template-instantiated run died at plan time
// with `LE001: cannot unmarshal !!str 'probe' into dsl.yamlWorkflowRaw` — an
// error that names the change id and a workflow, never the template that caused
// it. These tests pin both halves of the fix: the source that gets stored, and
// the refusal that now happens where the cause is still obvious.
package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nexus/levee/internal/config"
	"github.com/nexus/levee/internal/dsl"
	"github.com/nexus/levee/internal/state"
	"github.com/nexus/levee/internal/template"
)

// tplWorkflowYAML is a current-dialect workflow with two placeholders: one fed
// by a required parameter, one inside a larger value (the label query), which
// is the substitution shape the instantiator's literal replacement supports.
const tplWorkflowYAML = `name: tpl-cli-e2e
version: "1.0"
target:
  type: host
  query: "group={{.target_group}} AND env=prod"
batches:
  strategy: percent
  steps: [1, 10, 100]
steps:
  - name: update
    action: shell.exec
    args:
      cmd: "yum update -y {{.package}}"
`

// restoreNewFlagVars resets the package-level --params value `new` binds;
// cobra keeps flag targets in process globals, so a value left set here would
// leak into the next test in the package.
func restoreNewFlagVars() { newOptParams = "" }

// registerTemplate writes a template record through the real library API into
// the directory this CLI env's config resolves templateDir() to — the same
// directory `levee new` reads, so the test exercises the on-disk format rather
// than an in-memory stand-in.
func registerTemplate(t *testing.T, e *cliEnv, name, content string, params ...template.TemplateParam) {
	t.Helper()
	cfg, err := config.Load(e.cfgPath)
	require.NoError(t, err)
	lib, err := template.NewTemplateLibrary(templateDir(cfg))
	require.NoError(t, err)
	require.NoError(t, lib.Save(context.Background(), &template.Template{
		Name:       name,
		Content:    content,
		Parameters: params,
	}))
}

// newRunID instantiates the named template through the CLI and returns the run
// id it printed.
func newRunID(t *testing.T, e *cliEnv, name, params string) string {
	t.Helper()
	out := mustRun(t, "--config", e.cfgPath, "new", name, "--params", params, "--json")
	var env struct {
		Data struct {
			RunID string `json:"run_id"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal([]byte(out), &env), "cli output: %s", out)
	require.NotEmpty(t, env.Data.RunID, "new must report a run id")
	return env.Data.RunID
}

func seedInventoryHosts(t *testing.T, e *cliEnv, hosts ...string) {
	t.Helper()
	store := e.open(t)
	defer func() { _ = store.Close() }()
	now := time.Now().UTC().Truncate(time.Millisecond)
	for _, h := range hosts {
		require.NoError(t, store.UpsertTarget(context.Background(), &state.Target{
			ID: "tgt-" + h, Hostname: h, Port: 22,
			ChannelType: "ssh", Status: "active", CreatedAt: now,
		}))
	}
}

func storedRun(t *testing.T, e *cliEnv, id string) *state.Run {
	t.Helper()
	store := e.open(t)
	defer func() { _ = store.Close() }()
	run, err := store.GetRun(context.Background(), id)
	require.NoError(t, err)
	require.NotNil(t, run)
	return run
}

// TestNewStoresRenderedWorkflowAndPlanSucceeds is the P0 regression: the whole
// documented lifecycle (template → new → plan) run through the command surface.
func TestNewStoresRenderedWorkflowAndPlanSucceeds(t *testing.T) {
	defer resetRootFlags()
	t.Cleanup(restoreNewFlagVars)
	e := newCLIEnv(t)
	registerTemplate(t, e, "patch", tplWorkflowYAML,
		template.TemplateParam{Name: "package", Type: "string", Required: true},
		template.TemplateParam{Name: "target_group", Type: "string", Default: "web"})
	seedInventoryHosts(t, e, "web-1")

	id := newRunID(t, e, "patch", "package=nginx")
	run := storedRun(t, e, id)

	// The defect: WorkflowName held "patch". The contract (plan.go
	// resolveWorkflow): it holds the source plan parses.
	assert.NotEqual(t, "patch", run.WorkflowName,
		"the run must carry the rendered workflow, not the template name")
	assert.Equal(t, "patch", run.TemplateName, "the template stays the template")
	assert.Contains(t, run.WorkflowName, "yum update -y nginx",
		"the required parameter is substituted into the stored source")
	assert.Contains(t, run.WorkflowName, "group=web AND env=prod",
		"substitution works inside a larger value, not only whole-value slots")
	assert.NotContains(t, run.WorkflowName, "{{", "no placeholder may survive")

	wf, err := dsl.NewParser().ParseBytes([]byte(run.WorkflowName))
	require.NoError(t, err, "the stored source is what plan parses")
	assert.Equal(t, "tpl-cli-e2e", wf.Meta.Name)

	// The failure this fixes was only visible here, one command later.
	out := mustRun(t, "--config", e.cfgPath, "plan", id, "--targets", "web-1")
	assert.NotContains(t, out, "LE001")
	assert.Contains(t, out, "1 batch")

	after := storedRun(t, e, id)
	assert.NotEmpty(t, after.PlanJSON, "a template run plans and persists like any other")
	assert.NotEmpty(t, after.PlanHash)
}

// TestNewRefusesTemplateThatRendersNoWorkflow is the fail-closed half: the
// refusal happens at `new`, names the template, and leaves no run behind for an
// operator to mistake for something plannable. The document used here is the
// old dialect the repo shipped as an example until the template path was fixed.
func TestNewRefusesTemplateThatRendersNoWorkflow(t *testing.T) {
	defer resetRootFlags()
	t.Cleanup(restoreNewFlagVars)
	e := newCLIEnv(t)
	const oldDialect = "params:\n  - name: package\n    required: true\nworkflow:\n  target:\n    labels:\n      group: \"{{.package}}\"\n  approval: high\n"
	registerTemplate(t, e, "old", oldDialect,
		template.TemplateParam{Name: "package", Type: "string", Required: true})

	err := runErr(t, "--config", e.cfgPath, "new", "old", "--params", "package=nginx")
	require.Error(t, err)
	msg := err.Error()
	assert.Contains(t, msg, `template "old"`, "the error must name the cause, not a downstream id")
	assert.Contains(t, msg, "exit=2")
	assert.NotContains(t, msg, "cannot unmarshal !!str",
		"the old plan-time message reported a workflow decode error with no mention of the template")

	store := e.open(t)
	defer func() { _ = store.Close() }()
	runs, lerr := store.ListRuns(context.Background(), state.RunFilter{})
	require.NoError(t, lerr)
	assert.Empty(t, runs, "a refused instantiation must not create a run row")
}

// TestNewRefusesUnsubstitutedPlaceholder covers the case neither of the two
// compile gates catches: "{{.x}}" is a legal YAML string, so a template whose
// placeholder no parameter feeds would otherwise be stored, planned, and sent
// to a target as literal template syntax.
func TestNewRefusesUnsubstitutedPlaceholder(t *testing.T) {
	defer resetRootFlags()
	t.Cleanup(restoreNewFlagVars)
	e := newCLIEnv(t)
	// Only "package" is declared, so {{.target_group}} has nothing to fill it.
	registerTemplate(t, e, "half", tplWorkflowYAML,
		template.TemplateParam{Name: "package", Type: "string", Required: true})

	err := runErr(t, "--config", e.cfgPath, "new", "half", "--params", "package=nginx")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unsubstituted")
	assert.Contains(t, err.Error(), "target_group",
		"the message must name the placeholder that has no parameter behind it")

	store := e.open(t)
	defer func() { _ = store.Close() }()
	runs, lerr := store.ListRuns(context.Background(), state.RunFilter{})
	require.NoError(t, lerr)
	assert.Empty(t, runs)
}

// TestNewRefusesRenderedWorkflowThatFailsValidation covers the second gate on
// its own: this document parses (so deleting the validation leg would go
// unnoticed by the parse test) but declares an impossible window. Refusing it at
// `new` is what keeps the plan-time window gate from being the first place an
// operator hears about a typo in a template.
func TestNewRefusesRenderedWorkflowThatFailsValidation(t *testing.T) {
	defer resetRootFlags()
	t.Cleanup(restoreNewFlagVars)
	e := newCLIEnv(t)
	const badWindow = "name: bad-window\nversion: \"1.0\"\ntarget:\n  type: host\n  query: \"env={{.env}}\"\nwindow:\n  start: \"25:99\"\n  end: \"26:00\"\n  timezone: UTC\nbatches:\n  strategy: percent\n  steps: [1, 100]\nsteps:\n  - name: x\n    action: shell.exec\n    args:\n      cmd: \"true\"\n"
	registerTemplate(t, e, "badwin", badWindow,
		template.TemplateParam{Name: "env", Type: "string", Required: true})

	err := runErr(t, "--config", e.cfgPath, "new", "badwin", "--params", "env=prod")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "fails validation")
	assert.Contains(t, err.Error(), "LE020",
		"the validation code travels to the operator, who can then grep it")

	store := e.open(t)
	defer func() { _ = store.Close() }()
	runs, lerr := store.ListRuns(context.Background(), state.RunFilter{})
	require.NoError(t, lerr)
	assert.Empty(t, runs)
}

// TestWorkflowDisplayCollapsesInlineSource pins the human-output rendering:
// run.WorkflowName is a source, and a multi-line YAML document cannot be a
// table cell. The workflow's declared name is shown instead; anything that is
// not a parsable document (a path, a bare name, a corrupt source) is shown
// exactly as stored rather than guessed at.
func TestWorkflowDisplayCollapsesInlineSource(t *testing.T) {
	assert.Equal(t, "tpl-cli-e2e", workflowDisplay(tplWorkflowYAML))

	// `change create` stores a file path: one line, nothing to collapse.
	assert.Equal(t, "workflows/patch.yaml", workflowDisplay("workflows/patch.yaml"))
	assert.Equal(t, "plain-name", workflowDisplay("  plain-name  "))
	assert.Empty(t, workflowDisplay(""))

	corrupt := "name: broken\nsteps:\n  - name: x\n\t bad indentation\n"
	assert.Equal(t, strings.TrimSpace(corrupt), workflowDisplay(corrupt),
		"an unparsable source must stay recognisable to whoever is debugging it — "+
			"shown as stored, minus the surrounding whitespace a cell cannot use")

	noName := "target:\n  type: host\n  query: \"env=test\"\nsteps: []\n"
	assert.Equal(t, strings.TrimSpace(noName), workflowDisplay(noName),
		"a document without a declared name has nothing better to show")
}

// TestListAndShowPrintOneLineWorkflowForTemplateRuns guards the output shape the
// fix implies: every human surface that prints the workflow must stay on one
// line, and the JSON row must carry the workflow's name.
func TestListAndShowPrintOneLineWorkflowForTemplateRuns(t *testing.T) {
	defer resetRootFlags()
	t.Cleanup(restoreNewFlagVars)
	e := newCLIEnv(t)
	registerTemplate(t, e, "patch", tplWorkflowYAML,
		template.TemplateParam{Name: "package", Type: "string", Required: true},
		template.TemplateParam{Name: "target_group", Type: "string", Default: "web"})

	id := newRunID(t, e, "patch", "package=nginx")

	out := mustRun(t, "--config", e.cfgPath, "list", "--json")
	var env struct {
		Data []struct {
			ID           string `json:"id"`
			WorkflowName string `json:"workflow_name"`
			TemplateName string `json:"template_name"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal([]byte(out), &env), "cli output: %s", out)
	require.Len(t, env.Data, 1)
	assert.Equal(t, "tpl-cli-e2e", env.Data[0].WorkflowName)
	assert.Equal(t, "patch", env.Data[0].TemplateName,
		"the stored source stays reachable by id; the row shows the name")

	show := mustRun(t, "--config", e.cfgPath, "show", id)
	var workflowLine string
	for _, line := range strings.Split(show, "\n") {
		if strings.HasPrefix(line, "  Workflow:") {
			workflowLine = line
		}
	}
	require.NotEmpty(t, workflowLine, "show must still report the workflow, got: %s", show)
	assert.Equal(t, "  Workflow:    tpl-cli-e2e", workflowLine,
		"a multi-line source in this row would garble every column after it")
	assert.Contains(t, show, "  Template:    patch")
}
