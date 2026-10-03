// cmd_run_test.go — the `levee run --shell` debug command. The happy-path
// rendering is asserted directly; the error paths pin the exit-code contract
// ([exit=N] markers parsed by root.go's exitCodeFor) that scripts depend on.
package main

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nexus/levee/internal/executor"
)

func TestRunCmd_EmptyShellRefused(t *testing.T) {
	resetRunFlags()
	cmd := newRunCmd()
	defer resetRunFlags()

	err := runRun(cmd, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "--shell")
	assert.Contains(t, err.Error(), "[exit=2]")
}

func TestRunCmd_NonZeroExitPropagatesChildCode(t *testing.T) {
	// `exit 3` works under both cmd /c and sh -c.
	resetRunFlags()
	cmd := newRunCmd()
	runOptShell = "exit 3"
	defer resetRunFlags()

	err := runRun(cmd, nil)
	require.Error(t, err, "a non-zero child exit must surface as a CLI error")
	assert.Contains(t, err.Error(), "code 3")
	assert.Contains(t, err.Error(), "[exit=3]", "the child's exit code must propagate via the [exit=N] contract")
}

func TestRunCmd_SuccessEchoesOutput(t *testing.T) {
	resetRunFlags()
	cmd := newRunCmd()
	runOptShell = "echo levee-run-ok"
	defer resetRunFlags()

	require.NoError(t, runRun(cmd, nil),
		"a zero child exit must not produce a CLI error")
}

func TestPrintRunResult_HumanAndJSON(t *testing.T) {
	result := &executor.ShellRunResult{
		Command:  "echo hi",
		ExitCode: 0,
		Stdout:   "hi\n",
		Stderr:   "",
		Duration: 5 * time.Millisecond,
	}

	prevJSON := optJSON
	defer func() { optJSON = prevJSON }()

	var buf bytes.Buffer
	optJSON = false
	printRunResult(&buf, result)
	out := buf.String()
	assert.Contains(t, out, "echo hi")
	assert.Contains(t, out, "exit:     0")
	assert.Contains(t, out, "--- stdout ---")
	assert.Contains(t, out, "hi")

	buf.Reset()
	optJSON = true
	printRunResult(&buf, result)
	out = buf.String()
	assert.Contains(t, out, `"command"`)
	assert.Contains(t, out, `"exit_code": 0`)
	assert.Contains(t, out, `"stdout"`)
}

func TestLastLines(t *testing.T) {
	assert.Equal(t, "(none)", lastLines("", 3))
	assert.Equal(t, "b | c", lastLines("a\nb\nc\n", 2))
	long := strings.Repeat("x", 500)
	assert.LessOrEqual(t, len(lastLines(long, 1)), 400, "error messages must stay bounded")
}
