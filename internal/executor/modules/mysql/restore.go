package mysql

// restore.go restores a schema object from the server's own SHOW CREATE TABLE
// output, with a deliberately narrow safety contract:
//
//   - the relation must NOT exist at restore time. An existing relation is a
//     manual-review state, not something LEVEE may DROP and recreate;
//   - the snapshot must be exactly one CREATE statement for the named table;
//   - after creation, SHOW CREATE TABLE must return the same definition.
//
// This is schema recovery, not row recovery. It never claims to restore data,
// and it never turns a failed apply into permission to overwrite an object.

import (
	"context"
	"fmt"
	"strings"

	"github.com/xwb1989/sqlparser"

	"github.com/nexus/levee/internal/executor"
	"github.com/nexus/levee/internal/executor/sqlguard"
)

func (m *Module) schemaRestore(ctx context.Context, input executor.ModuleInput) (*executor.ModuleOutput, error) {
	tableArg, err := stringArg(input.Args, "table")
	if err != nil {
		return nil, fmt.Errorf("mysql.schema_restore: %w", err)
	}
	tables, err := sqlguard.TableList(map[string]any{"tables": []any{tableArg}}, "tables")
	if err != nil {
		return nil, fmt.Errorf("mysql.schema_restore: %w", err)
	}
	if len(tables) != 1 {
		return nil, fmt.Errorf("mysql.schema_restore: exactly one table is required, got %d", len(tables))
	}
	table := tables[0]
	snapshot, err := stringArg(input.Args, "snapshot")
	if err != nil {
		return nil, fmt.Errorf("mysql.schema_restore: %w", err)
	}
	snapshot = normalizeSnapshotDDL(snapshot)
	if err := validateCreateSnapshot(snapshot, table); err != nil {
		return nil, fmt.Errorf("mysql.schema_restore: %w", err)
	}
	if v, ok := stringOk(input.Args, "password"); ok && v != "" {
		return nil, fmt.Errorf("mysql.schema_restore: args.password is not supported — use ~/.my.cnf (mode 0600) on the target")
	}
	conn := connFromArgs(input)

	// Existence is checked server-side. An existing table is NEVER replaced:
	// its data and its post-failure definition are both outside this action's
	// authority. The operator must inspect that state instead.
	_, tableName := sqlguard.SplitQualifier(table)
	existsSQL := fmt.Sprintf("SELECT COUNT(*) FROM information_schema.tables WHERE table_schema = %s AND table_name = %s",
		schemaRestoreSchemaExpr(table), quoteSQLString(tableName))
	probe, err := runMysqlRead(ctx, input.Channel, conn, existsSQL)
	if err != nil {
		return nil, fmt.Errorf("mysql.schema_restore: existence check: %w", err)
	}
	if probe.ExitCode != 0 {
		return nil, fmt.Errorf("mysql.schema_restore: existence check failed: %s", strings.TrimSpace(probe.Stderr))
	}
	if strings.TrimSpace(probe.Stdout) != "0" {
		return nil, fmt.Errorf("mysql.schema_restore: relation %q already exists; refusing to overwrite it — schema recovery is allowed only when the table is missing", table)
	}

	out, err := runMysql(ctx, input.Channel, conn, snapshot)
	if err != nil {
		return nil, fmt.Errorf("mysql.schema_restore: apply snapshot: %w", err)
	}
	if out.ExitCode != 0 {
		return out, fmt.Errorf("mysql.schema_restore: apply snapshot failed: %s", strings.TrimSpace(out.Stderr))
	}

	// Recovery is not a claim made from exit code 0 alone. Read the object back
	// and require the server's definition to match the captured one exactly.
	verify, err := runMysqlRead(ctx, input.Channel, conn, snapshotSQL([]string{table}))
	if err != nil {
		return nil, fmt.Errorf("mysql.schema_restore: verification read: %w", err)
	}
	if verify.ExitCode != 0 {
		return nil, fmt.Errorf("mysql.schema_restore: verification failed: %s", strings.TrimSpace(verify.Stderr))
	}
	got := normalizeSnapshotDDL(verify.Stdout)
	want := normalizeSnapshotDDL(snapshot)
	if got != want {
		return nil, fmt.Errorf("mysql.schema_restore: verification mismatch: restored definition differs from the snapshot")
	}
	out.Stdout = strings.TrimSpace(out.Stdout + "\n" + verify.Stdout)
	out.Changed = true
	return out, nil
}

// normalizeSnapshotDDL removes the optional "table<TAB>" label emitted by
// SHOW CREATE TABLE, leaving exactly the executable CREATE statement. The same
// normalization is used for the captured and the verification payload.
func normalizeSnapshotDDL(snapshot string) string {
	snapshot = strings.TrimSpace(snapshot)
	if strings.HasPrefix(strings.ToUpper(snapshot), "CREATE") {
		return snapshot
	}
	if tab := strings.IndexByte(snapshot, '\t'); tab >= 0 {
		return strings.TrimSpace(snapshot[tab+1:])
	}
	return snapshot
}

// schemaRestoreSchemaExpr resolves the database half of a possibly qualified
// table. An unqualified name uses the connection's current database; a
// qualified one uses its explicit schema. The value is a SQL string literal,
// not an identifier, so quoteSQLString handles the escaping.
func schemaRestoreSchemaExpr(table string) string {
	schema, _ := sqlguard.SplitQualifier(table)
	if schema == "" {
		return "DATABASE()"
	}
	return quoteSQLString(schema)
}

func quoteSQLString(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}

// validateCreateSnapshot accepts exactly one CREATE statement for table. A
// strict DDL parse is required: a partially read prefix is not a restorable
// definition, and a second statement could smuggle a destructive action into
// the recovery path.
func validateCreateSnapshot(snapshot, table string) error {
	pieces, err := sqlparser.SplitStatementToPieces(snapshot)
	if err != nil {
		return fmt.Errorf("split snapshot: %w", err)
	}
	if len(pieces) != 1 || strings.TrimSpace(pieces[0]) == "" {
		return fmt.Errorf("snapshot must contain exactly one CREATE TABLE statement, got %d", len(pieces))
	}
	stmt, err := sqlparser.ParseStrictDDL(pieces[0])
	if err != nil {
		return fmt.Errorf("snapshot is not a fully readable CREATE TABLE statement: %w", err)
	}
	ddl, ok := stmt.(*sqlparser.DDL)
	if !ok || ddl.Action != sqlparser.CreateStr {
		return fmt.Errorf("snapshot must contain CREATE TABLE, not another statement type")
	}
	got := normalizeSQLIdent(tableNameFromCreateSQL(pieces[0]))
	want := normalizeSQLIdent(table)
	if got != want {
		return fmt.Errorf("snapshot table %q does not match requested table %q", got, want)
	}
	return nil
}

// tableNameFromCreateSQL reads the table token after CREATE TABLE [IF NOT
// EXISTS]. The strict parser has already accepted the full statement, but its
// DDL.Table field is empty on this grammar version, so the AST alone cannot
// supply the name. Table names accepted by sqlguard.TableList cannot contain
// whitespace, which keeps this prefix extraction unambiguous.
func tableNameFromCreateSQL(sqlText string) string {
	rest := strings.TrimSpace(sqlText)
	const create = "create table"
	if len(rest) < len(create) || !strings.EqualFold(rest[:len(create)], create) {
		return ""
	}
	rest = strings.TrimSpace(rest[len(create):])
	const missing = "if not exists"
	if len(rest) >= len(missing) && strings.EqualFold(rest[:len(missing)], missing) {
		rest = strings.TrimSpace(rest[len(missing):])
	}
	end := strings.IndexAny(rest, " \t\r\n(;")
	if end < 0 {
		end = len(rest)
	}
	return rest[:end]
}

func normalizeSQLIdent(s string) string {
	return strings.ToLower(strings.ReplaceAll(s, "`", ""))
}
