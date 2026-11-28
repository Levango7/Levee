package pg

// restore.go applies a validated schema snapshot only when the target relation
// is absent. It never DROPs or overwrites an existing object, and it verifies
// the restored definition with a fresh pg_dump before reporting Changed=true.

import (
	"context"
	"fmt"
	"strings"

	"github.com/nexus/levee/internal/executor"
	"github.com/nexus/levee/internal/executor/sqlguard"
)

func (m *Module) schemaRestore(ctx context.Context, input executor.ModuleInput) (*executor.ModuleOutput, error) {
	if rawSet, ok := stringOk(input.Args, "snapshots"); ok && strings.TrimSpace(rawSet) != "" {
		return m.schemaRestoreSet(ctx, input, rawSet)
	}
	tableArg, err := stringArg(input.Args, "table")
	if err != nil {
		return nil, fmt.Errorf("pg.schema_restore: %w", err)
	}
	tables, err := sqlguard.TableList(map[string]any{"tables": []any{tableArg}}, "tables")
	if err != nil || len(tables) != 1 {
		return nil, fmt.Errorf("pg.schema_restore: exactly one valid table is required")
	}
	raw, err := stringArg(input.Args, "snapshot")
	if err != nil {
		return nil, fmt.Errorf("pg.schema_restore: %w", err)
	}
	manifest, err := decodeSchemaSnapshot(raw, tableArg)
	if err != nil {
		return nil, fmt.Errorf("pg.schema_restore: %w", err)
	}
	if err := refuseSnapshotPassword(input.Args, "pg.schema_restore"); err != nil {
		return nil, err
	}
	conn := connFromArgs(input)
	ref := regclassLiteral(tableArg, "")
	probe, err := runPsqlRead(ctx, input.Channel, conn, "SELECT to_regclass("+quoteLiteral(ref)+") IS NULL")
	if err != nil {
		return nil, fmt.Errorf("pg.schema_restore: existence check: %w", err)
	}
	if probe.ExitCode != 0 {
		return nil, fmt.Errorf("pg.schema_restore: existence check failed: %s", strings.TrimSpace(probe.Stderr))
	}
	if strings.TrimSpace(probe.Stdout) != "t" {
		return nil, fmt.Errorf("pg.schema_restore: relation %q already exists; refusing to overwrite it — schema recovery is allowed only when the table is missing", tableArg)
	}

	out, err := runPsql(ctx, input.Channel, conn, manifest.DDL)
	if err != nil {
		return nil, fmt.Errorf("pg.schema_restore: apply snapshot: %w", err)
	}
	if out.ExitCode != 0 {
		return out, fmt.Errorf("pg.schema_restore: apply snapshot failed: %s", strings.TrimSpace(out.Stderr))
	}

	verify, restoredDDL, err := captureSchemaDDL(ctx, input.Channel, conn, tableArg)
	if err != nil {
		return nil, fmt.Errorf("pg.schema_restore: verification failed: %w", err)
	}
	if restoredDDL != manifest.DDL {
		return nil, fmt.Errorf("pg.schema_restore: verification mismatch: restored definition differs from the snapshot")
	}
	out.Stdout = restoredDDL
	out.Stderr = strings.TrimSpace(out.Stderr + "\n" + verify.Stderr)
	out.Changed = true
	return out, nil
}
