// benchmark_audit_seal_test.go quantifies the cost the auditchain.go comment
// describes but never measured: Record creates a row and then Seals the whole
// chain, and Sealing walks every visible row. This benchmark makes that number
// reproducible instead of folklore, so the next person who considers changing
// the seal's shape starts from this output rather than from a guess.
//
// What it is FOR: the design doc docs/audit-chain-scale-design.md records a
// "maintain as is, with a benchmark" decision. A decision made without a number
// is the same failure mode the repo keeps paying for elsewhere (a ledger
// statement nobody re-reads); a decision made with one can be revisited when
// the number stops holding.
//
// Why it is not a CI gate: a benchmark on CI runners measures the runner. The
// shape of the curve is what matters here (linear in row count), and a wall
// clock threshold on shared hardware would either be noise or a sleeping bug.
// Run it locally:
//
//	go test -run XXX -bench BenchmarkAuditRecordSealsWholeChain -benchtime 20x ./internal/audit/
package audit

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/nexus/levee/internal/state"
)

// seedAuditRows inserts n audit rows with strictly increasing timestamps and
// returns the store. Rows are inserted WITHOUT sealing, so the caller decides
// when the chain reaches its steady state — that is the state Record operates
// in on every real write.
func seedAuditRows(b *testing.B, n int) state.Store {
	b.Helper()
	dbPath := filepath.Join(b.TempDir(), "bench_audit.db")
	store, err := state.NewSQLiteStore(context.Background(), dbPath)
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { _ = store.Close() })
	ctx := context.Background()
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for i := 0; i < n; i++ {
		// Distinct second-resolution timestamps with a unique id: the chain
		// order is (timestamp, id), and rows sharing both would be the same
		// row. Millisecond spacing keeps every row strictly ordered.
		if err := store.CreateAudit(ctx, &state.Audit{
			ID:        fmt.Sprintf("seed-%08d", i),
			Action:    "benchmark.seed",
			Result:    state.AuditResultSuccess,
			Actor:     "bench",
			Timestamp: base.Add(time.Duration(i) * time.Millisecond),
		}); err != nil {
			b.Fatal(err)
		}
	}
	return store
}

// newWriteAudit builds the row a measured iteration appends. Its timestamp sits
// far ahead of the seeded rows so it always lands at the tail, which is where a
// real append lands too.
func newWriteAudit(i int) *state.Audit {
	return &state.Audit{
		ID:        fmt.Sprintf("measured-%08d", i),
		Action:    "benchmark.write",
		Result:    state.AuditResultSuccess,
		Actor:     "bench",
		Timestamp: time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC).Add(time.Duration(i) * time.Millisecond),
	}
}

// BenchmarkAuditRecordSealsWholeChain measures audit.Record at three chain
// sizes. The three points exist to show the SHAPE of the cost: if this ever
// stops being roughly linear in rows, the note that says "sealing still walks
// every visible row" is stale and the design doc's decision needs revisiting.
func BenchmarkAuditRecordSealsWholeChain(b *testing.B) {
	for _, rows := range []int{100, 1000, 5000} {
		b.Run(fmt.Sprintf("sealed_rows=%d", rows), func(b *testing.B) {
			store := seedAuditRows(b, rows)
			ctx := context.Background()

			// Put the chain in its steady state: every seeded row sealed. This
			// is the precondition the second Seal inside Record actually runs
			// under, and it is also the worst case for the walk — nothing to
			// write, everything to read and hash.
			bld, err := NewAuditChainBuilder(store)
			if err != nil {
				b.Fatal(err)
			}
			if _, err := bld.Seal(ctx); err != nil {
				b.Fatalf("seed seal: %v", err)
			}

			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				// Record is the shape a caller sees: create + seal. The error is
				// not asserted because a seal failure is a warning by design
				// (record.go), and this benchmark measures the common path.
				if err := Record(ctx, store, newWriteAudit(i)); err != nil {
					b.Fatalf("record: %v", err)
				}
			}
		})
	}
}

// BenchmarkAuditChainSealSteadyState isolates the seal from the insert, so a
// reader can tell "the walk is expensive" from "the insert is expensive". It
// Seals a chain that is already sealed, which is exactly what every Record
// after the first one on a quiet system pays.
func BenchmarkAuditChainSealSteadyState(b *testing.B) {
	for _, rows := range []int{100, 1000, 5000} {
		b.Run(fmt.Sprintf("sealed_rows=%d", rows), func(b *testing.B) {
			store := seedAuditRows(b, rows)
			ctx := context.Background()
			if _, err := newBenchBuilder(b, store).Seal(ctx); err != nil {
				b.Fatalf("seed seal: %v", err)
			}
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				bld := newBenchBuilder(b, store)
				if _, err := bld.Seal(ctx); err != nil {
					b.Fatalf("seal: %v", err)
				}
			}
		})
	}
}

// newBenchBuilder is a benchmark-local helper: the builder constructor
// returns an error only for a nil store, which the benchmarks cannot produce by
// construction, so failing the benchmark is the honest way to handle it.
func newBenchBuilder(b *testing.B, store state.Store) *AuditChainBuilder {
	b.Helper()
	bld, err := NewAuditChainBuilder(store)
	if err != nil {
		b.Fatalf("new audit chain builder: %v", err)
	}
	return bld
}
