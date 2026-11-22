package sqlguard

import (
	"testing"

	"github.com/nexus/levee/internal/sqlreview"
)

// MySQL uses the same fail-severity boundary as Review, with the extra
// idempotent-surface restriction that DROP is rejected even though SR-07 is
// only a warning. This is the MySQL counterpart of the PG conformance battery.
func TestMySQLPlanAndExecutionConformance(t *testing.T) {
	cases := []struct {
		sql   string
		allow bool
	}{
		{"SELECT id FROM users WHERE id = 1", true},
		{"INSERT INTO users(id) VALUES (1)", true},
		{"CREATE TABLE t (id INT PRIMARY KEY)", true},
		{"CREATE TABLE archive AS SELECT * FROM users", true},
		{"ALTER TABLE users ADD COLUMN note INT", true},
		{"DELETE FROM users WHERE id = 7", true},
		{"UPDATE users SET active = 1 WHERE id = 7", true},
		{"DELETE FROM users", false},
		{"UPDATE users SET active = 1", false},
		{"DELETE FROM users WHERE 1 = 1", false},
		{"UPDATE users SET active = 1 WHERE 1 = 1", false},
		{"DELETE FROM users WHERE id = 1 OR 1 = 1", false},
		{"TRUNCATE TABLE users", false},
		{"DROP TABLE users", false},
		{"DROP DATABASE db1", false},
		{"THIS IS NOT SQL ((", false},
	}
	for _, tc := range cases {
		t.Run(tc.sql, func(t *testing.T) {
			res, err := sqlreview.Review(tc.sql)
			if err != nil {
				t.Fatal(err)
			}
			want := res.Verdict != sqlreview.VerdictFail
			for _, statement := range res.Statements {
				if statement.Impact.Operation == "drop" || statement.Impact.Operation == "drop database" || statement.Impact.Operation == "truncate" {
					want = false
				}
			}
			err = RejectDestructive(tc.sql, "mysql.query")
			got := err == nil
			if got != want {
				t.Fatalf("plan review allow=%t, execution RejectDestructive=%t (err=%v)", want, got, err)
			}
		})
	}
}
