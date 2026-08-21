// Package calendar implements the LEVEE change calendar subsystem.
//
// It provides CRUD over change windows (recurring or one-shot), freeze-period
// enforcement that blocks new changes from being created against frozen target
// sets, and conflict detection between overlapping windows that share targets.
//
// All timestamps are stored in UTC. The schema is created lazily via
// EnsureSchema on top of an existing *sql.DB handle (typically the same SQLite
// database used by internal/state, so calendar data lives next to run/batch/
// step/trace data in a single file).
package calendar

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

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
// SQLite schema
// =========================================================================

// calendarSchemaSQL creates the calendar_windows table. Target labels are
// stored as a JSON array in a TEXT column; queries that need to filter by
// label perform the matching in Go after loading candidate rows. For the
// expected MVP scale (tens to low hundreds of windows) this is sufficient
// and avoids the complexity of a join table.
const calendarSchemaSQL = `
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

// EnsureSchema applies the calendar schema to the given database. It is
// idempotent: running it on an already-migrated database is a no-op. Callers
// typically invoke it once at process start, right after opening the shared
// *sql.DB handle.
func EnsureSchema(ctx context.Context, db *sql.DB) error {
	if db == nil {
		return fmt.Errorf("calendar: ensure schema: nil db handle")
	}
	if _, err := db.ExecContext(ctx, calendarSchemaSQL); err != nil {
		return fmt.Errorf("calendar: apply schema: %w", err)
	}
	return nil
}

// =========================================================================
// SQLiteStore
// =========================================================================

// SQLiteStore is the SQLite-backed implementation of CalendarStore. It relies
// on the caller to provide a *sql.DB handle (typically shared with
// internal/state). Concurrency safety comes from database/sql's connection
// pool plus SQLite WAL mode.
type SQLiteStore struct {
	db *sql.DB
}

// NewSQLiteStore wraps an existing *sql.DB handle and ensures the calendar
// schema exists. The caller retains ownership of the handle; Close on the
// returned store is a no-op (the caller closes the shared handle).
func NewSQLiteStore(ctx context.Context, db *sql.DB) (*SQLiteStore, error) {
	if db == nil {
		return nil, fmt.Errorf("calendar: new sqlite store: nil db handle")
	}
	if err := EnsureSchema(ctx, db); err != nil {
		return nil, err
	}
	return &SQLiteStore{db: db}, nil
}

// DB exposes the underlying handle for advanced use cases (e.g. backups).
func (s *SQLiteStore) DB() *sql.DB { return s.db }

// Close is a no-op: the store does not own the *sql.DB handle.
func (s *SQLiteStore) Close() error { return nil }

// CreateWindow inserts a new calendar window row.
func (s *SQLiteStore) CreateWindow(ctx context.Context, w *Window) error {
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
	_, err = s.db.ExecContext(ctx, `INSERT INTO calendar_windows
		(id, name, start_time, end_time, target_labels, is_frozen,
		 repeat_rule, cron_expr, created_at, updated_at)
		VALUES (?,?,?,?,?,?,?,?,?,?)`,
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
func (s *SQLiteStore) GetWindow(ctx context.Context, id string) (*Window, error) {
	row := s.db.QueryRowContext(ctx, `SELECT
		id, name, start_time, end_time, target_labels, is_frozen,
		repeat_rule, cron_expr, created_at, updated_at
		FROM calendar_windows WHERE id = ?`, id)
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
func (s *SQLiteStore) ListWindows(ctx context.Context, filter WindowFilter) ([]*Window, error) {
	var (
		clauses []string
		args    []any
	)
	if filter.Name != "" {
		clauses = append(clauses, "name = ?")
		args = append(args, filter.Name)
	}
	if filter.IsFrozen != nil {
		clauses = append(clauses, "is_frozen = ?")
		args = append(args, boolToInt(*filter.IsFrozen))
	}
	now := filter.Now
	if now.IsZero() {
		now = time.Now().UTC()
	}
	if filter.OnlyActive {
		clauses = append(clauses, "start_time <= ?")
		clauses = append(clauses, "end_time > ?")
		args = append(args, now.UTC(), now.UTC())
	}

	q := `SELECT id, name, start_time, end_time, target_labels, is_frozen,
		repeat_rule, cron_expr, created_at, updated_at
		FROM calendar_windows`
	if len(clauses) > 0 {
		q += " WHERE " + strings.Join(clauses, " AND ")
	}
	q += " ORDER BY start_time ASC"
	if filter.Limit > 0 {
		q += " LIMIT ?"
		args = append(args, filter.Limit)
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
func (s *SQLiteStore) UpdateWindow(ctx context.Context, w *Window) error {
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
	res, err := s.db.ExecContext(ctx, `UPDATE calendar_windows SET
		name=?, start_time=?, end_time=?, target_labels=?, is_frozen=?,
		repeat_rule=?, cron_expr=?, updated_at=?
		WHERE id=?`,
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
func (s *SQLiteStore) DeleteWindow(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM calendar_windows WHERE id = ?`, id)
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
	at = at.UTC()
	windows, err := s.store.ListWindows(ctx, WindowFilter{
		IsFrozen:   ptrBool(true),
		OnlyActive: true,
		Now:        at,
	})
	if err != nil {
		return false, fmt.Errorf("calendar: list frozen windows: %w", err)
	}
	if len(targetLabels) == 0 {
		return len(windows) > 0, nil
	}
	labelSet := toSet(targetLabels)
	for _, w := range windows {
		if intersects(labelSet, w.TargetLabels) {
			return true, nil
		}
	}
	return false, nil
}

// AssertNotFrozen returns an error if any of targetLabels is currently frozen.
// The error message lists the offending windows so callers can surface a
// helpful diagnostic. When emergency is true the check is bypassed (the
// caller has explicit emergency-approval authority).
func (s *CalendarService) AssertNotFrozen(ctx context.Context, targetLabels []string, emergency bool) error {
	if emergency {
		return nil
	}
	frozen, err := s.IsFrozen(ctx, targetLabels)
	if err != nil {
		return err
	}
	if frozen {
		return fmt.Errorf("calendar: target set %v is currently frozen; pass emergency approval to override",
			targetLabels)
	}
	return nil
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
