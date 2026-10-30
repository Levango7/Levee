package main

// cmd_serve_ownership_test.go covers the serve-side half of the host-ownership
// registry: loading target-ownership.yaml (missing / broken / conflicting) and
// the fact that the service factory hands the loaded value to the diagnosis
// resolver.
//
// internal/inventory already unit-tests the comparison itself. What had no
// test at all was everything between the file and the process:
//
//   - a file that cannot be parsed, or declares a host without an environment,
//     must refuse startup. Falling back to caller-asserted labels would be a
//     silent downgrade of the one fact source that policy is not allowed to
//     take from a request.
//   - a registry/label conflict must be an audit line, not an outage. The
//     registry is the answer policy uses; a row that still says "dev" under a
//     registry fact of "prod" is a relabeling task for the operator, and
//     refusing to serve over it would trade one visible inconsistency for a
//     dead deployment.
//   - buildServeServices must pass the loaded registry into TargetEnvResolver.
//     A registry nobody passes is a file, not a fact source; runServe is a long
//     startup sequence a unit test cannot drive, so that link is pinned by
//     reading the call site (same trade-off, and same reason, as
//     serve_gateway_wiring_test.go).

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nexus/levee/internal/config"
	"github.com/nexus/levee/internal/grpc"
	"github.com/nexus/levee/internal/inventory"
	"github.com/nexus/levee/internal/log"
	"github.com/nexus/levee/internal/state"
)

func ownershipCfg(dir string) *config.Config {
	cfg := &config.Config{}
	cfg.Server.DataDir = dir
	return cfg
}

func writeOwnershipFile(t *testing.T, dir, body string) {
	t.Helper()
	require.NoError(t, os.WriteFile(filepath.Join(dir, inventory.OwnershipFileName), []byte(body), 0o600))
}

// captureServeLog redirects the process logger for the length of one test and
// restores the default sink afterwards. InitLoggerWithWriter replaces the
// singleton explicitly for this purpose; no test in this package runs in
// parallel, so the window is not shared.
func captureServeLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	buf := &bytes.Buffer{}
	log.InitLoggerWithWriter("info", "text", buf)
	t.Cleanup(func() { log.InitLoggerWithWriter("info", "text", os.Stderr) })
	return buf
}

func TestLoadTargetOwnershipRegistryMissingFileIsLabelsOnly(t *testing.T) {
	reg, err := loadTargetOwnershipRegistry(ownershipCfg(t.TempDir()), newWiringTestStore(t))
	require.NoError(t, err, "no file is the documented labels-only posture, not a startup failure")
	require.NotNil(t, reg, "the loader must return a usable registry even when nothing is configured")
	assert.False(t, reg.Configured())
}

func TestLoadTargetOwnershipRegistryBrokenFileRefusesStartup(t *testing.T) {
	cases := map[string]string{
		// Half an ownership fact is worse than none: a deployment that opted in
		// and broke must not degrade to caller-asserted labels.
		"unparseable":    "hosts: [",
		"no environment": "hosts:\n  - hostname: db-1\n",
		"no hostname":    "hosts:\n  - environment: prod\n",
		// Two rows naming one host is exactly the ambiguity the file exists to end.
		"duplicate host": "hosts:\n  - hostname: db-1\n    environment: prod\n  - hostname: DB-1\n    environment: dev\n",
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			writeOwnershipFile(t, dir, body)

			reg, err := loadTargetOwnershipRegistry(ownershipCfg(dir), newWiringTestStore(t))
			require.Error(t, err, "a broken ownership file must fail startup, not silently disable the fact source")
			assert.Nil(t, reg)
			assert.Contains(t, err.Error(), "target ownership",
				"the error must name the fact source so the operator knows which file to fix")
		})
	}
}

func TestLoadTargetOwnershipRegistryConflictIsAuditNotOutage(t *testing.T) {
	buf := captureServeLog(t)

	dir := t.TempDir()
	writeOwnershipFile(t, dir, "hosts:\n  - hostname: db-1\n    environment: prod\n")

	store := newWiringTestStore(t)
	row := &state.Target{
		ID:          "tgt-db-1",
		Hostname:    "db-1",
		Port:        22,
		ChannelType: "ssh",
		Status:      "active",
		Labels:      map[string]string{"env": "dev"},
	}
	require.NoError(t, store.UpsertTarget(context.Background(), row))

	reg, err := loadTargetOwnershipRegistry(ownershipCfg(dir), store)
	require.NoError(t, err, "a label conflict is a relabeling task, not a reason to refuse to serve")
	require.NotNil(t, reg)
	require.True(t, reg.Configured())

	env, ok := reg.Lookup("db-1")
	require.True(t, ok)
	assert.Equal(t, "prod", env, "the registry is the platform-side fact")

	// The audit must actually fire: it is the only thing standing between a
	// registry and the rows it governs, and "no WARN" is indistinguishable from
	// "nobody compared them" in an operator's log.
	out := buf.String()
	assert.Contains(t, out, "level=WARN", "a conflicting row must produce a warning")
	assert.Contains(t, out, "db-1")
	assert.Contains(t, out, "prod", "the warning must say which environment the platform believes")
	assert.Contains(t, out, "dev", "the warning must say which environment the row claims")

	// And policy must judge the registry, not the stale row: this is the
	// security-relevant half of the same fact.
	resolve := grpc.TargetEnvResolver(store, reg)
	got, ok := resolve(context.Background(), "tgt-db-1")
	require.True(t, ok)
	assert.Equal(t, "prod", got,
		"resolution by target id must prefer the registry over the row's own label")
}

func TestLoadTargetOwnershipRegistryConsistentInventoryDoesNotWarn(t *testing.T) {
	buf := captureServeLog(t)

	dir := t.TempDir()
	writeOwnershipFile(t, dir, "hosts:\n  - hostname: db-1\n    environment: prod\n")

	store := newWiringTestStore(t)
	require.NoError(t, store.UpsertTarget(context.Background(), &state.Target{
		ID:          "tgt-db-1",
		Hostname:    "db-1",
		Port:        22,
		ChannelType: "ssh",
		Status:      "active",
		Labels:      map[string]string{"env": "prod", "role": "db"},
	}))

	reg, err := loadTargetOwnershipRegistry(ownershipCfg(dir), store)
	require.NoError(t, err)
	require.True(t, reg.Configured())

	// Extra labels are not a conflict; only the resolved env value is compared.
	out := buf.String()
	assert.NotContains(t, out, "level=WARN",
		"a consistent inventory must not warn; a warning here would train operators to ignore the audit")
	assert.Contains(t, out, "consistent with inventory labels")
}

func TestServeWiresOwnershipRegistryIntoTheDiagnosisResolver(t *testing.T) {
	// Tests run with the package directory as cwd, so this is the file the
	// compiler used.
	src, err := os.ReadFile("cmd_serve.go")
	require.NoError(t, err, "the wiring gate reads cmd_serve.go next to itself")
	text := string(src)
	compact := strings.Join(strings.Fields(text), "")

	assert.Contains(t, compact, "ownershipReg,oerr:=loadTargetOwnershipRegistry(cfg,store)",
		"buildServeServices must load target-ownership.yaml; a registry that is never loaded is a decoration")

	// Swallowing the load error would reintroduce exactly the silent downgrade
	// the loader refuses to make on its own.
	call := ownershipCallSite(t, compact)
	assert.Contains(t, call, "ifoerr!=nil{returnserveServices{},oerr}",
		"the ownership load error must abort service construction, not be logged and dropped")

	assert.Contains(t, compact, "WithTargetEnvResolver(grpc.TargetEnvResolver(store,ownershipReg))",
		"the loaded registry must feed the diagnosis env resolver; passing nil restores caller-asserted labels")

	// The value in between is pinned too. The compiler already rejects passing
	// nil while ownershipReg sits unused, so the substitution that can still get
	// through is a reassignment to some other registry: exactly two mentions
	// (the declaration and the argument) are allowed between load and resolver.
	const resolver = "WithTargetEnvResolver(grpc.TargetEnvResolver(store,ownershipReg))"
	start := strings.Index(compact, "ownershipReg,oerr:=loadTargetOwnershipRegistry(cfg,store)")
	end := strings.Index(compact, resolver)
	require.GreaterOrEqual(t, start, 0)
	require.Greater(t, end, start)
	between := compact[start : end+len(resolver)]
	assert.Equal(t, 2, strings.Count(between, "ownershipReg"),
		"the registry handed to the resolver must be the value the loader returned, not one re-derived in between")
	assert.NotContains(t, between, "ownershipReg=",
		"reassigning ownershipReg between the load and the resolver would silently replace the fact source")
}

// ownershipCallSite returns the text starting at the ownership load and
// covering the next few statements, so the error handling can be judged where
// it lives rather than anywhere in the file.
func ownershipCallSite(t *testing.T, compact string) string {
	t.Helper()
	const anchor = "ownershipReg,oerr:=loadTargetOwnershipRegistry(cfg,store)"
	i := strings.Index(compact, anchor)
	require.GreaterOrEqual(t, i, 0)
	end := i + 240
	if end > len(compact) {
		end = len(compact)
	}
	return compact[i:end]
}
