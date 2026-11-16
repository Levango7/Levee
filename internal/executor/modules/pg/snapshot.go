package pg

// snapshot.go builds the introspection SQL behind `pg.schema_snapshot`.
//
// Why introspection rather than a dump: PostgreSQL has no SHOW CREATE TABLE,
// and the nearest authoritative tool is pg_dump — which would add a client
// binary (and a server/client version coupling) to every target that wants a
// pre-change snapshot. The introspection below needs nothing but psql, which
// this module already requires.
//
// The trade is stated rather than hidden: this is NOT restorable DDL. It is
// the facts a reviewer (or a future undo generator) needs — columns with types
// and defaults, constraints, indexes — captured as text. The capture says so
// in its own first line, because the one unaffordable reading is "this is a
// dump; we can restore from it".
//
// Two decisions keep a failed lookup LOUD instead of silently empty:
//
//   - every table is resolved ONCE, by name, into a regclass oid
//     (to_regclass), and every subsequent query joins on that oid. Filtering
//     information_schema/pg_indexes by (schema, name) string equality would
//     return zero rows for a name the search_path does not reach — an empty
//     capture that exits 0 and reads like "this table has no columns";
//   - an existence guard RAISES when to_regclass resolves to NULL. Under
//     ON_ERROR_STOP=1 (which every psql call in this module carries) that
//     fails the step, which is the same outcome the mysql side gets for free
//     from the client's abort-on-error default.

import (
	"context"
	"fmt"
	"strings"

	"github.com/nexus/levee/internal/executor"
	"github.com/nexus/levee/internal/executor/sqlguard"
)

// schemaSnapshot captures the definition of args["tables"].
func (m *Module) schemaSnapshot(ctx context.Context, input executor.ModuleInput) (*executor.ModuleOutput, error) {
	tables, err := sqlguard.TableList(input.Args, "tables")
	if err != nil {
		return nil, fmt.Errorf("pg.schema_snapshot: %w", err)
	}
	schema, err := optionalSchemaArg(input.Args)
	if err != nil {
		return nil, fmt.Errorf("pg.schema_snapshot: %w", err)
	}
	// Same refusal as pg.query (see the package doc): the module never accepts
	// a credential argument, and silently ignoring one is how an operator ends
	// up debugging an auth failure they cannot see.
	if v, ok := stringOk(input.Args, "password"); ok && v != "" {
		return nil, fmt.Errorf("pg.schema_snapshot: args.password is not supported — LEVEE never puts credentials on a command line; " +
			"configure ~/.pgpass (mode 0600) or peer authentication on the target and drop the argument")
	}
	out, err := runPsqlRead(ctx, input.Channel, connFromArgs(input), snapshotSQL(tables, schema))
	if err != nil {
		return nil, fmt.Errorf("pg.schema_snapshot: %w", err)
	}
	return out, nil
}

// optionalSchemaArg reads args["schema"], the schema an UNQUALIFIED table name
// is resolved in. Empty means "let the server decide" (the session's
// search_path) — the resolution happens server-side inside to_regclass rather
// than being guessed here, because a snapshot taken against the wrong schema
// is worse than no snapshot.
func optionalSchemaArg(args map[string]any) (string, error) {
	v, ok := stringOk(args, "schema")
	if !ok {
		return "", nil
	}
	v = strings.TrimSpace(v)
	if v == "" {
		return "", nil
	}
	if !identRe.MatchString(v) {
		return "", fmt.Errorf("invalid schema %q: only letters, digits, underscore and dollar "+
			"are allowed (the name is spliced into a quoted identifier)", v)
	}
	return v, nil
}

// snapshotSQL renders the capture: an existence guard per table, then its
// columns, constraints and indexes, in the author's order.
func snapshotSQL(tables []string, schema string) string {
	var b strings.Builder
	b.WriteString("-- levee.schema_snapshot (introspection: columns, constraints, indexes — " +
		"NOT pg_dump output and not restorable DDL)\n")
	for _, t := range tables {
		ref := regclassLiteral(t, schema)
		fmt.Fprintf(&b, "-- table %s\n", ref)
		b.WriteString(existenceGuard(ref))
		b.WriteString(columnsQuery(ref))
		b.WriteString(constraintsQuery(ref))
		b.WriteString(indexesQuery(ref))
	}
	return b.String()
}

// regclassLiteral renders the name to_regclass resolves: a quoted identifier
// pair for the qualified form, a plain one otherwise. An unqualified name is
// resolved by the session's search_path, which is the honest reading of "the
// table named t" on that connection.
func regclassLiteral(table, schema string) string {
	qualSchema, name := sqlguard.SplitQualifier(table)
	if qualSchema != "" {
		return literal(qualSchema) + "." + literal(name)
	}
	if schema != "" {
		return literal(schema) + "." + literal(name)
	}
	return literal(name)
}

// existenceGuard raises when the name does not resolve. to_regclass returns
// NULL rather than raising for a missing relation, so this is where "the table
// is not there" becomes a failed step instead of an empty capture.
func existenceGuard(ref string) string {
	return fmt.Sprintf(
		"DO $levee$ BEGIN IF to_regclass(%s) IS NULL THEN RAISE EXCEPTION "+
			"'levee.schema_snapshot: relation %% does not exist or is not visible on this "+
			"connection''s search_path', %s; END IF; END $levee$;\n",
		quoteLiteral(ref), quoteLiteral(ref))
}

// columnsQuery lists the columns in declaration order (attnum), with the
// server's own type rendering and default expression.
func columnsQuery(ref string) string {
	return fmt.Sprintf("SELECT a.attname || ' ' || format_type(a.atttypid, a.atttypmod) || "+
		"CASE WHEN a.attnotnull THEN ' NOT NULL' ELSE '' END || "+
		"coalesce(' DEFAULT ' || pg_get_expr(d.adbin, d.adrelid), '') "+
		"FROM pg_attribute a LEFT JOIN pg_attrdef d ON d.adrelid = a.attrelid AND d.adnum = a.attnum "+
		"WHERE a.attrelid = to_regclass(%s) AND a.attnum > 0 AND NOT a.attisdropped "+
		"ORDER BY a.attnum;\n", quoteLiteral(ref))
}

// constraintsQuery lists primary keys, uniques, checks and foreign keys with
// the server's own rendering of each definition.
func constraintsQuery(ref string) string {
	return fmt.Sprintf("SELECT contype || ' ' || conname || ' ' || pg_get_constraintdef(oid) "+
		"FROM pg_constraint WHERE conrelid = to_regclass(%s) ORDER BY conname;\n", quoteLiteral(ref))
}

// indexesQuery lists index definitions. Indexes that back a constraint appear
// in this list too, on purpose: the constraint list names the rule, the index
// list names the object, and a reader comparing them wants both.
func indexesQuery(ref string) string {
	return fmt.Sprintf("SELECT c.relname || ' ' || pg_get_indexdef(i.indexrelid) "+
		"FROM pg_index i JOIN pg_class c ON c.oid = i.indexrelid "+
		"WHERE i.indrelid = to_regclass(%s) ORDER BY c.relname;\n", quoteLiteral(ref))
}

// literal renders one identifier as a double-quoted SQL identifier, with an
// embedded quote doubled (the SQL standard escape). The identifier charset is
// validated upstream, so this is defence in depth rather than the barrier.
func literal(s string) string {
	return `"` + strings.ReplaceAll(s, `"`, `""`) + `"`
}

// quoteLiteral renders a SQL string literal.
func quoteLiteral(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}
