// priority_migration_test.go — the v9 backfill rule, checked against the vocabulary
// rather than against a copy of the SQL.
//
// The step copies a pre-split value out of runs.approval_level into runs.priority
// ONLY where the value's provenance is unambiguous. Which spellings qualify is not a
// matter of opinion and must not be a hand-copied list: it is "every priority that is
// not also a tier". "high" is exactly the one spelling both vocabularies use, so a
// pre-v9 row holding it cannot be attributed — guessing there would invent a priority
// (or a tier) the engine never recorded, which is the failure this whole change exists
// to stop repeating.
//
// So the expected set is derived from state.PriorityNames() ∩ dsl.ApprovalLevels, and
// the literal list inside the migration statement is parsed back out and compared in
// both directions. Editing one side without the other fails here.
package state

import (
	"context"
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nexus/levee/internal/dsl"
)

// v9Stmts returns the statements of the version-9 step, failing loudly if the step is
// gone (a renamed or dropped step would otherwise turn this test into a no-op).
func v9Stmts(t *testing.T, steps []migrationStep) []string {
	t.Helper()
	for _, step := range steps {
		if step.version == 9 {
			require.NotEmpty(t, step.stmts, "the version-9 step has no statements")
			return step.stmts
		}
	}
	t.Fatalf("no version-9 step in the ladder: the priority split is gone")
	return nil
}

// expectedBackfillPriorities is the unambiguous set, computed from the two owners.
func expectedBackfillPriorities(t *testing.T) []string {
	t.Helper()
	out := []string{}
	for _, p := range PriorityNames() {
		if !dsl.IsApprovalLevel(p) {
			out = append(out, p)
		}
	}
	require.NotEmpty(t, out, "every priority is also a tier — the backfill could attribute nothing")
	for _, p := range out {
		assert.NotEqual(t, PriorityHigh, p,
			`"high" must stay out of the moved set: it is the shared spelling`)
	}
	return out
}

func TestV9BackfillLiteralListMatchesTheVocabulary(t *testing.T) {
	stmts := v9Stmts(t, migrations)
	want := expectedBackfillPriorities(t)

	var update string
	for _, s := range stmts {
		if regexp.MustCompile(`(?i)^UPDATE runs SET priority`).MatchString(s) {
			update = s
		}
	}
	require.NotEmpty(t, update, "the v9 step has no backfill UPDATE — historical rows would be left unreadable")

	in := regexp.MustCompile(`'([a-z_]+)'`).FindAllStringSubmatch(update, -1)
	got := []string{}
	for _, m := range in {
		got = append(got, m[1])
	}
	require.NotEmpty(t, got, "could not parse the moved-value list out of the backfill statement")

	for _, w := range want {
		assert.Contains(t, got, w, "the vocabulary says %q is attributable but the migration does not move it", w)
	}
	for _, g := range got {
		assert.Contains(t, want, g,
			"the migration moves %q, which is either a tier or the shared spelling — neither can be attributed to the create path", g)
	}
	assert.Equal(t, len(want), len(got), "the moved set and the vocabulary-derived set differ in size")

	// The PG ladder must agree with the SQLite one; two ladders with two rules is how
	// an engine starts disagreeing with its twin about what history means.
	pg := v9Stmts(t, pgMigrations)
	var pgUpdate string
	for _, s := range pg {
		if regexp.MustCompile(`(?i)^UPDATE runs SET priority`).MatchString(s) {
			pgUpdate = s
		}
	}
	require.NotEmpty(t, pgUpdate, "the PostgreSQL v9 step has no backfill UPDATE")
	assert.Equal(t, update, pgUpdate, "the two engines backfill differently")
}

func TestV9BackfillMovesOnlyUnambiguousValues(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)
	db := store.DB()

	// Simulate a database that stopped at v8: the column does not exist yet and the
	// ledger says 8, so Migrate applies exactly one forward step. Every column a
	// LATER step appends has to go too — a v8 table predates them all, and leaving
	// them in place would make the replay trip over its own ADD COLUMN.
	_, err := db.ExecContext(ctx, `ALTER TABLE runs DROP COLUMN priority`)
	require.NoError(t, err)
	for _, col := range []string{"label", "team"} {
		// SQLite has no DROP COLUMN IF EXISTS; the columns are guaranteed absent
		// by the simulation above.
		_, err = db.ExecContext(ctx, `ALTER TABLE runs DROP COLUMN `+col)
		require.NoError(t, err, "dropping %s", col)
	}
	_, err = db.ExecContext(ctx, `DELETE FROM schema_version`)
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, `INSERT INTO schema_version (version) VALUES (8)`)
	require.NoError(t, err)

	seed := []struct {
		id     string
		level  string
		expect string // the priority value the row must carry after the step
	}{
		{"r-low", "low", "low"},
		{"r-normal", "normal", "normal"},
		{"r-urgent", "urgent", "urgent"},
		{"r-high", "high", ""},         // shared spelling: unattributable, must stay put
		{"r-standard", "standard", ""}, // a tier: never a priority
		{"r-emergency", "emergency", ""},
		{"r-empty", "", ""},
	}
	for _, s := range seed {
		_, err := db.ExecContext(ctx,
			`INSERT INTO runs (id, workflow_name, template_name, params, plan_hash, status,
				approval_status, approval_level, created_at, updated_at, creator, incident_id, tenant_id)
				VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			s.id, "wf", "tpl", "{}", "h", "draft", "pending", s.level,
			"2026-01-01 00:00:00", "2026-01-01 00:00:00", "op", "prod", "")
		require.NoError(t, err, "seeding %s", s.id)
	}

	require.NoError(t, Migrate(ctx, db))

	var version int
	require.NoError(t, db.QueryRowContext(ctx, `SELECT MAX(version) FROM schema_version`).Scan(&version))
	require.Equal(t, currentSchemaVersion, version, "the ladder must land on the current version")

	for _, s := range seed {
		var gotPriority, gotLevel string
		require.NoError(t, db.QueryRowContext(ctx,
			`SELECT priority, approval_level FROM runs WHERE id = ?`, s.id).Scan(&gotPriority, &gotLevel))
		assert.Equal(t, s.expect, gotPriority, "%s (approval_level=%q): wrong priority after the backfill", s.id, s.level)
		assert.Equal(t, s.level, gotLevel,
			"%s: the step must not rewrite the tier column — clearing it would change what the auto-approval gate sees for historical rows", s.id)
	}
}

// runsDeclaredColumns parses the runs CREATE TABLE block of one schema file and returns
// its column names in declaration order. Table-level constraints and comment lines are
// skipped, and the leading-comma style the PostgreSQL file uses for appended columns is
// stripped so both files parse the same way.
func runsDeclaredColumns(t *testing.T, rel string) []string {
	t.Helper()
	raw, err := os.ReadFile(schemaRepoFile(t, rel))
	require.NoError(t, err)
	block := regexp.MustCompile(`(?ms)CREATE TABLE IF NOT EXISTS runs \(.*?\n\);`).FindString(string(raw))
	require.NotEmpty(t, block, "could not find the runs CREATE TABLE block in %s", rel)

	skip := regexp.MustCompile(`(?i)^(CREATE TABLE|\)|--|PRIMARY KEY|UNIQUE|CHECK|FOREIGN KEY|CONSTRAINT)`)
	identifier := regexp.MustCompile(`^[a-z_]+$`)
	var out []string
	for _, line := range strings.Split(block, "\n") {
		trimmed := strings.TrimPrefix(strings.TrimSpace(line), ",")
		trimmed = strings.TrimSpace(trimmed)
		if trimmed == "" || skip.MatchString(trimmed) {
			continue
		}
		field := strings.Fields(trimmed)[0]
		if !identifier.MatchString(field) {
			continue
		}
		out = append(out, field)
	}
	require.NotEmpty(t, out, "parsed no columns out of the runs block in %s — the DDL shape changed", rel)
	return out
}

// TestV9PriorityIsDeclaredLastInBothSchemas turns the ordering claim written in the two
// schema comments into something the suite checks.
//
// Why it needs its own assertion: the comparison those comments point at —
// TestMigrate_FreshAndUpgraded_SchemaShapesMatch — builds SQLite stores. It is what makes
// "declare the new column last" load-bearing on that side, and there is no PostgreSQL
// equivalent for runs, even though ALTER TABLE ADD COLUMN appends there too. Left as a
// comment, the claim would cite coverage the suite does not provide. So both engines are
// checked the same way here, and against each other: two ladders whose column order
// drifted apart is exactly the disagreement this change exists to prevent.
func TestV9PriorityIsDeclaredLastInBothSchemas(t *testing.T) {
	const sqliteRel = "internal/state/schema.sql"
	const pgRel = "internal/state/pgschema.sql"

	sqliteCols := runsDeclaredColumns(t, sqliteRel)
	pgCols := runsDeclaredColumns(t, pgRel)

	assert.Equal(t, sqliteCols, pgCols,
		"the runs column order differs between the two schemas; the two ladders must mean the same shape")

	// The rule is stated the way it was always meant rather than as a list that
	// every new column would have to update: the columns each ladder APPENDS
	// (ALTER TABLE ADD COLUMN, in version order) are exactly the tail of the
	// declaration, in that same order. It began as "runs.priority must be last";
	// v11 appends label and team, and priority is no longer the newest, so a
	// hand-written tail would have silently under-pinned the claim.
	for _, tc := range []struct {
		rel   string
		cols  []string
		steps []migrationStep
	}{
		{sqliteRel, sqliteCols, migrations},
		{pgRel, pgCols, pgMigrations},
	} {
		want := appendedRunsColumns(t, tc.steps)
		require.NotEmpty(t, want, "%s: no ladder step appends a runs column", tc.rel)
		got := tc.cols[len(tc.cols)-len(want):]
		assert.Equal(t, want, got,
			"%s: the ladder-appended runs columns must be the last declared ones, in append order — "+
				"ADD COLUMN appends, so any other placement makes a fresh database differ from an upgraded one", tc.rel)

		declared := map[string]int{}
		for _, c := range tc.cols {
			declared[c]++
		}
		for _, name := range want {
			assert.Equal(t, 1, declared[name], "%s: runs.%s is declared %d times", tc.rel, name, declared[name])
		}
	}

	// The declaration only mirrors a step if that step really is an append of
	// that column — including the original priority split this test was written
	// for.
	for _, tc := range []struct {
		rel   string
		steps []migrationStep
	}{{sqliteRel, migrations}, {pgRel, pgMigrations}} {
		add := ""
		for _, s := range v9Stmts(t, tc.steps) {
			if regexp.MustCompile(`(?i)^ALTER TABLE runs ADD COLUMN`).MatchString(s) {
				add = s
			}
		}
		require.NotEmpty(t, add, "%s: the v9 step adds no runs column", tc.rel)
		assert.Contains(t, add, "priority", "%s: the v9 step appends something other than priority", tc.rel)
	}
}

// appendedRunsColumns lists, in ladder order, every runs column a step adds.
func appendedRunsColumns(t *testing.T, steps []migrationStep) []string {
	t.Helper()
	var out []string
	// The PostgreSQL ladder writes `ADD COLUMN IF NOT EXISTS` (its replay test
	// drives the ladder over a pgschema.sql fixture that already has the column),
	// so the optional clause is part of the grammar, not noise.
	re := regexp.MustCompile(`(?i)^ALTER TABLE runs ADD COLUMN\s+(?:IF NOT EXISTS\s+)?(\w+)`)
	for _, step := range steps {
		for _, stmt := range step.stmts {
			if m := re.FindStringSubmatch(stmt); m != nil {
				out = append(out, strings.ToLower(m[1]))
			}
		}
	}
	return out
}
