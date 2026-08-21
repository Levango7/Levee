// llm.go defines the LLM client abstraction used by the recommend package to
// talk to large language model backends. Two production adapters are provided:
//
//   - openAIClient  — calls the OpenAI Chat Completions REST API
//     (POST https://api.openai.com/v1/chat/completions).
//   - ollamaClient  — calls a local Ollama server
//     (POST http://localhost:11434/api/chat by default).
//
// Both adapters use only the standard library net/http package: no third-party
// LLM SDK is pulled in, keeping the dependency surface small and the behaviour
// deterministic for offline tests.
//
// A MockLLMClient is also provided for unit tests. It records every call and
// returns canned responses keyed by the last user message, so higher-level
// code can be exercised without any network access.
//
// All clients are safe for concurrent use. Every Chat call honours the
// context deadline; when the LLMConfig.Timeout is non-zero it is also applied
// to the underlying http.Client as a safety net for misbehaving servers.
package recommend

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/nexus/levee/internal/log"
)

// --- Sentinel errors --------------------------------------------------------

var (
	// ErrUnknownProvider is returned by NewLLMClient when the LLMConfig.Provider
	// field is not one of the supported values.
	ErrUnknownProvider = errors.New("recommend: unknown llm provider")
	// ErrEmptyModel is returned when the model name is missing.
	ErrEmptyModel = errors.New("recommend: empty llm model")
	// ErrEmptyAPIKey is returned when an OpenAI client is built without an API
	// key (neither LLMConfig.APIKey nor the OPENAI_API_KEY env var is set).
	ErrEmptyAPIKey = errors.New("recommend: empty openai api key")
	// ErrLLMRequest is the generic sentinel for a failed HTTP request.
	ErrLLMRequest = errors.New("recommend: llm request failed")
	// ErrLLMNon2xx is returned when the LLM endpoint responds with a non-2xx
	// status code.
	ErrLLMNon2xx = errors.New("recommend: llm non-2xx response")
	// ErrLLMEmptyContent is returned when the response payload does not contain
	// any assistant content.
	ErrLLMEmptyContent = errors.New("recommend: llm empty content")
)

// --- Defaults ---------------------------------------------------------------

const (
	// DefaultOpenAIBaseURL is the canonical OpenAI chat completions endpoint.
	DefaultOpenAIBaseURL = "https://api.openai.com/v1/chat/completions"
	// DefaultOllamaBaseURL is the default Ollama chat endpoint.
	DefaultOllamaBaseURL = "http://localhost:11434/api/chat"
	// DefaultLLMTimeout is applied when LLMConfig.Timeout is zero.
	DefaultLLMTimeout = 60 * time.Second
	// DefaultMaxTokens is applied when LLMConfig.MaxTokens is zero.
	DefaultMaxTokens = 1024
	// DefaultTemperature is applied when LLMConfig.Temperature is zero.
	DefaultTemperature = 0.2
	// EnvOpenAIAPIKey is the environment variable name for the OpenAI API key.
	EnvOpenAIAPIKey = "OPENAI_API_KEY"
)

// --- Public types -----------------------------------------------------------

// LLMMessage is a single chat message exchanged with the LLM. Role is one of
// "system", "user" or "assistant"; Content is the raw text payload.
type LLMMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// LLMConfig configures an LLMClient. Construct a client with NewLLMClient.
type LLMConfig struct {
	Provider    string        // "openai" / "ollama" / "mock"
	APIKey      string        // OpenAI API key (falls back to OPENAI_API_KEY env)
	Model       string        // e.g. "gpt-4", "gpt-3.5-turbo", "llama2"
	BaseURL     string        // custom endpoint; defaults are applied when empty
	MaxTokens   int           // max tokens in the completion (0 → DefaultMaxTokens)
	Temperature float64       // sampling temperature 0-1 (0 → DefaultTemperature)
	Timeout     time.Duration // per-request timeout (0 → DefaultLLMTimeout)
}

// LLMClient is the transport-agnostic interface for chatting with a large
// language model. All implementations must be safe for concurrent use and
// must honour the context deadline.
type LLMClient interface {
	// Chat sends the given messages and returns the assistant's reply text.
	Chat(ctx context.Context, messages []LLMMessage) (string, error)

	// ChatWithSystem prepends a system prompt and then calls Chat.
	ChatWithSystem(ctx context.Context, systemPrompt string, messages []LLMMessage) (string, error)

	// Name returns the client identifier, e.g. "openai" or "ollama".
	Name() string
}

// NewLLMClient builds the LLMClient for the configured provider. Supported
// providers are "openai", "ollama" and "mock". The returned client is safe
// for concurrent use.
func NewLLMClient(cfg LLMConfig) (LLMClient, error) {
	// Apply defaults that are common to every provider.
	if cfg.Timeout <= 0 {
		cfg.Timeout = DefaultLLMTimeout
	}
	if cfg.MaxTokens <= 0 {
		cfg.MaxTokens = DefaultMaxTokens
	}
	if cfg.Temperature <= 0 {
		cfg.Temperature = DefaultTemperature
	}

	switch strings.ToLower(strings.TrimSpace(cfg.Provider)) {
	case "openai":
		return newOpenAIClient(cfg)
	case "ollama":
		return newOllamaClient(cfg)
	case "mock", "":
		return NewMockLLMClient(), nil
	default:
		return nil, fmt.Errorf("recommend: new llm client: %w: %q", ErrUnknownProvider, cfg.Provider)
	}
}

// --- OpenAI adapter ---------------------------------------------------------

// openAIClient talks to the OpenAI Chat Completions REST API.
type openAIClient struct {
	cfg    LLMConfig
	client *http.Client
	log    *slog.Logger
}

// newOpenAIClient validates the config and builds an openAIClient. The API key
// is read from cfg.APIKey, falling back to the OPENAI_API_KEY environment
// variable when the field is empty.
func newOpenAIClient(cfg LLMConfig) (*openAIClient, error) {
	if strings.TrimSpace(cfg.Model) == "" {
		return nil, fmt.Errorf("recommend: new openai: %w", ErrEmptyModel)
	}
	if strings.TrimSpace(cfg.APIKey) == "" {
		if env := os.Getenv(EnvOpenAIAPIKey); env != "" {
			cfg.APIKey = env
		}
	}
	if strings.TrimSpace(cfg.APIKey) == "" {
		return nil, fmt.Errorf("recommend: new openai: %w", ErrEmptyAPIKey)
	}
	if strings.TrimSpace(cfg.BaseURL) == "" {
		cfg.BaseURL = DefaultOpenAIBaseURL
	}
	return &openAIClient{
		cfg:    cfg,
		client: &http.Client{Timeout: cfg.Timeout},
		log:    log.With("component", "llm", "provider", "openai"),
	}, nil
}

// Name returns "openai".
func (c *openAIClient) Name() string { return "openai" }

// Chat sends the messages to OpenAI and returns the assistant reply.
func (c *openAIClient) Chat(ctx context.Context, messages []LLMMessage) (string, error) {
	body := openAIRequest{
		Model:       c.cfg.Model,
		Messages:    messages,
		MaxTokens:   c.cfg.MaxTokens,
		Temperature: c.cfg.Temperature,
	}
	resp, err := c.doJSON(ctx, c.cfg.BaseURL, body, true)
	if err != nil {
		return "", err
	}
	return extractOpenAIContent(resp)
}

// ChatWithSystem prepends the system prompt and delegates to Chat.
func (c *openAIClient) ChatWithSystem(ctx context.Context, systemPrompt string, messages []LLMMessage) (string, error) {
	return chatWithSystem(ctx, c, systemPrompt, messages)
}

// doJSON POSTs body to url and unmarshals the JSON response into a generic
// map. When auth is true the OpenAI bearer token header is added.
func (c *openAIClient) doJSON(ctx context.Context, url string, body any, auth bool) (map[string]any, error) {
	payload, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("recommend: openai marshal: %w", err)
	}
	headers := map[string]string{"Content-Type": "application/json"}
	if auth {
		headers["Authorization"] = "Bearer " + c.cfg.APIKey
	}
	return doLLMRequest(ctx, c.client, url, payload, headers, c.log)
}

// openAIRequest is the request body for the OpenAI chat completions API.
type openAIRequest struct {
	Model       string       `json:"model"`
	Messages    []LLMMessage `json:"messages"`
	MaxTokens   int          `json:"max_tokens"`
	Temperature float64      `json:"temperature"`
}

// extractOpenAIContent pulls the assistant text out of the OpenAI response
// payload. It tolerates both the "content" string and the newer list-of-parts
// shape.
func extractOpenAIContent(resp map[string]any) (string, error) {
	choices, ok := resp["choices"].([]any)
	if !ok || len(choices) == 0 {
		return "", fmt.Errorf("recommend: openai parse: %w", ErrLLMEmptyContent)
	}
	first, ok := choices[0].(map[string]any)
	if !ok {
		return "", fmt.Errorf("recommend: openai parse: %w", ErrLLMEmptyContent)
	}
	msg, ok := first["message"].(map[string]any)
	if !ok {
		return "", fmt.Errorf("recommend: openai parse: %w", ErrLLMEmptyContent)
	}
	if s, ok := msg["content"].(string); ok && s != "" {
		return s, nil
	}
	// Newer streaming format: content is a list of {type:text, text:"..."}.
	if parts, ok := msg["content"].([]any); ok {
		var sb strings.Builder
		for _, p := range parts {
			pm, ok := p.(map[string]any)
			if !ok {
				continue
			}
			if t, ok := pm["text"].(string); ok {
				sb.WriteString(t)
			}
		}
		if sb.Len() > 0 {
			return sb.String(), nil
		}
	}
	return "", fmt.Errorf("recommend: openai parse: %w", ErrLLMEmptyContent)
}

// --- Ollama adapter ---------------------------------------------------------

// ollamaClient talks to a local Ollama server.
type ollamaClient struct {
	cfg    LLMConfig
	client *http.Client
	log    *slog.Logger
}

// newOllamaClient validates the config and builds an ollamaClient.
func newOllamaClient(cfg LLMConfig) (*ollamaClient, error) {
	if strings.TrimSpace(cfg.Model) == "" {
		return nil, fmt.Errorf("recommend: new ollama: %w", ErrEmptyModel)
	}
	if strings.TrimSpace(cfg.BaseURL) == "" {
		cfg.BaseURL = DefaultOllamaBaseURL
	}
	return &ollamaClient{
		cfg:    cfg,
		client: &http.Client{Timeout: cfg.Timeout},
		log:    log.With("component", "llm", "provider", "ollama"),
	}, nil
}

// Name returns "ollama".
func (c *ollamaClient) Name() string { return "ollama" }

// Chat sends the messages to Ollama and returns the assistant reply.
func (c *ollamaClient) Chat(ctx context.Context, messages []LLMMessage) (string, error) {
	body := ollamaRequest{
		Model:    c.cfg.Model,
		Messages: messages,
		Stream:   false,
		Options: ollamaOptions{
			Temperature: c.cfg.Temperature,
			NumPredict:  c.cfg.MaxTokens,
		},
	}
	resp, err := c.doJSON(ctx, c.cfg.BaseURL, body)
	if err != nil {
		return "", err
	}
	return extractOllamaContent(resp)
}

// ChatWithSystem prepends the system prompt and delegates to Chat.
func (c *ollamaClient) ChatWithSystem(ctx context.Context, systemPrompt string, messages []LLMMessage) (string, error) {
	return chatWithSystem(ctx, c, systemPrompt, messages)
}

// doJSON POSTs body to url and unmarshals the JSON response into a generic
// map.
func (c *ollamaClient) doJSON(ctx context.Context, url string, body any) (map[string]any, error) {
	payload, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("recommend: ollama marshal: %w", err)
	}
	headers := map[string]string{"Content-Type": "application/json"}
	return doLLMRequest(ctx, c.client, url, payload, headers, c.log)
}

// ollamaRequest is the request body for the Ollama /api/chat endpoint.
type ollamaRequest struct {
	Model    string        `json:"model"`
	Messages []LLMMessage  `json:"messages"`
	Stream   bool          `json:"stream"`
	Options  ollamaOptions `json:"options"`
}

// ollamaOptions carries the sampling parameters Ollama accepts under options.
type ollamaOptions struct {
	Temperature float64 `json:"temperature"`
	NumPredict  int     `json:"num_predict"`
}

// extractOllamaContent pulls the assistant text out of the Ollama response.
func extractOllamaContent(resp map[string]any) (string, error) {
	if msg, ok := resp["message"].(map[string]any); ok {
		if s, ok := msg["content"].(string); ok && s != "" {
			return s, nil
		}
	}
	// Some Ollama versions return a top-level "response" string.
	if s, ok := resp["response"].(string); ok && s != "" {
		return s, nil
	}
	return "", fmt.Errorf("recommend: ollama parse: %w", ErrLLMEmptyContent)
}

// --- Shared helpers ---------------------------------------------------------

// chatWithSystem is the shared implementation of ChatWithSystem: it prepends
// a system message and delegates to the underlying Chat method.
func chatWithSystem(ctx context.Context, c LLMClient, systemPrompt string, messages []LLMMessage) (string, error) {
	all := make([]LLMMessage, 0, len(messages)+1)
	all = append(all, LLMMessage{Role: "system", Content: systemPrompt})
	all = append(all, messages...)
	return c.Chat(ctx, all)
}

// doLLMRequest is the shared HTTP POST helper used by both adapters. It sends
// the pre-encoded payload, attaches the given headers, honours the context
// deadline and unmarshals the JSON response into a generic map. The map is
// always non-nil on a nil error.
func doLLMRequest(ctx context.Context, client *http.Client, url string, payload []byte, headers map[string]string, lg *slog.Logger) (map[string]any, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("recommend: llm build request: %w", err)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("recommend: llm do request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("recommend: llm read body: %w", err)
	}

	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		lg.Debug("llm non-2xx", "status", resp.StatusCode, "body", string(raw))
		return nil, fmt.Errorf("recommend: llm status %d: %w", resp.StatusCode, ErrLLMNon2xx)
	}

	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("recommend: llm unmarshal: %w", err)
	}
	return out, nil
}

// --- Mock client (for testing) ----------------------------------------------

// MockLLMClient is an in-memory LLMClient intended for unit tests. It records
// every Chat call in Calls and returns a canned response from Responses keyed
// by the last user message; when no matching key exists a default echo string
// is returned so callers always get a deterministic, non-empty reply.
//
// MockLLMClient is safe for concurrent use.
type MockLLMClient struct {
	// Responses maps last-user-message → canned reply.
	Responses map[string]string
	// Calls records every message slice passed to Chat, in arrival order. Each
	// entry is a defensive copy of the input slice so later mutations by the
	// caller do not retroactively change the recorded history.
	Calls [][]LLMMessage

	mu sync.Mutex
}

// NewMockLLMClient returns a ready-to-use MockLLMClient with an empty
// response table.
func NewMockLLMClient() *MockLLMClient {
	return &MockLLMClient{Responses: make(map[string]string)}
}

// SetResponse registers a canned reply for the given prompt key. The prompt
// is matched against the last user message in a Chat call.
func (m *MockLLMClient) SetResponse(prompt, response string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.Responses[prompt] = response
}

// Chat records the messages and returns the canned response for the last user
// message (or a deterministic echo when no entry matches).
func (m *MockLLMClient) Chat(_ context.Context, messages []LLMMessage) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	// Record a flat copy so later mutations by the caller do not retroactively
	// change the history.
	cp := make([]LLMMessage, len(messages))
	copy(cp, messages)
	m.Calls = append(m.Calls, cp)

	key := lastUserMessage(messages)
	if resp, ok := m.Responses[key]; ok {
		return resp, nil
	}
	if resp, ok := m.Responses[""]; ok {
		return resp, nil
	}
	return defaultMockReply(messages), nil
}

// ChatWithSystem prepends the system prompt and delegates to Chat.
func (m *MockLLMClient) ChatWithSystem(ctx context.Context, systemPrompt string, messages []LLMMessage) (string, error) {
	return chatWithSystem(ctx, m, systemPrompt, messages)
}

// Name returns "mock".
func (m *MockLLMClient) Name() string { return "mock" }

// CallsCount returns the number of Chat calls recorded so far. It is safe
// for concurrent use.
func (m *MockLLMClient) CallsCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.Calls)
}

// lastUserMessage returns the content of the last user-role message, or the
// empty string when there is none.
func lastUserMessage(messages []LLMMessage) string {
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Role == "user" {
			return messages[i].Content
		}
	}
	return ""
}

// defaultMockReply produces a deterministic non-empty reply when no canned
// response matches. It echoes the last message so tests can assert on it.
func defaultMockReply(messages []LLMMessage) string {
	if len(messages) == 0 {
		return "[mock: empty]"
	}
	last := messages[len(messages)-1]
	return fmt.Sprintf("[mock:%s] %s", last.Role, last.Content)
}
