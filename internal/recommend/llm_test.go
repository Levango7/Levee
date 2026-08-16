package recommend

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// --- MockLLMClient ----------------------------------------------------------

// TestMockLLMName verifies the Name accessor.
func TestMockLLMName(t *testing.T) {
	m := NewMockLLMClient()
	assert.Equal(t, "mock", m.Name())
}

// TestMockLLMChatDefaultReply verifies that a Chat call without a registered
// response returns the deterministic echo reply.
func TestMockLLMChatDefaultReply(t *testing.T) {
	m := NewMockLLMClient()
	ctx := context.Background()

	messages := []LLMMessage{
		{Role: "user", Content: "hello"},
	}
	out, err := m.Chat(ctx, messages)
	require.NoError(t, err)
	assert.Contains(t, out, "hello")
	assert.Contains(t, out, "mock:user")
}

// TestMockLLMChatEmptyMessages verifies the empty-messages edge case.
func TestMockLLMChatEmptyMessages(t *testing.T) {
	m := NewMockLLMClient()
	out, err := m.Chat(context.Background(), nil)
	require.NoError(t, err)
	assert.Equal(t, "[mock: empty]", out)
}

// TestMockLLMChatRegisteredResponse verifies that a registered response is
// returned when the last user message matches.
func TestMockLLMChatRegisteredResponse(t *testing.T) {
	m := NewMockLLMClient()
	m.SetResponse("what is 2+2", "4")

	messages := []LLMMessage{
		{Role: "user", Content: "what is 2+2"},
	}
	out, err := m.Chat(context.Background(), messages)
	require.NoError(t, err)
	assert.Equal(t, "4", out)
}

// TestMockLLMChatFallbackResponse verifies that the empty-key response is used
// when no specific key matches.
func TestMockLLMChatFallbackResponse(t *testing.T) {
	m := NewMockLLMClient()
	m.SetResponse("", "fallback")

	out, err := m.Chat(context.Background(), []LLMMessage{{Role: "user", Content: "unknown"}})
	require.NoError(t, err)
	assert.Equal(t, "fallback", out)
}

// TestMockLLMChatRecordsCalls verifies that every Chat call is recorded in
// Calls with a defensive copy.
func TestMockLLMChatRecordsCalls(t *testing.T) {
	m := NewMockLLMClient()
	ctx := context.Background()

	messages := []LLMMessage{{Role: "user", Content: "first"}}
	_, _ = m.Chat(ctx, messages)

	// Mutate the original slice after the call; the recorded copy must not
	// change.
	messages[0].Content = "mutated"

	_, _ = m.Chat(ctx, []LLMMessage{{Role: "user", Content: "second"}})

	require.Equal(t, 2, m.CallsCount())
	assert.Equal(t, "first", m.Calls[0][0].Content, "recorded copy must not be affected by later mutation")
	assert.Equal(t, "second", m.Calls[1][0].Content)
}

// TestMockLLMChatWithSystem verifies that ChatWithSystem prepends the system
// message.
func TestMockLLMChatWithSystem(t *testing.T) {
	m := NewMockLLMClient()
	m.SetResponse("question", "answer")

	out, err := m.ChatWithSystem(context.Background(), "you are a bot", []LLMMessage{{Role: "user", Content: "question"}})
	require.NoError(t, err)
	assert.Equal(t, "answer", out)

	// The recorded call must include the system prompt.
	require.Len(t, m.Calls, 1)
	require.Len(t, m.Calls[0], 2)
	assert.Equal(t, "system", m.Calls[0][0].Role)
	assert.Equal(t, "you are a bot", m.Calls[0][0].Content)
	assert.Equal(t, "user", m.Calls[0][1].Role)
}

// TestMockLLMConcurrent verifies that the mock is safe for concurrent use.
func TestMockLLMConcurrent(t *testing.T) {
	m := NewMockLLMClient()
	m.SetResponse("ping", "pong")

	const goroutines = 50
	var wg sync.WaitGroup
	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			out, err := m.Chat(context.Background(), []LLMMessage{{Role: "user", Content: "ping"}})
			assert.NoError(t, err)
			assert.Equal(t, "pong", out)
		}()
	}
	wg.Wait()
	assert.Equal(t, goroutines, m.CallsCount())
}

// --- NewLLMClient factory ---------------------------------------------------

// TestNewLLMClientMock verifies that the mock provider is returned for the
// "mock" and empty provider strings.
func TestNewLLMClientMock(t *testing.T) {
	for _, provider := range []string{"mock", ""} {
		t.Run(provider, func(t *testing.T) {
			c, err := NewLLMClient(LLMConfig{Provider: provider})
			require.NoError(t, err)
			assert.Equal(t, "mock", c.Name())
		})
	}
}

// TestNewLLMClientUnknownProvider verifies the error path for an unknown
// provider.
func TestNewLLMClientUnknownProvider(t *testing.T) {
	c, err := NewLLMClient(LLMConfig{Provider: "watson"})
	require.Error(t, err)
	assert.Nil(t, c)
	assert.ErrorIs(t, err, ErrUnknownProvider)
}

// TestNewLLMClientOpenAIEnvKey verifies that the OpenAI adapter picks up the
// API key from the OPENAI_API_KEY environment variable when the config field
// is empty.
func TestNewLLMClientOpenAIEnvKey(t *testing.T) {
	t.Setenv(EnvOpenAIAPIKey, "env-key-123")

	c, err := NewLLMClient(LLMConfig{Provider: "openai", Model: "gpt-4"})
	require.NoError(t, err)
	assert.Equal(t, "openai", c.Name())
}

// TestNewLLMClientOpenAIEmptyKey verifies the error when no API key is
// available.
func TestNewLLMClientOpenAIEmptyKey(t *testing.T) {
	t.Setenv(EnvOpenAIAPIKey, "")

	c, err := NewLLMClient(LLMConfig{Provider: "openai", Model: "gpt-4"})
	require.Error(t, err)
	assert.Nil(t, c)
	assert.ErrorIs(t, err, ErrEmptyAPIKey)
}

// TestNewLLMClientOpenAIEmptyModel verifies the error when the model is empty.
func TestNewLLMClientOpenAIEmptyModel(t *testing.T) {
	c, err := NewLLMClient(LLMConfig{Provider: "openai", APIKey: "k"})
	require.Error(t, err)
	assert.Nil(t, c)
	assert.ErrorIs(t, err, ErrEmptyModel)
}

// TestNewLLMClientOllamaEmptyModel verifies the error when the model is empty.
func TestNewLLMClientOllamaEmptyModel(t *testing.T) {
	c, err := NewLLMClient(LLMConfig{Provider: "ollama"})
	require.Error(t, err)
	assert.Nil(t, c)
	assert.ErrorIs(t, err, ErrEmptyModel)
}

// TestNewLLMClientDefaultsApplied verifies that zero config values are
// replaced with the documented defaults.
func TestNewLLMClientDefaultsApplied(t *testing.T) {
	t.Setenv(EnvOpenAIAPIKey, "k")
	c, err := NewLLMClient(LLMConfig{Provider: "openai", Model: "gpt-4"})
	require.NoError(t, err)

	// The defaults are applied on the internal config; we verify indirectly by
	// making a call against a test server that echoes the request body.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req openAIRequest
		_ = json.Unmarshal(body, &req)
		assert.Equal(t, DefaultMaxTokens, req.MaxTokens)
		assert.Equal(t, DefaultTemperature, req.Temperature)
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"ok"}}]}`))
	}))
	defer srv.Close()

	oc, ok := c.(*openAIClient)
	require.True(t, ok)
	oc.cfg.BaseURL = srv.URL

	out, err := c.Chat(context.Background(), []LLMMessage{{Role: "user", Content: "hi"}})
	require.NoError(t, err)
	assert.Equal(t, "ok", out)
}

// --- OpenAI adapter ---------------------------------------------------------

// openAITestServer builds an httptest.Server that mimics the OpenAI chat
// completions endpoint. It records the last request for assertions.
func openAITestServer(t *testing.T, status int, respBody string) (*httptest.Server, *http.Request) {
	t.Helper()
	var captured http.Request
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		captured = *r
		captured.Body = io.NopCloser(strings.NewReader(""))
		if status != http.StatusOK {
			http.Error(w, respBody, status)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(respBody))
	}))
	return srv, &captured
}

// TestOpenAIChatSuccess verifies a happy-path OpenAI chat call.
func TestLLMOpenAIChatSuccess(t *testing.T) {
	srv, captured := openAITestServer(t, http.StatusOK, `{"choices":[{"message":{"content":"hello back"}}]}`)
	defer srv.Close()

	c, err := NewLLMClient(LLMConfig{
		Provider: "openai",
		APIKey:   "test-key",
		Model:    "gpt-4",
		BaseURL:  srv.URL,
	})
	require.NoError(t, err)

	out, err := c.Chat(context.Background(), []LLMMessage{{Role: "user", Content: "hello"}})
	require.NoError(t, err)
	assert.Equal(t, "hello back", out)

	// The request must carry the bearer token.
	assert.Equal(t, "Bearer test-key", captured.Header.Get("Authorization"))
	assert.Equal(t, "application/json", captured.Header.Get("Content-Type"))
}

// TestOpenAIChatContentParts verifies that the newer list-of-parts content
// shape is parsed correctly.
func TestLLMOpenAIChatContentParts(t *testing.T) {
	srv, _ := openAITestServer(t, http.StatusOK, `{"choices":[{"message":{"content":[{"type":"text","text":"part1"},{"type":"text","text":"part2"}]}}]}`)
	defer srv.Close()

	c, err := NewLLMClient(LLMConfig{Provider: "openai", APIKey: "k", Model: "gpt-4", BaseURL: srv.URL})
	require.NoError(t, err)

	out, err := c.Chat(context.Background(), []LLMMessage{{Role: "user", Content: "hi"}})
	require.NoError(t, err)
	assert.Equal(t, "part1part2", out)
}

// TestOpenAIChatNon2xx verifies that a non-2xx response produces an error.
func TestLLMOpenAIChatNon2xx(t *testing.T) {
	srv, _ := openAITestServer(t, http.StatusUnauthorized, `{"error":"bad key"}`)
	defer srv.Close()

	c, err := NewLLMClient(LLMConfig{Provider: "openai", APIKey: "k", Model: "gpt-4", BaseURL: srv.URL})
	require.NoError(t, err)

	_, err = c.Chat(context.Background(), []LLMMessage{{Role: "user", Content: "hi"}})
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrLLMNon2xx)
}

// TestOpenAIChatEmptyContent verifies that a response without content produces
// an error.
func TestLLMOpenAIChatEmptyContent(t *testing.T) {
	srv, _ := openAITestServer(t, http.StatusOK, `{"choices":[]}`)
	defer srv.Close()

	c, err := NewLLMClient(LLMConfig{Provider: "openai", APIKey: "k", Model: "gpt-4", BaseURL: srv.URL})
	require.NoError(t, err)

	_, err = c.Chat(context.Background(), []LLMMessage{{Role: "user", Content: "hi"}})
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrLLMEmptyContent)
}

// TestOpenAIChatInvalidJSON verifies that a malformed response body produces
// an error.
func TestLLMOpenAIChatInvalidJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`not json`))
	}))
	defer srv.Close()

	c, err := NewLLMClient(LLMConfig{Provider: "openai", APIKey: "k", Model: "gpt-4", BaseURL: srv.URL})
	require.NoError(t, err)

	_, err = c.Chat(context.Background(), []LLMMessage{{Role: "user", Content: "hi"}})
	require.Error(t, err)
}

// TestOpenAIChatContextCancelled verifies that a cancelled context aborts the
// call.
func TestLLMOpenAIChatContextCancelled(t *testing.T) {
	// A server that never responds.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(2 * time.Second)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	c, err := NewLLMClient(LLMConfig{Provider: "openai", APIKey: "k", Model: "gpt-4", BaseURL: srv.URL, Timeout: 10 * time.Second})
	require.NoError(t, err)

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	_, err = c.Chat(ctx, []LLMMessage{{Role: "user", Content: "hi"}})
	require.Error(t, err)
	assert.ErrorIs(t, err, context.DeadlineExceeded)
}

// TestOpenAIChatWithSystem verifies the system-prompt helper.
func TestLLMOpenAIChatWithSystem(t *testing.T) {
	srv, captured := openAITestServer(t, http.StatusOK, `{"choices":[{"message":{"content":"ok"}}]}`)
	defer srv.Close()

	c, err := NewLLMClient(LLMConfig{Provider: "openai", APIKey: "k", Model: "gpt-4", BaseURL: srv.URL})
	require.NoError(t, err)

	out, err := c.ChatWithSystem(context.Background(), "be brief", []LLMMessage{{Role: "user", Content: "hi"}})
	require.NoError(t, err)
	assert.Equal(t, "ok", out)

	// Inspect the request body to confirm the system message was prepended.
	body, err := io.ReadAll(captured.Body)
	// captured.Body was replaced with an empty reader in the helper; re-issue
	// the call against a fresh server that captures the body instead.
	if err != nil || len(body) == 0 {
		// Fall back to a body-capturing server.
		srv2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			raw, _ := io.ReadAll(r.Body)
			var req openAIRequest
			_ = json.Unmarshal(raw, &req)
			require.Len(t, req.Messages, 2)
			assert.Equal(t, "system", req.Messages[0].Role)
			assert.Equal(t, "be brief", req.Messages[0].Content)
			assert.Equal(t, "user", req.Messages[1].Role)
			_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"ok"}}]}`))
		}))
		defer srv2.Close()

		c2, err := NewLLMClient(LLMConfig{Provider: "openai", APIKey: "k", Model: "gpt-4", BaseURL: srv2.URL})
		require.NoError(t, err)
		_, err = c2.ChatWithSystem(context.Background(), "be brief", []LLMMessage{{Role: "user", Content: "hi"}})
		require.NoError(t, err)
	}
}

// TestOpenAIChatRequestShape verifies that the request body carries the model,
// messages and sampling parameters.
func TestLLMOpenAIChatRequestShape(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var req openAIRequest
		require.NoError(t, json.Unmarshal(raw, &req))
		assert.Equal(t, "gpt-4", req.Model)
		require.Len(t, req.Messages, 1)
		assert.Equal(t, "user", req.Messages[0].Role)
		assert.Equal(t, "hello", req.Messages[0].Content)
		assert.Equal(t, 256, req.MaxTokens)
		assert.InDelta(t, 0.7, req.Temperature, 1e-9)
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"ok"}}]}`))
	}))
	defer srv.Close()

	c, err := NewLLMClient(LLMConfig{
		Provider:    "openai",
		APIKey:      "k",
		Model:       "gpt-4",
		BaseURL:     srv.URL,
		MaxTokens:   256,
		Temperature: 0.7,
	})
	require.NoError(t, err)

	out, err := c.Chat(context.Background(), []LLMMessage{{Role: "user", Content: "hello"}})
	require.NoError(t, err)
	assert.Equal(t, "ok", out)
}

// --- Ollama adapter ---------------------------------------------------------

// ollamaTestServer builds an httptest.Server that mimics the Ollama /api/chat
// endpoint.
func ollamaTestServer(t *testing.T, status int, respBody string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if status != http.StatusOK {
			http.Error(w, respBody, status)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(respBody))
	}))
}

// TestOllamaChatSuccess verifies a happy-path Ollama chat call.
func TestLLMOllamaChatSuccess(t *testing.T) {
	srv := ollamaTestServer(t, http.StatusOK, `{"message":{"content":"hi there"}}`)
	defer srv.Close()

	c, err := NewLLMClient(LLMConfig{Provider: "ollama", Model: "llama2", BaseURL: srv.URL})
	require.NoError(t, err)
	assert.Equal(t, "ollama", c.Name())

	out, err := c.Chat(context.Background(), []LLMMessage{{Role: "user", Content: "hello"}})
	require.NoError(t, err)
	assert.Equal(t, "hi there", out)
}

// TestOllamaChatResponseField verifies the top-level "response" fallback.
func TestLLMOllamaChatResponseField(t *testing.T) {
	srv := ollamaTestServer(t, http.StatusOK, `{"response":"top-level"}`)
	defer srv.Close()

	c, err := NewLLMClient(LLMConfig{Provider: "ollama", Model: "llama2", BaseURL: srv.URL})
	require.NoError(t, err)

	out, err := c.Chat(context.Background(), []LLMMessage{{Role: "user", Content: "hi"}})
	require.NoError(t, err)
	assert.Equal(t, "top-level", out)
}

// TestOllamaChatNon2xx verifies the error path.
func TestLLMOllamaChatNon2xx(t *testing.T) {
	srv := ollamaTestServer(t, http.StatusInternalServerError, `internal error`)
	defer srv.Close()

	c, err := NewLLMClient(LLMConfig{Provider: "ollama", Model: "llama2", BaseURL: srv.URL})
	require.NoError(t, err)

	_, err = c.Chat(context.Background(), []LLMMessage{{Role: "user", Content: "hi"}})
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrLLMNon2xx)
}

// TestOllamaChatEmptyContent verifies the empty-content error.
func TestLLMOllamaChatEmptyContent(t *testing.T) {
	srv := ollamaTestServer(t, http.StatusOK, `{"message":{}}`)
	defer srv.Close()

	c, err := NewLLMClient(LLMConfig{Provider: "ollama", Model: "llama2", BaseURL: srv.URL})
	require.NoError(t, err)

	_, err = c.Chat(context.Background(), []LLMMessage{{Role: "user", Content: "hi"}})
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrLLMEmptyContent)
}

// TestOllamaChatWithSystem verifies the system-prompt helper.
func TestLLMOllamaChatWithSystem(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var req ollamaRequest
		require.NoError(t, json.Unmarshal(raw, &req))
		require.Len(t, req.Messages, 2)
		assert.Equal(t, "system", req.Messages[0].Role)
		assert.Equal(t, "you are local", req.Messages[0].Content)
		assert.False(t, req.Stream, "stream must be false")
		_, _ = w.Write([]byte(`{"message":{"content":"ok"}}`))
	}))
	defer srv.Close()

	c, err := NewLLMClient(LLMConfig{Provider: "ollama", Model: "llama2", BaseURL: srv.URL})
	require.NoError(t, err)

	out, err := c.ChatWithSystem(context.Background(), "you are local", []LLMMessage{{Role: "user", Content: "hi"}})
	require.NoError(t, err)
	assert.Equal(t, "ok", out)
}

// TestOllamaChatContextCancelled verifies that a cancelled context aborts the
// call.
func TestLLMOllamaChatContextCancelled(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(2 * time.Second)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	c, err := NewLLMClient(LLMConfig{Provider: "ollama", Model: "llama2", BaseURL: srv.URL, Timeout: 10 * time.Second})
	require.NoError(t, err)

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	_, err = c.Chat(ctx, []LLMMessage{{Role: "user", Content: "hi"}})
	require.Error(t, err)
	assert.ErrorIs(t, err, context.DeadlineExceeded)
}

// --- Helpers ----------------------------------------------------------------

// TestLastUserMessage verifies the helper that picks the last user message.
func TestLLMLastUserMessage(t *testing.T) {
	cases := []struct {
		name     string
		messages []LLMMessage
		want     string
	}{
		{"empty", nil, ""},
		{"only_system", []LLMMessage{{Role: "system", Content: "s"}}, ""},
		{"only_assistant", []LLMMessage{{Role: "assistant", Content: "a"}}, ""},
		{"single_user", []LLMMessage{{Role: "user", Content: "u"}}, "u"},
		{"multi_last_user", []LLMMessage{{Role: "user", Content: "first"}, {Role: "assistant", Content: "a"}, {Role: "user", Content: "second"}}, "second"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, lastUserMessage(tc.messages))
		})
	}
}

// TestDefaultMockReply verifies the deterministic echo helper.
func TestDefaultMockReply(t *testing.T) {
	assert.Equal(t, "[mock: empty]", defaultMockReply(nil))
	assert.Equal(t, "[mock: empty]", defaultMockReply([]LLMMessage{}))

	out := defaultMockReply([]LLMMessage{{Role: "user", Content: "hi"}})
	assert.Contains(t, out, "mock:user")
	assert.Contains(t, out, "hi")
}

// TestExtractOpenAIContentMalformed verifies the parse helpers are robust
// against malformed payloads.
func TestLLMExtractOpenAIContentMalformed(t *testing.T) {
	cases := []map[string]any{
		{},
		{"choices": "not-array"},
		{"choices": []any{}},
		{"choices": []any{"not-object"}},
		{"choices": []any{map[string]any{"no_message": true}}},
		{"choices": []any{map[string]any{"message": "not-object"}}},
		{"choices": []any{map[string]any{"message": map[string]any{}}}},
		{"choices": []any{map[string]any{"message": map[string]any{"content": 123}}}},
	}
	for i, c := range cases {
		_, err := extractOpenAIContent(c)
		require.Error(t, err, "case %d", i)
		assert.ErrorIs(t, err, ErrLLMEmptyContent, "case %d", i)
	}
}

// TestExtractOllamaContentMalformed verifies the Ollama parse helper.
func TestLLMExtractOllamaContentMalformed(t *testing.T) {
	cases := []map[string]any{
		{},
		{"message": "not-object"},
		{"message": map[string]any{}},
		{"message": map[string]any{"content": 123}},
	}
	for i, c := range cases {
		_, err := extractOllamaContent(c)
		require.Error(t, err, "case %d", i)
		assert.ErrorIs(t, err, ErrLLMEmptyContent, "case %d", i)
	}
}

// TestSanitizerRuleErrorUnwrap verifies the error wrapper.
func TestSanitizerRuleErrorUnwrap(t *testing.T) {
	inner := errors.New("bad regex")
	e := errSanitizerRule("myrule", "[", inner)
	assert.Contains(t, e.Error(), "myrule")
	assert.Contains(t, e.Error(), "bad regex")
	assert.ErrorIs(t, e, inner)
}