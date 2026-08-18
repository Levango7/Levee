package rag

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sync"
	"testing"
)

// newTestStore returns an InMemoryVectorStore with the given dimension,
// failing the test on construction error.
func newTestStore(t *testing.T, dim int) *InMemoryVectorStore {
	t.Helper()
	s, err := NewInMemoryVectorStore(dim)
	if err != nil {
		t.Fatalf("NewInMemoryVectorStore: %v", err)
	}
	return s
}

// TestNewInMemoryVectorStore_InvalidDimension verifies that a non-positive
// dimension is rejected.
func TestNewInMemoryVectorStore_InvalidDimension(t *testing.T) {
	t.Parallel()
	for _, dim := range []int{0, -1} {
		_, err := NewInMemoryVectorStore(dim)
		if !errors.Is(err, ErrInvalidDimension) {
			t.Fatalf("dim=%d: expected ErrInvalidDimension, got %v", dim, err)
		}
	}
}

// TestInMemoryVectorStore_Add covers the Add happy path and its error cases.
func TestInMemoryVectorStore_Add(t *testing.T) {
	t.Parallel()
	const dim = 4
	s := newTestStore(t, dim)
	ctx := context.Background()

	doc := Document{
		ID:        "doc-1",
		Content:   "hello",
		Metadata:  map[string]string{"src": "test"},
		Embedding: []float32{1, 0, 0, 0},
	}
	if err := s.Add(ctx, doc); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if s.Size() != 1 {
		t.Fatalf("Size = %d, want 1", s.Size())
	}

	// Duplicate ID.
	if err := s.Add(ctx, doc); !errors.Is(err, ErrDocumentExists) {
		t.Fatalf("expected ErrDocumentExists, got %v", err)
	}

	// Empty ID.
	if err := s.Add(ctx, Document{ID: "", Embedding: []float32{1, 0, 0, 0}}); !errors.Is(err, ErrEmptyID) {
		t.Fatalf("expected ErrEmptyID, got %v", err)
	}

	// Dimension mismatch.
	if err := s.Add(ctx, Document{ID: "bad", Embedding: []float32{1, 0, 0}}); !errors.Is(err, ErrDimensionMismatch) {
		t.Fatalf("expected ErrDimensionMismatch, got %v", err)
	}

	// Stored document is a deep copy: mutating the input must not affect the
	// store.
	doc.Metadata["src"] = "mutated"
	doc.Embedding[0] = 999
	got, err := s.Search(ctx, []float32{1, 0, 0, 0}, 1)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if got[0].Document.Metadata["src"] != "test" {
		t.Fatalf("metadata not deep-copied: %v", got[0].Document.Metadata)
	}
	if got[0].Document.Embedding[0] != 1 {
		t.Fatalf("embedding not deep-copied: %v", got[0].Document.Embedding)
	}
}

// TestInMemoryVectorStore_AddBatch covers batch insertion and atomicity.
func TestInMemoryVectorStore_AddBatch(t *testing.T) {
	t.Parallel()
	const dim = 2
	s := newTestStore(t, dim)
	ctx := context.Background()

	docs := []Document{
		{ID: "a", Content: "alpha", Embedding: []float32{1, 0}},
		{ID: "b", Content: "beta", Embedding: []float32{0, 1}},
		{ID: "c", Content: "gamma", Embedding: []float32{1, 1}},
	}
	if err := s.AddBatch(ctx, docs); err != nil {
		t.Fatalf("AddBatch: %v", err)
	}
	if s.Size() != 3 {
		t.Fatalf("Size = %d, want 3", s.Size())
	}

	// Duplicate within batch -> atomic failure, no inserts.
	bad := []Document{
		{ID: "x", Embedding: []float32{1, 0}},
		{ID: "x", Embedding: []float32{0, 1}},
	}
	if err := s.AddBatch(ctx, bad); !errors.Is(err, ErrDocumentExists) {
		t.Fatalf("expected ErrDocumentExists, got %v", err)
	}
	// "x" must not have been inserted.
	if _, err := s.Search(ctx, []float32{1, 0}, 10); err != nil {
		t.Fatalf("Search: %v", err)
	}

	// Collision with existing ID -> atomic failure.
	collide := []Document{
		{ID: "y", Embedding: []float32{1, 0}},
		{ID: "a", Embedding: []float32{0, 1}}, // "a" already present
	}
	if err := s.AddBatch(ctx, collide); !errors.Is(err, ErrDocumentExists) {
		t.Fatalf("expected ErrDocumentExists, got %v", err)
	}
	// "y" must not have been inserted.
	if s.Size() != 3 {
		t.Fatalf("Size = %d, want 3 (atomicity broken)", s.Size())
	}

	// Dimension mismatch in batch.
	if err := s.AddBatch(ctx, []Document{{ID: "z", Embedding: []float32{1}}}); !errors.Is(err, ErrDimensionMismatch) {
		t.Fatalf("expected ErrDimensionMismatch, got %v", err)
	}

	// Empty batch is a no-op.
	if err := s.AddBatch(ctx, nil); err != nil {
		t.Fatalf("AddBatch(nil): %v", err)
	}
}

// TestInMemoryVectorStore_Search verifies cosine-similarity ranking and the
// error cases.
func TestInMemoryVectorStore_Search(t *testing.T) {
	t.Parallel()
	const dim = 3
	s := newTestStore(t, dim)
	ctx := context.Background()

	// Three documents with known embeddings.
	docs := []Document{
		{ID: "x-axis", Content: "x", Embedding: []float32{1, 0, 0}},
		{ID: "y-axis", Content: "y", Embedding: []float32{0, 1, 0}},
		{ID: "diag", Content: "diag", Embedding: []float32{1, 1, 0}}, // not normalised
	}
	if err := s.AddBatch(ctx, docs); err != nil {
		t.Fatalf("AddBatch: %v", err)
	}

	// Query along x-axis: x-axis should be top, y-axis should be 0.
	results, err := s.Search(ctx, []float32{1, 0, 0}, 3)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(results) != 3 {
		t.Fatalf("len(results) = %d, want 3", len(results))
	}
	if results[0].Document.ID != "x-axis" {
		t.Fatalf("top result = %s, want x-axis", results[0].Document.ID)
	}
	if math.Abs(results[0].Score-1.0) > 1e-6 {
		t.Fatalf("top score = %f, want 1.0", results[0].Score)
	}

	// Find y-axis result and check score is 0.
	var yScore float64
	for _, r := range results {
		if r.Document.ID == "y-axis" {
			yScore = r.Score
		}
	}
	if math.Abs(yScore) > 1e-6 {
		t.Fatalf("y-axis score = %f, want 0", yScore)
	}

	// topK larger than store size returns all docs.
	all, err := s.Search(ctx, []float32{1, 0, 0}, 100)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(all) != 3 {
		t.Fatalf("len(all) = %d, want 3", len(all))
	}

	// topK = 1 returns only the best.
	one, err := s.Search(ctx, []float32{1, 0, 0}, 1)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(one) != 1 {
		t.Fatalf("len(one) = %d, want 1", len(one))
	}

	// Empty query vector.
	if _, err := s.Search(ctx, nil, 1); !errors.Is(err, ErrEmptyQuery) {
		t.Fatalf("expected ErrEmptyQuery, got %v", err)
	}
	if _, err := s.Search(ctx, []float32{}, 1); !errors.Is(err, ErrEmptyQuery) {
		t.Fatalf("expected ErrEmptyQuery, got %v", err)
	}

	// Invalid topK.
	if _, err := s.Search(ctx, []float32{1, 0, 0}, 0); !errors.Is(err, ErrInvalidTopK) {
		t.Fatalf("expected ErrInvalidTopK, got %v", err)
	}
	if _, err := s.Search(ctx, []float32{1, 0, 0}, -1); !errors.Is(err, ErrInvalidTopK) {
		t.Fatalf("expected ErrInvalidTopK, got %v", err)
	}

	// Empty store returns an empty slice, not nil.
	empty := newTestStore(t, dim)
	got, err := empty.Search(ctx, []float32{1, 0, 0}, 5)
	if err != nil {
		t.Fatalf("Search empty store: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("expected empty slice, got %v", got)
	}
}

// TestInMemoryVectorStore_Delete covers deletion and its error cases.
func TestInMemoryVectorStore_Delete(t *testing.T) {
	t.Parallel()
	const dim = 2
	s := newTestStore(t, dim)
	ctx := context.Background()

	if err := s.Add(ctx, Document{ID: "a", Embedding: []float32{1, 0}}); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if err := s.Add(ctx, Document{ID: "b", Embedding: []float32{0, 1}}); err != nil {
		t.Fatalf("Add: %v", err)
	}

	if err := s.Delete(ctx, "a"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if s.Size() != 1 {
		t.Fatalf("Size = %d, want 1", s.Size())
	}

	// Deleting a missing ID is an error.
	if err := s.Delete(ctx, "missing"); !errors.Is(err, ErrDocumentNotFound) {
		t.Fatalf("expected ErrDocumentNotFound, got %v", err)
	}
	// Deleting the same ID again is still an error.
	if err := s.Delete(ctx, "a"); !errors.Is(err, ErrDocumentNotFound) {
		t.Fatalf("expected ErrDocumentNotFound, got %v", err)
	}
}

// TestInMemoryVectorStore_Concurrent exercises concurrent reads and writes
// to detect data races when run with -race.
func TestInMemoryVectorStore_Concurrent(t *testing.T) {
	t.Parallel()
	const dim = 4
	s := newTestStore(t, dim)
	ctx := context.Background()

	const writers = 8
	const docsPerWriter = 50

	var wg sync.WaitGroup
	wg.Add(writers)
	for w := 0; w < writers; w++ {
		go func(worker int) {
			defer wg.Done()
			for i := 0; i < docsPerWriter; i++ {
				id := fmt.Sprintf("w%d-d%d", worker, i)
				doc := Document{
					ID:        id,
					Content:   id,
					Embedding: []float32{1, 0, 0, 0},
				}
				if err := s.Add(ctx, doc); err != nil {
					t.Errorf("worker %d Add: %v", worker, err)
					return
				}
			}
		}(w)
	}

	// Concurrent searches while writes are happening.
	const searchers = 4
	wg.Add(searchers)
	for r := 0; r < searchers; r++ {
		go func() {
			defer wg.Done()
			for i := 0; i < docsPerWriter; i++ {
				if _, err := s.Search(ctx, []float32{1, 0, 0, 0}, 5); err != nil {
					t.Errorf("Search: %v", err)
					return
				}
			}
		}()
	}

	wg.Wait()

	want := writers * docsPerWriter
	if s.Size() != want {
		t.Fatalf("Size = %d, want %d", s.Size(), want)
	}
}

// TestCosineSimilarity checks the cosine similarity helper directly.
func TestCosineSimilarity(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		a    []float32
		b    []float32
		want float64
	}{
		{"identical", []float32{1, 0, 0}, []float32{1, 0, 0}, 1.0},
		{"opposite", []float32{1, 0, 0}, []float32{-1, 0, 0}, -1.0},
		{"orthogonal", []float32{1, 0, 0}, []float32{0, 1, 0}, 0.0},
		{"45deg", []float32{1, 0}, []float32{1, 1}, 1.0 / math.Sqrt(2)},
		{"scaled", []float32{2, 0, 0}, []float32{5, 0, 0}, 1.0},
		{"zero_a", []float32{0, 0, 0}, []float32{1, 0, 0}, 0.0},
		{"zero_b", []float32{1, 0, 0}, []float32{0, 0, 0}, 0.0},
		{"empty", []float32{}, []float32{}, 0.0},
		{"len_mismatch", []float32{1, 0}, []float32{1, 0, 0}, 0.0},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := cosineSimilarity(tc.a, tc.b)
			if math.Abs(got-tc.want) > 1e-6 {
				t.Fatalf("cosineSimilarity = %f, want %f", got, tc.want)
			}
		})
	}
}