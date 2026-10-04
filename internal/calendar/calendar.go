// Package calendar implements the LEVEE change calendar subsystem.
//
// It provides CRUD over change windows (recurring or one-shot), freeze-period
// enforcement that blocks new changes from being created against frozen target
// sets, and conflict detection between overlapping windows that share targets.
//
// All timestamps are stored in UTC. The schema is created lazily via
// EnsureSchema on top of an existing *sql.DB handle, which is typically the one
// internal/state already opened, so calendar data lives next to run/batch/
// step/trace data instead of in a separate file.
//
// The handle may be SQLite or PostgreSQL, and the store has to be told which:
// the two dialects disagree on placeholders (`?` vs `$n`) and on the timestamp
// type (`DATETIME` does not exist in PostgreSQL — measured: applying the SQLite
// DDL to a live PostgreSQL fails with `type "datetime" does not exist`). Dialect
// is therefore a required argument with no zero value, because guessing wrong
// produces either a schema that cannot be created or queries that cannot bind.
package calendar

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Dialect is the SQL surface a Store speaks against its *sql.DB handle.
type Dialect int

const (
	// DialectUnknown is the zero value and is refused: a store that has not been
	// told what it is talking to would default to SQLite against a PostgreSQL
	// handle, which fails at schema time in the best case and silently mis-binds
	// in the worst.
	DialectUnknown Dialect = iota
	// DialectSQLite is a handle opened by state.NewSQLiteStore.
	DialectSQLite
	// DialectPostgres is a handle opened by state.NewPGStore (pgx driver).
	DialectPostgres
)

// String names the dialect for error messages and logs.
func (d Dialect) String() string {
	switch d {
	case DialectSQLite:
		return "sqlite"
	case DialectPostgres:
		return "postgres"
	default:
		return "unknown"
	}
}

// =========================================================================
// Domain types
// =========================================================================

// Window is a single change window. A window either represents a one-shot
// maintenance slot (RepeatRule empty, CronExpr empty) or a recurring slot
// expanded by a cron expression. When IsFrozen is true the window acts as a
// freeze period: new changes against any of TargetLabels are blocked while the
// window is active, unless the caller passes an emergency-approval override.
type Window struct {
	ID           string    `json:"id"`
	Name         string    `json:"name"`
	StartTime    time.Time `json:"start_time"`
	EndTime      time.Time `json:"end_time"`
	TargetLabels []string  `json:"target_labels"`
	IsFrozen     bool      `json:"is_frozen"`
	RepeatRule   string    `json:"repeat_rule,omitempty"` // human-readable hint, e.g. "weekly"
	CronExpr     string    `json:"cron_expr,omitempty"`   // 5-field cron: min hour day month weekday
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
}

// WindowFilter narrows ListWindows results. Empty fields are ignored; non-empty
// fields combine with AND semantics. OnlyActive limits results to windows that
// contain `now` (StartTime <= now < EndTime).
type WindowFilter struct {
	Name       string
	IsFrozen   *bool
	OnlyActive bool
	Now        time.Time // reference time for OnlyActive; zero means time.Now().UTC()
	Limit      int
}

// CalendarStore is the persistence abstraction for change windows.
// Implementations must be safe for concurrent use.
//
// Convention:
//   - CreateWindow inserts a new row; the caller must set ID.
//   - GetWindow returns (nil, nil) when the row does not exist.
//   - UpdateWindow overwrites all mutable columns; missing ID is an error.
//   - ListWindows applies the filter and returns a slice (possibly empty).
//   - DeleteWindow removes a row by ID; missing ID is not an error.
type CalendarStore interface {
	CreateWindow(ctx context.Context, w *Window) error
	GetWindow(ctx context.Context, id string) (*Window, error)
	ListWindows(ctx context.Context, filter WindowFilter) ([]*Window, error)
	UpdateWindow(ctx context.Context, w *Window) error
	DeleteWindow(ctx context.Context, id string) error
	Close() error
}

// =========================================================================
// Schema
// =========================================================================

// calendarSchemaSQLite creates the calendar_windows table on SQLite. Target
// labels are stored as a JSON array in a TEXT column; queries that need to
// filter by label perform the matching in Go after loading candidate rows. For
// the expected MVP scale (tens to low hundreds of windows) this is sufficient
// and avoids the complexity of a join table.
const calendarSchemaSQLite = `
CREATE TABLE IF NOT EXISTS calendar_windows (
    id            TEXT    PRIMARY KEY,
    name          TEXT    NOT NULL,
    start_time    DATETIME NOT NULL,
    end_time      DATETIME NOT NULL,
    target_labels TEXT    NOT NULL DEFAULT '[]', -- JSON array of labels
    is_frozen     INTEGER NOT NULL DEFAULT 0,    -- 0 = change window, 1 = freeze period
    repeat_rule   TEXT    NOT NULL DEFAULT '',
    cron_expr     TEXT    NOT NULL DEFAULT '',
    created_at    DATETIME NOT NULL,
    updated_at    DATETIME NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_calendar_windows_start_time ON calendar_windows (start_time);
CREATE INDEX IF NOT EXISTS idx_calendar_windows_end_time   ON calendar_windows (end_time);
CREATE INDEX IF NOT EXISTS idx_calendar_windows_is_frozen  ON calendar_windows (is_frozen);
CREATE INDEX IF NOT EXISTS idx_calendar_windows_name       ON calendar_windows (name);
`

// calendarSchemaPostgres is the same table in the shape PostgreSQL accepts:
// TIMESTAMPTZ instead of the (non-existent) DATETIME, per internal/state's
// pgschema.sql convention.
//
// is_frozen stays INTEGER rather than becoming BOOLEAN on purpose: the Go side
// binds 0/1 (boolToInt) and scans an int back (scanWindow), because one of the
// two dialects has no boolean type. Making this column BOOLEAN would mean two
// bind paths for the one field; the cost of the shared int is a check constraint
// that keeps it 0 or 1, so a hand-edited row cannot invent a third state.
const calendarSchemaPostgres = `
CREATE TABLE IF NOT EXISTS calendar_windows (
    id            TEXT PRIMARY KEY,
    name          TEXT NOT NULL,
    start_time    TIMESTAMPTZ NOT NULL,
    end_time      TIMESTAMPTZ NOT NULL,
    target_labels TEXT NOT NULL DEFAULT '[]',
    is_frozen     INTEGER NOT NULL DEFAULT 0 CHECK (is_frozen IN (0, 1)),
    repeat_rule   TEXT NOT NULL DEFAULT '',
    cron_expr     TEXT NOT NULL DEFAULT '',
    created_at    TIMESTAMPTZ NOT NULL,
    updated_at    TIMESTAMPTZ NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_calendar_windows_start_time ON calendar_windows (start_time);
CREATE INDEX IF NOT EXISTS idx_calendar_windows_end_time   ON calendar_windows (end_time);
CREATE INDEX IF NOT EXISTS idx_calendar_windows_is_frozen  ON calendar_windows (is_frozen);
CREATE INDEX IF NOT EXISTS idx_calendar_windows_name       ON calendar_windows (name);
`

// SchemaDDLFor returns the DDL for one dialect. Exported so internal/dbschema
// style consistency checks and deployment docs can quote the same text the
// store applies instead of retyping it.
func SchemaDDLFor(d Dialect) string {
	if d == DialectPostgres {
		return calendarSchemaPostgres
	}
	return calendarSchemaSQLite
}

// EnsureSchema applies the calendar schema for the given dialect. It is
// idempotent: running it on an already-migrated database is a no-op. Callers
// typically invoke it once at process start, right after opening the shared
// *sql.DB handle.
func EnsureSchema(ctx context.Context, db *sql.DB, d Dialect) error {
	if db == nil {
		return fmt.Errorf("calendar: ensure schema: nil db handle")
	}
	if d != DialectSQLite && d != DialectPostgres {
		return fmt.Errorf("calendar: ensure schema: dialect must be DialectSQLite or DialectPostgres, got %d", int(d))
	}
	if _, err := db.ExecContext(ctx, SchemaDDLFor(d)); err != nil {
		return fmt.Errorf("calendar: apply schema (%s): %w", d, err)
	}
	return nil
}

// =========================================================================
// Store
// =========================================================================

// Store is the SQL-backed implementation of CalendarStore. It relies on the
// caller to provide a *sql.DB handle (typically shared with internal/state) and
// to declare which dialect that handle speaks. Concurrency safety comes from
// database/sql's connection pool plus SQLite WAL mode.
type Store struct {
	db      *sql.DB
	dialect Dialect
}

// NewStore wraps an existing *sql.DB handle and ensures the calendar schema
// exists in the given dialect. The caller retains ownership of the handle;
// Close on the returned store is a no-op (the caller closes the shared handle).
//
// An undeclared dialect is refused rather than defaulted: applying the SQLite
// DDL to a PostgreSQL handle fails at CREATE TABLE, while applying `?`
// placeholders to it fails at every query — both are quieter than refusing here.
func NewStore(ctx context.Context, db *sql.DB, d Dialect) (*Store, error) {
	if db == nil {
		return nil, fmt.Errorf("calendar: new store: nil db handle")
	}
	if err := EnsureSchema(ctx, db, d); err != nil {
		return nil, err
	}
	return &Store{db: db, dialect: d}, nil
}

// ph renders the placeholder for the n-th bound argument (1-based).
func (s *Store) ph(n int) string {
	if s.dialect == DialectPostgres {
		return "$" + strconv.Itoa(n)
	}
	return "?"
}

// phList renders n placeholders for a VALUES list or an IN clause, numbered
// from `first`.
func (s *Store) phList(n, first int) string {
	parts := make([]string, 0, n)
	for i := 0; i < n; i++ {
		parts = append(parts, s.ph(first+i))
	}
	return strings.Join(parts, ", ")
}

// compile-time proof that the store satisfies the interface it documents.
var _ CalendarStore = (*Store)(nil)

// Dialect reports which SQL surface this store speaks.
func (s *Store) Dialect() Dialect { return s.dialect }

// DB exposes the underlying handle for advanced use cases (e.g. backups).
func (s *Store) DB() *sql.DB { return s.db }

// Close is a no-op: the store does not own the *sql.DB handle.
func (s *Store) Close() error { return nil }

// CreateWindow inserts a new calendar window row.
func (s *Store) CreateWindow(ctx context.Context, w *Window) error {
	if w == nil {
		return fmt.Errorf("calendar: create window: nil window")
	}
	if err := validateWindow(w); err != nil {
		return err
	}
	labelsJSON, err := json.Marshal(w.TargetLabels)
	if err != nil {
		return fmt.Errorf("calendar: marshal target labels: %w", err)
	}
	_, err = s.db.ExecContext(ctx, fmt.Sprintf(`INSERT INTO calendar_windows
		(id, name, start_time, end_time, target_labels, is_frozen,
		 repeat_rule, cron_expr, created_at, updated_at)
		VALUES (%s)`, s.phList(10, 1)),
		w.ID, w.Name, w.StartTime.UTC(), w.EndTime.UTC(), string(labelsJSON),
		boolToInt(w.IsFrozen), w.RepeatRule, w.CronExpr,
		w.CreatedAt.UTC(), w.UpdatedAt.UTC(),
	)
	if err != nil {
		return fmt.Errorf("calendar: create window: %w", err)
	}
	return nil
}

// GetWindow returns the window with the given id, or (nil, nil) if not found.
func (s *Store) GetWindow(ctx context.Context, id string) (*Window, error) {
	row := s.db.QueryRowContext(ctx, fmt.Sprintf(`SELECT
		id, name, start_time, end_time, target_labels, is_frozen,
		repeat_rule, cron_expr, created_at, updated_at
		FROM calendar_windows WHERE id = %s`, s.ph(1)), id)
	w, err := scanWindow(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("calendar: get window %q: %w", id, err)
	}
	return w, nil
}

// ListWindows returns windows matching the filter, ordered by start_time
// ascending.
func (s *Store) ListWindows(ctx context.Context, filter WindowFilter) ([]*Window, error) {
	var (
		clauses []string
		args    []any
	)
	// Placeholders are numbered by the position the value lands in `args`, so a
	// clause that forgot to bump this counter would bind the wrong column rather
	// than fail loudly — hence one helper for both.
	next := func(v any) string {
		args = append(args, v)
		return s.ph(len(args))
	}
	if filter.Name != "" {
		clauses = append(clauses, "name = "+next(filter.Name))
	}
	if filter.IsFrozen != nil {
		clauses = append(clauses, "is_frozen = "+next(boolToInt(*filter.IsFrozen)))
	}
	now := filter.Now
	if now.IsZero() {
		now = time.Now().UTC()
	}
	if filter.OnlyActive {
		clauses = append(clauses, "start_time <= "+next(now.UTC()))
		clauses = append(clauses, "end_time > "+next(now.UTC()))
	}

	q := `SELECT id, name, start_time, end_time, target_labels, is_frozen,
		repeat_rule, cron_expr, created_at, updated_at
		FROM calendar_windows`
	if len(clauses) > 0 {
		q += " WHERE " + strings.Join(clauses, " AND ") // #nosec G202 -- clause fragments are static; all values bind via placeholders
	}
	q += " ORDER BY start_time ASC"
	if filter.Limit > 0 {
		// #nosec G202 -- next() emits a placeholder token ($n / ?) and appends the
		// value to args; the limit never reaches the statement as SQL text.
		q += " LIMIT " + next(filter.Limit)
	}

	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("calendar: list windows: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []*Window
	for rows.Next() {
		w, err := scanWindow(rows)
		if err != nil {
			return nil, fmt.Errorf("calendar: list windows scan: %w", err)
		}
		out = append(out, w)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("calendar: list windows rows: %w", err)
	}
	return out, nil
}

// UpdateWindow overwrites all mutable columns of an existing window.
func (s *Store) UpdateWindow(ctx context.Context, w *Window) error {
	if w == nil {
		return fmt.Errorf("calendar: update window: nil window")
	}
	if err := validateWindow(w); err != nil {
		return err
	}
	labelsJSON, err := json.Marshal(w.TargetLabels)
	if err != nil {
		return fmt.Errorf("calendar: marshal target labels: %w", err)
	}
	// SET clauses bind as arguments 1..8 and the WHERE id as 9 — the order the
	// varargs below are written in.
	cols := []string{"name", "start_time", "end_time", "target_labels", "is_frozen",
		"repeat_rule", "cron_expr", "updated_at"}
	sets := make([]string, 0, len(cols))
	for i, c := range cols {
		sets = append(sets, c+"="+s.ph(i+1))
	}
	res, err := s.db.ExecContext(ctx, fmt.Sprintf(`UPDATE calendar_windows SET
		%s
		WHERE id=%s`, strings.Join(sets, ", "), s.ph(9)),
		w.Name, w.StartTime.UTC(), w.EndTime.UTC(), string(labelsJSON),
		boolToInt(w.IsFrozen), w.RepeatRule, w.CronExpr,
		w.UpdatedAt.UTC(), w.ID,
	)
	if err != nil {
		return fmt.Errorf("calendar: update window %q: %w", w.ID, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("calendar: update window %q: not found", w.ID)
	}
	return nil
}

// DeleteWindow removes a window by id. Missing id is not an error.
func (s *Store) DeleteWindow(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx, fmt.Sprintf(`DELETE FROM calendar_windows WHERE id = %s`, s.ph(1)), id)
	if err != nil {
		return fmt.Errorf("calendar: delete window %q: %w", id, err)
	}
	return nil
}

// =========================================================================
// Helpers
// =========================================================================

// scanner abstracts *sql.Row and *sql.Rows for scanWindow.
type scanner interface {
	Scan(dest ...any) error
}

func scanWindow(s scanner) (*Window, error) {
	w := &Window{}
	var (
		labelsJSON string
		frozenInt  int
	)
	err := s.Scan(
		&w.ID, &w.Name, &w.StartTime, &w.EndTime, &labelsJSON, &frozenInt,
		&w.RepeatRule, &w.CronExpr, &w.CreatedAt, &w.UpdatedAt,
	)
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal([]byte(labelsJSON), &w.TargetLabels); err != nil {
		return nil, fmt.Errorf("unmarshal target labels: %w", err)
	}
	w.IsFrozen = frozenInt != 0
	// SQLite stores DATETIME as TEXT in ISO8601 which database/sql parses into
	// local time. Normalise to UTC for consistent downstream comparisons.
	w.StartTime = w.StartTime.UTC()
	w.EndTime = w.EndTime.UTC()
	w.CreatedAt = w.CreatedAt.UTC()
	w.UpdatedAt = w.UpdatedAt.UTC()
	return w, nil
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// validateWindow performs structural validation shared by Create and Update.
func validateWindow(w *Window) error {
	if w.ID == "" {
		return fmt.Errorf("calendar: window id is required")
	}
	if w.Name == "" {
		return fmt.Errorf("calendar: window name is required")
	}
	if w.EndTime.Before(w.StartTime) {
		return fmt.Errorf("calendar: end_time %s is before start_time %s",
			w.EndTime.Format(time.RFC3339), w.StartTime.Format(time.RFC3339))
	}
	return nil
}

// =========================================================================
// CalendarService
// =========================================================================

// CalendarService wraps a CalendarStore and adds business logic: freeze-period
// enforcement and target-overlap helpers. It is the layer that the rest of
// LEVEE (engine, plan phase, CLI) talks to; raw store access is reserved for
// tests and admin tooling.
type CalendarService struct {
	store CalendarStore
}

// NewCalendarService builds a CalendarService backed by the given store.
func NewCalendarService(store CalendarStore) *CalendarService {
	return &CalendarService{store: store}
}

// Store exposes the underlying store for admin / test access.
func (s *CalendarService) Store() CalendarStore { return s.store }

// CreateWindow inserts a new window. It normalises timestamps to UTC and
// sorts TargetLabels for deterministic comparison. It does NOT enforce
// freeze-period semantics; callers that need that should call
// AssertNotFrozen first.
func (s *CalendarService) CreateWindow(ctx context.Context, w *Window) error {
	normaliseWindow(w)
	return s.store.CreateWindow(ctx, w)
}

// GetWindow returns the window with the given id, or (nil, nil) if not found.
func (s *CalendarService) GetWindow(ctx context.Context, id string) (*Window, error) {
	return s.store.GetWindow(ctx, id)
}

// ListWindows returns windows matching the filter.
func (s *CalendarService) ListWindows(ctx context.Context, filter WindowFilter) ([]*Window, error) {
	return s.store.ListWindows(ctx, filter)
}

// UpdateWindow overwrites all mutable columns of an existing window.
func (s *CalendarService) UpdateWindow(ctx context.Context, w *Window) error {
	normaliseWindow(w)
	return s.store.UpdateWindow(ctx, w)
}

// DeleteWindow removes a window by id.
func (s *CalendarService) DeleteWindow(ctx context.Context, id string) error {
	return s.store.DeleteWindow(ctx, id)
}

// IsFrozen reports whether any freeze-period window is currently active and
// covers at least one of the supplied target labels. The reference time is
// time.Now().UTC(); pass an explicit time via IsFrozenAt for tests.
func (s *CalendarService) IsFrozen(ctx context.Context, targetLabels []string) (bool, error) {
	return s.IsFrozenAt(ctx, targetLabels, time.Now().UTC())
}

// IsFrozenAt is the time-injective variant of IsFrozen. A target label is
// considered frozen if there exists a window with IsFrozen=true whose
// [StartTime, EndTime) contains `at` and whose TargetLabels intersect the
// supplied set. An empty targetLabels slice means "any target" — the function
// returns true if any freeze window is active at `at`.
func (s *CalendarService) IsFrozenAt(ctx context.Context, targetLabels []string, at time.Time) (bool, error) {
	windows, err := s.FreezeWindowsAt(ctx, targetLabels, at)
	if err != nil {
		return false, err
	}
	return len(windows) > 0, nil
}

// FreezeWindowsAt returns the active freeze windows covering targetLabels at
// `at`, i.e. exactly what makes a change refusal, with the names an operator
// has to look up to clear it. An error here is a store failure, not "frozen".
func (s *CalendarService) FreezeWindowsAt(ctx context.Context, targetLabels []string, at time.Time) ([]*Window, error) {
	at = at.UTC()
	windows, err := s.store.ListWindows(ctx, WindowFilter{
		IsFrozen:   ptrBool(true),
		OnlyActive: true,
		Now:        at,
	})
	if err != nil {
		return nil, fmt.Errorf("calendar: list frozen windows: %w", err)
	}
	var hits []*Window
	for _, w := range windows {
		if len(targetLabels) == 0 || intersects(toSet(targetLabels), w.TargetLabels) {
			hits = append(hits, w)
		}
	}
	return hits, nil
}

// ErrFrozen reports a change refused by the calendar. Callers map it to their
// own transport (gRPC FailedPrecondition, CLI exit code) the same way they map
// a closed change window — a refusal the operator cannot distinguish from a
// policy error is a refusal they cannot act on.
var ErrFrozen = errors.New("calendar: target set is inside an active freeze period")

// AssertNotFrozen returns an error if any of targetLabels is currently frozen.
// See AssertNotFrozenAt for the time-injective form.
func (s *CalendarService) AssertNotFrozen(ctx context.Context, targetLabels []string, emergency bool) error {
	return s.AssertNotFrozenAt(ctx, targetLabels, time.Now().UTC(), emergency)
}

// AssertNotFrozenAt is AssertNotFrozen with the reference instant supplied, so a
// test (or a scheduler deciding about a future moment) can pin the verdict
// without racing the wall clock. The error names the offending windows (id, name,
// end time) so the operator learns which freeze to look at and when it lifts;
// `emergency` bypasses the check and is the caller's declaration that it holds
// emergency authority — see wiring's gate for who is allowed to set it.
func (s *CalendarService) AssertNotFrozenAt(ctx context.Context, targetLabels []string, at time.Time, emergency bool) error {
	if emergency {
		return nil
	}
	hits, err := s.FreezeWindowsAt(ctx, targetLabels, at)
	if err != nil {
		return err
	}
	if len(hits) == 0 {
		return nil
	}
	parts := make([]string, 0, len(hits))
	for _, w := range hits {
		parts = append(parts, fmt.Sprintf("%s (%q, lifts %s)", w.ID, w.Name, w.EndTime.UTC().Format(time.RFC3339)))
	}
	return fmt.Errorf("%w: targets %v — freeze windows: %s", ErrFrozen, targetLabels, strings.Join(parts, ", "))
}

// ActiveWindowsAt returns all (non-frozen and frozen) windows whose
// [StartTime, EndTime) contains `at`. Useful for status dashboards.
func (s *CalendarService) ActiveWindowsAt(ctx context.Context, at time.Time) ([]*Window, error) {
	return s.store.ListWindows(ctx, WindowFilter{
		OnlyActive: true,
		Now:        at.UTC(),
	})
}

// normaliseWindow sorts TargetLabels and forces all timestamps to UTC. It
// mutates the window in place.
func normaliseWindow(w *Window) {
	if w == nil {
		return
	}
	sort.Strings(w.TargetLabels)
	w.StartTime = w.StartTime.UTC()
	w.EndTime = w.EndTime.UTC()
	w.CreatedAt = w.CreatedAt.UTC()
	w.UpdatedAt = w.UpdatedAt.UTC()
}

// ptrBool returns a pointer to b. Used to set WindowFilter.IsFrozen.
func ptrBool(b bool) *bool { return &b }

// toSet converts a slice of strings to a set keyed by element. Duplicate
// elements collapse.
func toSet(xs []string) map[string]struct{} {
	m := make(map[string]struct{}, len(xs))
	for _, x := range xs {
		m[x] = struct{}{}
	}
	return m
}

// intersects reports whether set and xs share at least one element.
func intersects(set map[string]struct{}, xs []string) bool {
	for _, x := range xs {
		if _, ok := set[x]; ok {
			return true
		}
	}
	return false
}
