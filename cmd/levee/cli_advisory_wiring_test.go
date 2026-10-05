// cli_advisory_wiring_test.go pins the advisory channel at its call sites.
//
// internal/dsl/compile_warnings_test.go proves Advise is correct; that alone was
// never the failure mode here. The catalogue has classified LE033/LE094/LE095/
// LE096 as CompileWarning since those codes were written, the spec promises them
// in three places, and nothing produced them — because there was nowhere for a
// warning to go: every consumer of Validator.Validate treats a non-empty result
// as fatal. So what matters at a call site is "the advisory reaches the
// operator", "it reaches stderr and not the document", and "it never changes the
// command's outcome". Each test drives a real command, so deleting one Advise
// call, moving its prose onto stdout, or making it fatal turns a test red — which
// is the difference between a wired gate and a function that exists.
package main

import (
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nexus/levee/internal/dsl"
)

// bareGovernanceWorkflow declares target and steps and nothing else: legal per
// spec, and it triggers all three "block missing" advisories at once.
const bareGovernanceWorkflow = `name: adv-cli
target:
  type: host
  query: "env=test"
steps:
  - name: reload
    action: shell.exec
    args:
      cmd: "systemctl reload nginx"
`

// advisoryLines returns the advisory prose lines from a captured stream.
func advisoryLines(stream string) []string {
	var out []string
	for _, line := range strings.Split(stream, "\n") {
		if strings.HasPrefix(line, "advisory: ") {
			out = append(out, line)
		}
	}
	return out
}

func TestCompileAdvisoriesReachStderrAndSummary(t *testing.T) {
	path := writeTempYAML(t, bareGovernanceWorkflow)
	stdout, stderr, err := runCompileStreams(t, path, nil)

	require.NoError(t, err, "advisories must not fail a compile: every one of them reports a "+
		"block that spec says may be omitted")

	lines := advisoryLines(stderr)
	require.Len(t, lines, 3, "a workflow missing window, approval and batches is told about all three:\n%s", stderr)
	assert.Contains(t, lines[0], "LE095")
	assert.Contains(t, lines[1], "LE094")
	assert.Contains(t, lines[2], "LE096")
	assert.Contains(t, stdout, "advisories=3", "the human summary counts them, so a script can grep one line")
	assert.NotContains(t, stdout, "advisory:", "the prose goes to stderr only")
}

func TestCompileSummaryCountIsTheSameListAsStderr(t *testing.T) {
	// The count and the lines come from one call, but a future edit could count
	// advisories from a filtered list while printing the unfiltered one.
	cases := []struct {
		name string
		src  string
		want int
	}{
		{name: "nothing declared", src: bareGovernanceWorkflow, want: 3},
		// validCompileYAML declares approval and batches but no between-batches
		// check, so it is told about the missing window (LE095) and the missing
		// post_batch gate (LE052).
		{name: "window only missing", src: validCompileYAML, want: 2},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := writeTempYAML(t, tc.src)
			stdout, stderr, err := runCompileStreams(t, path, nil)
			require.NoError(t, err)
			assert.Len(t, advisoryLines(stderr), tc.want, "stderr: %s", stderr)
			assert.Contains(t, stdout, fmt.Sprintf("advisories=%d)\n", tc.want),
				"summary agrees with the lines: %s", stdout)
		})
	}
}

func TestAdvisoriesChangeNoCompileMode(t *testing.T) {
	// The "never blocks" claim has to hold in every mode a consumer runs, not
	// just the default one: strict escalates type errors, and an advisory that
	// leaked into that path would refuse every bare workflow.
	modes := []struct {
		name  string
		setup func()
	}{
		{"default (strict)", nil},
		{"lenient", func() { compileOptStrict = false; compileOptLenient = true }},
		{"check-only", func() { compileOptCheckOnly = true }},
		{"emit-ir", func() { compileOptIR = true }},
	}
	for _, m := range modes {
		t.Run(m.name, func(t *testing.T) {
			path := writeTempYAML(t, bareGovernanceWorkflow)
			stdout, stderr, err := runCompileStreams(t, path, m.setup)
			require.NoError(t, err)
			assert.NotEmpty(t, advisoryLines(stderr), "the advisory must survive into this mode, not be dropped by it")
			assert.NotContains(t, stdout, "advisory:")
		})
	}
}

func TestImportAdvisesWithoutPollutingTheDocument(t *testing.T) {
	resetImportFlags()
	defer resetImportFlags()

	playbook := writePlaybook(t, samplePlaybook)
	stdout, stderr, err := runSplit(t, "import", "ansible", playbook)
	require.NoError(t, err)

	// stdout is the deliverable: if advisory prose landed in front of it, the
	// parser would fail on the first line.
	_, perr := dsl.NewParser().ParseBytes([]byte(stdout))
	require.NoError(t, perr, "imported workflow must parse:\n%s", stdout)

	assert.NotEmpty(t, advisoryLines(stderr),
		"a translated playbook has no window, no approval tier and one batch — the operator "+
			"has to learn that before the first rollout, not at it")
}

func TestNewAdvisesAboutTheRenderedTemplate(t *testing.T) {
	e := newCLIEnv(t)
	defer restoreNewFlagVars()

	registerTemplate(t, e, "bare-cli", `name: bare-cli
target:
  type: host
  query: "env=test"
steps:
  - name: reload
    action: shell.exec
    args:
      cmd: "systemctl reload nginx"
`)

	stdout, stderr, err := runSplit(t, "--config", e.cfgPath, "new", "bare-cli")
	require.NoError(t, err, "stdout: %s", stdout)

	lines := advisoryLines(stderr)
	require.NotEmpty(t, lines, "the run was minted from a document with no governance blocks declared")
	for _, l := range lines {
		assert.Contains(t, l, "advisory: template bare-cli", "each advisory names the template it is about: %s", l)
	}
	assert.NotContains(t, stdout, "advisory:", "the run report on stdout stays clean")
}
