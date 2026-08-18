
// embedding.go defines the embedding abstraction used by the RAG pipeline to
// convert text into dense float32 vectors. The interface is intentionally
// minimal so that production adapters (OpenAI, Ollama, local models) can be
// plugged in without dragging their SDKs into the package.
//
// A MockEmbeddingProvider is shipped for unit tests and offline development.
// It uses FNV-1a hashing to derive a deterministic seed from the input text
// and then expands that seed into a fixed-length vector via a linear
// congruential generator. The same text always yields the same vector, which
// makes the mock reproducible and easy to reason about in assertions.
//
// All implementations must be safe for concurrent use.
package rag

import (
	"context"
	"errors"
	"hash/fnv"
	"math"
)

// --- Sentinel errors --------------------------------------------------------

var (
	// ErrInvalidDimension is returned when an embedding provider or vector
	// store is constructed with a non-positive dimension.
	ErrInvalidDimension = errors.New("rag: invalid embedding dimension")
	// ErrEmptyText is returned when an embedding is requested for an empty
	// text string.
	ErrEmptyText = errors.New("rag: empty text")
)

// --- EmbeddingProvider ------------------------------------------------------

// EmbeddingProvider converts text into dense float32 vectors. Implementations
// must be deterministic for the same input (modulo model drift in production
// backends) and safe for concurrent use.
type EmbeddingProvider interface {
	// Embed returns the embedding vector for a single text.
	Embed(ctx context.Context, text string) ([]float32, error)
	// EmbedBatch returns the embedding vectors for a batch of texts. The
	// returned slice has the same length and order as the input.
	EmbedBatch(ctx context.Context, texts []string) ([][]float32, error)
	// Dimension returns the dimensionality of the vectors produced by this
	// provider. It is constant for the lifetime of the provider.
	Dimension() int
}

// --- MockEmbeddingProvider --------------------------------------------------

// MockEmbeddingProvider is a deterministic, dependency-free embedding
// provider intended for tests and offline development. It derives a 64-bit
// seed from the input text using FNV-1a and expands the seed into a
// dimension-length vector via a linear congruential generator (LCG). The
// resulting vector is L2-normalised so that cosine similarity reduces to a
// simple dot product.
//
// The mock is safe for concurrent use: it carries no mutable state.
type MockEmbeddingProvider struct {
	dimension int
}

// NewMockEmbeddingProvider returns a MockEmbeddingProvider that emits vectors
// of the given dimension. The dimension must be positive; otherwise
// ErrInvalidDimension is returned.
func NewMockEmbeddingProvider(dimension int) (*MockEmbeddingProvider, error) {
	if dimension <= 0 {
		return nil, ErrInvalidDimension
	}
	return &MockEmbeddingProvider{dimension: dimension}, nil
}

// Embed returns a deterministic embedding for the given text. Empty text is
// rejected with ErrEmptyText. The context is accepted for interface
// symmetry but is not currently consulted.
func (m *MockEmbeddingProvider) Embed(_ context.Context, text string) ([]float32, error) {
	if text == "" {
		return nil, ErrEmptyText
	}
	return m.embed(text), nil
}

// EmbedBatch embeds each text in the batch. An empty batch returns an empty
// slice without error. If any individual text is empty, the call fails with
// ErrEmptyText and no partial results are returned.
func (m *MockEmbeddingProvider) EmbedBatch(ctx context.Context, texts []string) ([][]float32, error) {
	out := make([][]float32, len(texts))
	for i, t := range texts {
		v, err := m.Embed(ctx, t)
		if err != nil {
			return nil, err
		}
		out[i] = v
	}
	return out, nil
}

// Dimension returns the dimensionality of the vectors produced by this
// provider.
func (m *MockEmbeddingProvider) Dimension() int {
	return m.dimension
}

// embed expands the text into a deterministic, L2-normalised vector. The
// algorithm is:
//
//  1. Hash the text with FNV-1a to obtain a 64-bit seed.
//  2. Use an LCG (Numerical Recipes constants) to generate dimension
//     float32 values in [-1, 1).
//  3. L2-normalise the vector. If the vector is degenerate (all zeros,
//     which is astronomically unlikely), fall back to the first basis
//     vector so callers never receive a zero vector.
func (m *MockEmbeddingProvider) embed(text string) []float32 {
	seed := fnv1aSeed(text)
	vec := make([]float32, m.dimension)

	state := seed
	if state == 0 {
		// LCG needs a non-zero state; FNV-1a never produces 0 for non-empty
		// input but we guard anyway.
		state = 1
	}

	var sum float64
	for i := 0; i < m.dimension; i++ {
		// Numerical Recipes LCG: x_{n+1} = 1664525 * x_n + 1013904223
		state = state*1664525 + 1013904223
		// Map to [-1, 1) using the high 24 bits of the state.
		v := float32(int32(state>>40))/float32(1<<23) - 0.5
		vec[i] = v * 2.0
		sum += float64(v) * float64(v) * 4.0
	}

	// L2-normalise. Guard against the degenerate all-zero vector.
	norm := math.Sqrt(sum)
	if norm < 1e-12 {
		vec[0] = 1.0
		return vec
	}
	inv := float32(1.0 / norm)
	for i := range vec {
		vec[i] *= inv
	}
	return vec
}

// fnv1aSeed computes the FNV-1a 64-bit hash of the text. FNV-1a is chosen
// because it is fast, dependency-free and distributes well over short
// strings, which is the typical input shape for embeddings.
func fnv1aSeed(text string) uint64 {
	h := fnv.New64a()
	h.Write([]byte(text))
	return h.Sum64()
}