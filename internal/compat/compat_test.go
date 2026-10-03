package compat

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nexus/levee/internal/dsl"
)

// samplePlaybook is a representative Ansible playbook exercising the shell,
// service and apt modules with both legacy (key=value) and modern (mapping)
// argument forms. It is used by several tests below.
const samplePlaybook = `---
- hosts: webservers
  become: yes
  tasks:
    - name: install nginx
      apt: name=nginx state=present
    - name: start nginx
      service: name=nginx state=started
    - name: run script
      shell: /opt/deploy.sh
`

// newImporter returns a fresh AnsiblePlaybookImporter for use in tests.
func newImporter() *AnsiblePlaybookImporter {
	return NewAnsiblePlaybookImporter()
}

// TestNameReturnsAnsible verifies that Name() returns "ansible" (req #13).
func TestNameReturnsAnsible(t *testing.T) {
	a := newImporter()
	assert.Equal(t, "ansible", a.Name())
}

// TestImportBytesBasic verifies that ImportBytes parses a valid playbook and
// returns a non-nil Workflow with the expected structure (req #1).
func TestImportBytesBasic(t *testing.T) {
	a := newImporter()
	wf, err := a.ImportBytes([]byte(samplePlaybook))
	require.NoError(t, err)
	require.NotNil(t, wf)

	// One play with one hosts entry -> one target group.
	assert.Len(t, wf.Targets, 1)
	assert.Equal(t, "webservers", wf.Targets[0].Name)
	assert.Contains(t, wf.Targets[0].Hosts, "webservers")

	// Three tasks -> three steps.
	assert.Len(t, wf.Steps, 3)

	// become=yes should be recorded in metadata.
	assert.Contains(t, wf.Meta.Description, "become=yes")
}

// TestImportFromFile verifies that Import reads a playbook from a temporary
// file and returns the same result as ImportBytes (req #2).
func TestImportFromFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "playbook.yml")
	require.NoError(t, os.WriteFile(path, []byte(samplePlaybook), 0o644))

	a := newImporter()
	wf, err := a.Import(path)
	require.NoError(t, err)
	require.NotNil(t, wf)
	assert.Len(t, wf.Steps, 3)
}

// TestImportFileMissing verifies that Import on a non-existent file returns
// an error wrapping ErrImportFailed.
func TestImportFileMissing(t *testing.T) {
	a := newImporter()
	_, err := a.Import(filepath.Join(t.TempDir(), "nope.yml"))
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrImportFailed), "want ErrImportFailed, got %v", err)
}

// TestShellModuleMapping verifies that a shell task maps to the shell.exec
// action with the command stored under args.cmd (req #3).
func TestShellModuleMapping(t *testing.T) {
	const pb = `---
- hosts: all
  tasks:
    - name: run deploy
      shell: /opt/deploy.sh
`
	a := newImporter()
	wf, err := a.ImportBytes([]byte(pb))
	require.NoError(t, err)
	require.Len(t, wf.Steps, 1)

	s := wf.Steps[0]
	assert.Equal(t, "run deploy", s.Name)
	assert.Equal(t, "shell", s.Module)
	assert.Equal(t, "exec", s.Action)
	assert.Equal(t, "/opt/deploy.sh", s.Args["cmd"])
}

// TestCommandModuleMapping verifies that a command task maps to shell.exec
// (req #4). Ansible's command module is functionally equivalent to shell for
// the MVP subset, so both map to the same LEVEE action.
func TestCommandModuleMapping(t *testing.T) {
	const pb = `---
- hosts: all
  tasks:
    - name: list files
      command: ls -la /var/log
`
	a := newImporter()
	wf, err := a.ImportBytes([]byte(pb))
	require.NoError(t, err)
	require.Len(t, wf.Steps, 1)

	s := wf.Steps[0]
	assert.Equal(t, "shell", s.Module)
	assert.Equal(t, "exec", s.Action)
	assert.Equal(t, "ls -la /var/log", s.Args["cmd"])
}

// TestFileCopyTemplateModuleMapping verifies the redefined contract: copy and
// template map to their real actions, while the ansible `file` module is
// REFUSED with an actionable error — the old flat table mapped it to
// file.manage, an action that does not exist in the executor registry (the
// playbook imported fine and then failed at execution time).
func TestFileCopyTemplateModuleMapping(t *testing.T) {
	const pb = `---
- hosts: all
  tasks:
    - name: copy config
      copy:
        src: /local/app.conf
        dest: /etc/app/app.conf
    - name: render template
      template:
        src: /local/app.j2
        dest: /etc/app/app.conf
`
	a := newImporter()
	wf, err := a.ImportBytes([]byte(pb))
	require.NoError(t, err)
	require.Len(t, wf.Steps, 2)

	// copy -> file.copy
	s0 := wf.Steps[0]
	assert.Equal(t, "file", s0.Module)
	assert.Equal(t, "copy", s0.Action)
	assert.Equal(t, "/local/app.conf", s0.Args["src"])
	assert.Equal(t, "/etc/app/app.conf", s0.Args["dest"])

	// template -> file.template
	s1 := wf.Steps[1]
	assert.Equal(t, "file", s1.Module)
	assert.Equal(t, "template", s1.Action)
	assert.Equal(t, "/local/app.j2", s1.Args["src"])

	// file -> refused, with the reason and the alternatives in the message.
	const pbFile = `---
- hosts: all
  tasks:
    - name: ensure dir
      file: path=/srv/app state=directory
`
	_, err = a.ImportBytes([]byte(pbFile))
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrUnsupportedModule), "want ErrUnsupportedModule, got %v", err)
	assert.Contains(t, err.Error(), "no faithful LEVEE action")
}

// TestPkgModuleMapping verifies that apt and yum both map to pkg.install
// (req #6).
func TestPkgModuleMapping(t *testing.T) {
	const pb = `---
- hosts: all
  tasks:
    - name: apt install
      apt: name=nginx state=present
    - name: yum install
      yum: name=nginx state=present
`
	a := newImporter()
	wf, err := a.ImportBytes([]byte(pb))
	require.NoError(t, err)
	require.Len(t, wf.Steps, 2)

	for _, s := range wf.Steps {
		assert.Equal(t, "pkg", s.Module)
		assert.Equal(t, "install", s.Action)
		assert.Equal(t, "nginx", s.Args["name"])
		assert.Equal(t, "present", s.Args["state"])
	}
}

// TestServiceModuleMapping verifies the state-aware service mapping: state
// selects the real svc action instead of the old state-blind svc.manage.
func TestServiceModuleMapping(t *testing.T) {
	const pb = `---
- hosts: all
  tasks:
    - name: start nginx
      service: name=nginx state=started
    - name: enable nginx
      service: name=nginx enabled=yes
    - name: stop nginx
      service: name=nginx state=stopped
`
	a := newImporter()
	wf, err := a.ImportBytes([]byte(pb))
	require.NoError(t, err)
	require.Len(t, wf.Steps, 3)

	assert.Equal(t, "svc", wf.Steps[0].Module)
	assert.Equal(t, "start", wf.Steps[0].Action)
	assert.Equal(t, "nginx", wf.Steps[0].Args["name"])
	assert.Equal(t, "started", wf.Steps[0].Args["state"])

	assert.Equal(t, "enable", wf.Steps[1].Action)
	assert.Equal(t, "stop", wf.Steps[2].Action)
}

// TestMultipleTasks verifies that a playbook with several tasks imports all
// of them in order (req #8).
func TestMultipleTasks(t *testing.T) {
	const pb = `---
- hosts: all
  tasks:
    - name: task one
      shell: echo 1
    - name: task two
      shell: echo 2
    - name: task three
      shell: echo 3
    - name: task four
      apt: name=foo state=present
`
	a := newImporter()
	wf, err := a.ImportBytes([]byte(pb))
	require.NoError(t, err)
	require.Len(t, wf.Steps, 4)

	expected := []string{"task one", "task two", "task three", "task four"}
	for i, want := range expected {
		assert.Equal(t, want, wf.Steps[i].Name)
	}
	// The last task should be pkg.install, not shell.exec.
	assert.Equal(t, "pkg", wf.Steps[3].Module)
	assert.Equal(t, "install", wf.Steps[3].Action)
}

// TestHostsToTargets verifies that the hosts field maps to Workflow.Targets
// (req #9). Both single-string and list forms are exercised.
func TestHostsToTargets(t *testing.T) {
	t.Run("single string", func(t *testing.T) {
		const pb = `---
- hosts: webservers
  tasks:
    - name: t
      shell: echo hi
`
		a := newImporter()
		wf, err := a.ImportBytes([]byte(pb))
		require.NoError(t, err)
		require.Len(t, wf.Targets, 1)
		assert.Equal(t, "webservers", wf.Targets[0].Name)
		assert.Equal(t, []string{"webservers"}, wf.Targets[0].Hosts)
	})

	t.Run("comma separated", func(t *testing.T) {
		const pb = `---
- hosts: web1,web2,web3
  tasks:
    - name: t
      shell: echo hi
`
		a := newImporter()
		wf, err := a.ImportBytes([]byte(pb))
		require.NoError(t, err)
		require.Len(t, wf.Targets, 1)
		assert.Equal(t, []string{"web1", "web2", "web3"}, wf.Targets[0].Hosts)
	})

	t.Run("list form", func(t *testing.T) {
		const pb = `---
- hosts:
    - web1
    - web2
  tasks:
    - name: t
      shell: echo hi
`
		a := newImporter()
		wf, err := a.ImportBytes([]byte(pb))
		require.NoError(t, err)
		require.Len(t, wf.Targets, 1)
		assert.Equal(t, []string{"web1", "web2"}, wf.Targets[0].Hosts)
	})
}

// TestUnsupportedModule verifies that an unknown module returns an error
// wrapping ErrUnsupportedModule (req #10).
func TestUnsupportedModule(t *testing.T) {
	const pb = `---
- hosts: all
  tasks:
    - name: use docker
      docker_container:
        name: web
        image: nginx
`
	a := newImporter()
	_, err := a.ImportBytes([]byte(pb))
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrUnsupportedModule), "want ErrUnsupportedModule, got %v", err)
	assert.Contains(t, err.Error(), "docker_container")
}

// TestEmptyPlaybook verifies that an empty playbook list returns
// ErrEmptyPlaybook (req #11).
func TestEmptyPlaybook(t *testing.T) {
	t.Run("empty list", func(t *testing.T) {
		a := newImporter()
		_, err := a.ImportBytes([]byte("[]"))
		require.Error(t, err)
		assert.True(t, errors.Is(err, ErrEmptyPlaybook), "want ErrEmptyPlaybook, got %v", err)
	})

	t.Run("empty document", func(t *testing.T) {
		a := newImporter()
		_, err := a.ImportBytes([]byte(""))
		require.Error(t, err)
		// An empty string unmarshals to nil -> len(plays)==0 -> ErrEmptyPlaybook.
		assert.True(t, errors.Is(err, ErrEmptyPlaybook), "want ErrEmptyPlaybook, got %v", err)
	})
}

// TestInvalidYAML verifies that malformed YAML returns an error wrapping
// ErrInvalidPlaybook (req #12).
func TestInvalidYAML(t *testing.T) {
	const pb = `---
- hosts: all
   tasks: this is not valid
    - broken
`
	a := newImporter()
	_, err := a.ImportBytes([]byte(pb))
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrInvalidPlaybook), "want ErrInvalidPlaybook, got %v", err)
}

// TestInvalidPlaybookShape verifies that a YAML document that is not a list
// of plays (e.g. a scalar) returns ErrInvalidPlaybook.
func TestInvalidPlaybookShape(t *testing.T) {
	const pb = `just a string`
	a := newImporter()
	_, err := a.ImportBytes([]byte(pb))
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrInvalidPlaybook), "want ErrInvalidPlaybook, got %v", err)
}

// TestCompatLayerInterface verifies that *AnsiblePlaybookImporter satisfies
// the CompatLayer interface at compile time.
func TestCompatLayerInterface(t *testing.T) {
	var _ CompatLayer = (*AnsiblePlaybookImporter)(nil)
	var _ CompatLayer = NewAnsiblePlaybookImporter()
}

// TestUserGroupModuleMapping verifies the redefined user/group contract:
// user maps state-aware to user.add/user.remove (user.add is idempotent, so
// ansible's present = create-or-ensure is faithful), while the ansible
// `group` module is REFUSED — the old table's user.group action does not
// exist anywhere in the executor registry.
func TestUserGroupModuleMapping(t *testing.T) {
	const pb = `---
- hosts: all
  tasks:
    - name: create user
      user: name=appuser state=present
    - name: remove stale user
      user: name=olduser state=absent
`
	a := newImporter()
	wf, err := a.ImportBytes([]byte(pb))
	require.NoError(t, err)
	require.Len(t, wf.Steps, 2)

	assert.Equal(t, "user", wf.Steps[0].Module)
	assert.Equal(t, "add", wf.Steps[0].Action)
	assert.Equal(t, "user", wf.Steps[1].Module)
	assert.Equal(t, "remove", wf.Steps[1].Action)

	const pbGroup = `---
- hosts: all
  tasks:
    - name: create group
      group: name=appgroup state=present
`
	_, err = a.ImportBytes([]byte(pbGroup))
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrUnsupportedModule), "want ErrUnsupportedModule, got %v", err)
	assert.Contains(t, err.Error(), "no LEVEE executor action")
}

// TestModernMappingArgs verifies that modern Ansible mapping-style arguments
// are passed through to the step args verbatim.
func TestModernMappingArgs(t *testing.T) {
	const pb = `---
- hosts: all
  tasks:
    - name: apt modern
      apt:
        name: nginx
        state: present
        update_cache: true
`
	a := newImporter()
	wf, err := a.ImportBytes([]byte(pb))
	require.NoError(t, err)
	require.Len(t, wf.Steps, 1)

	s := wf.Steps[0]
	assert.Equal(t, "pkg", s.Module)
	assert.Equal(t, "install", s.Action)
	assert.Equal(t, "nginx", s.Args["name"])
	assert.Equal(t, "present", s.Args["state"])
	assert.Equal(t, true, s.Args["update_cache"])
}

// TestMultiplePlays verifies that a playbook with multiple plays accumulates
// targets and steps from all plays.
func TestMultiplePlays(t *testing.T) {
	const pb = `---
- hosts: web
  tasks:
    - name: web task
      shell: echo web
- hosts: db
  tasks:
    - name: db task
      shell: echo db
`
	a := newImporter()
	wf, err := a.ImportBytes([]byte(pb))
	require.NoError(t, err)
	assert.Len(t, wf.Targets, 2)
	assert.Len(t, wf.Steps, 2)
	assert.Equal(t, "web", wf.Targets[0].Name)
	assert.Equal(t, "db", wf.Targets[1].Name)
}

// TestPlayWithoutTasks verifies that a play with no tasks is accepted (it
// contributes only its target group).
func TestPlayWithoutTasks(t *testing.T) {
	const pb = `---
- hosts: all
`
	a := newImporter()
	wf, err := a.ImportBytes([]byte(pb))
	require.NoError(t, err)
	assert.Len(t, wf.Targets, 1)
	assert.Empty(t, wf.Steps)
}

// TestTaskWithoutModule verifies that a task with only reserved keys and no
// module returns ErrInvalidPlaybook.
func TestTaskWithoutModule(t *testing.T) {
	const pb = `---
- hosts: all
  tasks:
    - name: no module here
      when: ansible_os_family == "Debian"
`
	a := newImporter()
	_, err := a.ImportBytes([]byte(pb))
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrInvalidPlaybook), "want ErrInvalidPlaybook, got %v", err)
}

// TestShellWithMappingArgs verifies that a shell task using the modern
// mapping form passes its keys (cmd, chdir, ...) through to args.
func TestShellWithMappingArgs(t *testing.T) {
	const pb = `---
- hosts: all
  tasks:
    - name: run in dir
      shell:
        cmd: ./deploy.sh
        chdir: /opt/app
`
	a := newImporter()
	wf, err := a.ImportBytes([]byte(pb))
	require.NoError(t, err)
	require.Len(t, wf.Steps, 1)

	s := wf.Steps[0]
	assert.Equal(t, "shell", s.Module)
	assert.Equal(t, "exec", s.Action)
	assert.Equal(t, "./deploy.sh", s.Args["cmd"])
	assert.Equal(t, "/opt/app", s.Args["chdir"])
}

// TestWorkflowIsDSLType verifies that the returned workflow is a *dsl.Workflow,
// ensuring the compat layer produces the correct AST type for downstream
// consumption.
func TestWorkflowIsDSLType(t *testing.T) {
	a := newImporter()
	wf, err := a.ImportBytes([]byte(samplePlaybook))
	require.NoError(t, err)
	require.IsType(t, &dsl.Workflow{}, wf)
}
