package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/nexus/levee/internal/grpc"
	"github.com/nexus/levee/internal/runstatus"
	"github.com/nexus/levee/internal/state"
)

// rollbackCLIEnv points the CLI at a throwaway SQLite store and returns the
// config path (the CLI's own commands take it) plus the store, so a test can
// seed runs and read the result back.
func rollbackCLIEnv(t *testing.T) (cfgPath string, store state.Store) {
	t.Helper()
	dir := t.TempDir()
	cfgPath = filepath.Join(t.TempDir(), "config.yaml")
	require.NoError(t, os.WriteFile(cfgPath, []byte("server:\n  data_dir: "+dir+"\n"), 0o600))

	prevCfg := optConfigPath
	optConfigPath = cfgPath
	t.Cleanup(func() { optConfigPath = prevCfg })

	store, err := openStore(context.Background())
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	return cfgPath, store
}

// seedRollbackRun stores a run in the given status.
func seedRollbackRun(t *testing.T, store state.Store, id, status string) {
	t.Helper()
	now := time.Now().UTC()
	require.NoError(t, store.CreateRun(context.Background(), &state.Run{
		ID: id, WorkflowName: "svc", Status: status, Creator: "cli-user",
		CreatedAt: now, UpdatedAt: now,
	}))
}

// stubRollbackEngine replaces the CLI's engine bridge with one whose rollback
// answers from a script instead of touching a host, and records the dry_run
// flag every call carried.
//
// The service is built over the store the CLI hands it, so the command and the
// test read and write the same database connection.
func stubRollbackEngine(t *testing.T, outcome grpc.RollbackOutcome, seen *[]bool) {
	t.Helper()
	adapter := &grpc.EngineAdapter{
		Rollback: func(_ context.Context, _, _ string, _ bool, dryRun bool) (grpc.RollbackOutcome, error) {
			if seen != nil {
				*seen = append(*seen, dryRun)
			}
			return outcome, nil
		},
	}
	prev := cliChangeServiceFactory
	cliChangeServiceFactory = func(s state.Store) (*grpc.ChangeService, error) {
		return grpc.NewChangeService(s, adapter, nil, nil), nil
	}
	t.Cleanup(func() { cliChangeServiceFactory = prev })
}

// rollbackAudits returns the rollback rows the command left in the audit chain.
func rollbackAudits(t *testing.T, store state.Store, runID string) []*state.Audit {
	t.Helper()
	audits, err := store.ListAudits(context.Background(), state.AuditFilter{RunID: runID})
	require.NoError(t, err)
	var out []*state.Audit
	for _, a := range audits {
		if a.Action == state.AuditActionRollback {
			out = append(out, a)
		}
	}
	return out
}

func TestRollbackCmdRegistered(t *testing.T) {
	defer resetRootFlags()
	cmd := findSub("rollback")
	require.NotNil(t, cmd, "rollback subcommand should be registered")
	assert.Equal(t, "rollback", cmd.Name())
}

func TestRollbackCmdFlags(t *testing.T) {
	defer resetRootFlags()
	cmd := findSub("rollback")
	require.NotNil(t, cmd)

	f := cmd.Flags().Lookup("dry-run")
	require.NotNil(t, f, "rollback command should have --dry-run flag")
	assert.Equal(t, "false", f.DefValue, "dry-run flag should default to false")

	// The retired --force is gone on purpose: it bypassed the CLI's own copy
	// of the admission guard, and the only thing it could reach was a run
	// stranded in a status no component consumed. The gate that matters is the
	// service's, and it is not overridable.
	assert.Nil(t, cmd.Flags().Lookup("force"),
		"the retired --force flag must not come back: it could only strand the run")
}

func TestRollbackCmdRequiresArg(t *testing.T) {
	defer resetRootFlags()
	cmd := findSub("rollback")
	require.NotNil(t, cmd)

	err := cmd.Args(cmd, []string{})
	assert.Error(t, err, "rollback without run-id arg should be rejected")
}

func TestRollbackCmdTooManyArgs(t *testing.T) {
	defer resetRootFlags()
	cmd := findSub("rollback")
	require.NotNil(t, cmd)

	err := cmd.Args(cmd, []string{"run-001", "extra"})
	assert.Error(t, err, "rollback with too many args should be rejected")
}

// TestRollbackCmdPreviewChangesNothing is the whole point of --dry-run: the
// operator sees what the rollback WOULD do, and nothing happens. Before this
// command reached the service at all, "preview" was not even a question the
// CLI could ask.
func TestRollbackCmdPreviewChangesNothing(t *testing.T) {
	cfgPath, store := rollbackCLIEnv(t)
	seedRollbackRun(t, store, "run-rb-preview", runstatus.StatusRolledBackPartial)

	var seen []bool
	stubRollbackEngine(t, grpc.RollbackOutcome{
		RolledBackHosts: []string{"web-1"},
		SkippedHosts:    []string{"web-2"},
	}, &seen)

	out, err := execKeepCfg(t, cfgPath, "rollback", "run-rb-preview", "--dry-run")
	require.NoError(t, err, "cli output: %s", out)

	// The evidence the operator asked for.
	assert.Contains(t, out, "web-1", "the preview must name the hosts it would compensate")
	assert.Contains(t, out, "web-2", "the preview must name the hosts it would leave alone")

	// The flag reached the engine.
	require.Len(t, seen, 1, "the engine must have been asked exactly once")
	assert.True(t, seen[0], "the engine must have been asked for a preview")

	// ...and nothing was changed.
	run, gerr := store.GetRun(context.Background(), "run-rb-preview")
	require.NoError(t, gerr)
	assert.Equal(t, runstatus.StatusRolledBackPartial, run.Status,
		"a preview must not rewrite the run status")
	assert.Empty(t, rollbackAudits(t, store, "run-rb-preview"),
		"a preview must not leave a rollback row in the audit chain")
}

// TestRollbackCmdPerformsTheRollback is the other half: without --dry-run the
// command must actually roll the change back. It used to write a status and
// stop, and the run it left behind was stranded.
func TestRollbackCmdPerformsTheRollback(t *testing.T) {
	cfgPath, store := rollbackCLIEnv(t)
	seedRollbackRun(t, store, "run-rb-real", runstatus.StatusCompleted)

	var seen []bool
	stubRollbackEngine(t, grpc.RollbackOutcome{
		RunID:           "rb-1",
		RolledBackHosts: []string{"web-1"},
		SkippedHosts:    []string{"web-2"},
	}, &seen)

	out, err := execKeepCfg(t, cfgPath, "rollback", "run-rb-real")
	require.NoError(t, err, "cli output: %s", out)

	require.Len(t, seen, 1)
	assert.False(t, seen[0], "an ordinary rollback is not a preview")

	run, gerr := store.GetRun(context.Background(), "run-rb-real")
	require.NoError(t, gerr)
	assert.Equal(t, runstatus.StatusRolledBack, run.Status,
		"a rollback must rewrite the status — that is the whole point of it")

	rows := rollbackAudits(t, store, "run-rb-real")
	require.Len(t, rows, 1, "the rollback must leave exactly one audit row")
	assert.Equal(t, "cli-user", rows[0].Actor, "the audit row must name the CLI actor")
}

// TestRollbackCmdRefusesAStatusTheServiceDoesNotAdmit pins the gate: the CLI
// no longer has a copy of it, and the service's refusal must reach the
// operator as a state conflict (exit 4) with the run untouched.
func TestRollbackCmdRefusesAStatusTheServiceDoesNotAdmit(t *testing.T) {
	cfgPath, store := rollbackCLIEnv(t)
	seedRollbackRun(t, store, "run-rb-draft", runstatus.StatusDraft)

	var seen []bool
	stubRollbackEngine(t, grpc.RollbackOutcome{}, &seen)

	err := runErrKeepCfg(t, cfgPath, "rollback", "run-rb-draft")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "[exit=4]", "a state refusal is exit 4")
	assert.Empty(t, seen, "the engine must not be reached for a refused run")

	run, gerr := store.GetRun(context.Background(), "run-rb-draft")
	require.NoError(t, gerr)
	assert.Equal(t, runstatus.StatusDraft, run.Status, "a refused rollback must not touch the run")
}

// TestRollbackCmdReleasesALegacyStrandedRun covers the run the retired CLI
// stranded: it sits in a status that no gate admits, so the command has to
// recognise it and put it back into a state it can be rolled back from.
func TestRollbackCmdReleasesALegacyStrandedRun(t *testing.T) {
	cfgPath, store := rollbackCLIEnv(t)
	seedRollbackRun(t, store, "run-rb-legacy", runstatus.StatusRollingBackLegacy)

	stubRollbackEngine(t, grpc.RollbackOutcome{RunID: "rb-legacy"}, nil)

	out, err := execKeepCfg(t, cfgPath, "rollback", "run-rb-legacy")
	require.NoError(t, err, "cli output: %s", out)

	run, gerr := store.GetRun(context.Background(), "run-rb-legacy")
	require.NoError(t, gerr)
	assert.Equal(t, runstatus.StatusRolledBack, run.Status,
		"a stranded run must be released and rolled back, not left in the legacy state")
}

// TestRollbackCmdPreviewDoesNotRepairTheLegacyState is the other half of the
// pair: a preview persists nothing, so it must not even perform the repair.
func TestRollbackCmdPreviewDoesNotRepairTheLegacyState(t *testing.T) {
	cfgPath, store := rollbackCLIEnv(t)
	seedRollbackRun(t, store, "run-rb-legacy-preview", runstatus.StatusRollingBackLegacy)

	var seen []bool
	stubRollbackEngine(t, grpc.RollbackOutcome{}, &seen)

	err := runErrKeepCfg(t, cfgPath, "rollback", "run-rb-legacy-preview", "--dry-run")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "[exit=4]")
	assert.Contains(t, err.Error(), runstatus.StatusRollingBackLegacy,
		"the refusal must name the state it found")
	assert.Empty(t, seen, "the engine must not be reached")

	run, gerr := store.GetRun(context.Background(), "run-rb-legacy-preview")
	require.NoError(t, gerr)
	assert.Equal(t, runstatus.StatusRollingBackLegacy, run.Status,
		"a preview must not repair anything")
}

// TestRollbackCmdJSONOutput pins the machine-readable shape: the host lists
// are the evidence the console reads, so they have to be in the envelope.
func TestRollbackCmdJSONOutput(t *testing.T) {
	cfgPath, store := rollbackCLIEnv(t)
	seedRollbackRun(t, store, "run-rb-json", runstatus.StatusRolledBackPartial)

	stubRollbackEngine(t, grpc.RollbackOutcome{
		RolledBackHosts: []string{"web-1"},
		SkippedHosts:    []string{"web-2"},
	}, nil)

	out, err := execKeepCfg(t, cfgPath, "rollback", "run-rb-json", "--dry-run", "--json")
	require.NoError(t, err, "cli output: %s", out)

	var env struct {
		Data struct {
			RunID           string   `json:"run_id"`
			DryRun          bool     `json:"dry_run"`
			RolledBackHosts []string `json:"rolled_back_hosts"`
			SkippedHosts    []string `json:"skipped_hosts"`
			Message         string   `json:"message"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal([]byte(out), &env))
	assert.Equal(t, "run-rb-json", env.Data.RunID)
	assert.True(t, env.Data.DryRun)
	assert.Equal(t, []string{"web-1"}, env.Data.RolledBackHosts)
	assert.Equal(t, []string{"web-2"}, env.Data.SkippedHosts)
	assert.NotEmpty(t, env.Data.Message)
}

// TestRollbackCmdJSONEmptyHostListsAreArrays pins the envelope's shape for
// the empty case: an empty host list is `[]`, not `null`.
//
// The envelope declares both fields as lists. A consumer that iterates one
// must not have to tell "no host was skipped" apart from "this field was not
// there" — and `null` is the shape that makes that distinction necessary
// (a `for x in list` over `null` raises in every language that has one).
func TestRollbackCmdJSONEmptyHostListsAreArrays(t *testing.T) {
	cfgPath, store := rollbackCLIEnv(t)
	seedRollbackRun(t, store, "run-rb-json-empty", runstatus.StatusRolledBackPartial)

	stubRollbackEngine(t, grpc.RollbackOutcome{}, nil)

	out, err := execKeepCfg(t, cfgPath, "rollback", "run-rb-json-empty", "--dry-run", "--json")
	require.NoError(t, err, "cli output: %s", out)

	// Read the raw values: unmarshalling into a []string cannot tell `null`
	// from `[]` apart, so the distinction has to be made on the raw JSON.
	var raw struct {
		Data map[string]json.RawMessage `json:"data"`
	}
	require.NoError(t, json.Unmarshal([]byte(out), &raw))

	for _, key := range []string{"rolled_back_hosts", "skipped_hosts"} {
		require.Contains(t, raw.Data, key, "the envelope must carry %s", key)
		assert.Equal(t, "[]", string(raw.Data[key]),
			"%s must be an empty array, not null", key)
	}
}

// TestRollbackCmdPreviewDoesNotClaimItCompensated pins the wording of a
// preview: it compensated nothing, so a label reading "Compensated hosts"
// would state the opposite of what happened — the same class of defect as a
// document promising a rollback that never ran.
func TestRollbackCmdPreviewDoesNotClaimItCompensated(t *testing.T) {
	cfgPath, store := rollbackCLIEnv(t)
	seedRollbackRun(t, store, "run-rb-wording", runstatus.StatusRolledBackPartial)

	stubRollbackEngine(t, grpc.RollbackOutcome{
		RolledBackHosts: []string{"web-1"},
		SkippedHosts:    []string{"web-2"},
	}, nil)

	out, err := execKeepCfg(t, cfgPath, "rollback", "run-rb-wording", "--dry-run")
	require.NoError(t, err, "cli output: %s", out)

	assert.Contains(t, out, "Would compensate hosts: web-1")
	assert.Contains(t, out, "Would skip hosts: web-2")
	assert.NotContains(t, out, "Compensated hosts",
		"a preview must not word its output as if the hosts had been compensated")
	assert.Contains(t, out, "unchanged by the preview",
		"a preview must say that the run's status was not rewritten")
}

// TestRollbackExitCode pins the exit-code contract of the service refusals.
func TestRollbackExitCode(t *testing.T) {
	assert.Equal(t, 4, rollbackExitCode(status.Error(codes.FailedPrecondition, "nope")))
	assert.Equal(t, 3, rollbackExitCode(status.Error(codes.PermissionDenied, "nope")))
	assert.Equal(t, 1, rollbackExitCode(status.Error(codes.Internal, "nope")))
}
