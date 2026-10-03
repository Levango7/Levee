// compat_oracle_test.go is the external half of the compat contract: every
// mapping OUTPUT is checked against the REAL executor registry instead of
// restating the mapping in test form. The package's production code
// deliberately does not import internal/executor (R8 independence), so this
// oracle lives in the external test package — the library's dependency
// graph stays pure while the test still holds the mapping to the registry
// that will actually execute the imported workflow.
//
// This is the guard the roadmap prescribed after the flat mapping shipped
// four phantom actions (file.manage / svc.manage / user.manage / user.group):
// a playbook imported fine and then failed only at plan/execute time.
package compat_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nexus/levee/internal/compat"
	"github.com/nexus/levee/internal/executor"

	// Register the real modules with executor.DefaultExecutor — the same
	// blank imports the binary uses (internal/wiring/exec.go). Without them
	// the registry would be empty and the oracle vacuous.
	_ "github.com/nexus/levee/internal/executor/modules/file"
	_ "github.com/nexus/levee/internal/executor/modules/mysql"
	_ "github.com/nexus/levee/internal/executor/modules/pkg"
	_ "github.com/nexus/levee/internal/executor/modules/shell"
	_ "github.com/nexus/levee/internal/executor/modules/svc"
	_ "github.com/nexus/levee/internal/executor/modules/user"
)

// importOneTask imports a playbook whose single task is the given fragment
// (already indented to task level) and returns the resulting step.
func importOneTask(t *testing.T, fragment string) (mod, action string, err error) {
	t.Helper()
	pb := "---\n- hosts: all\n  tasks:\n    - name: probe\n" + fragment + "\n"
	wf, err := compat.NewAnsiblePlaybookImporter().ImportBytes([]byte(pb))
	if err != nil {
		return "", "", err
	}
	require.Len(t, wf.Steps, 1)
	return wf.Steps[0].Module, wf.Steps[0].Action, nil
}

// TestMappingResolvesToRealExecutorActions is the oracle: for every ansible
// construct the layer accepts, the resolved (module, action) must exist in
// the executor registry that actually runs the step. A phantom action fails
// here instead of surfacing at execution time.
func TestMappingResolvesToRealExecutorActions(t *testing.T) {
	ex := executor.DefaultExecutor()
	require.NotNil(t, ex)
	// Sanity: the registry must be populated, or the oracle is vacuous.
	_, ok := ex.Module("shell")
	require.True(t, ok, "shell module must be registered — blank imports failed?")

	cases := []struct {
		name     string
		fragment string
		mod      string
		action   string
	}{
		{"shell", "      shell: echo hi", "shell", "exec"},
		{"command", "      command: ls -la", "shell", "exec"},
		{"copy", "      copy:\n        src: /a\n        dest: /b", "file", "copy"},
		{"template", "      template:\n        src: /a.j2\n        dest: /b", "file", "template"},
		{"apt present", "      apt: name=nginx state=present", "pkg", "install"},
		{"apt absent", "      apt: name=nginx state=absent", "pkg", "remove"},
		{"yum latest", "      yum: name=nginx state=latest", "pkg", "upgrade"},
		{"yum default", "      yum: name=nginx", "pkg", "install"},
		{"svc started", "      service: name=nginx state=started", "svc", "start"},
		{"svc stopped", "      service: name=nginx state=stopped", "svc", "stop"},
		{"svc restarted", "      service: name=nginx state=restarted", "svc", "restart"},
		{"svc reloaded", "      service: name=nginx state=reloaded", "svc", "reload"},
		{"svc enabled yes", "      service: name=nginx enabled=yes", "svc", "enable"},
		{"svc enabled no", "      service: name=nginx enabled=no", "svc", "disable"},
		{"user present", "      user: name=app state=present", "user", "add"},
		{"user default", "      user: name=app", "user", "add"},
		{"user absent", "      user: name=app state=absent", "user", "remove"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mod, action, err := importOneTask(t, tc.fragment)
			require.NoError(t, err)
			assert.Equal(t, tc.mod, mod)
			assert.Equal(t, tc.action, action)

			m, ok := ex.Module(mod)
			require.True(t, ok, "module %q is not registered in the executor", mod)
			assert.Contains(t, m.Actions(), action,
				"resolved action %s.%s does not exist in the executor registry (phantom action)", mod, action)
		})
	}
}

// TestUnmappableConstructsAreRefused pins the fail-closed side: constructs
// with no faithful LEVEE action must FAIL the import with an error naming
// the reason — never silently produce a step with different semantics.
func TestUnmappableConstructsAreRefused(t *testing.T) {
	cases := []struct {
		name     string
		fragment string
		wantMsg  string
	}{
		{"file module", "      file: path=/srv state=directory", "no faithful LEVEE action"},
		{"group module", "      group: name=appgrp", "no LEVEE executor action"},
		{"apt bad state", "      apt: name=nginx state=frobnicated", "state="},
		{"service missing state", "      service: name=nginx", "requires state"},
		{"service state+enabled", "      service: name=nginx state=started enabled=yes", "single-purpose"},
		{"user bad state", "      user: name=app state=deleted", "state="},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, _, err := importOneTask(t, tc.fragment)
			require.Error(t, err, "unmappable construct must be refused")
			assert.Contains(t, err.Error(), tc.wantMsg,
				"error must name the reason (want substring %q, got %q)", tc.wantMsg, err.Error())
		})
	}
}
