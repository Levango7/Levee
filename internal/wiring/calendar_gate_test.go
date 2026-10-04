package wiring

// calendar_gate_test.go covers the change calendar as an enforcement point.
//
// Every test here exists because the calendar's verdict functions were, up to
// this change, complete and unreachable: `CalendarService` was constructed only
// by `cmd/levee/cmd_calendar.go`, so `levee calendar freeze` wrote rows into a
// table nothing read. Unit tests of IsFrozen/AssertNotFrozen could therefore be
// all green while the product enforced nothing. So the claims under test are
// about the seam: does a freeze refuse a plan, does it refuse an apply that was
// approved before the freeze existed, does the refusal stay out of the rollback
// path, and what exactly counts as "covered by the freeze".

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/nexus/levee/internal/calendar"
	"github.com/nexus/levee/internal/grpc"
	"github.com/nexus/levee/internal/grpc/pb"
	"github.com/nexus/levee/internal/state"
)

// newCalendarEngine builds an Engine over a real in-memory SQLite store with the
// calendar sharing that same handle — the arrangement `serve` uses. Deliberately
// not a fake calendar: the freeze matching runs through the same SQL the
// deployment runs.
func newCalendarEngine(t *testing.T) (*Engine, *state.SQLiteStore, *calendar.CalendarService, *calendar.Store) {
	t.Helper()
	ctx := context.Background()
	store, err := state.NewSQLiteStore(ctx, ":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })

	calStore, err := calendar.NewStore(ctx, store.DB(), calendar.DialectSQLite)
	require.NoError(t, err)
	svc := calendar.NewCalendarService(calStore)
	return NewEngine(store, WithChangeCalendar(svc)), store, svc, calStore
}

// seedLabeledTarget puts a host in the inventory with labels and a group, since
// the freeze vocabulary is derived from them. A non-empty group is created
// first: targets reference it, and inventing the name anyway would fail on the
// foreign key rather than test anything.
func seedLabeledTarget(t *testing.T, store state.Store, host, group string, labels map[string]string) {
	t.Helper()
	if group != "" {
		require.NoError(t, store.UpsertInventoryGroup(context.Background(), &state.InventoryGroup{
			ID: group, Name: group, CreatedAt: time.Now().UTC(),
		}))
	}
	require.NoError(t, store.UpsertTarget(context.Background(), &state.Target{
		ID: "tgt-" + host, Hostname: host, Port: 22, ChannelType: "ssh",
		Status: "active", GroupID: group, Labels: labels,
		CreatedAt: time.Now().UTC(),
	}))
}

// seedFreeze writes an active freeze window (now-1h .. now+1h) for the labels.
func seedFreeze(t *testing.T, svc *calendar.CalendarService, id string, labels ...string) {
	t.Helper()
	now := time.Now().UTC()
	require.NoError(t, svc.CreateWindow(context.Background(), &calendar.Window{
		ID: id, Name: "freeze-" + id,
		StartTime: now.Add(-time.Hour), EndTime: now.Add(time.Hour),
		TargetLabels: labels, IsFrozen: true,
		CreatedAt: now, UpdatedAt: now,
	}))
}

// seedMaintenance writes an active, non-frozen change window for the labels.
func seedMaintenance(t *testing.T, svc *calendar.CalendarService, id string, labels ...string) {
	t.Helper()
	now := time.Now().UTC()
	require.NoError(t, svc.CreateWindow(context.Background(), &calendar.Window{
		ID: id, Name: "window-" + id,
		StartTime: now.Add(-time.Hour), EndTime: now.Add(time.Hour),
		TargetLabels: labels, IsFrozen: false,
		CreatedAt: now, UpdatedAt: now,
	}))
}

// calWorkflow is a plannable document; `level` optionally adds an approval block.
func calWorkflow(level string) string {
	src := "name: cal-gate\ntarget:\n  type: host\n  hosts: [\"web-1\"]\n"
	if level != "" {
		src += "approval:\n  level: " + level + "\n"
	}
	return src + "steps:\n  - name: restart\n    action: svc.restart\n"
}

// ---------------------------------------------------------------------------
// plan-time gate
// ---------------------------------------------------------------------------

func TestGeneratePlan_RefusedByFreezeAndLeavesNoArtifact(t *testing.T) {
	ctx := context.Background()
	eng, store, svc, _ := newCalendarEngine(t)
	seedLabeledTarget(t, store, "web-1", "payments", map[string]string{"env": "prod", "tier": "core"})
	seedFreeze(t, svc, "frz-1", "env=prod")
	seedRun(t, store, "run-frz", calWorkflow("high"))

	p, stored, err := eng.GeneratePlan(ctx, "run-frz", []string{"web-1"})
	require.Error(t, err, "a freeze covering the targets must refuse the plan")
	assert.Nil(t, p)
	assert.Nil(t, stored)
	assert.True(t, errors.Is(err, calendar.ErrFrozen),
		"the refusal has to be matchable as a freeze, got: %v", err)
	// The message must let the operator act: which freeze, when it lifts.
	assert.Contains(t, err.Error(), "frz-1")
	assert.Contains(t, err.Error(), "freeze-frz-1")
	assert.Contains(t, err.Error(), "lifts", "the message must say when the freeze ends")

	run, gerr := store.GetRun(ctx, "run-frz")
	require.NoError(t, gerr)
	assert.Empty(t, run.PlanJSON, "a refused plan leaves no artifact behind")
	assert.Empty(t, run.PlanHash)
}

func TestGeneratePlan_FreezeOnOtherTargetsAllows(t *testing.T) {
	ctx := context.Background()
	eng, store, svc, _ := newCalendarEngine(t)
	seedLabeledTarget(t, store, "web-1", "payments", map[string]string{"env": "prod"})
	seedFreeze(t, svc, "frz-staging", "env=staging")
	seedRun(t, store, "run-ok", calWorkflow("high"))

	_, stored, err := eng.GeneratePlan(ctx, "run-ok", []string{"web-1"})
	require.NoError(t, err, "a freeze that covers other targets must not stop this change")
	require.NotNil(t, stored)
}

// TestGeneratePlan_FreezeVocabulary pins what "covers these targets" means,
// because the documented `--targets` values are bare words (`prod`, `web`) while
// inventory labels are `key=value` pairs. Both forms have to bite, and a bare
// label *key* must not (every host has an `env`, so `env` would freeze all of
// them).
func TestGeneratePlan_FreezeVocabulary(t *testing.T) {
	cases := []struct {
		entry    string
		wantFail bool
		why      string
	}{
		{entry: "env=prod", wantFail: true, why: "key=value form"},
		{entry: "prod", wantFail: true, why: "bare label value, the documented shape"},
		{entry: "payments", wantFail: true, why: "inventory group"},
		{entry: "web-1", wantFail: true, why: "hostname"},
		{entry: "env", wantFail: false, why: "a bare label KEY must not freeze the fleet"},
		{entry: "unrelated", wantFail: false, why: "no overlap"},
	}
	for _, tc := range cases {
		t.Run(tc.entry, func(t *testing.T) {
			ctx := context.Background()
			eng, store, svc, _ := newCalendarEngine(t)
			seedLabeledTarget(t, store, "web-1", "payments", map[string]string{"env": "prod"})
			seedFreeze(t, svc, "frz-vocab", tc.entry)
			seedRun(t, store, "run-v", calWorkflow("high"))

			_, _, err := eng.GeneratePlan(ctx, "run-v", []string{"web-1"})
			if tc.wantFail {
				require.Error(t, err, "%s: %q must cover the target", tc.why, tc.entry)
				assert.True(t, errors.Is(err, calendar.ErrFrozen), "got: %v", err)
				return
			}
			require.NoError(t, err, "%s: %q must not cover the target", tc.why, tc.entry)
		})
	}
}

// TestGeneratePlan_EmergencyOverrideIsTheAuthorsDeclaration: only a workflow
// whose author wrote `approval.level: emergency` may cross a freeze. A derived
// tier would let a risk score unlock the override by itself.
func TestGeneratePlan_EmergencyOverrideIsTheAuthorsDeclaration(t *testing.T) {
	cases := []struct {
		level    string
		wantPass bool
		why      string
	}{
		{level: "emergency", wantPass: true, why: "declared by the author"},
		{level: "high", wantPass: false, why: "a high tier is not an emergency declaration"},
		{level: "standard", wantPass: false, why: "the default tier"},
		{level: "", wantPass: false, why: "no approval block at all"},
	}
	for _, tc := range cases {
		name := tc.level
		if name == "" {
			name = "none"
		}
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			eng, store, svc, _ := newCalendarEngine(t)
			seedLabeledTarget(t, store, "web-1", "", map[string]string{"env": "prod"})
			seedFreeze(t, svc, "frz-emergency", "env=prod")
			seedRun(t, store, "run-em", calWorkflow(tc.level))

			_, _, err := eng.GeneratePlan(ctx, "run-em", []string{"web-1"})
			if tc.wantPass {
				require.NoError(t, err, "%s: override expected", tc.why)
				return
			}
			require.Error(t, err, "%s: override must not apply", tc.why)
			assert.True(t, errors.Is(err, calendar.ErrFrozen), "got: %v", err)
		})
	}
}

// TestGeneratePlan_OverlappingWindowIsAdvisory keeps the two calendar verdicts
// apart: an overlapping maintenance window is information, only a freeze refuses.
func TestGeneratePlan_OverlappingWindowIsAdvisory(t *testing.T) {
	ctx := context.Background()
	eng, store, svc, _ := newCalendarEngine(t)
	seedLabeledTarget(t, store, "web-1", "", map[string]string{"env": "prod"})
	seedMaintenance(t, svc, "rel-window", "env=prod")
	seedRun(t, store, "run-adv", calWorkflow("high"))

	_, stored, err := eng.GeneratePlan(ctx, "run-adv", []string{"web-1"})
	require.NoError(t, err, "an overlapping change window must not refuse the plan")
	require.NotNil(t, stored)
}

// TestGeneratePlan_UnreadableCalendarRefuses: a calendar that cannot answer is
// not a calendar that says proceed. The freeze check is the call that carries
// this claim — it reads the store first, so any store failure surfaces there
// before the advisory conflict read is even attempted.
func TestGeneratePlan_UnreadableCalendarRefuses(t *testing.T) {
	ctx := context.Background()
	eng, store, _, calStore := newCalendarEngine(t)
	seedLabeledTarget(t, store, "web-1", "", map[string]string{"env": "prod"})
	seedRun(t, store, "run-broken", calWorkflow("high"))

	// Drop the table out from under the service — the closest honest analogue
	// to a calendar that errors, short of a fault-injection store.
	_, err := calStore.DB().ExecContext(ctx, "DROP TABLE calendar_windows")
	require.NoError(t, err)

	_, _, perr := eng.GeneratePlan(ctx, "run-broken", []string{"web-1"})
	require.Error(t, perr, "a calendar that cannot answer must refuse the plan")
	assert.Contains(t, perr.Error(), "frozen windows", "the message must say what failed: %v", perr)
}

// TestGeneratePlan_WithoutCalendarStillPlans documents the inert default, and
// TestCalendarGateIsForwardOnly plus the serve/CLI wiring tests are what keep
// "inert" from becoming the production state.
func TestGeneratePlan_WithoutCalendarStillPlans(t *testing.T) {
	ctx := context.Background()
	eng, store := newTestEngine(t, "web-1")
	seedRun(t, store, "run-nocal", calWorkflow("high"))

	_, stored, err := eng.GeneratePlan(ctx, "run-nocal", []string{"web-1"})
	require.NoError(t, err)
	require.NotNil(t, stored)
}

// TestWithChangeCalendarInstallsTheService is the one-line guard against an
// option whose body was dropped: the plan refusal above would then be the only
// evidence, and it goes through the same field anyway.
func TestWithChangeCalendarInstallsTheService(t *testing.T) {
	_, store, svc, _ := newCalendarEngine(t)
	eng := NewEngine(store, WithChangeCalendar(svc))
	require.NotNil(t, eng.calendar, "WithChangeCalendar must install the service")
	assert.Same(t, svc, eng.calendar)

	// And clearing it must actually clear it — a nil service is what makes the
	// gate inert, so an option that ignores nil would hide that state.
	assert.Nil(t, NewEngine(store, WithChangeCalendar(svc), WithChangeCalendar(nil)).calendar)
}

// ---------------------------------------------------------------------------
// apply-time re-check
// ---------------------------------------------------------------------------

// TestExecutionGuardRefusesLateFreeze is the second enforcement point: a freeze
// raised after the change was approved still stops it, because the guard runs
// before any target is touched.
func TestExecutionGuardRefusesLateFreeze(t *testing.T) {
	ctx := context.Background()
	eng, store, svc, _ := newCalendarEngine(t)
	seedLabeledTarget(t, store, "web-1", "", map[string]string{"env": "prod"})
	seedRun(t, store, "run-late", calWorkflow("high"))

	_, stored, err := eng.GeneratePlan(ctx, "run-late", []string{"web-1"})
	require.NoError(t, err, "no freeze exists yet")
	require.NotNil(t, stored)

	seedFreeze(t, svc, "frz-late", "env=prod")

	rx, rerr := newRunExec(ctx, eng, nil)
	require.NoError(t, rerr)
	require.NotNil(t, rx)
	gerr := eng.assertCalendarAllowsExecution(ctx, rx, []string{"web-1"}, false)
	require.Error(t, gerr, "the apply-time guard must catch a freeze raised after approval")
	assert.True(t, errors.Is(gerr, calendar.ErrFrozen), "got: %v", gerr)

	assert.NoError(t, eng.assertCalendarAllowsExecution(ctx, rx, []string{"web-1"}, true),
		"the declared emergency override applies at both points, not only at plan")
}

// TestRunRunnerInstallsTheCalendarGuard is the boundary test for the apply-side
// gate: assertCalendarAllowsExecution being correct proves nothing if
// newRunRunner never puts it in the host guard — the exact failure mode the
// whole calendar had. It reads the closure the runner installs, so deleting the
// call turns this red.
func TestRunRunnerInstallsTheCalendarGuard(t *testing.T) {
	src, err := os.ReadFile("run.go")
	require.NoError(t, err)
	body := functionBody(string(src), "func (e *Engine) newRunRunner")
	require.NotEmpty(t, body, "newRunRunner must be found to inspect its host guard")

	start := strings.Index(body, "engine.WithHostGuard(")
	require.Greater(t, start, 0, "the runner installs a host guard")
	end := strings.Index(body[start:], "\n\t\t}),")
	require.Greater(t, end, 0, "the host guard closure must be findable")
	guard := body[start : start+end]

	assert.Contains(t, guard, "inventory.ValidateNotFrozen",
		"the pre-existing inventory freeze check stays (two different freezes)")
	assert.Contains(t, guard, "assertCalendarAllowsExecution",
		"the change-calendar re-check must run in the same guard, before any mutation")
	assert.Contains(t, guard, "calendarEmergency",
		"the override must come from the stored declaration, not from a literal")
}

// ---------------------------------------------------------------------------
// structure and transport
// ---------------------------------------------------------------------------

// TestCalendarGateIsForwardOnly pins the two halves of the placement claim: the
// gate is consulted by exactly the forward paths, and recovery never passes it.
// A rollback blocked by a freeze raised during the incident would strand a
// half-applied change, which is a worse outcome than the freeze prevents.
func TestCalendarGateIsForwardOnly(t *testing.T) {
	callsites := map[string]int{}
	rollbackBody := ""

	require.NoError(t, filepath.Walk("../../internal", func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || !strings.HasSuffix(path, ".go") ||
			strings.HasSuffix(path, "_test.go") {
			return nil
		}
		src, rerr := os.ReadFile(path)
		if rerr != nil {
			return nil
		}
		rel := relativePOSIX(path)
		// Count call sites, not definitions: the gate functions themselves live
		// in calendar_gate.go and must not count as consumers.
		for _, line := range strings.Split(string(src), "\n") {
			if strings.Contains(line, "e.assertCalendarAllows(") ||
				strings.Contains(line, "e.assertCalendarAllowsExecution(") {
				callsites[rel]++
			}
		}
		if strings.HasSuffix(rel, "internal/wiring/run.go") {
			rollbackBody = functionBody(string(src), "func (e *Engine) rollbackChange")
		}
		return nil
	}))

	assert.Equal(t, map[string]int{
		"internal/wiring/plan.go": 1,
		"internal/wiring/run.go":  1,
	}, callsites, "exactly one plan-time and one apply-time call site, nowhere else")

	require.NotEmpty(t, rollbackBody, "rollbackChange must be found to prove it stays outside the gate")
	assert.NotContains(t, rollbackBody, "assertCalendarAllows",
		"rollback must never be gated by the calendar")
	assert.NotContains(t, rollbackBody, "GeneratePlan",
		"rollback loads the stored plan; regenerating it would put recovery behind the gate")
}

// TestPlanChange_ReportsFreezeAsFailedPrecondition pins the RPC shape: an
// operator-facing refusal, not Internal.
func TestPlanChange_ReportsFreezeAsFailedPrecondition(t *testing.T) {
	ctx := context.Background()
	eng, store, svc, _ := newCalendarEngine(t)
	seedLabeledTarget(t, store, "web-1", "", map[string]string{"env": "prod"})
	seedFreeze(t, svc, "frz-rpc", "env=prod")
	seedRun(t, store, "run-rpc", calWorkflow("high"))

	svcChange := grpc.NewChangeService(store, eng.Adapter(), nil, nil)
	_, err := svcChange.PlanChange(ctx, &pb.PlanChangeRequest{
		ChangeId:    "run-rpc",
		TargetHosts: []string{"web-1"},
	})
	require.Error(t, err)
	assert.Equal(t, codes.FailedPrecondition, status.Convert(err).Code(),
		"a freeze refusal must not surface as Internal, got: %v", err)
	assert.Contains(t, err.Error(), "freeze-frz-rpc")
}
