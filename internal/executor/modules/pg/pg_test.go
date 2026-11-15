package pg

// pg_test.go pins the PostgreSQL module's contract. The two properties that
// matter beyond "it runs a command":
//
//   - ON_ERROR_STOP=1 is present, always. Without it psql exits 0 after a
//     failed statement and the run reports success for a change that never
//     happened — the worst failure mode a change-governance product has.
//   - a password argument is REFUSED, not ignored. Over a shell channel there
//     is no way to hand psql a credential without putting it in argv, so the
//     honest answer is "use ~/.pgpass"; silently dropping the input is how an
//     operator ends up debugging an auth failure they cannot see.

import (
	"context"
	"encoding/base64"
	"io"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nexus/levee/internal/channel"
	"github.com/nexus/levee/internal/executor"
)

type mockChannel struct {
	mu     sync.Mutex
	execs  []string
	result *channel.ExecResult
	err    error
}

func (m *mockChannel) Connect(context.Context) error                   { return nil }
func (m *mockChannel) IsConnected() bool                               { return true }
func (m *mockChannel) Close() error                                    { return nil }
func (m *mockChannel) Upload(context.Context, string, io.Reader) error { return nil }
func (m *mockChannel) Download(context.Context, string) (io.Reader, error) {
	return nil, io.EOF
}

func (m *mockChannel) Exec(_ context.Context, cmd string) (*channel.ExecResult, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.execs = append(m.execs, cmd)
	if m.err != nil {
		return nil, m.err
	}
	if m.result != nil {
		res := *m.result
		return &res, nil
	}
	return &channel.ExecResult{ExitCode: 0}, nil
}

func (m *mockChannel) only(t *testing.T) string {
	t.Helper()
	m.mu.Lock()
	defer m.mu.Unlock()
	require.Len(t, m.execs, 1, "expected exactly one remote command")
	return m.execs[0]
}

type mockTarget struct{ host string }

func (t mockTarget) Host() string                       { return t.host }
func (t mockTarget) Port() int                          { return 5432 }
func (t mockTarget) Type() string                       { return "ssh" }
func (t mockTarget) Credentials() channel.CredentialRef { return channel.CredentialRef{} }

func input(ch *mockChannel, args map[string]any) executor.ModuleInput {
	return executor.ModuleInput{
		Args:    args,
		Target:  mockTarget{host: "pg01.prod"},
		Channel: ch,
	}
}

func TestModuleContract(t *testing.T) {
	m := New()
	assert.Equal(t, "pg", m.Name())
	assert.Equal(t, []string{"query"}, m.Actions())
	assert.False(t, m.Idempotent(),
		"query is idempotent by authorship only; the module cannot verify that claim")
}

func TestQuery_BuildsPsqlWithOnErrorStop(t *testing.T) {
	ch := &mockChannel{}
	out, err := New().Execute(context.Background(), "query", input(ch, map[string]any{
		"sql": "ALTER TABLE t ADD COLUMN c INT",
	}))
	require.NoError(t, err)
	assert.Equal(t, 0, out.ExitCode)
	assert.True(t, out.Changed)

	cmd := ch.only(t)
	assert.Contains(t, cmd, "psql")
	assert.Contains(t, cmd, "-v ON_ERROR_STOP=1",
		"without it psql exits 0 after a failed statement and the run lies")
	assert.Contains(t, cmd, "-h pg01.prod")
	assert.Contains(t, cmd, "-p 5432", "psql's own default, made explicit")
	assert.Contains(t, cmd, "-U postgres", "the default user, stated in the command")
	// Scope the check to the psql invocation: "base64 -d" carries the same
	// two characters earlier in the pipeline.
	psqlPart := cmd[strings.Index(cmd, "psql "):]
	assert.NotContains(t, psqlPart, " -d ", "no database argument when none was given")

	// The SQL rides a base64 pipe, not the command line: quoting-based
	// injection has nothing to break out of.
	assert.Contains(t, cmd, base64.StdEncoding.EncodeToString([]byte("ALTER TABLE t ADD COLUMN c INT")))
	assert.Contains(t, cmd, "base64 -d")
}

func TestQuery_ArgOverridesAndDatabase(t *testing.T) {
	ch := &mockChannel{}
	_, err := New().Execute(context.Background(), "query", input(ch, map[string]any{
		"sql":      "SELECT 1",
		"host":     "db.internal",
		"port":     "6432",
		"user":     "levee_app",
		"database": "orders",
	}))
	require.NoError(t, err)

	cmd := ch.only(t)
	assert.Contains(t, cmd, "-h db.internal")
	assert.Contains(t, cmd, "-p 6432")
	assert.Contains(t, cmd, "-U levee_app")
	assert.Contains(t, cmd, "-d orders")
}

func TestQuery_ExitCodePropagates(t *testing.T) {
	ch := &mockChannel{result: &channel.ExecResult{ExitCode: 3, Stderr: "ERROR: boom"}}
	out, err := New().Execute(context.Background(), "query", input(ch, map[string]any{
		"sql": "ALTER TABLE nope ADD COLUMN c INT",
	}))
	require.NoError(t, err, "a non-zero remote exit is a result, not a module failure")
	assert.Equal(t, 3, out.ExitCode)
	assert.False(t, out.Changed, "a failed statement did not change anything")
	assert.Contains(t, out.Stderr, "boom")
}

func TestQuery_RefusesPasswordArgument(t *testing.T) {
	ch := &mockChannel{}
	_, err := New().Execute(context.Background(), "query", input(ch, map[string]any{
		"sql":      "SELECT 1",
		"password": "hunter2",
	}))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "~/.pgpass", "the refusal must name the supported alternative")
	assert.Empty(t, ch.execs, "nothing may reach the target when the call is refused")
}

func TestQuery_RefusesDestructiveStatement(t *testing.T) {
	ch := &mockChannel{}
	_, err := New().Execute(context.Background(), "query", input(ch, map[string]any{
		"sql": "DROP TABLE t",
	}))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "pg.query", "the refusal names the surface")
	assert.Empty(t, ch.execs)
}

func TestQuery_RefusesInvalidIdentifiers(t *testing.T) {
	// A database name that would change the meaning of the command line must
	// be refused before anything is sent: the flags are string-interpolated.
	for _, args := range []map[string]any{
		{"sql": "SELECT 1", "database": "orders; rm -rf /"},
		{"sql": "SELECT 1", "user": "app'--"},
		{"sql": "SELECT 1", "port": "5432 -c bad"},
	} {
		ch := &mockChannel{}
		_, err := New().Execute(context.Background(), "query", input(ch, args))
		require.Error(t, err, "args %v must be refused", args)
		assert.Empty(t, ch.execs, "nothing may reach the target for %v", args)
	}
}

func TestQuery_RequiresSql(t *testing.T) {
	ch := &mockChannel{}
	_, err := New().Execute(context.Background(), "query", input(ch, nil))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "sql")
}

func TestQuery_RefusesEmptySql(t *testing.T) {
	ch := &mockChannel{}
	_, err := New().Execute(context.Background(), "query", input(ch, map[string]any{"sql": "   "}))
	require.Error(t, err)
	assert.True(t, strings.Contains(err.Error(), "empty sql"))
}

func TestExecute_UnknownAction(t *testing.T) {
	_, err := New().Execute(context.Background(), "vacuum", input(&mockChannel{}, nil))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unsupported action")
}
