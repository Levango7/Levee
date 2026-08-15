// Package tests provides shared test helpers, mocks and fixtures for the
// LEVEE test suite. It is compiled into every *_test.go that imports it
// and carries no production runtime dependency.
package tests

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TempDB creates a temporary SQLite database file path for the current
// test and registers a cleanup that removes the file (and its WAL/SHM
// siblings) when the test finishes. The returned path ends in .db and
// the parent directory is guaranteed to exist.
//
// The database file itself is created empty so callers that stat before
// opening succeed. Callers open it via their normal sqlite driver; this
// keeps the helper free of driver imports.
func TempDB(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "levee-test.db")
	f, err := os.Create(p)
	require.NoError(t, err)
	require.NoError(t, f.Close())

	t.Cleanup(func() {
		// Best-effort removal of the db and SQLite sidecar files.
		_ = os.Remove(p)
		_ = os.Remove(p + "-wal")
		_ = os.Remove(p + "-shm")
		_ = os.Remove(p + "-journal")
	})
	return p
}

// TempDir creates a temporary directory for the current test and
// registers a cleanup that removes it recursively on test completion.
// The returned path is absolute.
func TempDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	abs, err := filepath.Abs(dir)
	require.NoError(t, err)
	return abs
}

// WriteFile writes content to a file inside dir and returns the full
// path. It is a convenience wrapper around os.WriteFile that fails the
// test on error. The parent directory must already exist.
func WriteFile(t *testing.T, dir, name string, content []byte) string {
	t.Helper()
	p := filepath.Join(dir, name)
	require.NoError(t, os.WriteFile(p, content, 0o644))
	return p
}

// TargetSpec describes a mock target machine for channel/executor tests.
// It is intentionally a plain struct (not an interface) so tests can
// construct literals without a builder.
type TargetSpec struct {
	ID         string        `json:"id"`
	Host       string        `json:"host"`
	Port       int           `json:"port"`
	OS         string        `json:"os"`      // linux | windows | aix
	Channel    string        `json:"channel"` // ssh | winrm | agent | api
	Username   string        `json:"username"`
	Password   string        `json:"password,omitempty"`
	KeyPath    string        `json:"key_path,omitempty"`
	Tags       []string      `json:"tags,omitempty"`
	Timeout    time.Duration `json:"timeout"`
	StrictHost bool          `json:"strict_host"`
}

// MockTarget returns a sensible default mock target spec suitable for
// unit tests. The Host is set to 127.0.0.1 and the channel to ssh so
// that tests don't accidentally hit a real network. Callers may mutate
// any field after construction.
func MockTarget() TargetSpec {
	return TargetSpec{
		ID:         "t-001",
		Host:       "127.0.0.1",
		Port:       22,
		OS:         "linux",
		Channel:    "ssh",
		Username:   "levee-test",
		KeyPath:    "",
		Tags:       []string{"env:test", "role:mock"},
		Timeout:    5 * time.Second,
		StrictHost: false,
	}
}

// MockTargetList returns n mock targets with sequential IDs and ports,
// useful for batch/wave tests.
func MockTargetList(n int) []TargetSpec {
	out := make([]TargetSpec, n)
	for i := 0; i < n; i++ {
		t := MockTarget()
		t.ID = fmt.Sprintf("t-%03d", i+1)
		t.Port = 2200 + i
		out[i] = t
	}
	return out
}

// AssertJSON asserts that actual JSON-decodes to the same value as
// expected. Both arguments are JSON-encoded byte slices. The comparison
// is order-insensitive for object keys (standard encoding/json behaviour).
// On mismatch, the diff of the re-marshalled canonical form is shown.
func AssertJSON(t *testing.T, expected, actual []byte, msgAndArgs ...interface{}) {
	t.Helper()
	var exp, act interface{}
	require.NoError(t, json.Unmarshal(expected, &exp), "failed to decode expected JSON")
	require.NoError(t, json.Unmarshal(actual, &act), "failed to decode actual JSON")
	assert.Equal(t, exp, act, msgAndArgs...)
}

// AssertJSONEqual is a convenience wrapper that marshals two Go values
// and compares their JSON representations. Useful when one side is a
// struct and the other is a raw []byte from a handler.
func AssertJSONEqual(t *testing.T, expected, actual interface{}, msgAndArgs ...interface{}) {
	t.Helper()
	expBytes, err := json.Marshal(expected)
	require.NoError(t, err, "failed to marshal expected value")
	actBytes, err := json.Marshal(actual)
	require.NoError(t, err, "failed to marshal actual value")
	AssertJSON(t, expBytes, actBytes, msgAndArgs...)
}

// MustMarshal marshals v to JSON and fails the test on error, returning
// the bytes. Handy for building expected payloads inline.
func MustMarshal(t *testing.T, v interface{}) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	require.NoError(t, err)
	return b
}

// EnvVar sets an environment variable for the duration of the test and
// restores the original value (or unsets it) on cleanup.
func EnvVar(t *testing.T, key, value string) {
	t.Helper()
	old, had := os.LookupEnv(key)
	require.NoError(t, os.Setenv(key, value))
	t.Cleanup(func() {
		if had {
			_ = os.Setenv(key, old)
		} else {
			_ = os.Unsetenv(key)
		}
	})
}

// EnvVars sets multiple environment variables at once; see EnvVar.
func EnvVars(t *testing.T, kv map[string]string) {
	t.Helper()
	for k, v := range kv {
		EnvVar(t, k, v)
	}
}
