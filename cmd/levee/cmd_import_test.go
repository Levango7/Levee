// cmd_import_test.go — `levee import ansible`: the emitted file must be
// compilable (parse + validate), the name must derive deterministically, and
// unmappable constructs must surface as errors.
package main

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nexus/levee/internal/dsl"
)

const samplePlaybook = `---
- name: web bootstrap
  hosts: web-1,web-2
  tasks:
    - name: install nginx
      apt: name=nginx state=present
    - name: start nginx
      service: name=nginx state=started
`

func writePlaybook(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "bootstrap.yaml")
	require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
	return path
}

func TestImportAnsible_WritesCompilableWorkflowToFile(t *testing.T) {
	resetImportFlags()
	defer resetImportFlags()
	cmd := newImportAnsibleCmd()

	path := writePlaybook(t, samplePlaybook)
	outPath := filepath.Join(t.TempDir(), "web-bootstrap.yaml")
	importOptOut = outPath

	require.NoError(t, runImportAnsible(cmd, []string{path}))

	data, err := os.ReadFile(outPath)
	require.NoError(t, err)

	reparsed, err := dsl.NewParser().ParseBytes(data)
	require.NoError(t, err, "the written file must parse:\n%s", string(data))
	assert.Empty(t, dsl.NewValidator().Validate(reparsed), "the written file must validate")
	assert.Equal(t, "web bootstrap", reparsed.Meta.Name, "the play name becomes the workflow name")
	require.Len(t, reparsed.Steps, 2)
	assert.Equal(t, "pkg", reparsed.Steps[0].Module)
	assert.Equal(t, "svc", reparsed.Steps[1].Module)
}

func TestImportAnsible_DerivesNameFromFileName(t *testing.T) {
	resetImportFlags()
	defer resetImportFlags()
	cmd := newImportAnsibleCmd()

	// A playbook without a play name: the workflow name must be derived from
	// the file (LE002 would otherwise reject the emitted YAML).
	path := writePlaybook(t, `---
- hosts: web-1
  tasks:
    - name: restart
      service: name=nginx state=restarted
`)
	var buf bytes.Buffer
	cmd.SetOut(&buf)

	require.NoError(t, runImportAnsible(cmd, []string{path}))
	out := buf.String()

	reparsed, err := dsl.NewParser().ParseBytes([]byte(out))
	require.NoError(t, err, "stdout output must parse:\n%s", out)
	assert.Equal(t, "bootstrap", reparsed.Meta.Name)
}

func TestImportAnsible_UnmappableConstructFailsWithGuidance(t *testing.T) {
	resetImportFlags()
	defer resetImportFlags()
	cmd := newImportAnsibleCmd()

	path := writePlaybook(t, `---
- name: bad
  hosts: web-1
  tasks:
    - name: ensure dir
      file: path=/srv state=directory
`)
	err := runImportAnsible(cmd, []string{path})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no faithful LEVEE action",
		"the refusal must name the reason so the operator can rewrite the task")
}

func TestImportAnsible_MissingFileFails(t *testing.T) {
	resetImportFlags()
	defer resetImportFlags()
	cmd := newImportAnsibleCmd()

	err := runImportAnsible(cmd, []string{filepath.Join(t.TempDir(), "nope.yaml")})
	require.Error(t, err)
}
