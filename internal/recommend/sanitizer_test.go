package recommend

import (
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestSanitizerNewDefault verifies that the default sanitizer loads all
// expected rules in the correct execution order.
func TestSanitizerNewDefault(t *testing.T) {
	s := NewSanitizer()
	rules := s.Rules()

	expected := []string{"db_conn", "jwt", "aws_access_key", "password", "api_key", "email", "phone", "ipv4"}
	assert.Equal(t, expected, rules, "default rule order must match the documented order")
}

// TestSanitizerRedactsIPv4 checks that bare IPv4 addresses are redacted.
func TestSanitizerRedactsIPv4(t *testing.T) {
	s := NewSanitizer()

	cases := []struct {
		name  string
		input string
	}{
		{"plain", "connect to 10.0.0.1 please"},
		{"loopback", "ping 127.0.0.1 failed"},
		{"multiple", "from 192.168.1.1 to 192.168.1.254"},
		{"with_port", "server at 10.20.30.40:8080 is down"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out := s.Sanitize(tc.input)
			assert.Contains(t, out, RedactIP)
			assert.NotContains(t, out, "10.0.0.1")
			assert.NotContains(t, out, "127.0.0.1")
			assert.NotContains(t, out, "192.168.1.1")
			assert.NotContains(t, out, "192.168.1.254")
			assert.NotContains(t, out, "10.20.30.40")
		})
	}
}

// TestSanitizerRedactsPassword checks password assignments.
func TestSanitizerRedactsPassword(t *testing.T) {
	s := NewSanitizer()

	cases := []struct {
		name  string
		input string
	}{
		{"lower_eq", "password=hunter2"},
		{"upper_eq", "PASSWORD=secret"},
		{"mixed_colon", "passwd: mypass"},
		{"pwd_space", "pwd = spaced"},
		{"in_sentence", "the password=abc123 was leaked"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out := s.Sanitize(tc.input)
			assert.Contains(t, out, RedactSecret, "should contain redaction marker")
			assert.NotContains(t, out, "hunter2")
			assert.NotContains(t, out, "secret")
			assert.NotContains(t, out, "mypass")
			assert.NotContains(t, out, "spaced")
			assert.NotContains(t, out, "abc123")
		})
	}
}

// TestSanitizerRedactsAPIKey checks API key / secret / token assignments
// with a long value.
func TestSanitizerRedactsAPIKey(t *testing.T) {
	s := NewSanitizer()

	longKey := strings.Repeat("a", 40)
	cases := []struct {
		name  string
		input string
	}{
		{"api_key", "api_key=" + longKey},
		{"api-key", "api-key=" + longKey},
		{"apikey", "apikey=" + longKey},
		{"secret", "secret=" + longKey},
		{"token", "token=" + longKey},
		{"upper", "API_KEY=" + longKey},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out := s.Sanitize(tc.input)
			assert.Contains(t, out, RedactSecret)
			assert.NotContains(t, out, longKey)
		})
	}
}

// TestSanitizerRedactsDBConn checks database connection strings.
func TestSanitizerRedactsDBConn(t *testing.T) {
	s := NewSanitizer()

	cases := []struct {
		name  string
		input string
	}{
		{"mysql", "mysql://root:password@localhost:3306/db"},
		{"postgres", "postgres://user:pass@db.example.com:5432/mydb"},
		{"mongodb", "mongodb://admin:secret@mongo:27017/admin"},
		{"redis", "redis://default:redis123@cache:6379/0"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out := s.Sanitize(tc.input)
			assert.Contains(t, out, RedactDBConn)
			assert.NotContains(t, out, "password")
			assert.NotContains(t, out, "pass")
			assert.NotContains(t, out, "secret")
			assert.NotContains(t, out, "redis123")
		})
	}
}

// TestSanitizerRedactsJWT checks JWT tokens.
func TestSanitizerRedactsJWT(t *testing.T) {
	s := NewSanitizer()

	// A realistic-looking JWT (header.payload.signature, all base64url).
	jwt := "eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0.SflKxwRJSMeKKF2QT4fwpMeJf36POk6yJV_adQssw5c"
	input := "Authorization: Bearer " + jwt

	out := s.Sanitize(input)
	assert.Contains(t, out, RedactJWT)
	assert.NotContains(t, out, jwt)
}

// TestSanitizerRedactsAWSKey checks AWS access key IDs.
func TestSanitizerRedactsAWSKey(t *testing.T) {
	s := NewSanitizer()

	cases := []struct {
		name  string
		input string
	}{
		{"plain", "AKIAIOSFODNN7EXAMPLE"},
		{"in_config", "aws_access_key_id=AKIAIOSFODNN7EXAMPLE"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out := s.Sanitize(tc.input)
			assert.Contains(t, out, RedactAWSKey)
			assert.NotContains(t, out, "AKIAIOSFODNN7EXAMPLE")
		})
	}
}

// TestSanitizerRedactsEmail checks email addresses.
func TestSanitizerRedactsEmail(t *testing.T) {
	s := NewSanitizer()

	cases := []struct {
		name  string
		input string
	}{
		{"simple", "contact alice@example.com"},
		{"dotted", "reach bob.dev@sub.example.co.uk"},
		{"numeric", "user123@domain.io"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out := s.Sanitize(tc.input)
			assert.Contains(t, out, RedactEmail)
			assert.NotContains(t, out, "alice@example.com")
			assert.NotContains(t, out, "bob.dev@sub.example.co.uk")
			assert.NotContains(t, out, "user123@domain.io")
		})
	}
}

// TestSanitizerRedactsPhone checks Chinese mobile numbers.
func TestSanitizerRedactsPhone(t *testing.T) {
	s := NewSanitizer()

	cases := []struct {
		name  string
		input string
	}{
		{"plain", "13800138000"},
		{"in_text", "call 15912345678 now"},
		{"189", "18900001111"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out := s.Sanitize(tc.input)
			assert.Contains(t, out, RedactPhone)
			assert.NotContains(t, out, "13800138000")
			assert.NotContains(t, out, "15912345678")
			assert.NotContains(t, out, "18900001111")
		})
	}
}

// TestSanitizerMixed verifies that multiple sensitive items in the same text
// are all redacted in a single pass.
func TestSanitizerMixed(t *testing.T) {
	s := NewSanitizer()

	input := "server 10.0.0.5 (admin:password=hunter2) email ops@example.com phone 13800138000"
	out := s.Sanitize(input)

	assert.Contains(t, out, RedactIP)
	assert.Contains(t, out, RedactSecret)
	assert.Contains(t, out, RedactEmail)
	assert.Contains(t, out, RedactPhone)
	assert.NotContains(t, out, "10.0.0.5")
	assert.NotContains(t, out, "hunter2")
	assert.NotContains(t, out, "ops@example.com")
	assert.NotContains(t, out, "13800138000")
}

// TestSanitizerPreservesCleanText verifies that text without sensitive data is
// returned verbatim.
func TestSanitizerPreservesCleanText(t *testing.T) {
	s := NewSanitizer()

	cases := []string{
		"",
		"the quick brown fox jumps over the lazy dog",
		"CPU usage is 89 percent on host web-01",
		"restart the nginx service",
	}

	for _, input := range cases {
		assert.Equal(t, input, s.Sanitize(input))
	}
}

// TestSanitizerEmpty verifies the empty-string fast path.
func TestSanitizerEmpty(t *testing.T) {
	s := NewSanitizer()
	assert.Equal(t, "", s.Sanitize(""))
}

// TestSanitizerMessages verifies that SanitizeMessages copies the slice and
// scrubs each content without mutating the input.
func TestSanitizerMessages(t *testing.T) {
	s := NewSanitizer()

	original := []LLMMessage{
		{Role: "system", Content: "you are a helpful assistant"},
		{Role: "user", Content: "my password=secret123 is leaked and my ip is 10.0.0.1"},
		{Role: "assistant", Content: "I see"},
	}

	out := s.SanitizeMessages(original)

	// Output must be a new slice (different backing array).
	require.Len(t, out, 3)
	if len(original) > 0 && len(out) > 0 {
		assert.NotSame(t, &original[0], &out[0], "output slice must not share storage with input")
	}
	require.Len(t, out, 3)

	// Roles preserved.
	assert.Equal(t, "system", out[0].Role)
	assert.Equal(t, "user", out[1].Role)
	assert.Equal(t, "assistant", out[2].Role)

	// Clean content unchanged.
	assert.Equal(t, original[0].Content, out[0].Content)
	assert.Equal(t, original[2].Content, out[2].Content)

	// Sensitive content redacted.
	assert.Contains(t, out[1].Content, RedactSecret)
	assert.Contains(t, out[1].Content, RedactIP)
	assert.NotContains(t, out[1].Content, "secret123")
	assert.NotContains(t, out[1].Content, "10.0.0.1")

	// Input slice must not be mutated.
	assert.Contains(t, original[1].Content, "secret123")
	assert.Contains(t, original[1].Content, "10.0.0.1")
}

// TestSanitizerMessagesNil verifies the nil-input contract.
func TestSanitizerMessagesNil(t *testing.T) {
	s := NewSanitizer()
	assert.Nil(t, s.SanitizeMessages(nil))
}

// TestSanitizerConcurrent verifies that Sanitize is safe for concurrent use.
func TestSanitizerConcurrent(t *testing.T) {
	s := NewSanitizer()
	input := "ip=10.0.0.1 password=hunter2 email=a@b.com phone=13800138000"

	const goroutines = 50
	const iterations = 200

	var wg sync.WaitGroup
	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			for j := 0; j < iterations; j++ {
				out := s.Sanitize(input)
				// Every call must produce the same redacted output.
				assert.Contains(t, out, RedactIP)
				assert.Contains(t, out, RedactSecret)
				assert.Contains(t, out, RedactEmail)
				assert.Contains(t, out, RedactPhone)
			}
		}()
	}
	wg.Wait()
}

// TestSanitizerCustomRules verifies that NewSanitizerWithRules honours a
// caller-supplied rule set.
func TestSanitizerCustomRules(t *testing.T) {
	rules := []SanitizerRule{
		{Name: "credit_card", Pattern: `\d{4}-\d{4}-\d{4}-\d{4}`, Replace: "[CC_REDACTED]"},
		{Name: "empty_pattern", Pattern: "", Replace: "ignored"}, // skipped
	}
	s, err := NewSanitizerWithRules(rules)
	require.NoError(t, err)
	assert.Equal(t, []string{"credit_card"}, s.Rules())

	out := s.Sanitize("card 4111-1111-1111-1111 expired")
	assert.Contains(t, out, "[CC_REDACTED]")
	assert.NotContains(t, out, "4111-1111-1111-1111")
}

// TestSanitizerInvalidRule verifies that an invalid regex produces an error.
func TestSanitizerInvalidRule(t *testing.T) {
	rules := []SanitizerRule{
		{Name: "bad", Pattern: `[unclosed`, Replace: "x"},
	}
	s, err := NewSanitizerWithRules(rules)
	require.Error(t, err)
	assert.Nil(t, s)
	assert.Contains(t, err.Error(), "bad")
}

// TestSanitizerRuleOrder verifies that more specific rules (db_conn) win over
// generic ones (ipv4, password) when they overlap.
func TestSanitizerRuleOrder(t *testing.T) {
	s := NewSanitizer()

	// The db_conn rule should collapse the whole URL before the ipv4 / password
	// rules get a chance to fire on the host / password fragments.
	input := "postgres://alice:hunter2@10.0.0.1:5432/app"
	out := s.Sanitize(input)

	assert.Contains(t, out, RedactDBConn)
	// The bare password and IP should not appear as separate redactions
	// because the whole URL was collapsed first.
	assert.NotContains(t, out, "hunter2")
}
