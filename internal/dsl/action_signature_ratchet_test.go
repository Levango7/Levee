package dsl_test

// action_signature_ratchet_test.go keeps the compile-time signature table
// (internal/dsl/typechecker.go, actionSignatures) and the runtime module
// registry describing the same set of actions.
//
// Why a ratchet and not a one-off: the table is deliberately partial, and an
// action that is NOT in it is SKIPPED by the type checker rather than refused.
// That makes every omission silent — pg.query was absent for as long as the pg
// module has existed, so PostgreSQL steps (the ones whose SQL the review gate
// scores) were the only SQL steps whose arguments went unchecked at compile
// time. Nothing failed; nothing was checked.
//
// The two directions mean different things:
//
//   - executable → table: an action a module can run but the table omits is an
//     action whose args are unchecked. It must be listed here (typed) or in
//     untypedActions (an exemption with a reason), so adding a module action
//     forces a decision instead of inheriting silence;
//   - table → executable: a key with no module behind it is a vocabulary row
//     that promises an action the platform cannot run. `shell.command` and
//     `file.write` sat in the table for a long time with no module (the shell
//     module runs exec/script; the file module runs copy/template).
//
// It lives in package dsl_test because it needs the executor registry, and
// internal/executor imports internal/dsl (executor/irreversible.go) — from
// inside package dsl that would close an import cycle.

import (
	"fmt"
	"sort"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nexus/levee/internal/dsl"
	"github.com/nexus/levee/internal/executor"

	// Registering the modules is what makes their Actions() visible.
	_ "github.com/nexus/levee/internal/executor/modules/file"
	_ "github.com/nexus/levee/internal/executor/modules/mysql"
	_ "github.com/nexus/levee/internal/executor/modules/pg"
	_ "github.com/nexus/levee/internal/executor/modules/pkg"
	_ "github.com/nexus/levee/internal/executor/modules/shell"
	_ "github.com/nexus/levee/internal/executor/modules/svc"
	_ "github.com/nexus/levee/internal/executor/modules/user"
)

// untypedActions lists the executable actions the signature table knowingly
// does not cover, each with the reason it is not typed yet. Every entry is a
// place where a wrong argument type is caught at RUNTIME instead of at
// compile time — a debt, not a design. Removing an entry (by typing the
// action) is the improvement; adding one needs a reason written down here.
var untypedActions = map[string]string{
	"file.template": "args are path/content/mode; typing them is a follow-up (the module validates its own inputs)",
	"pkg.remove":    "the MVP table covered install/upgrade only; remove was added later",
	"user.add":      "the user module validates name/shell/keys itself",
	"user.remove":   "the user module validates its own inputs",
	"user.modify":   "the user module validates its own inputs",
	"svc.start":     "the service actions take {name} and are validated by the module",
	"svc.enable":    "the service actions take {name} and are validated by the module",
	"svc.disable":   "the service actions take {name} and are validated by the module",
	"svc.reload":    "the service actions take {name} and are validated by the module",
	"shell.script":  "script takes {path} / {content}; exec is the typed one",
}

// executableActions is every (module.action) a registered module can run.
func executableActions(t *testing.T) []string {
	t.Helper()
	var out []string
	names := executor.DefaultExecutor().Modules()
	require.NotEmpty(t, names, "no modules registered; this guard would pass vacuously")
	for _, name := range names {
		mod, ok := executor.DefaultExecutor().Module(name)
		if !ok {
			continue
		}
		for _, action := range mod.Actions() {
			out = append(out, name+"."+action)
		}
	}
	sort.Strings(out)
	return out
}

func TestExecutableActionsAreTypedOrExempt(t *testing.T) {
	signatures := dsl.ActionSignatures()
	for _, key := range executableActions(t) {
		_, typed := signatures[key]
		reason, exempt := untypedActions[key]
		assert.True(t, typed || exempt,
			"%s can be executed but has no signature and no documented exemption: "+
				"its arguments are not type-checked, and nothing says so", key)
		assert.False(t, typed && exempt,
			"%s is both typed and listed as untyped; remove it from untypedActions", key)
		if exempt {
			assert.NotEmpty(t, reason,
				"%s is exempt from type checking; the exemption needs a reason", key)
		}
	}
}

// The other direction: no key may promise an action the platform cannot run.
func TestSignatureTableNamesOnlyExecutableActions(t *testing.T) {
	executable := map[string]bool{}
	for _, key := range executableActions(t) {
		executable[key] = true
	}
	for key := range dsl.ActionSignatures() {
		assert.True(t, executable[key],
			"actionSignatures declares %s, but no registered module executes it — "+
				"a workflow written against this row compiles and then dies at execution", key)
	}
}

// The database modules are the ones whose arguments carry the SQL the review
// gate scores, so their coverage is not left to the exemption map: this pins
// the actions explicitly.
func TestDatabaseModuleActionsAreTyped(t *testing.T) {
	signatures := dsl.ActionSignatures()
	for _, key := range []string{
		"mysql.query", "mysql.pt_osc", "mysql.replica_switch", "mysql.schema_snapshot", "mysql.preflight",
		"pg.query", "pg.schema_snapshot", "pg.schema_snapshot_ddl", "pg.schema_restore", "pg.preflight",
	} {
		_, typed := signatures[key]
		assert.True(t, typed,
			"%s must be type-checked: an unlisted action is skipped, not refused", key)
		_, exempt := untypedActions[key]
		assert.False(t, exempt, "%s is typed; it must not also be exempt", key)
	}
}

// A worked example of the ratchet's value: `tables` is a list, and a bare
// string must not type-check as one.
func TestSchemaSnapshotTablesMustBeAList(t *testing.T) {
	sig, ok := dsl.ActionSignatures()["mysql.schema_snapshot"]
	require.True(t, ok)
	expected, ok := sig["tables"]
	require.True(t, ok, "the snapshot signature must declare tables")
	assert.False(t, expected.Compatible(dsl.TypeString{}),
		"a string must not be accepted where a list of tables is required")
	assert.True(t, expected.Compatible(dsl.TypeList{ElementType: dsl.TypeString{}}),
		fmt.Sprintf("a list of strings is the declared shape, got %T", expected))
}
