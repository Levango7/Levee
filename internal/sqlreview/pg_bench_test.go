package sqlreview

import "testing"

const pgBenchSQL = "UPDATE users SET active = true WHERE id = 7 RETURNING id"

func BenchmarkReviewPostgres(b *testing.B) {
	for i := 0; i < b.N; i++ {
		if _, err := ReviewPostgres(pgBenchSQL); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkReviewMySQLForComparison(b *testing.B) {
	for i := 0; i < b.N; i++ {
		if _, err := Review("UPDATE users SET active = true WHERE id = 7"); err != nil {
			b.Fatal(err)
		}
	}
}
