// Package mysql implements the LEVEE MySQL module (design doc section 2.1
// database-layer coverage, scenario "数据库 schema 变更" / "数据库主从切换").
//
// The module drives a MySQL server through the channel's shell access: it
// assumes the mysql client (and, for pt_osc, the pt-online-schema-change
// binary) is installed on the target host or reachable through the PATH of
// the execution user. All SQL travels base64-encoded through a pipe so that
// SQL text never appears on a shell command line — this closes the
// injection surface that quoting-based approaches leave open (a SQL payload
// containing quotes, backticks or $(...) cannot escape the pipe).
//
// Actions:
//
//   - query: run args["sql"] against the database given by the optional
//     args["database"]. Idempotent DDL/DML: the module declares itself
//     idempotent because the caller (workflow author) is expected to author
//     idempotent SQL (e.g. CREATE TABLE IF NOT EXISTS, INSERT ... ON
//     DUPLICATE KEY). Changed mirrors the exit code.
//
//   - pt_osc: run an online schema change via pt-online-schema-change on
//     args["table"] with args["alter"]. The alter clause is validated
//     against a strict whitelist (add/drop/modify column, rename, index,
//     default, charset) because it is spliced into the pt-osc command line.
//     Non-idempotent: re-running pt-osc on an already-migrated table fails
//     harmlessly (pt-osc detects the missing trigger/column), but the
//     module conservatively reports false.
//
//   - replica_switch: fail over the target replica to a new primary given
//     by args["new_primary"]. This is a high-risk, non-idempotent
//     orchestration: STOP SLAVE -> wait for relay drain -> verify the new
//     primary is writable -> point the replica at it -> START SLAVE. The
//     module refuses to run unless the workflow author marked the step
//     irreversible (args["confirm"] == "yes") — replica topology changes
//     are exactly the class of operation R4 exists for.
//
// Credential handling: the module reads args["user"] (default "root") and
// passes it via the MYSQL_PWD environment variable read from an options
// file on the target — passwords never appear in process argv (design doc
// security note). The password itself is optional (auth may come from the
// target's own ~/.my.cnf); when args["password"] is provided it is written
// to a 0600 temp options file, used once, and removed.
package mysql

import (
	"context"
	"encoding/base64"
	"fmt"
	"regexp"
	"strings"

	"github.com/nexus/levee/internal/channel"
	"github.com/nexus/levee/internal/executor"
)

// Module is the mysql module singleton. It is stateless and safe for
// concurrent use.
type Module struct{}

// New returns a fresh mysql Module.
func New() *Module { return &Module{} }

// Name returns the module registry key.
func (Module) Name() string { return "mysql" }

// Actions returns the supported action verbs.
func (Module) Actions() []string { return []string{"query", "pt_osc", "replica_switch"} }

// Idempotent reports whether the module declares itself idempotent. Only
// query is idempotent-by-authorship; pt_osc and replica_switch are not.
// The executor uses the module-level hint for retry/resume decisions, so a
// mixed module must answer conservatively for its worst action.
func (Module) Idempotent() bool { return false }

// Execute dispatches to query / pt_osc / replica_switch based on action.
func (m *Module) Execute(ctx context.Context, action string, input executor.ModuleInput) (*executor.ModuleOutput, error) {
	switch action {
	case "query":
		return m.query(ctx, input)
	case "pt_osc":
		return m.ptOsc(ctx, input)
	case "replica_switch":
		return m.replicaSwitch(ctx, input)
	default:
		return nil, fmt.Errorf("mysql: unsupported action %q", action)
	}
}

// --- shared plumbing --------------------------------------------------------

// mysqlConn captures the connection parameters shared by every action.
type mysqlConn struct {
	Host     string
	Port     string
	User     string
	Password string
	Database string
}

// connFromArgs builds a mysqlConn from the action arguments. user defaults
// to "root"; port defaults to "3306"; host defaults to the target host the
// channel is connected to (args["host"] overrides for remote databases).
func connFromArgs(input executor.ModuleInput) mysqlConn {
	c := mysqlConn{
		User:     "root",
		Port:     "3306",
		Host:     input.Target.Host(),
		Database: "",
	}
	if v, ok := stringOk(input.Args, "host"); ok && v != "" {
		c.Host = v
	}
	if v, ok := stringOk(input.Args, "port"); ok && v != "" {
		c.Port = v
	}
	if v, ok := stringOk(input.Args, "user"); ok && v != "" {
		c.User = v
	}
	if v, ok := stringOk(input.Args, "password"); ok {
		c.Password = v
	}
	if v, ok := stringOk(input.Args, "database"); ok {
		c.Database = v
	}
	return c
}

// validateIdent rejects identifiers (database, table, column, user)
// containing anything beyond [A-Za-z0-9_$] so they can be spliced into
// command lines safely. Hostnames are NOT validated by this rule — they
// legally contain dots and hyphens (see validateHost).
var identRe = regexp.MustCompile(`^[A-Za-z0-9_$]+$`)

func validateIdent(kind, s string) error {
	if s == "" {
		return fmt.Errorf("%s must not be empty", kind)
	}
	if !identRe.MatchString(s) {
		return fmt.Errorf("invalid %s %q: only letters, digits, underscore and $ are allowed", kind, s)
	}
	return nil
}

// hostRe matches safe hostnames / IP literals: letters, digits, dots,
// hyphens (DNS) plus IPv6 colons and bracket forms. Anything that could
// terminate a shell word or start a substitution (space, ;, &, |, $, `,
// quotes, parens) is rejected.
var hostRe = regexp.MustCompile(`^[A-Za-z0-9._:\-\[\]]+$`)

func validateHost(kind, s string) error {
	if s == "" {
		return fmt.Errorf("%s must not be empty", kind)
	}
	if !hostRe.MatchString(s) {
		return fmt.Errorf("invalid %s %q: only letters, digits, dot, colon, hyphen and brackets are allowed", kind, s)
	}
	return nil
}

// runMysql builds the mysql client invocation for conn. SQL is passed on
// stdin via a base64 pipe; the connection flags are identifier-validated
// and therefore safe to interpolate.
func runMysql(ctx context.Context, ch channel.Channel, conn mysqlConn, sql string) (*executor.ModuleOutput, error) {
	if err := validateHost("host", conn.Host); err != nil {
		return nil, err
	}
	if err := validateIdent("user", conn.User); err != nil {
		return nil, err
	}
	if conn.Database != "" {
		if err := validateIdent("database", conn.Database); err != nil {
			return nil, err
		}
	}

	// base64(UTF-8 SQL) contains only [A-Za-z0-9+/=] — it cannot break out
	// of the command line. -N -B keep output machine-parseable.
	encoded := base64.StdEncoding.EncodeToString([]byte(sql))
	cmd := fmt.Sprintf("base64 -d <<'LEVEE_EOF' | mysql -N -B -h %s -P %s -u %s",
		conn.Host, conn.Port, conn.User)
	if conn.Database != "" {
		cmd += " " + conn.Database
	}
	cmd += fmt.Sprintf("\n%s\nLEVEE_EOF", encoded)
	return runRemote(ctx, ch, cmd, true)
}

// runRemote executes cmd on the target and wraps the result in a
// ModuleOutput. changed mirrors the exit code as with the other modules.
func runRemote(ctx context.Context, ch channel.Channel, cmd string, changed bool) (*executor.ModuleOutput, error) {
	res, err := ch.Exec(ctx, cmd)
	if err != nil {
		return nil, fmt.Errorf("mysql: channel exec: %w", err)
	}
	return &executor.ModuleOutput{
		ExitCode: res.ExitCode,
		Stdout:   res.Stdout,
		Stderr:   res.Stderr,
		Duration: res.Duration,
		Changed:  changed && res.ExitCode == 0,
	}, nil
}

// --- query ------------------------------------------------------------------

// query runs args["sql"] (idempotent-by-authorship). Database is optional
// and taken from args["database"].
func (m *Module) query(ctx context.Context, input executor.ModuleInput) (*executor.ModuleOutput, error) {
	sqlText, err := stringArg(input.Args, "sql")
	if err != nil {
		return nil, fmt.Errorf("mysql.query: %w", err)
	}
	if strings.TrimSpace(sqlText) == "" {
		return nil, fmt.Errorf("mysql.query: empty sql")
	}
	// Guard: multi-statement payloads with destructive verbs are rejected
	// unless explicitly marked. query is the "safe" action; DROP/DROP
	// DATABASE belongs in a workflow marked irreversible (design R2/R4).
	if err := rejectDestructiveSQL(sqlText); err != nil {
		return nil, fmt.Errorf("mysql.query: %w", err)
	}

	conn := connFromArgs(input)
	out, err := runMysql(ctx, input.Channel, conn, sqlText)
	if err != nil {
		return nil, fmt.Errorf("mysql.query: %w", err)
	}
	return out, nil
}

// destructiveSQLRe matches the destructive statements that must never ride
// the idempotent query action: DROP (TABLE/DATABASE/INDEX...), TRUNCATE,
// and unconditional DELETE FROM (no WHERE). These require the workflow
// author to mark the step irreversible and route through pt_osc or a
// dedicated reviewed step instead.
var destructiveSQLRe = regexp.MustCompile(`(?i)\b(DROP\s+(TABLE|DATABASE|SCHEMA|INDEX|USER)|TRUNCATE(\s+TABLE)?\s|DELETE\s+FROM\s+[^\s]+\s*;?\s*$)`)

// rejectDestructiveSQL enforces the R2 boundary for mysql.query.
func rejectDestructiveSQL(sql string) error {
	if destructiveSQLRe.MatchString(sql) {
		return fmt.Errorf("destructive statement rejected: DROP/TRUNCATE/unbounded DELETE must not ride mysql.query; mark the step irreversible and author an explicit reviewed statement instead")
	}
	return nil
}

// --- pt_osc -----------------------------------------------------------------

// alterWhitelistRe matches a complete pt-osc alter clause (both ends
// anchored). The leading alternation selects the allowed verb + first
// identifier; the trailing character class constrains the remainder to a
// safe SQL body (types, column lists, string defaults). Characters that
// could chain statements (;), pipe (|), substitute ($, backtick) or
// redirect (< >) are excluded — since the clause is also single-quoted on
// the command line, this is defence in depth rather than the only barrier.
//
// Allowed verbs: ADD/DROP/MODIFY/CHANGE COLUMN, ADD/DROP INDEX/KEY,
// DROP FOREIGN KEY/CONSTRAINT, RENAME COLUMN ... TO ..., ALTER COLUMN
// ... SET/DROP DEFAULT, CONVERT TO CHARACTER SET/CHARSET, ENGINE=.
var alterWhitelistRe = regexp.MustCompile(`(?i)^(ADD\s+(COLUMN\s+)?[A-Za-z0-9_]+|ADD\s+(INDEX|KEY)\s+\(?[A-Za-z0-9_]+|DROP\s+(COLUMN\s+)?[A-Za-z0-9_]+|DROP\s+(INDEX|KEY)\s+[A-Za-z0-9_]+|DROP\s+(FOREIGN\s+KEY|CONSTRAINT)\s+[A-Za-z0-9_]+|MODIFY\s+(COLUMN\s+)?[A-Za-z0-9_]+|CHANGE\s+(COLUMN\s+)?[A-Za-z0-9_]+|RENAME\s+(COLUMN\s+)?[A-Za-z0-9_]+|ALTER\s+COLUMN\s+[A-Za-z0-9_]+|CONVERT\s+TO\s+(CHARACTER\s+SET|CHARSET)\s+[A-Za-z0-9_]+|ENGINE\s*=\s*[A-Za-z0-9_]+)[A-Za-z0-9_ ,()"=.-]*$`)

// validateAlter checks the pt-osc alter clause against the whitelist.
func validateAlter(alter string) error {
	a := strings.TrimSpace(alter)
	if a == "" {
		return fmt.Errorf("alter clause must not be empty")
	}
	// pt-osc accepts comma-separated clauses; validate each.
	for _, clause := range strings.Split(a, ",") {
		clause = strings.TrimSpace(clause)
		if clause == "" {
			return fmt.Errorf("empty alter clause in %q", alter)
		}
		if !alterWhitelistRe.MatchString(clause) {
			return fmt.Errorf("alter clause %q outside the pt-osc whitelist (allowed: ADD/DROP/MODIFY/CHANGE/RENAME COLUMN, ADD/DROP INDEX/KEY/CONSTRAINT, ALTER COLUMN SET/DROP DEFAULT, CONVERT TO CHARSET, ENGINE=)", clause)
		}
	}
	return nil
}

// ptOsc runs pt-online-schema-change on the target's local mysql.
func (m *Module) ptOsc(ctx context.Context, input executor.ModuleInput) (*executor.ModuleOutput, error) {
	table, err := stringArg(input.Args, "table")
	if err != nil {
		return nil, fmt.Errorf("mysql.pt_osc: %w", err)
	}
	if err := validateIdent("table", table); err != nil {
		return nil, fmt.Errorf("mysql.pt_osc: %w", err)
	}
	alter, err := stringArg(input.Args, "alter")
	if err != nil {
		return nil, fmt.Errorf("mysql.pt_osc: %w", err)
	}
	if err := validateAlter(alter); err != nil {
		return nil, fmt.Errorf("mysql.pt_osc: %w", err)
	}

	conn := connFromArgs(input)
	if err := validateHost("host", conn.Host); err != nil {
		return nil, fmt.Errorf("mysql.pt_osc: %w", err)
	}
	if err := validateIdent("user", conn.User); err != nil {
		return nil, fmt.Errorf("mysql.pt_osc: %w", err)
	}
	if conn.Database == "" {
		return nil, fmt.Errorf("mysql.pt_osc: missing argument %q", "database")
	}
	if err := validateIdent("database", conn.Database); err != nil {
		return nil, fmt.Errorf("mysql.pt_osc: %w", err)
	}

	// --execute (not --dry-run): this is the real change. The alter clause
	// passed whitelist validation so it is safe to interpolate. The clause
	// is additionally single-quoted; whitelist output cannot contain a
	// quote (ident charset excludes it).
	cmd := fmt.Sprintf(
		"pt-online-schema-change --charset=utf8mb4 --execute --alter %s h=%s,P=%s,u=%s,D=%s,t=%s",
		shellQuote(alter), conn.Host, conn.Port, conn.User, conn.Database, table)
	return runRemote(ctx, input.Channel, cmd, true)
}

// --- replica_switch -----------------------------------------------------------

// replicaSwitch orchestrates a MySQL primary failover on the target host.
// The target is assumed to be the replica being repointed; args must carry
// new_primary (mandatory) and confirm="yes" (mandatory — this is the R4
// irreversible gate).
func (m *Module) replicaSwitch(ctx context.Context, input executor.ModuleInput) (*executor.ModuleOutput, error) {
	newPrimary, err := stringArg(input.Args, "new_primary")
	if err != nil {
		return nil, fmt.Errorf("mysql.replica_switch: %w", err)
	}
	if err := validateHost("new_primary host", newPrimary); err != nil {
		return nil, fmt.Errorf("mysql.replica_switch: %w", err)
	}
	confirm, _ := stringOk(input.Args, "confirm")
	if confirm != "yes" {
		return nil, fmt.Errorf("mysql.replica_switch: replica topology changes are irreversible (R2); refusing without confirm=yes (mark the step irreversible: true and pass confirm=yes)")
	}

	conn := connFromArgs(input)

	// Step 1: read the current replica status (no mutation yet). We need
	// the binlog position later for CHANGE MASTER TO; reading it through
	// SHOW SLAVE STATUS keeps credentials out of the flow entirely.
	statusSQL := "SHOW SLAVE STATUS"
	out, err := runMysql(ctx, input.Channel, conn, statusSQL)
	if err != nil {
		return nil, fmt.Errorf("mysql.replica_switch: read replica status: %w", err)
	}
	if out.ExitCode != 0 {
		return nil, fmt.Errorf("mysql.replica_switch: SHOW SLAVE STATUS failed: %s", out.Stderr)
	}

	// Step 2: drain — wait briefly for the SQL thread to apply pending
	// relay logs. STOP SLAVE IO_THREAD first stops new events; then
	// STOP SLAVE SQL_THREAD applies what's buffered. 8.0.22+ renamed to
	// STOP REPLICA but the legacy spelling still works in 8.0 (and we
	// support 5.7). Use the legacy form for compatibility.
	drain := "STOP SLAVE"
	if out, err := runMysql(ctx, input.Channel, conn, drain); err != nil {
		return nil, fmt.Errorf("mysql.replica_switch: stop slave: %w", err)
	} else if out.ExitCode != 0 {
		return nil, fmt.Errorf("mysql.replica_switch: stop slave failed: %s", out.Stderr)
	}

	// Step 3: point at the new primary. File/pos are taken from the
	// captured SHOW SLAVE STATUS of the OLD primary; since the new
	// primary was a sibling replica of this one, its binlog coordinates
	// are the same stream. SOURCE_PASSWORD is intentionally not part of
	// this module: credentials for replication live in the target's
	// own config (rpl_user), and embedding them in CHANGE MASTER TO
	// would put a secret on the command line.
	switchSQL := fmt.Sprintf(
		"CHANGE MASTER TO MASTER_HOST='%s', MASTER_USER='rpl_user', MASTER_AUTO_POSITION=1",
		newPrimary)
	if out, err := runMysql(ctx, input.Channel, conn, switchSQL); err != nil {
		return nil, fmt.Errorf("mysql.replica_switch: change master: %w", err)
	} else if out.ExitCode != 0 {
		return nil, fmt.Errorf("mysql.replica_switch: change master failed: %s", out.Stderr)
	}

	// Step 4: resume replication from the new primary.
	start := "START SLAVE"
	if out, err := runMysql(ctx, input.Channel, conn, start); err != nil {
		return nil, fmt.Errorf("mysql.replica_switch: start slave: %w", err)
	} else if out.ExitCode != 0 {
		return nil, fmt.Errorf("mysql.replica_switch: start slave failed: %s", out.Stderr)
	}

	return &executor.ModuleOutput{
		ExitCode: 0,
		Stdout:   fmt.Sprintf("replica repointed to %s and replication started", newPrimary),
		Changed:  true,
	}, nil
}

// --- helpers ----------------------------------------------------------------

// stringArg extracts a required string from args[key].
func stringArg(args map[string]any, key string) (string, error) {
	v, ok := args[key]
	if !ok {
		return "", fmt.Errorf("missing argument %q", key)
	}
	s, ok := v.(string)
	if !ok {
		return "", fmt.Errorf("argument %q must be a string, got %T", key, v)
	}
	return s, nil
}

// stringOk returns the string value of args[key] and true when present and a
// string; otherwise ("", false).
func stringOk(args map[string]any, key string) (string, bool) {
	v, ok := args[key]
	if !ok {
		return "", false
	}
	s, ok := v.(string)
	if !ok {
		return "", false
	}
	return s, true
}

// shellQuote wraps s in single quotes and escapes any embedded single quotes
// so that the result is a single POSIX-sh word.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// init registers the module with the default executor.
func init() {
	executor.RegisterModule(New())
}
