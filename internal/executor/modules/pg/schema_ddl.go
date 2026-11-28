package pg

// schema_ddl.go captures and validates the versioned, restorable PostgreSQL
// schema format. The older schema_snapshot action intentionally remains an
// introspection report; this file is the separate recovery path.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	pg_query "github.com/sqlc-dev/oliphant"

	"github.com/nexus/levee/internal/channel"
	"github.com/nexus/levee/internal/executor"
	"github.com/nexus/levee/internal/executor/sqlguard"
)

const (
	schemaSnapshotFormat = "levee.pg.schema/v1"
	maxSchemaSnapshotLen = 4 << 20
)

type schemaSnapshot struct {
	Format string `json:"format"`
	Table  string `json:"table"`
	DDL    string `json:"ddl"`
	SHA256 string `json:"sha256"`
}

// schemaSnapshotDDL resolves one table server-side, captures pg_dump's
// schema-only definition, and wraps it in the independently validated format.
func (m *Module) schemaSnapshotDDL(ctx context.Context, input executor.ModuleInput) (*executor.ModuleOutput, error) {
	tables, err := sqlguard.TableList(input.Args, "tables")
	if err != nil {
		return nil, fmt.Errorf("pg.schema_snapshot_ddl: %w", err)
	}
	if len(tables) != 1 {
		return nil, fmt.Errorf("pg.schema_snapshot_ddl: exactly one table per payload is required, got %d", len(tables))
	}
	schema, err := optionalSchemaArg(input.Args)
	if err != nil {
		return nil, fmt.Errorf("pg.schema_snapshot_ddl: %w", err)
	}
	if err := refuseSnapshotPassword(input.Args, "pg.schema_snapshot_ddl"); err != nil {
		return nil, err
	}
	conn := connFromArgs(input)
	ref := regclassLiteral(tables[0], schema)
	resolveSQL := "SELECT n.nspname || '.' || c.relname FROM pg_class c " +
		"JOIN pg_namespace n ON n.oid = c.relnamespace WHERE c.oid = to_regclass(" + quoteLiteral(ref) + ")"
	resolved, err := runPsqlRead(ctx, input.Channel, conn, resolveSQL)
	if err != nil {
		return nil, fmt.Errorf("pg.schema_snapshot_ddl: resolve table: %w", err)
	}
	if resolved.ExitCode != 0 {
		return nil, fmt.Errorf("pg.schema_snapshot_ddl: resolve table failed: %s", strings.TrimSpace(resolved.Stderr))
	}
	canonical := strings.TrimSpace(resolved.Stdout)
	if canonical == "" || strings.ContainsAny(canonical, "\r\n") {
		return nil, fmt.Errorf("pg.schema_snapshot_ddl: relation %q does not exist or is not visible", tables[0])
	}
	valid, err := sqlguard.TableList(map[string]any{"tables": []any{canonical}}, "tables")
	if err != nil || len(valid) != 1 {
		return nil, fmt.Errorf("pg.schema_snapshot_ddl: server returned an invalid canonical relation %q", canonical)
	}
	schemaName, tableName := sqlguard.SplitQualifier(canonical)
	if schemaName == "" || tableName == "" {
		return nil, fmt.Errorf("pg.schema_snapshot_ddl: canonical relation %q is not schema-qualified", canonical)
	}

	out, ddl, err := captureSchemaDDL(ctx, input.Channel, conn, canonical)
	if err != nil {
		return nil, fmt.Errorf("pg.schema_snapshot_ddl: %w", err)
	}
	manifest, err := encodeSchemaSnapshot(canonical, ddl)
	if err != nil {
		return nil, fmt.Errorf("pg.schema_snapshot_ddl: %w", err)
	}
	out.Stdout = manifest
	out.Stderr = strings.TrimSpace(resolved.Stderr + "\n" + out.Stderr)
	out.Changed = false
	return out, nil
}

// captureSchemaDDL runs pg_dump against an already resolved, schema-qualified
// table. The output is base64 on the wire and validated before it can be stored.
func captureSchemaDDL(ctx context.Context, ch channel.Channel, conn pgConn, table string) (*executor.ModuleOutput, string, error) {
	if err := validateConnection(conn); err != nil {
		return nil, "", err
	}
	schemaName, tableName := sqlguard.SplitQualifier(table)
	pattern := literal(schemaName) + "." + literal(tableName)
	cmd := fmt.Sprintf("pg_dump --schema-only --no-owner --no-privileges --no-tablespaces --no-comments --no-security-labels --quote-all-identifiers --restrict-key=leveepgschema1 -h %s -p %s -U %s",
		conn.Host, conn.Port, conn.User)
	if conn.Database != "" {
		cmd += " -d " + conn.Database
	}
	cmd = "set -o pipefail; " + cmd + " -t " + shellQuoteSingle(pattern) + " | base64"
	res, err := ch.Exec(ctx, cmd)
	if err != nil {
		return nil, "", fmt.Errorf("pg_dump: channel exec: %w", err)
	}
	out := &executor.ModuleOutput{ExitCode: res.ExitCode, Stdout: res.Stdout, Stderr: res.Stderr, Duration: res.Duration}
	if res.ExitCode != 0 {
		return out, "", fmt.Errorf("pg_dump failed for %q: %s", table, strings.TrimSpace(res.Stderr))
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(res.Stdout))
	if err != nil {
		return out, "", fmt.Errorf("pg_dump output is not base64: %w", err)
	}
	if strings.TrimSpace(string(raw)) == "" {
		return out, "", fmt.Errorf("pg_dump returned no schema output for %q: %s", table, strings.TrimSpace(res.Stderr))
	}
	ddl := strings.TrimSpace(string(raw)) + "\n"
	if err := validateSchemaRestoreScriptCapture(ddl, table); err != nil {
		return out, "", fmt.Errorf("pg_dump output for %q is not a supported restore script: %w (stderr: %s)", table, err, strings.TrimSpace(res.Stderr))
	}
	return out, ddl, nil
}

func encodeSchemaSnapshot(table, ddl string) (string, error) {
	ddl = strings.TrimSpace(ddl) + "\n"
	sum := sha256.Sum256([]byte(ddl))
	manifest := schemaSnapshot{Format: schemaSnapshotFormat, Table: table, DDL: ddl, SHA256: hex.EncodeToString(sum[:])}
	if err := validateSchemaRestoreScriptCapture(ddl, table); err != nil {
		return "", err
	}
	data, err := json.Marshal(manifest)
	if err != nil {
		return "", fmt.Errorf("encode snapshot: %w", err)
	}
	return string(data), nil
}

func decodeSchemaSnapshot(raw, requestedTable string) (*schemaSnapshot, error) {
	manifest, err := decodeSchemaSnapshotManifest(raw, requestedTable)
	if err != nil {
		return nil, err
	}
	if err := validateSchemaRestoreScript(manifest.DDL, manifest.Table); err != nil {
		return nil, err
	}
	return manifest, nil
}

func decodeSchemaSnapshotManifest(raw, requestedTable string) (*schemaSnapshot, error) {
	if len(raw) == 0 || len(raw) > maxSchemaSnapshotLen {
		return nil, fmt.Errorf("snapshot must be non-empty and no larger than %d bytes", maxSchemaSnapshotLen)
	}
	var manifest schemaSnapshot
	dec := json.NewDecoder(bytes.NewBufferString(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&manifest); err != nil {
		return nil, fmt.Errorf("decode snapshot: %w", err)
	}
	if err := ensureJSONEOF(dec); err != nil {
		return nil, err
	}
	if manifest.Format != schemaSnapshotFormat {
		return nil, fmt.Errorf("unsupported snapshot format %q (want %q)", manifest.Format, schemaSnapshotFormat)
	}
	if manifest.Table != requestedTable {
		return nil, fmt.Errorf("snapshot table %q does not match requested table %q", manifest.Table, requestedTable)
	}
	if len(manifest.DDL) == 0 || len(manifest.DDL) > maxSchemaSnapshotLen {
		return nil, fmt.Errorf("snapshot DDL is empty or exceeds the size limit")
	}
	got, err := hex.DecodeString(manifest.SHA256)
	if err != nil || len(got) != sha256.Size {
		return nil, fmt.Errorf("snapshot SHA-256 is malformed")
	}
	want := sha256.Sum256([]byte(manifest.DDL))
	if subtle.ConstantTimeCompare(got, want[:]) != 1 {
		return nil, fmt.Errorf("snapshot SHA-256 mismatch: payload changed after capture")
	}
	return &manifest, nil
}

func ensureJSONEOF(dec *json.Decoder) error {
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		if err == nil {
			return fmt.Errorf("snapshot contains a second JSON value")
		}
		return fmt.Errorf("snapshot has trailing data: %w", err)
	}
	return nil
}

// normalizePgDumpMeta handles the psql \restrict/\unrestrict pair introduced
// by newer pg_dump releases. The pair stays in the persisted and executed DDL;
// it is removed only for AST analysis. Any other psql meta-command is refused.
func normalizePgDumpMeta(batch string) (string, error) {
	const (
		restrict   = `\restrict leveepgschema1`
		unrestrict = `\unrestrict leveepgschema1`
	)
	lines := strings.Split(batch, "\n")
	restrictAt, unrestrictAt := -1, -1
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if !strings.HasPrefix(trimmed, `\`) {
			continue
		}
		switch trimmed {
		case restrict:
			if restrictAt >= 0 || unrestrictAt >= 0 {
				return "", fmt.Errorf("snapshot has duplicate or misplaced %s", restrict)
			}
			restrictAt = i
		case unrestrict:
			if restrictAt < 0 || unrestrictAt >= 0 {
				return "", fmt.Errorf("snapshot has an unmatched %s", unrestrict)
			}
			unrestrictAt = i
		default:
			return "", fmt.Errorf("snapshot contains unsupported psql meta-command %q", trimmed)
		}
	}
	if (restrictAt < 0) != (unrestrictAt < 0) {
		return "", fmt.Errorf("snapshot must contain a matching pg_dump restrict pair")
	}
	if restrictAt < 0 {
		return batch, nil // older pg_dump: no psql restriction metadata
	}
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "--") || i == restrictAt || i == unrestrictAt {
			continue
		}
		if i < restrictAt || i > unrestrictAt {
			return "", fmt.Errorf("snapshot has SQL outside the pg_dump restrict pair")
		}
	}
	out := make([]string, 0, len(lines)-2)
	for i, line := range lines {
		if i != restrictAt && i != unrestrictAt {
			out = append(out, line)
		}
	}
	return strings.Join(out, "\n"), nil
}

// validateSchemaRestoreScript is the security boundary for both freshly dumped
// and persisted scripts. Only a self-contained table, its owned SERIAL
// sequences, constraints and indexes are accepted. Everything else is refused.
func validateSelfContainedSchemaRestoreScript(batch, table string) error {
	if strings.TrimSpace(batch) == "" {
		return fmt.Errorf("snapshot DDL is empty")
	}
	if _, name := sqlguard.SplitQualifier(table); name == table {
		return fmt.Errorf("snapshot table %q must be schema-qualified", table)
	}
	normalized, err := normalizePgDumpMeta(batch)
	if err != nil {
		return err
	}
	tree, err := pg_query.Parse(normalized)
	if err != nil {
		return fmt.Errorf("snapshot DDL is not fully readable PostgreSQL: %w", err)
	}
	statements := tree.GetStmts()
	if len(statements) == 0 {
		return fmt.Errorf("snapshot DDL contains no statements")
	}

	ownedSequences := map[string]bool{}
	for _, raw := range statements {
		stmt := raw.GetStmt().GetAlterSeqStmt()
		if stmt == nil {
			continue
		}
		name := relationName(stmt.GetSequence())
		if sequenceOwnedBy(stmt, table) {
			ownedSequences[name] = true
		}
	}

	createTables := 0
	for _, raw := range statements {
		node := raw.GetStmt().GetNode()
		switch stmt := node.(type) {
		case *pg_query.Node_VariableSetStmt:
			if !allowedDumpSet(stmt.VariableSetStmt.GetName()) {
				return fmt.Errorf("snapshot contains unsupported SET %q", stmt.VariableSetStmt.GetName())
			}
		case *pg_query.Node_SelectStmt:
			if normalizedStatement(normalized, raw) != "select pg_catalog.set_config('search_path', '', false)" {
				return fmt.Errorf("snapshot contains a SELECT other than pg_dump's search_path guard")
			}
		case *pg_query.Node_CreateStmt:
			createTables++
			if err := validateCreateTable(stmt.CreateStmt, table); err != nil {
				return err
			}
		case *pg_query.Node_AlterTableStmt:
			if err := validateAlterTable(stmt.AlterTableStmt, table); err != nil {
				return err
			}
		case *pg_query.Node_IndexStmt:
			if relationName(stmt.IndexStmt.GetRelation()) != table || stmt.IndexStmt.GetConcurrent() || stmt.IndexStmt.GetIfNotExists() {
				return fmt.Errorf("snapshot contains an index that is not a plain index on %q", table)
			}
		case *pg_query.Node_CreateSeqStmt:
			if !ownedSequences[relationName(stmt.CreateSeqStmt.GetSequence())] || stmt.CreateSeqStmt.GetIfNotExists() {
				return fmt.Errorf("snapshot sequence %q is not proven to be owned by %q", relationName(stmt.CreateSeqStmt.GetSequence()), table)
			}
		case *pg_query.Node_AlterSeqStmt:
			if !sequenceOwnedBy(stmt.AlterSeqStmt, table) || stmt.AlterSeqStmt.GetMissingOk() {
				return fmt.Errorf("snapshot contains an ALTER SEQUENCE outside %q", table)
			}
		default:
			return fmt.Errorf("snapshot contains unsupported statement type %T; only self-contained table DDL is restorable", node)
		}
	}
	if createTables != 1 {
		return fmt.Errorf("snapshot must contain exactly one CREATE TABLE for %q, got %d", table, createTables)
	}
	return nil
}

func validateCreateTable(stmt *pg_query.CreateStmt, table string) error {
	if relationName(stmt.GetRelation()) != table || stmt.GetIfNotExists() {
		return fmt.Errorf("CREATE TABLE relation does not match %q", table)
	}
	if len(stmt.GetInhRelations()) != 0 || stmt.GetPartbound() != nil || stmt.GetPartspec() != nil || stmt.GetOfTypename() != nil {
		return fmt.Errorf("partitioned, inherited and typed tables are outside this recovery format")
	}
	for _, node := range append(stmt.GetTableElts(), stmt.GetConstraints()...) {
		switch item := node.GetNode().(type) {
		case *pg_query.Node_ColumnDef:
			if item.ColumnDef.GetIdentity() != "" || item.ColumnDef.GetIdentitySequence() != nil {
				return fmt.Errorf("IDENTITY columns are outside this recovery format; SERIAL is supported")
			}
			for _, rawConstraint := range item.ColumnDef.GetConstraints() {
				if err := validateTableConstraint(rawConstraint.GetConstraint(), table); err != nil {
					return err
				}
			}
		case *pg_query.Node_Constraint:
			if err := validateTableConstraint(item.Constraint, table); err != nil {
				return err
			}
		}
	}
	return nil
}

func validateAlterTable(stmt *pg_query.AlterTableStmt, table string) error {
	if relationName(stmt.GetRelation()) != table || stmt.GetMissingOk() {
		return fmt.Errorf("ALTER TABLE relation does not match %q", table)
	}
	for _, node := range stmt.GetCmds() {
		cmd := node.GetAlterTableCmd()
		if cmd == nil {
			return fmt.Errorf("snapshot contains an unreadable ALTER TABLE command")
		}
		switch cmd.GetSubtype().String() {
		case "AT_AddConstraint":
			if err := validateTableConstraint(cmd.GetDef().GetConstraint(), table); err != nil {
				return err
			}
		case "AT_ColumnDefault", "AT_AlterColumnDefault":
			// pg_dump uses this for SERIAL defaults.
		case "AT_AddIdentity", "AT_SetIdentity":
			// pg_dump also emits ADD/SET GENERATED for IDENTITY columns.
		default:
			return fmt.Errorf("snapshot contains unsupported ALTER TABLE command %s", cmd.GetSubtype().String())
		}
	}
	return nil
}

func validateTableConstraint(constraint *pg_query.Constraint, table string) error {
	if constraint == nil {
		return fmt.Errorf("snapshot contains an unreadable table constraint")
	}
	kind := constraint.GetContype().String()
	if kind == "CONSTR_IDENTITY" {
		if generated := constraint.GetGeneratedWhen(); generated != "a" && generated != "d" {
			return fmt.Errorf("unsupported IDENTITY generation mode %q", generated)
		}
		if len(constraint.GetOptions()) != 0 {
			return fmt.Errorf("IDENTITY sequence options are outside this recovery format; use a plain IDENTITY column or SERIAL")
		}
		return nil
	}
	if kind == "CONSTR_FOREIGN" && relationName(constraint.GetPktable()) != table {
		return fmt.Errorf("cross-table foreign keys are outside this recovery format")
	}
	return nil
}

func sequenceOwnedBy(stmt *pg_query.AlterSeqStmt, table string) bool {
	if len(stmt.GetOptions()) != 1 {
		return false
	}
	def := stmt.GetOptions()[0].GetDefElem()
	if def == nil || def.GetDefname() != "owned_by" || def.GetArg().GetList() == nil {
		return false
	}
	items := def.GetArg().GetList().GetItems()
	if len(items) != 3 {
		return false
	}
	schemaName := items[0].GetString_().GetSval()
	tableName := items[1].GetString_().GetSval()
	return schemaName != "" && tableName != "" && schemaName+"."+tableName == table
}

func relationName(relation *pg_query.RangeVar) string {
	if relation == nil || relation.GetCatalogname() != "" || relation.GetSchemaname() == "" || relation.GetRelname() == "" {
		return ""
	}
	return relation.GetSchemaname() + "." + relation.GetRelname()
}

func normalizedStatement(batch string, raw *pg_query.RawStmt) string {
	start := int(raw.GetStmtLocation())
	end := start + int(raw.GetStmtLen())
	if start < 0 || end <= start || end > len(batch) {
		return ""
	}
	return strings.ToLower(strings.Join(strings.Fields(batch[start:end]), " "))
}

func allowedDumpSet(name string) bool {
	switch strings.ToLower(name) {
	case "statement_timeout", "lock_timeout", "idle_in_transaction_session_timeout",
		"client_encoding", "standard_conforming_strings", "check_function_bodies",
		"xmloption", "client_min_messages", "row_security", "search_path", "default_table_access_method":
		return true
	default:
		return false
	}
}

func refuseSnapshotPassword(args map[string]any, action string) error {
	if value, ok := stringOk(args, "password"); ok && value != "" {
		return fmt.Errorf("%s: args.password is not supported — use ~/.pgpass (mode 0600) or peer authentication on the target", action)
	}
	return nil
}

func shellQuoteSingle(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\\''") + "'"
}
