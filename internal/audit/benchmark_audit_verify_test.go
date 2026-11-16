// BenchmarkAuditChainVerifySteadyState measures AuditChainBuilder.Verify at
// chain sizes up to 100k rows. Verify walks the whole chain exactly once and
// re-hashes every row — unlike Seal's fix-up branches it is a pure read path,
// so this is the number an auditor or a bid-response appendix quotes.
//
// Same scale caveat as benchmark_audit_seal_test.go: this is a LOCAL measure,
// not a CI gate, and the absolute ceiling depends on the bench host.

package audit

import (
	"context"
	"fmt"
	"testing"
)

func BenchmarkAuditChainVerifySteadyState(b *testing.B) {
	for _, rows := range []int{100, 1000, 5000, 25000, 100000} {
		b.Run(fmt.Sprintf("sealed_rows=%d", rows), func(b *testing.B) {
			store := seedAuditRows(b, rows)
			ctx := context.Background()
			if _, err := newBenchBuilder(b, store).Seal(ctx); err != nil {
				b.Fatalf("seed seal: %v", err)
			}
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				res, err := newBenchBuilder(b, store).Verify(ctx)
				if err != nil {
					b.Fatalf("verify: %v", err)
				}
				if !res.Valid {
					b.Fatalf("expected valid chain, got failures: %v", res.Failures)
				}
			}
		})
	}
}
