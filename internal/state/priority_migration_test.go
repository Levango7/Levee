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
	"regexp"
	"testing"

	"github.com/nexus/levee/internal/dsl"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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
	// ledger says 8, so Migrate applies exactly one forward step.
	_, err := db.ExecContext(ctx, `ALTER TABLE runs DROP COLUMN priority`)
	require.NoError(t, err)
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
