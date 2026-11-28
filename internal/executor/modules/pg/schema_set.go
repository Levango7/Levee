package pg

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	pg_query "github.com/sqlc-dev/oliphant"

	"github.com/nexus/levee/internal/executor"
	"github.com/nexus/levee/internal/executor/sqlguard"
)

const maxSchemaSnapshotSetSize = 32

type schemaPartitionOptions struct {
	allowCrossTable   bool
	deferCrossTable   bool
	crossTableTargets map[string]bool
}

// validateSchemaRestoreScript keeps the original single-table contract. A
// cross-table FK needs the explicit set path below, never a one-off CREATE.
func validateSchemaRestoreScript(batch, table string) error {
	_, _, err := partitionSchemaRestoreScript(batch, table, schemaPartitionOptions{})
	return err
}

// Capture validates evidence, not an execution plan. Cross-table ALTER FOREIGN
// KEY statements are excluded only for this analysis; the persisted DDL keeps
// them verbatim. Inline cross-table constraints remain unsupported.
func validateSchemaRestoreScriptCapture(batch, table string) error {
	_, _, err := partitionSchemaRestoreScript(batch, table, schemaPartitionOptions{allowCrossTable: true})
	return err
}

func splitSchemaRestoreSetScript(batch, table string, targets map[string]bool) (string, []string, error) {
	return partitionSchemaRestoreScript(batch, table, schemaPartitionOptions{
		allowCrossTable: true, deferCrossTable: true, crossTableTargets: targets,
	})
}

func partitionSchemaRestoreScript(batch, table string, options schemaPartitionOptions) (string, []string, error) {
	normalized, err := normalizePgDumpMeta(batch)
	if err != nil {
		return "", nil, err
	}
	tree, err := pg_query.Parse(normalized)
	if err != nil {
		return "", nil, fmt.Errorf("snapshot DDL is not fully readable PostgreSQL: %w", err)
	}
	base := make([]string, 0)
	deferred := make([]string, 0)
	for _, raw := range tree.GetStmts() {
		reference, err := crossTableReference(raw, table)
		if err != nil {
			return "", nil, err
		}
		text, err := rawStatementText(normalized, raw)
		if err != nil {
			return "", nil, err
		}
		if reference != "" {
			if !options.allowCrossTable {
				return "", nil, fmt.Errorf("cross-table foreign key on %q requires a snapshot set, not single-table restore", table)
			}
			if options.crossTableTargets != nil && !options.crossTableTargets[reference] {
				return "", nil, fmt.Errorf("foreign key target %q is outside the snapshot set", reference)
			}
			if options.deferCrossTable {
				deferred = append(deferred, text)
			}
			continue
		}
		base = append(base, text)
	}
	baseBatch := strings.Join(base, ";\n")
	if len(base) > 0 {
		baseBatch += ";"
	}
	if err := validateSelfContainedSchemaRestoreScript(baseBatch, table); err != nil {
		return "", nil, err
	}
	return baseBatch, deferred, nil
}

func crossTableReference(raw *pg_query.RawStmt, table string) (string, error) {
	switch stmt := raw.GetStmt().GetNode().(type) {
	case *pg_query.Node_CreateStmt:
		for _, node := range append(stmt.CreateStmt.GetTableElts(), stmt.CreateStmt.GetConstraints()...) {
			switch item := node.GetNode().(type) {
			case *pg_query.Node_ColumnDef:
				for _, rawConstraint := range item.ColumnDef.GetConstraints() {
					if reference := foreignReference(rawConstraint.GetConstraint(), table); reference != "" {
						return "", fmt.Errorf("inline cross-table foreign keys on %q are not supported; grouped restore requires pg_dump's ALTER TABLE form", table)
					}
				}
			case *pg_query.Node_Constraint:
				if reference := foreignReference(item.Constraint, table); reference != "" {
					return "", fmt.Errorf("inline cross-table foreign keys on %q are not supported; grouped restore requires pg_dump's ALTER TABLE form", table)
				}
			}
		}
	case *pg_query.Node_AlterTableStmt:
		cmds := stmt.AlterTableStmt.GetCmds()
		for _, node := range cmds {
			cmd := node.GetAlterTableCmd()
			if cmd == nil || cmd.GetSubtype().String() != "AT_AddConstraint" {
				continue
			}
			if reference := foreignReference(cmd.GetDef().GetConstraint(), table); reference != "" {
				if len(cmds) != 1 {
					return "", fmt.Errorf("cannot defer a compound ALTER TABLE on %q", table)
				}
				return reference, nil
			}
		}
	}
	return "", nil
}

func foreignReference(constraint *pg_query.Constraint, table string) string {
	if constraint == nil || constraint.GetContype().String() != "CONSTR_FOREIGN" {
		return ""
	}
	reference := relationName(constraint.GetPktable())
	if reference == table {
		return ""
	}
	return reference
}

func rawStatementText(batch string, raw *pg_query.RawStmt) (string, error) {
	start := int(raw.GetStmtLocation())
	end := start + int(raw.GetStmtLen())
	if start < 0 || end <= start || end > len(batch) {
		return "", fmt.Errorf("snapshot contains an unreadable statement boundary")
	}
	return strings.TrimSpace(batch[start:end]), nil
}

type schemaSetEntry struct {
	manifest *schemaSnapshot
	base     string
	deferred []string
}

func (m *Module) schemaRestoreSet(ctx context.Context, input executor.ModuleInput, raw string) (*executor.ModuleOutput, error) {
	if len(raw) == 0 || len(raw) > maxSchemaSnapshotLen {
		return nil, fmt.Errorf("pg.schema_restore: snapshot set must be non-empty and no larger than %d bytes", maxSchemaSnapshotLen)
	}
	var raws []string
	dec := json.NewDecoder(bytes.NewBufferString(raw))
	if err := dec.Decode(&raws); err != nil {
		return nil, fmt.Errorf("pg.schema_restore: decode snapshot set: %w", err)
	}
	if err := ensureJSONEOF(dec); err != nil {
		return nil, fmt.Errorf("pg.schema_restore: %w", err)
	}
	if len(raws) == 0 || len(raws) > maxSchemaSnapshotSetSize {
		return nil, fmt.Errorf("pg.schema_restore: snapshot set must contain 1..%d tables", maxSchemaSnapshotSetSize)
	}
	if err := refuseSnapshotPassword(input.Args, "pg.schema_restore"); err != nil {
		return nil, err
	}

	entries := make([]schemaSetEntry, 0, len(raws))
	targets := make(map[string]bool, len(raws))
	seen := make(map[string]bool, len(raws))
	for _, rawManifest := range raws {
		var header struct {
			Table string `json:"table"`
		}
		if err := json.Unmarshal([]byte(rawManifest), &header); err != nil {
			return nil, fmt.Errorf("pg.schema_restore: read snapshot table: %w", err)
		}
		valid, err := sqlguard.TableList(map[string]any{"tables": []any{header.Table}}, "tables")
		if err != nil || len(valid) != 1 || valid[0] != header.Table {
			return nil, fmt.Errorf("pg.schema_restore: invalid snapshot table %q", header.Table)
		}
		if seen[header.Table] {
			return nil, fmt.Errorf("pg.schema_restore: duplicate table %q in snapshot set", header.Table)
		}
		seen[header.Table] = true
		manifest, err := decodeSchemaSnapshotManifest(rawManifest, header.Table)
		if err != nil {
			return nil, fmt.Errorf("pg.schema_restore: %w", err)
		}
		targets[manifest.Table] = true
		entries = append(entries, schemaSetEntry{manifest: manifest})
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].manifest.Table < entries[j].manifest.Table })
	for i := range entries {
		base, deferred, err := splitSchemaRestoreSetScript(entries[i].manifest.DDL, entries[i].manifest.Table, targets)
		if err != nil {
			return nil, fmt.Errorf("pg.schema_restore: %w", err)
		}
		entries[i].base = base
		entries[i].deferred = deferred
	}

	conn := connFromArgs(input)
	refs := make([]string, 0, len(entries))
	for _, entry := range entries {
		refs = append(refs, "COALESCE(to_regclass("+quoteLiteral(regclassLiteral(entry.manifest.Table, ""))+")::text, '')")
	}
	probe, err := runPsqlRead(ctx, input.Channel, conn, "SELECT "+strings.Join(refs, " || '|' || "))
	if err != nil {
		return nil, fmt.Errorf("pg.schema_restore: existence check: %w", err)
	}
	if probe.ExitCode != 0 {
		return nil, fmt.Errorf("pg.schema_restore: existence check failed: %s", strings.TrimSpace(probe.Stderr))
	}
	if strings.TrimSpace(probe.Stdout) != strings.Repeat("|", len(entries)-1) {
		return nil, fmt.Errorf("pg.schema_restore: at least one relation in the snapshot set already exists; refusing to overwrite it")
	}

	var script strings.Builder
	script.WriteString("BEGIN;\n")
	for _, entry := range entries {
		script.WriteString(entry.base)
		script.WriteByte('\n')
	}
	for _, entry := range entries {
		for _, statement := range entry.deferred {
			script.WriteString(statement)
			script.WriteString(";\n")
		}
	}
	script.WriteString("COMMIT;\n")
	out, err := runPsql(ctx, input.Channel, conn, script.String())
	if err != nil {
		return nil, fmt.Errorf("pg.schema_restore: apply snapshot set: %w", err)
	}
	if out.ExitCode != 0 {
		return out, fmt.Errorf("pg.schema_restore: apply snapshot set failed: %s", strings.TrimSpace(out.Stderr))
	}
	for _, entry := range entries {
		verify, restoredDDL, err := captureSchemaDDL(ctx, input.Channel, conn, entry.manifest.Table)
		if err != nil {
			return nil, fmt.Errorf("pg.schema_restore: verification failed for %q: %w", entry.manifest.Table, err)
		}
		if restoredDDL != entry.manifest.DDL {
			return nil, fmt.Errorf("pg.schema_restore: verification mismatch for %q", entry.manifest.Table)
		}
		out.Stderr = strings.TrimSpace(out.Stderr + "\n" + verify.Stderr)
	}
	out.Stdout = raw
	out.Changed = true
	return out, nil
}
