package grpc

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/nexus/levee/internal/authz"
	"github.com/nexus/levee/internal/runstatus"
	"github.com/nexus/levee/internal/state"
	"github.com/nexus/levee/internal/tenant"
)

// The trend endpoint's whole safety story is in three places, so those are what
// these tests pin:
//
//  1. the read gate judges the RESOLVED environment (default_env when the
//     caller sends none) — and the aggregate is filtered by the SAME value.
//     Judging one env and totalling another is the aggregate leak.
//  2. the caller's timezone offset shifts the DAY BOUNDARY, not the data.
//  3. a capped window says so instead of quietly charting a sample.
func newTrendGateway(t *testing.T, store state.Store, a *authz.Authorizer) *Gateway {
	t.Helper()
	gw := NewGateway(ServeGatewayConfig{})
	gw.store = store
	gw.WithAuthorizer(a)
	return gw
}

// trendRequest issues a request AS a named subject. Without the subject the
// read gate's "no provable identity" exemption admits everything, and the 403
// case below could never fire — so every request here carries one.
func trendRequest(t *testing.T, gw *Gateway, subject, query string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/system/change-trend"+query, nil)
	req = req.WithContext(ContextWithSubject(req.Context(), subject))
	rec := httptest.NewRecorder()
	gw.handleChangeTrend(rec, req)
	return rec
}

func TestChangeTrend_FiltersByTheEnvironmentTheGateJudged(t *testing.T) {
	store := newTestStore(t)
	now := time.Now()
	// A run in the caller's default environment and one in another: if the
	// endpoint filtered by the raw "" instead of the resolved "prod", both
	// would be counted for a caller entitled only to prod.
	mustCreateRun(t, store, "prod-1", runstatus.StatusCompleted, "prod", now.Add(-time.Hour))
	mustCreateRun(t, store, "dev-1", runstatus.StatusCompleted, "dev", now.Add(-2*time.Hour))

	a := loadTestAuthorizer(t, store, "prod")
	gw := newTrendGateway(t, store, a)

	// No env parameter: the gate resolves prod (default_env), so the aggregate
	// must count only prod's run.
	rec := trendRequest(t, gw, "liwg", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var payload changeTrendPayload
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode: %v", err)
	}
	total := 0
	for _, b := range payload.Buckets {
		total += b.Total
	}
	if total != 1 {
		t.Fatalf("caller entitled to prod counted %d runs (want 1: the dev run must not leak)", total)
	}
	if len(payload.Series) != len(state.TrendOutcomes) {
		t.Fatalf("payload must carry the legend order, got %v", payload.Series)
	}

	// An environment the matrix never granted is refused, not silently
	// emptied: a 403 here is the difference between "you may not look" and an
	// empty chart the operator would read as "nothing ran".
	rec = trendRequest(t, gw, "liwg", "?env=staging")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("want 403 for an unreadable environment, got %d", rec.Code)
	}
}

func TestChangeTrend_RejectsBadParameters(t *testing.T) {
	store := newTestStore(t)
	gw := newTrendGateway(t, store, nil)
	for _, q := range []string{"?days=0", "?days=-1", "?days=abc", "?tz_offset=9999", "?tz_offset=abc"} {
		rec := trendRequest(t, gw, "liwg", q)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s: want 400, got %d", q, rec.Code)
		}
	}
	// A sane offset is accepted and changes nothing about the row set — it only
	// moves day boundaries, which the store test covers.
	rec := trendRequest(t, gw, "liwg", "?tz_offset=-480&days=7")
	if rec.Code != http.StatusOK {
		t.Fatalf("tz_offset=-480 must be accepted, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestChangeTrend_NoStoreConfigured(t *testing.T) {
	gw := NewGateway(ServeGatewayConfig{})
	rec := trendRequest(t, gw, "liwg", "")
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("want 503 without a store, got %d", rec.Code)
	}
}

// --- test helpers -----------------------------------------------------------

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func mustCreateRun(t *testing.T, s state.Store, id, status, env string, at time.Time) {
	t.Helper()
	if err := s.CreateRun(context.Background(), &state.Run{
		ID: id, WorkflowName: "wf", TemplateName: "tpl", Status: status,
		CreatedAt: at, UpdatedAt: at, Creator: "liwg", IncidentID: env,
	}); err != nil {
		t.Fatalf("create run %s: %v", id, err)
	}
}

// loadTestAuthorizer writes a matrix that grants one team view+plan in prod
// only, registers a subject in it, and loads the authorizer.
func loadTestAuthorizer(t *testing.T, s state.Store, defaultEnv string) *authz.Authorizer {
	t.Helper()
	dir := t.TempDir()
	writeFile(t, dir+"/users.yaml", "users:\n  - name: liwg\n    team: platform\n    role: sre\n")
	writeFile(t, dir+"/permissions.yaml",
		"teams:\n  - name: platform\n    environments:\n      - name: prod\n        actions: [view, plan]\n      - name: dev\n        actions: [view]\n")
	a, err := authz.Load(dir, defaultEnv)
	if err != nil {
		t.Fatalf("load authorizer: %v", err)
	}
	return a
}

func TestChangeTrend_TenantStoreScopesTheAggregate(t *testing.T) {
	// The trend reads every row in the window, so without tenant scoping it
	// would be the one aggregate that leaks across tenants — which is exactly
	// what the TenantStore.RunTrend override exists to prevent. This asserts
	// the override is wired (enabled resolver + a tenant-free context is
	// refused), and that a scoped context sees only its own rows.
	base := newTestStore(t)
	now := time.Now()
	if err := base.CreateRun(context.Background(), &state.Run{
		ID: "t1", WorkflowName: "wf", TemplateName: "tpl", Status: runstatus.StatusCompleted,
		CreatedAt: now.Add(-time.Hour), UpdatedAt: now, Creator: "liwg", IncidentID: "prod",
		TenantID: "tenant-a",
	}); err != nil {
		t.Fatalf("create tenant-a run: %v", err)
	}
	if err := base.CreateRun(context.Background(), &state.Run{
		ID: "t2", WorkflowName: "wf", TemplateName: "tpl", Status: runstatus.StatusCompleted,
		CreatedAt: now.Add(-2 * time.Hour), UpdatedAt: now, Creator: "liwg", IncidentID: "prod",
		TenantID: "tenant-b",
	}); err != nil {
		t.Fatalf("create tenant-b run: %v", err)
	}

	ts := tenant.NewTenantStore(base, tenant.NewResolver(true), nil)

	// No tenant in context: the resolver refuses rather than guessing.
	if _, _, err := ts.RunTrend(context.Background(), state.RunTrendFilter{Days: 3}); err == nil {
		t.Fatal("want the resolver to refuse a context with no tenant")
	}

	ctx := tenant.ContextWithTenant(context.Background(), "tenant-a")
	buckets, _, err := ts.RunTrend(ctx, state.RunTrendFilter{Days: 3})
	if err != nil {
		t.Fatalf("RunTrend(scoped): %v", err)
	}
	total := 0
	for _, b := range buckets {
		total += b.Total
	}
	if total != 1 {
		t.Fatalf("tenant-a counted %d runs (want 1: tenant-b's run must not leak)", total)
	}
}
