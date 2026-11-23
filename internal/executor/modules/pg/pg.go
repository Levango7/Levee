// Package pg implements the LEVEE PostgreSQL module: the database-change
// wedge's second execution surface (design doc section 2.1 database-layer
// coverage). Before it, `mysql.query` was the only way to change a database
// from a governed workflow — a database-change product that could not touch
// the database most of its customers run.
//
// Like the mysql module it drives the server through the channel's shell
// access: it assumes the `psql` client is installed on the target host or
// reachable through the execution user's PATH. All SQL travels base64-encoded
// through a pipe so SQL text never appears on a shell command line — this
// closes the injection surface that quoting-based approaches leave open (a
// payload containing quotes, backticks or $(...) cannot escape the pipe).
//
// Actions:
//
//   - query: run args["sql"] against the database given by the optional
//     args["database"] (psql falls back to the user name, as it does
//     natively). Idempotent DDL/DML by AUTHORSHIP: the module declares itself
//     non-idempotent because it cannot verify that the SQL is safe to re-run
//     (CREATE TABLE IF NOT EXISTS and INSERT ... ON CONFLICT are, a bare
//     INSERT is not). Changed mirrors the exit code.
//
//   - schema_snapshot: capture the pre-change definition of the tables named
//     by args["tables"] into the step's stdout — which the run persists as
//     evidence. Put it BEFORE the step that alters those tables. PostgreSQL
//     has no SHOW CREATE TABLE, so this is an INTROSPECTION dump (columns,
//     constraints, indexes, read from information_schema / pg_catalog), not
//     pg_dump output: the capture says so itself, because a reader who takes
//     it for a restorable dump would be wrong. An unknown table is an ERROR
//     (an existence guard runs first), never an empty capture. Read-only:
//     Changed is always false. It does NOT generate an undo; see
//     docs/blocked-items.md D-5.
//
// Credentials: the module NEVER accepts a password argument. The design's
// security note is that credentials do not travel on a command line, and the
// only way to honour that over a shell channel is to let the target's own
// client configuration supply them — a `~/.pgpass` file (mode 0600) or peer
// authentication. `args["password"]` is refused loudly rather than silently
// ignored: an accepted-but-unused credential input is how an operator ends up
// debugging an authentication failure they cannot see.
//
// ON_ERROR_STOP=1 is not optional: without it psql exits 0 after a failed
// statement, and the run would report success for a change that never
// happened.
package pg

import (
	"context"
	"encoding/base64"
	"fmt"
	"regexp"
	"strings"

	"github.com/nexus/levee/internal/channel"
	"github.com/nexus/levee/internal/executor"
	"github.com/nexus/levee/internal/executor/sqlguard"
)

// Module is the pg module singleton. It is stateless and safe for concurrent
// use.
type Module struct{}

// New returns a new pg module.
func New() *Module { return &Module{} }

// Name returns the module identifier used by workflow steps (`action: pg.query`).
func (Module) Name() string { return "pg" }

// Actions lists the verbs this module supports.
func (Module) Actions() []string { return []string{"query", "schema_snapshot", "preflight"} }

// Idempotent reports whether the module declares itself idempotent. It answers
// false: `query` is idempotent-by-authorship, but the module cannot verify the
// author's claim, and the executor uses this hint for retry/resume decisions —
// claiming idempotence the module cannot check would let a retry re-run a bare
// INSERT.
func (Module) Idempotent() bool { return false }

// Execute dispatches to the module's actions.
func (m *Module) Execute(ctx context.Context, action string, input executor.ModuleInput) (*executor.ModuleOutput, error) {
	switch action {
	case "query":
		return m.query(ctx, input)
	case "schema_snapshot":
		return m.schemaSnapshot(ctx, input)
	case "preflight":
		return m.preflight(ctx, input)
	default:
		return nil, fmt.Errorf("pg: unsupported action %q", action)
	}
}

// --- connection --------------------------------------------------------------

// pgConn captures the connection parameters shared by every action.
type pgConn struct {
	Host     string
	Port     string
	User     string
	Database string
}

// defaultPort is psql's own default, stated here so the refusal messages and
// the built command agree about what "no port given" means.
const defaultPort = "5432"

// connFromArgs builds a pgConn from the action arguments. user defaults to
// "postgres"; port defaults to 5432; host defaults to the target host the
// channel is connected to (args["host"] overrides for a database that does not
// live on the target itself).
func connFromArgs(input executor.ModuleInput) pgConn {
	c := pgConn{
		User: "postgres",
		Port: defaultPort,
		Host: input.Target.Host(),
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
	if v, ok := stringOk(input.Args, "database"); ok {
		c.Database = v
	}
	return c
}

// --- query -------------------------------------------------------------------

// query runs args["sql"] through psql with ON_ERROR_STOP, so a failed statement
// fails the step instead of being reported as a successful no-op.
func (m *Module) query(ctx context.Context, input executor.ModuleInput) (*executor.ModuleOutput, error) {
	sqlText, err := stringArg(input.Args, "sql")
	if err != nil {
		return nil, fmt.Errorf("pg.query: %w", err)
	}
	if strings.TrimSpace(sqlText) == "" {
		return nil, fmt.Errorf("pg.query: empty sql")
	}
	// Refuse a password argument instead of ignoring it. See the package doc:
	// an accepted-but-unused credential input is a silent auth failure.
	if v, ok := stringOk(input.Args, "password"); ok && v != "" {
		return nil, fmt.Errorf("pg.query: args.password is not supported — LEVEE never puts credentials on a command line; " +
			"configure ~/.pgpass (mode 0600) or peer authentication on the target and drop the argument")
	}
	// Same R2 boundary as mysql.query, from the same classifier: the rule is
	// about the SQL, not about the client binary.
	if err := sqlguard.RejectDestructive(sqlText, "pg.query"); err != nil {
		return nil, fmt.Errorf("pg.query: %w", err)
	}

	conn := connFromArgs(input)
	out, err := runPsql(ctx, input.Channel, conn, sqlText)
	if err != nil {
		return nil, fmt.Errorf("pg.query: %w", err)
	}
	return out, nil
}

// --- plumbing ----------------------------------------------------------------

// hostRe matches the characters a host may contain: letters, digits, dot,
// colon, hyphen and brackets (IPv6 literals). Mirrors the mysql module's guard
// so the two surfaces accept the same targets.
var hostRe = regexp.MustCompile(`^[A-Za-z0-9._:\-\[\]]+$`)

// identRe matches identifiers (database, user) that may be spliced into the
// command: PostgreSQL allows more than this, but the connection flags here are
// built by string interpolation, so the accepted set is deliberately the one
// that cannot change the meaning of the command line.
var identRe = regexp.MustCompile(`^[A-Za-z0-9_$]+$`)

func validateHost(s string) error {
	if s == "" || !hostRe.MatchString(s) {
		return fmt.Errorf("invalid host %q: only letters, digits, dot, colon, hyphen and brackets are allowed", s)
	}
	return nil
}

func validateIdent(kind, s string) error {
	if s == "" || !identRe.MatchString(s) {
		return fmt.Errorf("invalid %s %q: only letters, digits, underscore and dollar are allowed", kind, s)
	}
	return nil
}

// runPsql builds the psql invocation for conn. SQL is passed on stdin via a
// base64 pipe; the connection flags are identifier-validated and therefore
// safe to interpolate.
//
// -X ignores ~/.psqlrc (a target-side config file must not change what this
// command prints); -q/-t/-A make the output machine-parseable.
//
// Changed mirrors the exit code because query is the caller that matters
// here; a read-only caller uses runPsqlRead, which reports Changed=false
// instead of telling the evidence the change already happened.
func runPsql(ctx context.Context, ch channel.Channel, conn pgConn, sql string) (*executor.ModuleOutput, error) {
	return runPsqlChanged(ctx, ch, conn, sql, true)
}

func runPsqlRead(ctx context.Context, ch channel.Channel, conn pgConn, sql string) (*executor.ModuleOutput, error) {
	return runPsqlChanged(ctx, ch, conn, sql, false)
}

func runPsqlChanged(ctx context.Context, ch channel.Channel, conn pgConn, sql string, changed bool) (*executor.ModuleOutput, error) {
	if err := validateHost(conn.Host); err != nil {
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
	if p := strings.TrimSpace(conn.Port); p != "" && !identRe.MatchString(p) {
		return nil, fmt.Errorf("invalid port %q: only digits are allowed", conn.Port)
	}

	// base64(UTF-8 SQL) contains only [A-Za-z0-9+/=] — it cannot break out of
	// the command line.
	encoded := base64.StdEncoding.EncodeToString([]byte(sql))
	cmd := fmt.Sprintf("base64 -d <<'LEVEE_EOF' | psql -X -q -t -A -v ON_ERROR_STOP=1 -h %s -p %s -U %s",
		conn.Host, conn.Port, conn.User)
	if conn.Database != "" {
		cmd += " -d " + conn.Database
	}
	cmd += fmt.Sprintf("\n%s\nLEVEE_EOF", encoded)

	res, err := ch.Exec(ctx, cmd)
	if err != nil {
		return nil, fmt.Errorf("pg: channel exec: %w", err)
	}
	return &executor.ModuleOutput{
		ExitCode: res.ExitCode,
		Stdout:   res.Stdout,
		Stderr:   res.Stderr,
		Duration: res.Duration,
		Changed:  changed && res.ExitCode == 0,
	}, nil
}

// stringArg reads a required string argument, naming the action in the error.
func stringArg(args map[string]any, key string) (string, error) {
	v, ok := args[key]
	if !ok {
		return "", fmt.Errorf("missing required argument %q", key)
	}
	s, ok := v.(string)
	if !ok {
		return "", fmt.Errorf("argument %q must be a string, got %T", key, v)
	}
	return s, nil
}

// stringOk reads an optional string argument.
func stringOk(args map[string]any, key string) (string, bool) {
	v, ok := args[key]
	if !ok {
		return "", false
	}
	s, ok := v.(string)
	return s, ok
}

func init() {
	executor.RegisterModule(New())
}
