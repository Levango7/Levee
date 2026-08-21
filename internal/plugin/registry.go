// Plugin registry: SQLite-backed persistence of plugin metadata and state.
//
// The Registry is the durable counterpart to the in-memory PluginManager.
// It stores one row per installed plugin in the plugin_registry table and
// is the single source of truth for:
//
//   - plugin metadata (name, version, type, author, description, entry
//     point, host-version compatibility range);
//   - plugin state (installed / enabled / disabled / error);
//   - install provenance (binary path, signature, installed_at, updated_at);
//   - plugin configuration (the YAML config blob, stored as TEXT).
//
// The Registry also enforces two cross-cutting concerns:
//
//   - Version compatibility: before a plugin is enabled, IsCompatible
//     checks the plugin's MinHostVersion / MaxHostVersion against the
//     running host version using semver semantics (major.minor.patch).
//   - Signature verification: when a signature is recorded, VerifySignature
//     re-computes the SHA-256 of the binary and compares it to the stored
//     digest. This is an optional, defence-in-depth measure; the host
//     trusts the registry itself to be tamper-evident (the audit trail
//     records every install / enable / disable).
//
// The Registry is safe for concurrent use. It opens a single SQLite
// database file (or :memory:) and creates the plugin_registry table on
// first use.

package plugin

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite" // pure-Go SQLite driver
)

// --- Schema -----------------------------------------------------------------

// pluginRegistrySchema is the DDL for the plugin_registry table. It is
// applied idempotently when the Registry is opened.
const pluginRegistrySchema = `
CREATE TABLE IF NOT EXISTS plugin_registry (
    name            TEXT    PRIMARY KEY,
    version         TEXT    NOT NULL,
    type            TEXT    NOT NULL,
    author          TEXT    NOT NULL DEFAULT '',
    description     TEXT    NOT NULL DEFAULT '',
    entry_point     TEXT    NOT NULL DEFAULT 'plugin',
    min_host_version TEXT   NOT NULL DEFAULT '',
    max_host_version TEXT   NOT NULL DEFAULT '',
    state           TEXT    NOT NULL DEFAULT 'installed',
    binary_path     TEXT    NOT NULL DEFAULT '',
    config_yaml     TEXT    NOT NULL DEFAULT '',
    signature       TEXT    NOT NULL DEFAULT '',
    installed_at    DATETIME NOT NULL DEFAULT (datetime('now')),
    updated_at      DATETIME NOT NULL DEFAULT (datetime('now')),
    error_msg       TEXT    NOT NULL DEFAULT ''
);

CREATE INDEX IF NOT EXISTS idx_plugin_registry_state ON plugin_registry (state);
CREATE INDEX IF NOT EXISTS idx_plugin_registry_type ON plugin_registry (type);
`

// --- Registry record --------------------------------------------------------

// RegistryRecord is the persisted representation of a plugin. It mirrors
// the plugin_registry table row by row.
type RegistryRecord struct {
	Name           string      `json:"name"`
	Version        string      `json:"version"`
	Type           PluginType  `json:"type"`
	Author         string      `json:"author"`
	Description    string      `json:"description"`
	EntryPoint     string      `json:"entry_point"`
	MinHostVersion string      `json:"min_host_version,omitempty"`
	MaxHostVersion string      `json:"max_host_version,omitempty"`
	State          PluginState `json:"state"`
	BinaryPath     string      `json:"binary_path"`
	ConfigYAML     string      `json:"config_yaml,omitempty"`
	Signature      string      `json:"signature,omitempty"`
	InstalledAt    time.Time   `json:"installed_at"`
	UpdatedAt      time.Time   `json:"updated_at"`
	ErrorMsg       string      `json:"error_msg,omitempty"`
}

// --- Registry ---------------------------------------------------------------

// Registry is the SQLite-backed plugin registry. It is created once at
// host start-up and shared by the PluginManager and the CLI.
type Registry struct {
	db *sql.DB
}

// NewRegistry opens (or creates) the registry at the given database path.
// Use ":memory:" for an in-memory registry (useful in tests). The DDL is
// applied idempotently.
func NewRegistry(ctx context.Context, dbPath string) (*Registry, error) {
	if dbPath == "" {
		return nil, fmt.Errorf("plugin: empty registry db path")
	}

	db, err := openRegistryDB(ctx, dbPath)
	if err != nil {
		return nil, fmt.Errorf("plugin: open registry: %w", err)
	}

	if _, err := db.ExecContext(ctx, pluginRegistrySchema); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("plugin: apply registry schema: %w", err)
	}

	return &Registry{db: db}, nil
}

// openRegistryDB opens the SQLite database with the appropriate pragmas.
// It mirrors the connection handling used by the state package.
func openRegistryDB(ctx context.Context, dbPath string) (*sql.DB, error) {
	dsn := dbPath
	if dbPath == ":memory:" {
		dsn = "file::memory:?cache=shared"
	}

	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}

	if dbPath == ":memory:" {
		db.SetMaxOpenConns(1)
		db.SetMaxIdleConns(1)
	}

	pragmas := []string{
		"PRAGMA journal_mode=WAL",
		"PRAGMA foreign_keys=ON",
		"PRAGMA busy_timeout=5000",
	}
	for _, p := range pragmas {
		if _, err := db.ExecContext(ctx, p); err != nil {
			_ = db.Close()
			return nil, fmt.Errorf("apply pragma %q: %w", p, err)
		}
	}
	return db, nil
}

// Close closes the underlying database handle.
func (r *Registry) Close() error {
	if r == nil || r.db == nil {
		return nil
	}
	return r.db.Close()
}

// DB exposes the underlying *sql.DB for advanced use cases (e.g. backups).
// Callers must not close it; use Close instead.
func (r *Registry) DB() *sql.DB { return r.db }

// --- CRUD -------------------------------------------------------------------

// Install records a new plugin in the registry. If a plugin with the same
// name already exists, Install returns ErrPluginExists. The binary at
// binaryPath is hashed (SHA-256) and the digest is stored in the
// signature column when verifySignature is true.
func (r *Registry) Install(ctx context.Context, meta PluginMeta, binaryPath, configYAML string, verifySignature bool) (*RegistryRecord, error) {
	if !meta.Type.Validate() {
		return nil, fmt.Errorf("%w: %q", ErrInvalidPluginType, meta.Type)
	}

	sig := ""
	if verifySignature {
		digest, err := hashFile(binaryPath)
		if err != nil {
			return nil, fmt.Errorf("plugin: hash binary: %w", err)
		}
		sig = digest
	}

	now := time.Now().UTC()
	entry := meta.EntryPoint
	if entry == "" {
		entry = "plugin"
	}

	_, err := r.db.ExecContext(ctx, `
INSERT INTO plugin_registry
    (name, version, type, author, description, entry_point,
     min_host_version, max_host_version, state, binary_path,
     config_yaml, signature, installed_at, updated_at, error_msg)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		meta.Name, meta.Version, string(meta.Type), meta.Author,
		meta.Description, entry, meta.MinHostVersion, meta.MaxHostVersion,
		string(StateInstalled), binaryPath, configYAML, sig, now, now, "",
	)
	if err != nil {
		if isUniqueConstraint(err) {
			return nil, fmt.Errorf("%w: %s", ErrPluginExists, meta.Name)
		}
		return nil, fmt.Errorf("plugin: insert registry row: %w", err)
	}

	return r.Get(ctx, meta.Name)
}

// Get returns the registry record for the named plugin, or
// ErrPluginNotFound when no such plugin is installed.
func (r *Registry) Get(ctx context.Context, name string) (*RegistryRecord, error) {
	row := r.db.QueryRowContext(ctx, registrySelectSQL(), name)
	rec, err := scanRegistryRecord(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("%w: %s", ErrPluginNotFound, name)
		}
		return nil, fmt.Errorf("plugin: query registry: %w", err)
	}
	return rec, nil
}

// List returns all registry records, optionally filtered by state. When
// state is empty all records are returned. The result is sorted by name.
func (r *Registry) List(ctx context.Context, state PluginState) ([]*RegistryRecord, error) {
	var rows *sql.Rows
	var err error
	if state == "" {
		rows, err = r.db.QueryContext(ctx, registrySelectAllSQL())
	} else {
		rows, err = r.db.QueryContext(ctx, `
SELECT name, version, type, author, description, entry_point,
       min_host_version, max_host_version, state, binary_path,
       config_yaml, signature, installed_at, updated_at, error_msg
  FROM plugin_registry WHERE state = ? ORDER BY name`, string(state))
	}
	if err != nil {
		return nil, fmt.Errorf("plugin: list registry: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []*RegistryRecord
	for rows.Next() {
		rec, err := scanRegistryRecord(rows)
		if err != nil {
			return nil, fmt.Errorf("plugin: scan registry row: %w", err)
		}
		out = append(out, rec)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("plugin: iterate registry rows: %w", err)
	}
	return out, nil
}

// SetState updates the state of the named plugin and records an optional
// error message (used when transitioning to StateError). updated_at is
// bumped to now.
func (r *Registry) SetState(ctx context.Context, name string, state PluginState, errMsg string) error {
	res, err := r.db.ExecContext(ctx, `
UPDATE plugin_registry
   SET state = ?, error_msg = ?, updated_at = ?
 WHERE name = ?`,
		string(state), errMsg, time.Now().UTC(), name,
	)
	if err != nil {
		return fmt.Errorf("plugin: update state: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return fmt.Errorf("%w: %s", ErrPluginNotFound, name)
	}
	return nil
}

// UpdateConfig replaces the config YAML for the named plugin.
func (r *Registry) UpdateConfig(ctx context.Context, name, configYAML string) error {
	res, err := r.db.ExecContext(ctx, `
UPDATE plugin_registry SET config_yaml = ?, updated_at = ? WHERE name = ?`,
		configYAML, time.Now().UTC(), name,
	)
	if err != nil {
		return fmt.Errorf("plugin: update config: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return fmt.Errorf("%w: %s", ErrPluginNotFound, name)
	}
	return nil
}

// Remove deletes the named plugin from the registry. It does not delete
// the binary file; the caller is responsible for that.
func (r *Registry) Remove(ctx context.Context, name string) error {
	res, err := r.db.ExecContext(ctx, `DELETE FROM plugin_registry WHERE name = ?`, name)
	if err != nil {
		return fmt.Errorf("plugin: delete registry row: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return fmt.Errorf("%w: %s", ErrPluginNotFound, name)
	}
	return nil
}

// --- Version compatibility --------------------------------------------------

// IsCompatible reports whether the plugin's declared version range
// includes the running host version. Both min and max are inclusive; an
// empty bound means "no limit" on that side. Version comparison uses
// semver major.minor.patch semantics; pre-release suffixes are compared
// lexicographically when present.
//
// When the host version cannot be parsed the function returns true (fail
// open) so that a misconfigured host does not lock out all plugins.
func (r *Registry) IsCompatible(rec *RegistryRecord, hostVersion string) bool {
	if rec.MinHostVersion == "" && rec.MaxHostVersion == "" {
		return true
	}
	hv, ok := parseSemver(hostVersion)
	if !ok {
		return true // fail open
	}
	if rec.MinHostVersion != "" {
		minv, ok := parseSemver(rec.MinHostVersion)
		if ok && compareSemver(hv, minv) < 0 {
			return false
		}
	}
	if rec.MaxHostVersion != "" {
		maxv, ok := parseSemver(rec.MaxHostVersion)
		if ok && compareSemver(hv, maxv) > 0 {
			return false
		}
	}
	return true
}

// --- Signature verification -------------------------------------------------

// VerifySignature re-computes the SHA-256 of the plugin binary and
// compares it to the digest stored in the registry. It returns nil when
// the digests match, ErrSignatureMismatch when they differ, and a
// wrapped error when the binary cannot be read.
//
// When the registry record has no stored signature (Signature == "") the
// function returns nil (verification is opt-in).
func (r *Registry) VerifySignature(ctx context.Context, name string) error {
	rec, err := r.Get(ctx, name)
	if err != nil {
		return err
	}
	if rec.Signature == "" {
		return nil
	}
	digest, err := hashFile(rec.BinaryPath)
	if err != nil {
		return fmt.Errorf("plugin: hash binary for verification: %w", err)
	}
	if !strings.EqualFold(digest, rec.Signature) {
		return fmt.Errorf("%w: %s", ErrSignatureMismatch, name)
	}
	return nil
}

// --- Helpers ----------------------------------------------------------------

// hashFile computes the SHA-256 of the file at path and returns it as a
// lowercase hex string.
func hashFile(path string) (string, error) {
	if path == "" {
		return "", fmt.Errorf("empty path")
	}
	data, err := os.ReadFile(filepath.Clean(path))
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}

// isUniqueConstraint reports whether err is a SQLite UNIQUE constraint
// violation. The modernc.org/sqlite driver surfaces these as a string
// containing "UNIQUE constraint failed".
func isUniqueConstraint(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(err.Error(), "UNIQUE constraint failed")
}

// registrySelectSQL returns the SELECT statement used by Get.
func registrySelectSQL() string {
	return `SELECT name, version, type, author, description, entry_point,
       min_host_version, max_host_version, state, binary_path,
       config_yaml, signature, installed_at, updated_at, error_msg
  FROM plugin_registry WHERE name = ?`
}

// registrySelectAllSQL returns the SELECT statement used by List.
func registrySelectAllSQL() string {
	return `SELECT name, version, type, author, description, entry_point,
       min_host_version, max_host_version, state, binary_path,
       config_yaml, signature, installed_at, updated_at, error_msg
  FROM plugin_registry ORDER BY name`
}

// scanner is the common interface satisfied by both *sql.Row and *sql.Rows
// that lets scanRegistryRecord work with either.
type scanner interface {
	Scan(dest ...any) error
}

// scanRegistryRecord scans a registry row from s into a RegistryRecord.
func scanRegistryRecord(s scanner) (*RegistryRecord, error) {
	var rec RegistryRecord
	var typ, state string
	var installedAt, updatedAt string
	err := s.Scan(
		&rec.Name, &rec.Version, &typ, &rec.Author, &rec.Description,
		&rec.EntryPoint, &rec.MinHostVersion, &rec.MaxHostVersion,
		&state, &rec.BinaryPath, &rec.ConfigYAML, &rec.Signature,
		&installedAt, &updatedAt, &rec.ErrorMsg,
	)
	if err != nil {
		return nil, err
	}
	rec.Type = PluginType(typ)
	rec.State = PluginState(state)
	rec.InstalledAt = parseTime(installedAt)
	rec.UpdatedAt = parseTime(updatedAt)
	return &rec, nil
}

// parseTime parses a SQLite datetime value. SQLite may return the value
// in several formats depending on the driver and the function used to
// produce it:
//
//   - "YYYY-MM-DD HH:MM:SS"        (datetime('now'))
//   - "YYYY-MM-DD HH:MM:SS.SSS"    (with fractional seconds)
//   - "YYYY-MM-DDTHH:MM:SSZ"       (ISO 8601, when stored via time.Time)
//
// parseTime tries each in turn and returns the zero value when none match.
func parseTime(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	layouts := []string{
		"2006-01-02 15:04:05.999999999",
		"2006-01-02 15:04:05",
		time.RFC3339,
		time.RFC3339Nano,
	}
	for _, layout := range layouts {
		if t, err := time.Parse(layout, s); err == nil {
			return t
		}
	}
	return time.Time{}
}

// --- Semver -----------------------------------------------------------------

// semver is a parsed major.minor.patch version with an optional
// pre-release suffix.
type semver struct {
	major, minor, patch int
	pre                 string
}

// parseSemver parses a version string of the form "major.minor.patch" or
// "major.minor.patch-pre". It returns ok == false when the string does
// not match the expected shape.
func parseSemver(s string) (semver, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return semver{}, false
	}
	var v semver
	pre := ""
	if idx := strings.IndexByte(s, '-'); idx >= 0 {
		pre = s[idx+1:]
		s = s[:idx]
	}
	parts := strings.Split(s, ".")
	if len(parts) != 3 {
		return semver{}, false
	}
	n, err := parseInts(parts)
	if err != nil {
		return semver{}, false
	}
	v.major, v.minor, v.patch = n[0], n[1], n[2]
	v.pre = pre
	return v, true
}

// parseInts parses three non-negative integers from a 3-element slice.
func parseInts(parts []string) ([3]int, error) {
	var out [3]int
	for i, p := range parts {
		n := 0
		for _, c := range p {
			if c < '0' || c > '9' {
				return out, fmt.Errorf("non-digit %q", c)
			}
			n = n*10 + int(c-'0')
		}
		out[i] = n
	}
	return out, nil
}

// compareSemver compares two semver values. It returns -1, 0, +1 in the
// usual way. Pre-release versions are considered lower than the
// corresponding release (e.g. 1.0.0-rc1 < 1.0.0).
func compareSemver(a, b semver) int {
	if a.major != b.major {
		return cmpInt(a.major, b.major)
	}
	if a.minor != b.minor {
		return cmpInt(a.minor, b.minor)
	}
	if a.patch != b.patch {
		return cmpInt(a.patch, b.patch)
	}
	// Pre-release: empty > non-empty (release > pre-release).
	if a.pre == b.pre {
		return 0
	}
	if a.pre == "" {
		return 1
	}
	if b.pre == "" {
		return -1
	}
	return cmpStr(a.pre, b.pre)
}

// cmpInt returns the sign of a - b.
func cmpInt(a, b int) int {
	if a < b {
		return -1
	}
	if a > b {
		return 1
	}
	return 0
}

// cmpStr returns the sign of a compared to b lexicographically.
func cmpStr(a, b string) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	default:
		return 0
	}
}
