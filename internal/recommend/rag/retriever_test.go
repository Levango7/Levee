package rag

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// newTestRetriever builds a Retriever wired to a fresh mock embedder and an
// in-memory store of the given dimension, failing the test on any
// construction error.
func newTestRetriever(t *testing.T, dim, topK int) (*Retriever, *MockEmbeddingProvider, *InMemoryVectorStore) {
	t.Helper()
	emb, err := NewMockEmbeddingProvider(dim)
	if err != nil {
		t.Fatalf("NewMockEmbeddingProvider: %v", err)
	}
	store, err := NewInMemoryVectorStore(dim)
	if err != nil {
		t.Fatalf("NewInMemoryVectorStore: %v", err)
	}
	r, err := NewRetriever(RetrieverConfig{
		Store:    store,
		Embedder: emb,
		TopK:     topK,
	})
	if err != nil {
		t.Fatalf("NewRetriever: %v", err)
	}
	return r, emb, store
}

// TestNewRetriever_NilStore verifies that a nil store is rejected.
func TestNewRetriever_NilStore(t *testing.T) {
	t.Parallel()
	emb, err := NewMockEmbeddingProvider(8)
	if err != nil {
		t.Fatalf("NewMockEmbeddingProvider: %v", err)
	}
	_, err = NewRetriever(RetrieverConfig{Store: nil, Embedder: emb})
	if !errors.Is(err, ErrNilStore) {
		t.Fatalf("expected ErrNilStore, got %v", err)
	}
}

// TestNewRetriever_NilEmbedder verifies that a nil embedder is rejected.
func TestNewRetriever_NilEmbedder(t *testing.T) {
	t.Parallel()
	store, err := NewInMemoryVectorStore(8)
	if err != nil {
		t.Fatalf("NewInMemoryVectorStore: %v", err)
	}
	_, err = NewRetriever(RetrieverConfig{Store: store, Embedder: nil})
	if !errors.Is(err, ErrNilEmbedder) {
		t.Fatalf("expected ErrNilEmbedder, got %v", err)
	}
}

// TestNewRetriever_Defaults checks that TopK defaults to DefaultTopK and that
// a nil logger is replaced with slog.Default().
func TestNewRetriever_Defaults(t *testing.T) {
	t.Parallel()
	r, _, _ := newTestRetriever(t, 4, 0)
	if r.topK != DefaultTopK {
		t.Fatalf("topK = %d, want %d", r.topK, DefaultTopK)
	}
	if r.log == nil {
		t.Fatal("logger not defaulted")
	}
}

// TestRetriever_AddDocument covers AddDocument happy path and error cases.
func TestRetriever_AddDocument(t *testing.T) {
	t.Parallel()
	r, _, store := newTestRetriever(t, 8, 5)
	ctx := context.Background()

	if err := r.AddDocument(ctx, "d1", "java oom", map[string]string{"k": "v"}); err != nil {
		t.Fatalf("AddDocument: %v", err)
	}
	if store.Size() != 1 {
		t.Fatalf("Size = %d, want 1", store.Size())
	}

	// Empty ID.
	if err := r.AddDocument(ctx, "", "x", nil); !errors.Is(err, ErrEmptyID) {
		t.Fatalf("expected ErrEmptyID, got %v", err)
	}
	// Empty content.
	if err := r.AddDocument(ctx, "d2", "", nil); !errors.Is(err, ErrEmptyText) {
		t.Fatalf("expected ErrEmptyText, got %v", err)
	}
	// Duplicate ID surfaces ErrDocumentExists wrapped by the retriever.
	if err := r.AddDocument(ctx, "d1", "dup", nil); !errors.Is(err, ErrDocumentExists) {
		t.Fatalf("expected ErrDocumentExists, got %v", err)
	}
}

// TestRetriever_AddDocuments covers batch insertion through the retriever.
func TestRetriever_AddDocuments(t *testing.T) {
	t.Parallel()
	r, _, store := newTestRetriever(t, 8, 5)
	ctx := context.Background()

	docs := []Document{
		{ID: "a", Content: "alpha"},
		{ID: "b", Content: "beta"},
	}
	if err := r.AddDocuments(ctx, docs); err != nil {
		t.Fatalf("AddDocuments: %v", err)
	}
	if store.Size() != 2 {
		t.Fatalf("Size = %d, want 2", store.Size())
	}

	// Empty batch is a no-op.
	if err := r.AddDocuments(ctx, nil); err != nil {
		t.Fatalf("AddDocuments(nil): %v", err)
	}
	if store.Size() != 2 {
		t.Fatalf("Size = %d, want 2", store.Size())
	}

	// Batch with empty ID is rejected.
	if err := r.AddDocuments(ctx, []Document{{ID: "", Content: "x"}}); !errors.Is(err, ErrEmptyID) {
		t.Fatalf("expected ErrEmptyID, got %v", err)
	}
	// Batch with empty content is rejected.
	if err := r.AddDocuments(ctx, []Document{{ID: "c", Content: ""}}); !errors.Is(err, ErrEmptyText) {
		t.Fatalf("expected ErrEmptyText, got %v", err)
	}
}

// TestRetriever_Retrieve verifies that Retrieve returns documents ranked by
// cosine similarity to the embedded query.
func TestRetriever_Retrieve(t *testing.T) {
	t.Parallel()
	r, _, _ := newTestRetriever(t, 16, 5)
	ctx := context.Background()

	corpus := []struct {
		id, content string
	}{
		{"java-oom", "java lang out of memory error heap space"},
		{"disk-full", "no space left on device disk usage above 90 percent"},
		{"db-pool", "database connection pool exhausted hikari"},
		{"net-part", "network partition timeout unreachable host"},
	}
	for _, d := range corpus {
		if err := r.AddDocument(ctx, d.id, d.content, nil); err != nil {
			t.Fatalf("AddDocument %s: %v", d.id, err)
		}
	}

	// Query close to the java-oom document.
	results, err := r.Retrieve(ctx, "java oom heap memory")
	if err != nil {
		t.Fatalf("Retrieve: %v", err)
	}
	if len(results) == 0 {
		t.Fatal("expected non-empty results")
	}
	// The exact top hit is not asserted (the mock embedding is a hash), but
	// all scores must be in [-1, 1] and sorted descending.
	for i, res := range results {
		if res.Score < -1.0001 || res.Score > 1.0001 {
			t.Fatalf("result %d score out of range: %f", i, res.Score)
		}
		if i > 0 && results[i-1].Score < res.Score {
			t.Fatalf("results not sorted descending at %d: %f < %f", i, results[i-1].Score, res.Score)
		}
	}

	// topK bound is respected.
	if len(results) > 5 {
		t.Fatalf("len(results) = %d, want <= 5", len(results))
	}

	// Empty query is rejected.
	if _, err := r.Retrieve(ctx, ""); !errors.Is(err, ErrEmptyQuery) {
		t.Fatalf("expected ErrEmptyQuery, got %v", err)
	}
}

// TestRetriever_Retrieve_TopK verifies that the configured topK is honoured.
func TestRetriever_Retrieve_TopK(t *testing.T) {
	t.Parallel()
	r, _, _ := newTestRetriever(t, 8, 2)
	ctx := context.Background()

	for i := 0; i < 5; i++ {
		id := string(rune('a' + i))
		if err := r.AddDocument(ctx, id, id+" content", nil); err != nil {
			t.Fatalf("AddDocument: %v", err)
		}
	}
	results, err := r.Retrieve(ctx, "query")
	if err != nil {
		t.Fatalf("Retrieve: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("len(results) = %d, want 2", len(results))
	}
}

// TestRetriever_AugmentPrompt checks the prompt format and that an empty
// result leaves the base prompt untouched.
func TestRetriever_AugmentPrompt(t *testing.T) {
	t.Parallel()
	r, _, _ := newTestRetriever(t, 16, 5)
	ctx := context.Background()

	// Empty store: base prompt returned unchanged.
	out, results, err := r.AugmentPrompt(ctx, "anything", "base prompt")
	if err != nil {
		t.Fatalf("AugmentPrompt: %v", err)
	}
	if out != "base prompt" {
		t.Fatalf("expected base prompt unchanged, got %q", out)
	}
	if len(results) != 0 {
		t.Fatalf("expected empty results, got %d", len(results))
	}

	// Add documents and augment.
	if err := r.AddDocument(ctx, "d1", "fix java oom by restarting jvm", nil); err != nil {
		t.Fatalf("AddDocument: %v", err)
	}
	if err := r.AddDocument(ctx, "d2", "clean disk by removing old logs", nil); err != nil {
		t.Fatalf("AddDocument: %v", err)
	}

	out, results, err = r.AugmentPrompt(ctx, "java oom", "You are a helpful SRE.")
	if err != nil {
		t.Fatalf("AugmentPrompt: %v", err)
	}
	if !strings.HasPrefix(out, "You are a helpful SRE.\n") {
		t.Fatalf("output does not start with base prompt: %q", out)
	}
	if !strings.Contains(out, "--- Relevant Knowledge ---") {
		t.Fatalf("missing knowledge header: %q", out)
	}
	for i, res := range results {
		marker := strings.NewReader(formatMarker(i + 1))
		_ = marker
		expected := formatMarker(i + 1)
		if !strings.Contains(out, expected) {
			t.Fatalf("output missing marker %q: %q", expected, out)
		}
		// The content should appear in the prompt.
		if !strings.Contains(out, res.Document.Content) {
			t.Fatalf("output missing content %q: %q", res.Document.Content, out)
		}
	}

	// Empty query is rejected.
	if _, _, err := r.AugmentPrompt(ctx, "", "base"); !errors.Is(err, ErrEmptyQuery) {
		t.Fatalf("expected ErrEmptyQuery, got %v", err)
	}

	// Empty base prompt: output starts with the knowledge header directly.
	out2, _, err := r.AugmentPrompt(ctx, "java oom", "")
	if err != nil {
		t.Fatalf("AugmentPrompt empty base: %v", err)
	}
	if !strings.HasPrefix(out2, "\n--- Relevant Knowledge ---\n") {
		t.Fatalf("empty base prompt output unexpected: %q", out2)
	}
}

// formatMarker returns the "[n] " marker for a 1-indexed result so the test
// can assert that each hit is rendered.
func formatMarker(i int) string {
	return "[" + itoa(i) + "] "
}

// itoa is a tiny dependency-free int -> string converter to avoid pulling
// strconv into the test helpers.
func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	neg := i < 0
	if neg {
		i = -i
	}
	var buf [20]byte
	pos := len(buf)
	for i > 0 {
		pos--
		buf[pos] = byte('0' + i%10)
		i /= 10
	}
	if neg {
		pos--
		buf[pos] = '-'
	}
	return string(buf[pos:])
}