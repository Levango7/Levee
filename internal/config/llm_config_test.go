package config

// Tests for the ai.llm section. The section's Config struct predates these
// keys being wired into the default/env/validate plumbing, which left the
// feature documented in config.example.yaml and reachable only from a YAML
// file — an operator running the container with LEVEE_* environment variables
// could not turn it on at all, and turning it on with no provider silently got
// them recommend's canned mock client. These pin the fixed behaviour.

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestLoad_AILLMDefaults checks the documented defaults are the ones the code
// actually applies. provider=openai in particular: config.example.yaml promises
// it, and without it NewLLMClient("") returns a mock client.
func TestLoad_AILLMDefaults(t *testing.T) {
	cfg, err := Load("")
	require.NoError(t, err)

	assert.False(t, cfg.AI.LLM.Enabled, "the LLM must be off unless a deployment asks for it")
	assert.Equal(t, "openai", cfg.AI.LLM.Provider)
	assert.Empty(t, cfg.AI.LLM.APIKey)
	assert.Empty(t, cfg.AI.LLM.Model)
	assert.Empty(t, cfg.AI.LLM.BaseURL)
	assert.Equal(t, 0, cfg.AI.LLM.MaxTokens, "0 means the client's own built-in default")
	assert.Equal(t, 0.0, cfg.AI.LLM.Temperature)
	assert.Equal(t, 30*time.Second, cfg.AI.LLM.Timeout)
}

// TestLoad_AILLMEnvOverride is the regression test for the gap itself: the
// keys were missing from allKeys(), so bindEnv never bound them and viper's
// Unmarshal never surfaced the values. Container deployments configure LEVEE_*
// only.
func TestLoad_AILLMEnvOverride(t *testing.T) {
	withEnv(t, map[string]string{
		"LEVEE_AI_LLM_ENABLED":    "true",
		"LEVEE_AI_LLM_PROVIDER":   "ollama",
		"LEVEE_AI_LLM_MODEL":      "llama3",
		"LEVEE_AI_LLM_BASE_URL":   "http://ollama.internal:11434",
		"LEVEE_AI_LLM_MAX_TOKENS": "2048",
		"LEVEE_AI_LLM_TIMEOUT":    "45s",
	})

	cfg, err := Load("")
	require.NoError(t, err, "an ollama deployment with defaults must validate")

	assert.True(t, cfg.AI.LLM.Enabled)
	assert.Equal(t, "ollama", cfg.AI.LLM.Provider)
	assert.Equal(t, "llama3", cfg.AI.LLM.Model)
	assert.Equal(t, "http://ollama.internal:11434", cfg.AI.LLM.BaseURL)
	assert.Equal(t, 2048, cfg.AI.LLM.MaxTokens)
	assert.Equal(t, 45*time.Second, cfg.AI.LLM.Timeout)
}

// TestLoad_AILLMFileConfig keeps the YAML path working — it always did, and the
// example file documents it.
func TestLoad_AILLMFileConfig(t *testing.T) {
	p := writeYAML(t, minimalValidYAML()+`
ai:
  llm:
    enabled: true
    provider: ollama
    model: llama3
    temperature: 0.2
`)

	cfg, err := Load(p)
	require.NoError(t, err)
	assert.True(t, cfg.AI.LLM.Enabled)
	assert.Equal(t, "ollama", cfg.AI.LLM.Provider)
	assert.Equal(t, "llama3", cfg.AI.LLM.Model)
	assert.InDelta(t, 0.2, cfg.AI.LLM.Temperature, 0.0001)
}

// TestValidate_AILLMRejectsUnusableProvider is why mock is not accepted: it
// would serve fabricated recommendations while every log line reported the LLM
// as healthy.
func TestValidate_AILLMRejectsUnusableProvider(t *testing.T) {
	cfg := validConfig(t)
	cfg.AI.LLM.Enabled = true
	cfg.AI.LLM.Provider = "mock"

	err := Validate(cfg)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "ai.llm.provider")
	assert.Contains(t, err.Error(), "openai|ollama")

	cfg.AI.LLM.Provider = "gpt"
	err = Validate(cfg)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "ai.llm.provider")
}

// TestValidate_AILLMDisabledStaysValid keeps the off-by-default promise: a
// deployment that never touches the section must not have to fill it in.
func TestValidate_AILLMDisabledStaysValid(t *testing.T) {
	cfg := validConfig(t)
	cfg.AI.LLM.Enabled = false
	cfg.AI.LLM.Provider = "" // zero value, never configured
	cfg.AI.LLM.Timeout = 0

	require.NoError(t, Validate(cfg), "a disabled LLM section must not block startup")
}

// TestValidate_AILLMBounds checks the two numeric guards.
func TestValidate_AILLMBounds(t *testing.T) {
	cfg := validConfig(t)
	cfg.AI.LLM.Enabled = true
	cfg.AI.LLM.Provider = "openai"

	cfg.AI.LLM.Timeout = 0
	err := Validate(cfg)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "ai.llm.timeout")

	cfg.AI.LLM.Timeout = 30 * time.Second
	cfg.AI.LLM.Temperature = 1.5
	err = Validate(cfg)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "ai.llm.temperature")
}

// TestAllKeys_CoversAILLM guards the binding list against future drift: a key
// missing from allKeys is silently unreachable from the environment, which is
// exactly the failure this file exists to prevent.
func TestAllKeys_CoversAILLM(t *testing.T) {
	want := []string{
		"ai.llm.enabled", "ai.llm.provider", "ai.llm.api_key", "ai.llm.model",
		"ai.llm.base_url", "ai.llm.max_tokens", "ai.llm.temperature", "ai.llm.timeout",
	}
	keys := map[string]bool{}
	for _, k := range allKeys() {
		keys[k] = true
	}
	for _, k := range want {
		assert.True(t, keys[k], "%s must be bound to LEVEE_%s", k, envNameFor(k))
	}
}

// validConfig returns a configuration that passes Validate, for the tests that
// mutate one section and re-validate.
func validConfig(t *testing.T) *Config {
	t.Helper()
	cfg, err := Load("")
	require.NoError(t, err)
	return cfg
}

// envNameFor renders a dotted key the way bindEnv's prefix+replacer would, so a
// failure message names the variable an operator would actually set.
func envNameFor(key string) string {
	out := "LEVEE_"
	for i, r := range key {
		if r == '.' {
			out += "_"
			continue
		}
		if i == 0 || key[i-1] == '.' {
			if r >= 'a' && r <= 'z' {
				r = r - 'a' + 'A'
			}
		}
		out += string(r)
	}
	return out
}
