package sqlguard

import (
	"testing"

	"github.com/nexus/levee/internal/sqlreview"
)

// The idempotent query surface has one extra restriction beyond the review
// verdict: DROP is rejected even though SR-07 is a warning. Everything else
// uses the same fail-severity boundary as ReviewPostgres. This battery is the
// conformance contract between plan-time review and execution-time rejection.
func TestPostgresPlanAndExecutionConformance(t *testing.T) {
	cases := []struct {
		sql   string
		allow bool
	}{
		{"SELECT id FROM users WHERE id = $1", true},
		{"INSERT INTO users(id) VALUES (1) ON CONFLICT (id) DO NOTHING", true},
		{"CREATE TABLE t (id SERIAL PRIMARY KEY)", true},
		{"CREATE TABLE archive AS SELECT * FROM users", true},
		{"ALTER TABLE users ADD COLUMN note text", true},
		{"DELETE FROM users WHERE id = 7", true},
		{"UPDATE users SET active = true WHERE id = 7", true},
		{"DELETE FROM users", false},
		{"UPDATE users SET active = true", false},
		{"DELETE FROM users WHERE 1 = 1", false},
		{"UPDATE users SET active = true WHERE 1 = 1", false},
		{"DELETE FROM users WHERE id = 1 OR 1 = 1", false},
		{"TRUNCATE TABLE users", false},
		{"DROP TABLE users", false},
		{"DROP TABLE users CASCADE", false},
		{"DROP DATABASE appdb", false},
		{"DROP INDEX idx_users_email", false},
		{"THIS IS NOT SQL ((", false},
	}
	for _, tc := range cases {
		t.Run(tc.sql, func(t *testing.T) {
			res, err := sqlreview.ReviewPostgres(tc.sql)
			if err != nil {
				t.Fatal(err)
			}
			want := res.Verdict != sqlreview.VerdictFail
			for _, statement := range res.Statements {
				if statement.Impact.Operation == "drop" || statement.Impact.Operation == "drop database" || statement.Impact.Operation == "truncate" {
					want = false
				}
			}
			err = RejectDestructive(tc.sql, "pg.query")
			got := err == nil
			if got != want {
				t.Fatalf("plan review allow=%t, execution RejectDestructive=%t (err=%v)", want, got, err)
			}
		})
	}
}
