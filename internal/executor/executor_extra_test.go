// executor_extra_test.go pins the IsIdempotent registry lookup that the
// wiring layer's resumable-retry logic depends on
// (docs/design-cluster-failover.md "断点续跑 v1").
package executor_test

import (
	"testing"

	"github.com/nexus/levee/internal/executor"
	"github.com/nexus/levee/internal/executor/modules/file"
	"github.com/nexus/levee/internal/executor/modules/pkg"
	"github.com/nexus/levee/internal/executor/modules/shell"
	"github.com/nexus/levee/internal/executor/modules/svc"
	"github.com/nexus/levee/internal/executor/modules/user"

	"github.com/stretchr/testify/assert"
)

func TestIsIdempotent_KnownModules(t *testing.T) {
	e := executor.NewExecutor()
	e.RegisterModule(file.New())
	e.RegisterModule(pkg.New())
	e.RegisterModule(shell.New())
	e.RegisterModule(svc.New())
	e.RegisterModule(user.New())

	assert.True(t, e.IsIdempotent("file"), "file module declares idempotent")
	assert.True(t, e.IsIdempotent("pkg"), "pkg module declares idempotent")
	assert.True(t, e.IsIdempotent("svc"), "svc module declares idempotent")
	assert.True(t, e.IsIdempotent("user"), "user module declares idempotent")
	assert.False(t, e.IsIdempotent("shell"), "shell module declares non-idempotent")
}

func TestIsIdempotent_UnknownModuleIsConservative(t *testing.T) {
	e := executor.NewExecutor()
	// No module registered under "ghost": resumable retry must never assume
	// idempotency that was not explicitly declared, so it reports false.
	assert.False(t, e.IsIdempotent("ghost"), "unknown module must be treated as non-idempotent")
}

func TestIsIdempotent_DefaultRegistry(t *testing.T) {
	// The default process-wide executor is populated by the modules' init()
	// funcs; verify the lookup resolves against it.
	assert.True(t, executor.DefaultExecutor().IsIdempotent("file"))
	assert.False(t, executor.DefaultExecutor().IsIdempotent("shell"))
}
