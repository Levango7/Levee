
// vectorstore.go defines the vector store abstraction used by the RAG
// pipeline to persist and retrieve knowledge-base documents together with
// their embedding vectors.
//
// An InMemoryVectorStore is provided for tests, single-node deployments and
// as a reference implementation. It performs a brute-force cosine-similarity
// scan over all stored vectors, which is O(N * D) per query — fine for the
// few-thousand-document catalogues typical of LEVEE's recommendation
// knowledge base but not suitable for web-scale retrieval.
//
// The store is safe for concurrent use: all reads and writes are guarded by
// a sync.RWMutex. No method ever panics; dimension mismatches and missing
// documents are reported through error returns.
package rag

import (
	"context"
	"errors"
	"math"
	"sort"
	"sync"
)

// --- Sentinel errors --------------------------------------------------------

var (
	// ErrDimensionMismatch is returned when a document's embedding length
	// does not match the store's configured dimension.
	ErrDimensionMismatch = errors.New("rag: embedding dimension mismatch")
	// ErrEmptyID is returned when a document with an empty ID is added.
	ErrEmptyID = errors.New("rag: empty document id")
	// ErrDocumentExists is returned when Add is called with an ID that is
	// already present in the store.
	ErrDocumentExists = errors.New("rag: document already exists")
	// ErrDocumentNotFound is returned when Delete or a lookup is performed
	// for an ID that is not in the store.
	ErrDocumentNotFound = errors.New("rag: document not found")
	// ErrEmptyQuery is returned when Search is called with a nil or empty
	// query vector.
	ErrEmptyQuery = errors.New("rag: empty query vector")
	// ErrInvalidTopK is returned when Search is called with a non-positive
	// topK.
	ErrInvalidTopK = errors.New("rag: invalid topK")
)

// --- Document ---------------------------------------------------------------

// Document is a single knowledge-base entry together with its precomputed
// embedding. The Metadata map carries arbitrary string tags (source, title,
// severity, ...) that callers can use for filtering or display.
type Document struct {
	ID        string
	Content   string
	Metadata  map[string]string
	Embedding []float32
}

// --- SearchResult -----------------------------------------------------------

// SearchResult is a single hit returned by VectorStore.Search. Score is the
// cosine similarity between the query vector and the document embedding,
// in the range [-1, 1]; higher is better.
type SearchResult struct {
	Document Document
	Score    float64
}

// --- VectorStore ------------------------------------------------------------

// VectorStore is the persistence and retrieval abstraction over documents
// and their embeddings. All methods must be safe for concurrent use.
type VectorStore interface {
	// Add inserts a single document. It returns ErrDocumentExists if the ID
	// is already present.
	Add(ctx context.Context, doc Document) error
	// AddBatch inserts multiple documents atomically. If any document fails
	// validation, no documents are inserted.
	AddBatch(ctx context.Context, docs []Document) error
	// Search returns the topK documents whose embeddings are most similar
	// to the query vector under cosine similarity, sorted by descending
	// score.
	Search(ctx context.Context, query []float32, topK int) ([]SearchResult, error)
	// Delete removes the document with the given ID. It returns
	// ErrDocumentNotFound if the ID is absent.
	Delete(ctx context.Context, id string) error
	// Size returns the number of documents currently stored.
	Size() int
}

// --- InMemoryVectorStore ----------------------------------------------------

// InMemoryVectorStore is an in-memory implementation of VectorStore. It is
// safe for concurrent use. Documents are keyed by ID; embeddings are stored
// as-is and compared with cosine similarity.
type InMemoryVectorStore struct {
	mu        sync.RWMutex
	docs      map[string]Document
	dimension int
}

// NewInMemoryVectorStore returns a new InMemoryVectorStore configured to
// accept embeddings of the given dimension. The dimension must be positive.
func NewInMemoryVectorStore(dimension int) (*InMemoryVectorStore, error) {
	if dimension <= 0 {
		return nil, ErrInvalidDimension
	}
	return &InMemoryVectorStore{
		docs:      make(map[string]Document),
		dimension: dimension,
	}, nil
}

// Add inserts a single document. The embedding length must match the
// store's dimension; the ID must be non-empty and not already present.
func (s *InMemoryVectorStore) Add(_ context.Context, doc Document) error {
	if doc.ID == "" {
		return ErrEmptyID
	}
	if len(doc.Embedding) != s.dimension {
		return ErrDimensionMismatch
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.docs[doc.ID]; ok {
		return ErrDocumentExists
	}
	s.docs[doc.ID] = cloneDocument(doc)
	return nil
}

// AddBatch inserts multiple documents. If any document fails validation or
// collides with an existing ID (or another ID in the same batch), the call
// fails and no documents are inserted.
func (s *InMemoryVectorStore) AddBatch(ctx context.Context, docs []Document) error {
	// Validate the entire batch first so the insert is atomic.
	seen := make(map[string]struct{}, len(docs))
	for i := range docs {
		d := docs[i]
		if d.ID == "" {
			return ErrEmptyID
		}
		if len(d.Embedding) != s.dimension {
			return ErrDimensionMismatch
		}
		if _, ok := seen[d.ID]; ok {
			return ErrDocumentExists
		}
		seen[d.ID] = struct{}{}
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	// Re-check against the store to preserve atomicity.
	for id := range seen {
		if _, ok := s.docs[id]; ok {
			return ErrDocumentExists
		}
	}
	for i := range docs {
		s.docs[docs[i].ID] = cloneDocument(docs[i])
	}
	return nil
}

// Search returns the topK most similar documents to the query vector under
// cosine similarity. Results are sorted by descending score. If the store
// contains fewer than topK documents, all documents are returned.
func (s *InMemoryVectorStore) Search(_ context.Context, query []float32, topK int) ([]SearchResult, error) {
	if len(query) == 0 {
		return nil, ErrEmptyQuery
	}
	if topK <= 0 {
		return nil, ErrInvalidTopK
	}

	s.mu.RLock()
	defer s.mu.RUnlock()

	if len(s.docs) == 0 {
		return []SearchResult{}, nil
	}

	results := make([]SearchResult, 0, len(s.docs))
	for _, doc := range s.docs {
		score := cosineSimilarity(query, doc.Embedding)
		results = append(results, SearchResult{Document: doc, Score: score})
	}

	sort.Slice(results, func(i, j int) bool {
		return results[i].Score > results[j].Score
	})

	if topK > len(results) {
		topK = len(results)
	}
	return results[:topK], nil
}

// Delete removes the document with the given ID. It is a no-op error if the
// ID is absent.
func (s *InMemoryVectorStore) Delete(_ context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.docs[id]; !ok {
		return ErrDocumentNotFound
	}
	delete(s.docs, id)
	return nil
}

// Size returns the number of documents currently stored.
func (s *InMemoryVectorStore) Size() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.docs)
}

// --- Helpers ----------------------------------------------------------------

// cloneDocument returns a deep copy of the document so that later mutations
// by the caller cannot affect the stored copy. The embedding slice and the
// metadata map are copied; the metadata map is nil-safe.
func cloneDocument(d Document) Document {
	emb := make([]float32, len(d.Embedding))
	copy(emb, d.Embedding)

	var meta map[string]string
	if d.Metadata != nil {
		meta = make(map[string]string, len(d.Metadata))
		for k, v := range d.Metadata {
			meta[k] = v
		}
	}
	return Document{
		ID:        d.ID,
		Content:   d.Content,
		Metadata:  meta,
		Embedding: emb,
	}
}

// cosineSimilarity computes the cosine of the angle between two vectors.
// If either vector is zero, the result is 0 (rather than NaN) so that
// degenerate embeddings do not poison the ranking. The result is in the
// range [-1, 1].
func cosineSimilarity(a, b []float32) float64 {
	if len(a) != len(b) || len(a) == 0 {
		return 0
	}
	var dot, na, nb float64
	for i := range a {
		ai := float64(a[i])
		bi := float64(b[i])
		dot += ai * bi
		na += ai * ai
		nb += bi * bi
	}
	if na < 1e-12 || nb < 1e-12 {
		return 0
	}
	return dot / (math.Sqrt(na) * math.Sqrt(nb))
}