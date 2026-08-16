package calendar

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newTestStore opens a fresh SQLite database in a temp dir and wraps it with
// a calendar SQLiteStore. The store is closed automatically when the test
// ends.
func newTestStore(t *testing.T) *SQLiteStore {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	path := filepath.Join(dir, "calendar-test.db")
	db, err := sql.Open("sqlite", path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	// Apply pragmas matching production.
	for _, p := range []string{
		"PRAGMA journal_mode=WAL",
		"PRAGMA foreign_keys=ON",
		"PRAGMA busy_timeout=5000",
	} {
		_, err := db.ExecContext(ctx, p)
		require.NoError(t, err)
	}

	store, err := NewSQLiteStore(ctx, db)
	require.NoError(t, err)
	return store
}

// mustParseTime parses an RFC3339 timestamp or fails the test.
func mustParseTime(t *testing.T, s string) time.Time {
	t.Helper()
	tt, err := time.Parse(time.RFC3339, s)
	require.NoError(t, err)
	return tt.UTC()
}

// parseTime parses an RFC3339 timestamp, panicking on error. Used in non-test
// helpers (like sampleWindow) where a *testing.T is not available.
func parseTime(s string) time.Time {
	tt, err := time.Parse(time.RFC3339, s)
	if err != nil {
		panic(err)
	}
	return tt.UTC()
}

// sampleWindow builds a window with sensible defaults; callers mutate the
// returned pointer before persisting.
func sampleWindow(id string) *Window {
	return &Window{
		ID:           id,
		Name:         "maintenance-" + id,
		StartTime:    parseTime("2026-08-16T10:00:00Z"),
		EndTime:      parseTime("2026-08-16T12:00:00Z"),
		TargetLabels: []string{"web", "db"},
		IsFrozen:     false,
		CreatedAt:    parseTime("2026-08-15T00:00:00Z"),
		UpdatedAt:    parseTime("2026-08-15T00:00:00Z"),
	}
}

// =========================================================================
// Schema / construction
// =========================================================================

func TestEnsureSchema_Idempotent(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	// Re-applying on the same DB must succeed.
	require.NoError(t, EnsureSchema(ctx, store.DB()))
}

func TestNewSQLiteStore_NilDB(t *testing.T) {
	_, err := NewSQLiteStore(context.Background(), nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "nil db handle")
}

// =========================================================================
// Window CRUD
// =========================================================================

func TestWindow_CRUD(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	w := sampleWindow("win-1")
	w.TargetLabels = []string{"web", "db", "cache"}

	// Create.
	require.NoError(t, store.CreateWindow(ctx, w))

	// Get.
	got, err := store.GetWindow(ctx, w.ID)
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, w.ID, got.ID)
	assert.Equal(t, w.Name, got.Name)
	assert.True(t, w.StartTime.Equal(got.StartTime))
	assert.True(t, w.EndTime.Equal(got.EndTime))
	assert.Equal(t, []string{"web", "db", "cache"}, got.TargetLabels,
		"target labels should round-trip in original order at store layer")
	assert.False(t, got.IsFrozen)

	// Update.
	got.Name = "renamed"
	got.IsFrozen = true
	got.UpdatedAt = got.UpdatedAt.Add(time.Hour)
	require.NoError(t, store.UpdateWindow(ctx, got))
	updated, err := store.GetWindow(ctx, w.ID)
	require.NoError(t, err)
	assert.Equal(t, "renamed", updated.Name)
	assert.True(t, updated.IsFrozen)

	// Delete.
	require.NoError(t, store.DeleteWindow(ctx, w.ID))
	deleted, err := store.GetWindow(ctx, w.ID)
	require.NoError(t, err)
	assert.Nil(t, deleted, "deleted window should be gone")
}

func TestWindow_GetNotFound(t *testing.T) {
	store := newTestStore(t)
	got, err := store.GetWindow(context.Background(), "nope")
	require.NoError(t, err)
	assert.Nil(t, got)
}

func TestWindow_UpdateNotFound(t *testing.T) {
	store := newTestStore(t)
	w := sampleWindow("missing")
	err := store.UpdateWindow(context.Background(), w)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not found")
}

func TestWindow_CreateValidation(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	cases := []struct {
		name string
		mod  func(*Window)
	}{
		{"empty id", func(w *Window) { w.ID = "" }},
		{"empty name", func(w *Window) { w.Name = "" }},
		{"end before start", func(w *Window) {
			w.StartTime = mustParseTime(t, "2026-08-16T12:00:00Z")
			w.EndTime = mustParseTime(t, "2026-08-16T10:00:00Z")
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := sampleWindow("val-" + tc.name)
			tc.mod(w)
			err := store.CreateWindow(ctx, w)
			require.Error(t, err)
		})
	}
}

func TestWindow_CreateNil(t *testing.T) {
	store := newTestStore(t)
	require.Error(t, store.CreateWindow(context.Background(), nil))
	require.Error(t, store.UpdateWindow(context.Background(), nil))
}

func TestWindow_List(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	for i, id := range []string{"win-a", "win-b", "win-c"} {
		w := sampleWindow(id)
		w.StartTime = w.StartTime.Add(time.Duration(i) * time.Hour)
		w.EndTime = w.EndTime.Add(time.Duration(i) * time.Hour)
		require.NoError(t, store.CreateWindow(ctx, w))
	}

	// List all.
	all, err := store.ListWindows(ctx, WindowFilter{})
	require.NoError(t, err)
	assert.Len(t, all, 3)
	// Ordered by start_time ascending.
	assert.Equal(t, "win-a", all[0].ID)
	assert.Equal(t, "win-c", all[2].ID)

	// List with limit.
	limited, err := store.ListWindows(ctx, WindowFilter{Limit: 2})
	require.NoError(t, err)
	assert.Len(t, limited, 2)

	// List by name.
	byName, err := store.ListWindows(ctx, WindowFilter{Name: "maintenance-win-b"})
	require.NoError(t, err)
	require.Len(t, byName, 1)
	assert.Equal(t, "win-b", byName[0].ID)
}

func TestWindow_ListByFrozen(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	frozen := sampleWindow("fz-1")
	frozen.IsFrozen = true
	require.NoError(t, store.CreateWindow(ctx, frozen))

	normal := sampleWindow("cw-1")
	require.NoError(t, store.CreateWindow(ctx, normal))

	frozenOnly := ptrBool(true)
	got, err := store.ListWindows(ctx, WindowFilter{IsFrozen: frozenOnly})
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, "fz-1", got[0].ID)

	normalOnly := ptrBool(false)
	got2, err := store.ListWindows(ctx, WindowFilter{IsFrozen: normalOnly})
	require.NoError(t, err)
	require.Len(t, got2, 1)
	assert.Equal(t, "cw-1", got2[0].ID)
}

func TestWindow_ListOnlyActive(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	// Window active at 2026-08-16T11:00:00Z.
	active := sampleWindow("active")
	active.StartTime = mustParseTime(t, "2026-08-16T10:00:00Z")
	active.EndTime = mustParseTime(t, "2026-08-16T12:00:00Z")
	require.NoError(t, store.CreateWindow(ctx, active))

	// Window already ended.
	past := sampleWindow("past")
	past.StartTime = mustParseTime(t, "2026-08-15T10:00:00Z")
	past.EndTime = mustParseTime(t, "2026-08-15T12:00:00Z")
	require.NoError(t, store.CreateWindow(ctx, past))

	// Window in the future.
	future := sampleWindow("future")
	future.StartTime = mustParseTime(t, "2026-08-17T10:00:00Z")
	future.EndTime = mustParseTime(t, "2026-08-17T12:00:00Z")
	require.NoError(t, store.CreateWindow(ctx, future))

	now := mustParseTime(t, "2026-08-16T11:00:00Z")
	got, err := store.ListWindows(ctx, WindowFilter{OnlyActive: true, Now: now})
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, "active", got[0].ID)
}

func TestWindow_UTCNormalisation(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	w := sampleWindow("tz-1")
	// Deliberately set a non-UTC zone; store should normalise to UTC.
	loc, _ := time.LoadLocation("America/New_York")
	w.StartTime = time.Date(2026, 8, 16, 10, 0, 0, 0, loc)
	w.EndTime = w.StartTime.Add(2 * time.Hour)
	require.NoError(t, store.CreateWindow(ctx, w))

	got, err := store.GetWindow(ctx, w.ID)
	require.NoError(t, err)
	assert.Equal(t, time.UTC, got.StartTime.Location())
	assert.Equal(t, time.UTC, got.EndTime.Location())
}

// =========================================================================
// CalendarService — freeze-period logic
// =========================================================================

func TestService_IsFrozen(t *testing.T) {
	store := newTestStore(t)
	svc := NewCalendarService(store)
	ctx := context.Background()

	// Freeze window active 2026-08-16T10:00:00Z..12:00:00Z covering "web".
	fz := sampleWindow("fz-1")
	fz.IsFrozen = true
	fz.StartTime = mustParseTime(t, "2026-08-16T10:00:00Z")
	fz.EndTime = mustParseTime(t, "2026-08-16T12:00:00Z")
	fz.TargetLabels = []string{"web", "db"}
	require.NoError(t, svc.CreateWindow(ctx, fz))

	// Inside the freeze window, target "web" is frozen.
	at := mustParseTime(t, "2026-08-16T11:00:00Z")
	frozen, err := svc.IsFrozenAt(ctx, []string{"web"}, at)
	require.NoError(t, err)
	assert.True(t, frozen, "web should be frozen at 11:00")

	// Outside the freeze window, not frozen.
	outside := mustParseTime(t, "2026-08-16T13:00:00Z")
	frozen2, err := svc.IsFrozenAt(ctx, []string{"web"}, outside)
	require.NoError(t, err)
	assert.False(t, frozen2, "web should not be frozen at 13:00")

	// Inside the window but unrelated target.
	frozen3, err := svc.IsFrozenAt(ctx, []string{"cache"}, at)
	require.NoError(t, err)
	assert.False(t, frozen3, "cache should not be frozen")

	// Multiple targets, one frozen.
	frozen4, err := svc.IsFrozenAt(ctx, []string{"cache", "db"}, at)
	require.NoError(t, err)
	assert.True(t, frozen4, "db is frozen so the set is frozen")

	// Empty target set with an active freeze => frozen.
	frozen5, err := svc.IsFrozenAt(ctx, nil, at)
	require.NoError(t, err)
	assert.True(t, frozen5, "empty target set with active freeze should be frozen")
}

func TestService_IsFrozen_NoFreezeWindows(t *testing.T) {
	store := newTestStore(t)
	svc := NewCalendarService(store)
	ctx := context.Background()

	// A normal (non-frozen) window does not freeze anything.
	w := sampleWindow("cw-1")
	w.IsFrozen = false
	require.NoError(t, svc.CreateWindow(ctx, w))

	frozen, err := svc.IsFrozenAt(ctx, []string{"web"}, w.StartTime.Add(time.Hour))
	require.NoError(t, err)
	assert.False(t, frozen)
}

func TestService_AssertNotFrozen(t *testing.T) {
	store := newTestStore(t)
	svc := NewCalendarService(store)
	ctx := context.Background()

	fz := sampleWindow("fz-1")
	fz.IsFrozen = true
	fz.TargetLabels = []string{"web"}
	require.NoError(t, svc.CreateWindow(ctx, fz))

	// At the freeze window's midpoint, asserting on "web" should fail.
	at := fz.StartTime.Add(time.Hour)
	// IsFrozenAt is time-injective; AssertNotFrozen uses time.Now(). To test
	// the assertion logic without depending on the wall clock we exercise
	// IsFrozenAt directly and replicate the assertion's branching.
	frozen, err := svc.IsFrozenAt(ctx, []string{"web"}, at)
	require.NoError(t, err)
	require.True(t, frozen)

	// Emergency override: the assertion should pass regardless.
	// We simulate by checking that emergency=true short-circuits.
	require.NoError(t, svc.AssertNotFrozen(ctx, []string{"web"}, true))
}

func TestService_ActiveWindowsAt(t *testing.T) {
	store := newTestStore(t)
	svc := NewCalendarService(store)
	ctx := context.Background()

	w1 := sampleWindow("w1")
	w1.StartTime = mustParseTime(t, "2026-08-16T10:00:00Z")
	w1.EndTime = mustParseTime(t, "2026-08-16T12:00:00Z")
	require.NoError(t, svc.CreateWindow(ctx, w1))

	w2 := sampleWindow("w2")
	w2.StartTime = mustParseTime(t, "2026-08-16T11:00:00Z")
	w2.EndTime = mustParseTime(t, "2026-08-16T13:00:00Z")
	require.NoError(t, svc.CreateWindow(ctx, w2))

	w3 := sampleWindow("w3")
	w3.StartTime = mustParseTime(t, "2026-08-16T14:00:00Z")
	w3.EndTime = mustParseTime(t, "2026-08-16T15:00:00Z")
	require.NoError(t, svc.CreateWindow(ctx, w3))

	at := mustParseTime(t, "2026-08-16T11:30:00Z")
	active, err := svc.ActiveWindowsAt(ctx, at)
	require.NoError(t, err)
	require.Len(t, active, 2)
	ids := []string{active[0].ID, active[1].ID}
	assert.ElementsMatch(t, []string{"w1", "w2"}, ids)
}

func TestService_NormalisesOnCreate(t *testing.T) {
	store := newTestStore(t)
	svc := NewCalendarService(store)
	ctx := context.Background()

	w := sampleWindow("norm-1")
	w.TargetLabels = []string{"db", "web", "app"}
	loc, _ := time.LoadLocation("Asia/Tokyo")
	w.StartTime = time.Date(2026, 8, 16, 10, 0, 0, 0, loc)
	w.EndTime = w.StartTime.Add(time.Hour)
	require.NoError(t, svc.CreateWindow(ctx, w))

	got, err := svc.GetWindow(ctx, w.ID)
	require.NoError(t, err)
	assert.Equal(t, []string{"app", "db", "web"}, got.TargetLabels, "labels should be sorted")
	assert.Equal(t, time.UTC, got.StartTime.Location())
}
