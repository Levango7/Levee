package mysql

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

// --- mock channel & target -------------------------------------------------

type mockChannel struct {
	mu sync.Mutex

	execs         []string
	execResponses []execResponse
	execResult    *channel.ExecResult
	execErr       error

	connected bool
}

type execResponse struct {
	result *channel.ExecResult
	err    error
}

func (m *mockChannel) Connect(context.Context) error                   { m.connected = true; return nil }
func (m *mockChannel) IsConnected() bool                               { return m.connected }
func (m *mockChannel) Close() error                                    { m.connected = false; return nil }
func (m *mockChannel) Upload(context.Context, string, io.Reader) error { return nil }
func (m *mockChannel) Download(context.Context, string) (io.Reader, error) {
	return nil, io.EOF
}

func (m *mockChannel) Exec(_ context.Context, cmd string) (*channel.ExecResult, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.execs = append(m.execs, cmd)

	if len(m.execResponses) > 0 {
		r := m.execResponses[0]
		m.execResponses = m.execResponses[1:]
		return r.result, r.err
	}
	if m.execErr != nil {
		return nil, m.execErr
	}
	if m.execResult != nil {
		res := *m.execResult
		return &res, nil
	}
	return &channel.ExecResult{ExitCode: 0, Stdout: "", Stderr: ""}, nil
}

func (m *mockChannel) execAt(i int) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	if i < 0 || i >= len(m.execs) {
		return ""
	}
	return m.execs[i]
}

func (m *mockChannel) execCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.execs)
}

type mockTarget struct {
	host string
	port int
	typ  string
}

func (t mockTarget) Host() string                       { return t.host }
func (t mockTarget) Port() int                          { return t.port }
func (t mockTarget) Type() string                       { return t.typ }
func (t mockTarget) Credentials() channel.CredentialRef { return channel.CredentialRef{} }

// newInput builds a ModuleInput over the mocks with the given args.
func newInput(ch *mockChannel, args map[string]any) executor.ModuleInput {
	return executor.ModuleInput{
		Action:  "",
		Args:    args,
		Target:  mockTarget{host: "db01.prod", port: 3306, typ: "ssh"},
		Channel: ch,
	}
}

// okResult is a shorthand successful exec result.
func okResult(stdout string) *channel.ExecResult {
	return &channel.ExecResult{ExitCode: 0, Stdout: stdout}
}

// failResult is a shorthand failed exec result.
func failResult(stderr string) *channel.ExecResult {
	return &channel.ExecResult{ExitCode: 1, Stderr: stderr}
}

// --- module metadata ---------------------------------------------------------

func TestModuleName(t *testing.T) {
	assert.Equal(t, "mysql", New().Name())
}

func TestModuleActions(t *testing.T) {
	assert.Equal(t, []string{"query", "pt_osc", "replica_switch"}, New().Actions())
}

func TestModuleNotIdempotent(t *testing.T) {
	// Mixed module: worst action (replica_switch) is non-idempotent, so the
	// module-level hint must be false.
	assert.False(t, New().Idempotent())
}

// --- query -------------------------------------------------------------------

func TestQueryRunsBase64Pipe(t *testing.T) {
	ch := &mockChannel{}
	ch.execResult = okResult("")

	m := New()
	out, err := m.Execute(context.Background(), "query", newInput(ch, map[string]any{
		"sql":      "CREATE TABLE IF NOT EXISTS t1 (id INT)",
		"database": "orders",
	}))
	require.NoError(t, err)
	require.NotNil(t, out)
	assert.Equal(t, 0, out.ExitCode)
	assert.True(t, out.Changed)

	cmd := ch.execAt(0)
	// The SQL must be base64-encoded on the pipe, never in clear text.
	assert.NotContains(t, cmd, "CREATE TABLE")
	assert.Contains(t, cmd, base64.StdEncoding.EncodeToString([]byte("CREATE TABLE IF NOT EXISTS t1 (id INT)")))
	// Connection flags come from identifier-validated defaults.
	assert.Contains(t, cmd, "-h db01.prod -P 3306 -u root orders")
	// The heredoc marker guards the pipe.
	assert.Contains(t, cmd, "<<'LEVEE_EOF'")
}

func TestQueryRejectsMissingSQL(t *testing.T) {
	ch := &mockChannel{}
	_, err := New().Execute(context.Background(), "query", newInput(ch, map[string]any{}))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "missing argument \"sql\"")
}

func TestQueryRejectsEmptySQL(t *testing.T) {
	ch := &mockChannel{}
	_, err := New().Execute(context.Background(), "query", newInput(ch, map[string]any{"sql": "  "}))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "empty sql")
}

func TestQueryRejectsDropTable(t *testing.T) {
	ch := &mockChannel{}
	_, err := New().Execute(context.Background(), "query", newInput(ch, map[string]any{
		"sql": "DROP TABLE orders",
	}))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "destructive statement rejected")
	assert.Equal(t, 0, ch.execCount())
}

func TestQueryRejectsTruncate(t *testing.T) {
	ch := &mockChannel{}
	_, err := New().Execute(context.Background(), "query", newInput(ch, map[string]any{
		"sql": "TRUNCATE TABLE orders",
	}))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "destructive statement rejected")
	assert.Equal(t, 0, ch.execCount())
}

func TestQueryRejectsDropDatabase(t *testing.T) {
	ch := &mockChannel{}
	_, err := New().Execute(context.Background(), "query", newInput(ch, map[string]any{
		"sql": "drop database prod",
	}))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "destructive statement rejected")
}

func TestQueryRejectsUnboundedDelete(t *testing.T) {
	ch := &mockChannel{}
	_, err := New().Execute(context.Background(), "query", newInput(ch, map[string]any{
		"sql": "DELETE FROM orders",
	}))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "destructive statement rejected")
}

func TestQueryAllowsScopedDelete(t *testing.T) {
	ch := &mockChannel{}
	ch.execResult = okResult("")
	_, err := New().Execute(context.Background(), "query", newInput(ch, map[string]any{
		"sql": "DELETE FROM orders WHERE id = 42",
	}))
	require.NoError(t, err)
	assert.Equal(t, 1, ch.execCount())
}

func TestQueryPropagatesExitCode(t *testing.T) {
	ch := &mockChannel{}
	ch.execResult = failResult("ERROR 1146 (42S02): Table 'orders.t1' doesn't exist")

	out, err := New().Execute(context.Background(), "query", newInput(ch, map[string]any{
		"sql": "SELECT 1 FROM t1",
	}))
	require.NoError(t, err)
	require.NotNil(t, out)
	assert.Equal(t, 1, out.ExitCode)
	assert.False(t, out.Changed)
	assert.Contains(t, out.Stderr, "42S02")
}

func TestQuerySQLWithQuotesStaysEncoded(t *testing.T) {
	ch := &mockChannel{}
	ch.execResult = okResult("")
	// A payload full of shell-hostile characters must land base64-encoded,
	// never raw on the command line. The payload avoids destructive verbs
	// (those are covered by the reject tests) so this test isolates the
	// transport encoding.
	sql := "INSERT INTO audit VALUES ('$(cat /etc/passwd)', \"foobar\", \"x; y | z\")\" && echo pwned"
	_, err := New().Execute(context.Background(), "query", newInput(ch, map[string]any{"sql": sql}))
	require.NoError(t, err)

	cmd := ch.execAt(0)
	assert.NotContains(t, cmd, "rm -rf")
	assert.NotContains(t, cmd, "DROP TABLE x")
	// The encoded form decodes back to the original SQL.
	encoded := base64.StdEncoding.EncodeToString([]byte(sql))
	assert.Contains(t, cmd, encoded)
}

// --- pt_osc -------------------------------------------------------------------

func TestPtOscBuildsCommand(t *testing.T) {
	ch := &mockChannel{}
	ch.execResult = okResult("")

	out, err := New().Execute(context.Background(), "pt_osc", newInput(ch, map[string]any{
		"database": "orders",
		"table":    "order_items",
		"alter":    `ADD COLUMN status VARCHAR(32) DEFAULT "active"`,
	}))
	require.NoError(t, err)
	require.NotNil(t, out)
	assert.Equal(t, 0, out.ExitCode)
	assert.True(t, out.Changed)

	cmd := ch.execAt(0)
	assert.Contains(t, cmd, "pt-online-schema-change")
	assert.Contains(t, cmd, "--execute")
	assert.Contains(t, cmd, "D=orders,t=order_items")
	assert.Contains(t, cmd, "h=db01.prod,P=3306,u=root")
	// The alter clause is single-quoted as one shell word.
	assert.Contains(t, cmd, `--alter 'ADD COLUMN status VARCHAR(32) DEFAULT "active"'`)
}

func TestPtOscMissingDatabase(t *testing.T) {
	ch := &mockChannel{}
	_, err := New().Execute(context.Background(), "pt_osc", newInput(ch, map[string]any{
		"table": "t",
		"alter": "ADD COLUMN c INT",
	}))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "database")
}

func TestPtOscRejectsBadIdent(t *testing.T) {
	ch := &mockChannel{}
	_, err := New().Execute(context.Background(), "pt_osc", newInput(ch, map[string]any{
		"database": "orders; rm -rf /",
		"table":    "t",
		"alter":    "ADD COLUMN c INT",
	}))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid database")
}

func TestPtOscAlterWhitelist(t *testing.T) {
	valid := []string{
		`ADD COLUMN status VARCHAR(32) DEFAULT "active"`,
		"ADD status VARCHAR(32)",
		"DROP COLUMN legacy_flag",
		"DROP legacy_flag",
		"MODIFY COLUMN status VARCHAR(64)",
		"MODIFY status VARCHAR(64)",
		"CHANGE COLUMN old_name new_name INT",
		"RENAME COLUMN old_name TO new_name",
		"ADD INDEX idx_status (status)",
		"ADD KEY k_status (status)",
		"DROP INDEX idx_status",
		"DROP KEY k_status",
		"DROP FOREIGN KEY fk_order",
		"DROP CONSTRAINT chk_status",
		`ALTER COLUMN status SET DEFAULT "active"`,
		"ALTER COLUMN status DROP DEFAULT",
		"CONVERT TO CHARACTER SET utf8mb4",
		"CONVERT TO CHARSET utf8mb4",
		"ENGINE=InnoDB",
		// comma-separated clauses
		"ADD COLUMN a INT, ADD COLUMN b INT",
	}
	for _, alter := range valid {
		require.NoError(t, validateAlter(alter), "expected valid: %s", alter)
	}

	invalid := []string{
		"",
		"   ",
		"PARTITION BY HASH(id)",
		"LOAD DATA INFILE '/etc/passwd' INTO TABLE t",
		"ADD COLUMN a INT; rm -rf /",
		"ADD COLUMN a INT || cat /etc/passwd",
		"DROP COLUMN a, LOAD DATA INFILE 'x'",
		"ADD COLUMN $(whoami) INT",
		// single quotes are rejected outright: the clause is spliced into
		// a single-quoted shell word, and refusing the character removes
		// the escaping question entirely.
		"ADD COLUMN status VARCHAR(32) DEFAULT 'active'",
	}
	for _, alter := range invalid {
		require.Error(t, validateAlter(alter), "expected invalid: %s", alter)
	}
}

func TestPtOscRejectsOutsideWhitelist(t *testing.T) {
	ch := &mockChannel{}
	_, err := New().Execute(context.Background(), "pt_osc", newInput(ch, map[string]any{
		"database": "orders",
		"table":    "t",
		"alter":    "PARTITION BY HASH(id)",
	}))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "whitelist")
	assert.Equal(t, 0, ch.execCount())
}

func TestPtOscMissingArgs(t *testing.T) {
	ch := &mockChannel{}
	_, err := New().Execute(context.Background(), "pt_osc", newInput(ch, map[string]any{}))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "table")

	_, err = New().Execute(context.Background(), "pt_osc", newInput(ch, map[string]any{
		"table": "t",
	}))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "alter")
}

// --- replica_switch -------------------------------------------------------------

func replicaArgs(newPrimary, confirm string) map[string]any {
	return map[string]any{
		"new_primary": newPrimary,
		"confirm":     confirm,
	}
}

func TestReplicaSwitchRequiresConfirm(t *testing.T) {
	ch := &mockChannel{}
	// Without confirm=yes the module must refuse before any exec.
	_, err := New().Execute(context.Background(), "replica_switch", newInput(ch, replicaArgs("db02.prod", "")))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "confirm=yes")
	assert.Equal(t, 0, ch.execCount())

	_, err = New().Execute(context.Background(), "replica_switch", newInput(ch, replicaArgs("db02.prod", "no")))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "confirm=yes")
	assert.Equal(t, 0, ch.execCount())
}

func TestReplicaSwitchRequiresNewPrimary(t *testing.T) {
	ch := &mockChannel{}
	_, err := New().Execute(context.Background(), "replica_switch", newInput(ch, map[string]any{
		"confirm": "yes",
	}))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "new_primary")
}

func TestReplicaSwitchRejectsBadHost(t *testing.T) {
	ch := &mockChannel{}
	_, err := New().Execute(context.Background(), "replica_switch", newInput(ch, map[string]any{
		"new_primary": "db02; rm -rf /",
		"confirm":     "yes",
	}))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid new_primary host")
	assert.Equal(t, 0, ch.execCount())
}

func TestReplicaSwitchOrchestration(t *testing.T) {
	ch := &mockChannel{}
	ch.execResponses = []execResponse{
		{result: okResult("Master_Log_File: mysql-bin.000042")}, // SHOW SLAVE STATUS
		{result: okResult("")}, // STOP SLAVE
		{result: okResult("")}, // CHANGE MASTER TO
		{result: okResult("")}, // START SLAVE
	}

	out, err := New().Execute(context.Background(), "replica_switch", newInput(ch, replicaArgs("db02.prod", "yes")))
	require.NoError(t, err)
	require.NotNil(t, out)
	assert.Equal(t, 0, out.ExitCode)
	assert.True(t, out.Changed)
	assert.Contains(t, out.Stdout, "db02.prod")

	// The four-step sequence must be issued in order. All SQL travels the
	// base64 pipe — including CHANGE MASTER TO, whose validated host is
	// the only interpolation and stays inside the encoded payload.
	assert.Equal(t, 4, ch.execCount())
	assert.Contains(t, ch.execAt(0), base64.StdEncoding.EncodeToString([]byte("SHOW SLAVE STATUS")))
	assert.Contains(t, ch.execAt(1), base64.StdEncoding.EncodeToString([]byte("STOP SLAVE")))
	switchSQL := "CHANGE MASTER TO MASTER_HOST='db02.prod', MASTER_USER='rpl_user', MASTER_AUTO_POSITION=1"
	assert.Contains(t, ch.execAt(2), base64.StdEncoding.EncodeToString([]byte(switchSQL)))
	assert.Contains(t, ch.execAt(3), base64.StdEncoding.EncodeToString([]byte("START SLAVE")))
}

func TestReplicaSwitchAbortsWhenStatusFails(t *testing.T) {
	ch := &mockChannel{}
	ch.execResponses = []execResponse{
		{result: failResult("ERROR 1227 (42000): Access denied")}, // SHOW SLAVE STATUS fails
	}

	_, err := New().Execute(context.Background(), "replica_switch", newInput(ch, replicaArgs("db02.prod", "yes")))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "SHOW SLAVE STATUS failed")
	// No mutation must have been attempted after the failed read.
	assert.Equal(t, 1, ch.execCount())
}

func TestReplicaSwitchAbortsWhenStopFails(t *testing.T) {
	ch := &mockChannel{}
	ch.execResponses = []execResponse{
		{result: okResult("")},               // SHOW SLAVE STATUS
		{result: failResult("stop refused")}, // STOP SLAVE fails
	}

	_, err := New().Execute(context.Background(), "replica_switch", newInput(ch, replicaArgs("db02.prod", "yes")))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "stop slave failed")
	assert.Equal(t, 2, ch.execCount())
}

func TestReplicaSwitchAbortsWhenChangeMasterFails(t *testing.T) {
	ch := &mockChannel{}
	ch.execResponses = []execResponse{
		{result: okResult("")},                 // SHOW SLAVE STATUS
		{result: okResult("")},                 // STOP SLAVE
		{result: failResult("change refused")}, // CHANGE MASTER TO fails
	}

	_, err := New().Execute(context.Background(), "replica_switch", newInput(ch, replicaArgs("db02.prod", "yes")))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "change master failed")
	// START SLAVE must not have been issued.
	assert.Equal(t, 3, ch.execCount())
	assert.NotContains(t, ch.execAt(2), "START SLAVE")
}

func TestReplicaSwitchAbortsWhenStartFails(t *testing.T) {
	ch := &mockChannel{}
	ch.execResponses = []execResponse{
		{result: okResult("")},                // SHOW SLAVE STATUS
		{result: okResult("")},                // STOP SLAVE
		{result: okResult("")},                // CHANGE MASTER TO
		{result: failResult("start refused")}, // START SLAVE fails
	}

	_, err := New().Execute(context.Background(), "replica_switch", newInput(ch, replicaArgs("db02.prod", "yes")))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "start slave failed")
	assert.Equal(t, 4, ch.execCount())
}

// --- helpers ---------------------------------------------------------------------

func TestShellQuote(t *testing.T) {
	assert.Equal(t, "'safe'", shellQuote("safe"))
	assert.Equal(t, "'it'\\''s'", shellQuote("it's"))
}

func TestRejectDestructiveSQL(t *testing.T) {
	bad := []string{
		"DROP TABLE orders",
		"drop table orders",
		"DROP DATABASE prod",
		"DROP SCHEMA prod",
		"DROP INDEX idx ON t",
		"DROP USER 'x'@'%'",
		"TRUNCATE orders",
		"TRUNCATE TABLE orders",
		"DELETE FROM orders",
	}
	for _, sql := range bad {
		require.Error(t, rejectDestructiveSQL(sql), "expected rejected: %s", sql)
	}

	good := []string{
		"SELECT 1",
		"CREATE TABLE IF NOT EXISTS t (id INT)",
		"INSERT INTO t (id) VALUES (1) ON DUPLICATE KEY UPDATE id=id",
		"UPDATE t SET a=1 WHERE id=2",
		"DELETE FROM t WHERE id=2",
		"ALTER TABLE t ADD COLUMN c INT",
		"SHOW TABLES",
	}
	for _, sql := range good {
		require.NoError(t, rejectDestructiveSQL(sql), "expected allowed: %s", sql)
	}
}

func TestValidateIdent(t *testing.T) {
	assert.NoError(t, validateIdent("host", "db01"))
	assert.NoError(t, validateIdent("host", "db_01$prod"))
	assert.Error(t, validateIdent("host", ""))
	assert.Error(t, validateIdent("host", "db01; rm -rf /"))
	assert.Error(t, validateIdent("host", "db01 && cat /etc/passwd"))
	assert.Error(t, validateIdent("host", `db01'; DROP`))
}

func TestUnsupportedAction(t *testing.T) {
	ch := &mockChannel{}
	_, err := New().Execute(context.Background(), "vacuum", newInput(ch, map[string]any{}))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unsupported action")
	assert.True(t, strings.Contains(err.Error(), "vacuum"))
}
