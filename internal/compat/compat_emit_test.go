// compat_emit_test.go — the translation round-trip: an imported playbook
// must marshal back into LEVEELang YAML that the standard pipeline accepts
// (parse + validate — the same two doors `levee compile` strict mode uses).
// This is what makes `levee import ansible` trustworthy: the emitted file is
// not "something we printed", it is a workflow the compiler has accepted.
package compat

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nexus/levee/internal/dsl"
)

func TestImportedWorkflowMarshalsAndRecompiles(t *testing.T) {
	const pb = `---
- name: web bootstrap
  hosts: web-1,web-2
  tasks:
    - name: copy config
      copy:
        src: /local/app.conf
        dest: /etc/app/app.conf
    - name: install nginx
      apt: name=nginx state=present
    - name: start nginx
      service: name=nginx state=started
    - name: create user
      user: name=appuser state=present
    - name: verify
      command: systemctl is-active nginx
`
	wf, err := NewAnsiblePlaybookImporter().ImportBytes([]byte(pb))
	require.NoError(t, err)

	out, err := dsl.MarshalWorkflow(wf)
	require.NoError(t, err, "the importer's supported field set must be fully emittable")

	// Door 1: the emitted YAML must parse.
	reparsed, err := dsl.NewParser().ParseBytes(out)
	require.NoError(t, err, "emitted workflow must parse:\n%s", string(out))

	// Door 2: and pass validation (strict-mode compile path).
	verrs := dsl.NewValidator().Validate(reparsed)
	assert.Empty(t, verrs, "emitted workflow must validate:\n%s", string(out))

	// Content survived the round trip.
	assert.Equal(t, "web bootstrap", reparsed.Meta.Name,
		"the play's name must become the workflow name (LE002 otherwise)")
	require.Len(t, reparsed.Steps, 5)
	assert.Equal(t, "file", reparsed.Steps[0].Module)
	assert.Equal(t, "copy", reparsed.Steps[0].Action)
	assert.Equal(t, "pkg", reparsed.Steps[1].Module)
	assert.Equal(t, "install", reparsed.Steps[1].Action)
	assert.Equal(t, "svc", reparsed.Steps[2].Module)
	assert.Equal(t, "start", reparsed.Steps[2].Action)
	assert.Equal(t, "user", reparsed.Steps[3].Module)
	assert.Equal(t, "add", reparsed.Steps[3].Action)
	assert.Equal(t, "shell", reparsed.Steps[4].Module)
	assert.Equal(t, "exec", reparsed.Steps[4].Action)
	require.Len(t, reparsed.Targets, 1)
	assert.Equal(t, []string{"web-1", "web-2"}, reparsed.Targets[0].Hosts)
}
