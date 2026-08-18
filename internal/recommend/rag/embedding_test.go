package rag

import (
	"context"
	"errors"
	"math"
	"testing"
)

// TestNewMockEmbeddingProvider_InvalidDimension verifies that a non-positive
// dimension is rejected.
func TestNewMockEmbeddingProvider_InvalidDimension(t *testing.T) {
	t.Parallel()
	for _, dim := range []int{0, -1, -100} {
		_, err := NewMockEmbeddingProvider(dim)
		if !errors.Is(err, ErrInvalidDimension) {
			t.Fatalf("dim=%d: expected ErrInvalidDimension, got %v", dim, err)
		}
	}
}

// TestMockEmbeddingProvider_Embed asserts that the mock is deterministic
// (same text -> same vector), produces the requested dimension, and emits
// a unit-length vector.
func TestMockEmbeddingProvider_Embed(t *testing.T) {
	t.Parallel()
	const dim = 16
	p, err := NewMockEmbeddingProvider(dim)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	ctx := context.Background()
	v1, err := p.Embed(ctx, "java oom in order-service")
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}
	if len(v1) != dim {
		t.Fatalf("len(v1) = %d, want %d", len(v1), dim)
	}

	// Determinism: same text -> identical vector.
	v2, err := p.Embed(ctx, "java oom in order-service")
	if err != nil {
		t.Fatalf("Embed second call: %v", err)
	}
	for i := range v1 {
		if v1[i] != v2[i] {
			t.Fatalf("non-deterministic at index %d: %v vs %v", i, v1[i], v2[i])
		}
	}

	// Different text -> different vector.
	v3, err := p.Embed(ctx, "disk full on log volume")
	if err != nil {
		t.Fatalf("Embed different text: %v", err)
	}
	same := true
	for i := range v1 {
		if v1[i] != v3[i] {
			same = false
			break
		}
	}
	if same {
		t.Fatal("different texts produced identical embeddings")
	}

	// Unit length (L2-normalised).
	var norm float64
	for _, x := range v1 {
		norm += float64(x) * float64(x)
	}
	if math.Abs(math.Sqrt(norm)-1.0) > 1e-5 {
		t.Fatalf("vector not unit length: %f", math.Sqrt(norm))
	}
}

// TestMockEmbeddingProvider_EmptyText verifies that empty text is rejected.
func TestMockEmbeddingProvider_EmptyText(t *testing.T) {
	t.Parallel()
	p, err := NewMockEmbeddingProvider(8)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, err := p.Embed(context.Background(), ""); !errors.Is(err, ErrEmptyText) {
		t.Fatalf("expected ErrEmptyText, got %v", err)
	}
}

// TestMockEmbeddingProvider_EmbedBatch checks that batch embedding preserves
// order and length, and rejects batches containing empty text.
func TestMockEmbeddingProvider_EmbedBatch(t *testing.T) {
	t.Parallel()
	const dim = 8
	p, err := NewMockEmbeddingProvider(dim)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	ctx := context.Background()
	texts := []string{"alpha", "beta", "gamma"}
	vecs, err := p.EmbedBatch(ctx, texts)
	if err != nil {
		t.Fatalf("EmbedBatch: %v", err)
	}
	if len(vecs) != len(texts) {
		t.Fatalf("len(vecs) = %d, want %d", len(vecs), len(texts))
	}
	for i, v := range vecs {
		if len(v) != dim {
			t.Fatalf("vec[%d] len = %d, want %d", i, len(v), dim)
		}
		// Cross-check against single Embed.
		single, err := p.Embed(ctx, texts[i])
		if err != nil {
			t.Fatalf("Embed: %v", err)
		}
		for j := range v {
			if v[j] != single[j] {
				t.Fatalf("batch vs single mismatch at %d:%d", i, j)
			}
		}
	}

	// Empty batch is allowed and returns an empty slice.
	empty, err := p.EmbedBatch(ctx, []string{})
	if err != nil {
		t.Fatalf("EmbedBatch empty: %v", err)
	}
	if len(empty) != 0 {
		t.Fatalf("expected empty slice, got %v", empty)
	}

	// Batch with an empty string is rejected.
	if _, err := p.EmbedBatch(ctx, []string{"ok", ""}); !errors.Is(err, ErrEmptyText) {
		t.Fatalf("expected ErrEmptyText, got %v", err)
	}
}

// TestMockEmbeddingProvider_Dimension checks the Dimension accessor.
func TestMockEmbeddingProvider_Dimension(t *testing.T) {
	t.Parallel()
	for _, dim := range []int{1, 8, 128, 1536} {
		p, err := NewMockEmbeddingProvider(dim)
		if err != nil {
			t.Fatalf("dim=%d: %v", dim, err)
		}
		if p.Dimension() != dim {
			t.Fatalf("Dimension() = %d, want %d", p.Dimension(), dim)
		}
	}
}