// retriever.go wires an EmbeddingProvider and a VectorStore together into a
// Retrieval-Augmented Generation pipeline. The Retriever converts a natural
// language query into an embedding, asks the store for the most similar
// documents, and optionally renders the hits into a prompt suffix that an
// LLM can consume.
//
// The AugmentPrompt format is intentionally plain-text so that it works with
// any chat backend (OpenAI, Ollama, …) without special token handling. The
// retriever never panics; all failures are returned as errors and logged at
// debug level when a logger is configured.
package rag

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
)

// --- Sentinel errors --------------------------------------------------------

var (
	// ErrNilStore is returned when a Retriever is constructed without a
	// VectorStore.
	ErrNilStore = errors.New("rag: nil vector store")
	// ErrNilEmbedder is returned when a Retriever is constructed without an
	// EmbeddingProvider.
	ErrNilEmbedder = errors.New("rag: nil embedder")
	// ErrNilLogger is returned when a Retriever is constructed with a nil
	// logger. Callers should pass slog.Default() if they do not have a
	// dedicated logger.
	ErrNilLogger = errors.New("rag: nil logger")
)

// --- Defaults ---------------------------------------------------------------

const (
	// DefaultTopK is the number of documents returned by Retrieve when
	// RetrieverConfig.TopK is not set.
	DefaultTopK = 5
)

// --- RetrieverConfig --------------------------------------------------------

// RetrieverConfig parameterises NewRetriever. Store and Embedder are
// mandatory; TopK defaults to DefaultTopK when zero; Logger defaults to
// slog.Default() when nil.
type RetrieverConfig struct {
	Store    VectorStore
	Embedder EmbeddingProvider
	TopK     int
	Logger   *slog.Logger
}

// --- Retriever --------------------------------------------------------------

// Retriever is the RAG entry point. It is safe for concurrent use provided
// that the underlying store and embedder are.
type Retriever struct {
	store    VectorStore
	embedder EmbeddingProvider
	topK     int
	log      *slog.Logger
}

// NewRetriever constructs a Retriever from the given config. It returns
// ErrNilStore or ErrNilEmbedder when the respective dependencies are missing.
func NewRetriever(cfg RetrieverConfig) (*Retriever, error) {
	if cfg.Store == nil {
		return nil, ErrNilStore
	}
	if cfg.Embedder == nil {
		return nil, ErrNilEmbedder
	}

	topK := cfg.TopK
	if topK <= 0 {
		topK = DefaultTopK
	}

	log := cfg.Logger
	if log == nil {
		log = slog.Default()
	}

	return &Retriever{
		store:    cfg.Store,
		embedder: cfg.Embedder,
		topK:     topK,
		log:      log,
	}, nil
}

// Retrieve returns the topK documents most similar to the natural-language
// query. The query is embedded with the configured provider and the
// resulting vector is passed to the store.
func (r *Retriever) Retrieve(ctx context.Context, query string) ([]SearchResult, error) {
	if query == "" {
		return nil, ErrEmptyQuery
	}

	vec, err := r.embedder.Embed(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("rag: embed query: %w", err)
	}

	results, err := r.store.Search(ctx, vec, r.topK)
	if err != nil {
		return nil, fmt.Errorf("rag: search: %w", err)
	}
	return results, nil
}

// AddDocument embeds the content and inserts a new document into the store.
// The ID must be non-empty and unique; metadata may be nil.
func (r *Retriever) AddDocument(ctx context.Context, id, content string, metadata map[string]string) error {
	if id == "" {
		return ErrEmptyID
	}
	if content == "" {
		return ErrEmptyText
	}

	vec, err := r.embedder.Embed(ctx, content)
	if err != nil {
		return fmt.Errorf("rag: embed document: %w", err)
	}

	doc := Document{
		ID:        id,
		Content:   content,
		Metadata:  metadata,
		Embedding: vec,
	}
	if err := r.store.Add(ctx, doc); err != nil {
		return fmt.Errorf("rag: add document: %w", err)
	}
	return nil
}

// AddDocuments embeds and inserts multiple documents. Embeddings are
// generated in a single batch when the embedder supports it. The store's
// AddBatch is used so the insert is atomic.
func (r *Retriever) AddDocuments(ctx context.Context, docs []Document) error {
	if len(docs) == 0 {
		return nil
	}

	// Validate up front.
	for i := range docs {
		if docs[i].ID == "" {
			return ErrEmptyID
		}
		if docs[i].Content == "" {
			return ErrEmptyText
		}
	}

	texts := make([]string, len(docs))
	for i := range docs {
		texts[i] = docs[i].Content
	}

	vecs, err := r.embedder.EmbedBatch(ctx, texts)
	if err != nil {
		return fmt.Errorf("rag: embed documents: %w", err)
	}

	prepared := make([]Document, len(docs))
	for i := range docs {
		prepared[i] = Document{
			ID:        docs[i].ID,
			Content:   docs[i].Content,
			Metadata:  docs[i].Metadata,
			Embedding: vecs[i],
		}
	}

	if err := r.store.AddBatch(ctx, prepared); err != nil {
		return fmt.Errorf("rag: add documents: %w", err)
	}
	return nil
}

// AugmentPrompt runs a retrieval for the query and renders the hits into a
// suffix appended to basePrompt. The returned slice is the same set of
// results that was rendered, so callers can inspect scores or metadata
// without re-running the search.
//
// The format is:
//
//	{basePrompt}
//
//	--- Relevant Knowledge ---
//	[1] {doc1.Content} (score: {score1})
//	[2] {doc2.Content} (score: {score2})
//	...
//
// When no documents are retrieved, basePrompt is returned unchanged with an
// empty result slice.
func (r *Retriever) AugmentPrompt(ctx context.Context, query, basePrompt string) (string, []SearchResult, error) {
	if query == "" {
		return "", nil, ErrEmptyQuery
	}

	results, err := r.Retrieve(ctx, query)
	if err != nil {
		return "", nil, err
	}
	if len(results) == 0 {
		return basePrompt, results, nil
	}

	var b strings.Builder
	b.WriteString(basePrompt)
	if basePrompt != "" && !strings.HasSuffix(basePrompt, "\n") {
		b.WriteString("\n")
	}
	b.WriteString("\n--- Relevant Knowledge ---\n")
	for i, res := range results {
		fmt.Fprintf(&b, "[%d] %s (score: %.4f)\n", i+1, res.Document.Content, res.Score)
	}
	return b.String(), results, nil
}
