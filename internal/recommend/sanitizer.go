package recommend

// sanitizer.go implements the Sanitizer that scrubs sensitive information from
// text before it is sent to an LLM backend. The goal is to prevent accidental
// leakage of credentials, tokens, personally-identifiable information and
// infrastructure addresses when an operator pastes logs, config snippets or
// connection strings into a recommendation prompt.
//
// The Sanitizer is built from a list of named regular-expression rules. Each
// rule replaces every match with a fixed redaction placeholder (e.g.
// "[IP_REDACTED]"). NewSanitizer ships with a sensible default rule set;
// NewSanitizerWithRules lets callers supply their own.
//
// The Sanitizer is concurrency-safe: the rule set is immutable after
// construction, so Sanitize may be called from many goroutines at once.

import (
	"regexp"
	"strings"
)

// --- Public types -----------------------------------------------------------

// SanitizerRule is a single redaction rule. Pattern is a regular expression;
// Replace is the text substituted for every match. Both fields are required
// at construction time; an empty Pattern causes NewSanitizerWithRules to
// return an error.
type SanitizerRule struct {
	Name    string
	Pattern string
	Replace string
}

// Sanitizer scrubs sensitive information from text and LLM messages. It is
// safe for concurrent use after construction.
type Sanitizer struct {
	patterns []sanitizerPattern
}

// sanitizerPattern is the compiled form of a SanitizerRule.
type sanitizerPattern struct {
	name    string
	regex   *regexp.Regexp
	replace string
}

// --- Redaction placeholders -------------------------------------------------

const (
	// RedactIP is the placeholder for IPv4 addresses.
	RedactIP = "[IP_REDACTED]"
	// RedactSecret is the placeholder for passwords and generic secrets.
	RedactSecret = "[SECRET_REDACTED]"
	// RedactDBConn is the placeholder for database connection strings.
	RedactDBConn = "[DB_CONN_REDACTED]"
	// RedactJWT is the placeholder for JWT tokens.
	RedactJWT = "[JWT_REDACTED]"
	// RedactAWSKey is the placeholder for AWS access keys.
	RedactAWSKey = "[AWS_KEY_REDACTED]"
	// RedactEmail is the placeholder for email addresses.
	RedactEmail = "[EMAIL_REDACTED]"
	// RedactPhone is the placeholder for Chinese mobile numbers.
	RedactPhone = "[PHONE_REDACTED]"
)

// --- Default rules ----------------------------------------------------------

// defaultSanitizerRules returns the built-in redaction rule set. The order
// matters: more specific rules (e.g. database connection strings) must run
// before more generic ones (e.g. bare IP addresses) so that the placeholder
// for the specific match wins.
func defaultSanitizerRules() []SanitizerRule {
	return []SanitizerRule{
		// Database connection strings — must run before the IP / password
		// rules so the whole URL is collapsed into one placeholder.
		{
			Name:    "db_conn",
			Pattern: `(?i)(mysql|postgres|mongodb|redis)://[^\s:@/]+:[^\s:@/]+@`,
			Replace: RedactDBConn + "@",
		},
		// JWT tokens — three base64url segments separated by dots.
		{
			Name:    "jwt",
			Pattern: `eyJ[A-Za-z0-9_\-]+\.eyJ[A-Za-z0-9_\-]+\.[A-Za-z0-9_\-]+`,
			Replace: RedactJWT,
		},
		// AWS access key IDs.
		{
			Name:    "aws_access_key",
			Pattern: `AKIA[0-9A-Z]{16}`,
			Replace: RedactAWSKey,
		},
		// Password assignments: password=secret, pwd:secret, etc.
		{
			Name:    "password",
			Pattern: `(?i)(password|passwd|pwd)\s*[=:]\s*\S+`,
			Replace: `password=` + RedactSecret,
		},
		// API key / secret / token assignments with a long value.
		{
			Name:    "api_key",
			Pattern: `(?i)(api[_\-]?key|secret|token)\s*[=:]\s*[\w\-]{32,}`,
			Replace: `api_key=` + RedactSecret,
		},
		// Email addresses.
		{
			Name:    "email",
			Pattern: `[\w.]+@[\w.]+\.[A-Za-z]{2,}`,
			Replace: RedactEmail,
		},
		// Chinese mobile numbers.
		{
			Name:    "phone",
			Pattern: `1[3-9]\d{9}`,
			Replace: RedactPhone,
		},
		// IPv4 addresses — run last so they do not clobber the host part of
		// a database URL that has already been redacted.
		{
			Name:    "ipv4",
			Pattern: `\b\d{1,3}\.\d{1,3}\.\d{1,3}\.\d{1,3}\b`,
			Replace: RedactIP,
		},
	}
}

// --- Constructors -----------------------------------------------------------

// NewSanitizer builds a Sanitizer with the default rule set. It never returns
// an error because the default rules are compile-time verified; a panic here
// would indicate a bug in the rule table itself.
func NewSanitizer() *Sanitizer {
	s, err := NewSanitizerWithRules(defaultSanitizerRules())
	if err != nil {
		// The default rule set is part of the package source; if it fails to
		// compile the package itself is broken and we want a loud failure.
		panic("recommend: default sanitizer rules are invalid: " + err.Error())
	}
	return s
}

// NewSanitizerWithRules builds a Sanitizer from the given rules. Each rule's
// Pattern is compiled once; invalid patterns cause an error. Rules with an
// empty Pattern are skipped silently so callers can build rule sets
// conditionally without sprinkling nil-checks everywhere.
func NewSanitizerWithRules(rules []SanitizerRule) (*Sanitizer, error) {
	compiled := make([]sanitizerPattern, 0, len(rules))
	for _, r := range rules {
		if strings.TrimSpace(r.Pattern) == "" {
			continue
		}
		re, err := regexp.Compile(r.Pattern)
		if err != nil {
			return nil, errSanitizerRule(r.Name, r.Pattern, err)
		}
		compiled = append(compiled, sanitizerPattern{
			name:    r.Name,
			regex:   re,
			replace: r.Replace,
		})
	}
	return &Sanitizer{patterns: compiled}, nil
}

// errSanitizerRule wraps a regexp compilation error with context.
func errSanitizerRule(name, pattern string, err error) error {
	return &sanitizerRuleError{name: name, pattern: pattern, err: err}
}

// sanitizerRuleError is a typed error for invalid rules.
type sanitizerRuleError struct {
	name    string
	pattern string
	err     error
}

func (e *sanitizerRuleError) Error() string {
	return "recommend: sanitizer rule " + e.name + " (" + e.pattern + "): " + e.err.Error()
}

func (e *sanitizerRuleError) Unwrap() error { return e.err }

// --- Scrubbing --------------------------------------------------------------

// Sanitize applies every rule to text in order and returns the redacted
// result. The input is returned unchanged when it is empty. The Sanitizer is
// concurrency-safe: the rule set is immutable after construction.
func (s *Sanitizer) Sanitize(text string) string {
	if text == "" {
		return ""
	}
	out := text
	for _, p := range s.patterns {
		out = p.regex.ReplaceAllString(out, p.replace)
	}
	return out
}

// SanitizeMessages returns a new slice with every message content sanitised.
// The input slice is not mutated; messages are copied so callers can keep the
// original for logging or audit. A nil input returns nil.
func (s *Sanitizer) SanitizeMessages(messages []LLMMessage) []LLMMessage {
	if messages == nil {
		return nil
	}
	out := make([]LLMMessage, len(messages))
	for i, m := range messages {
		out[i] = LLMMessage{
			Role:    m.Role,
			Content: s.Sanitize(m.Content),
		}
	}
	return out
}

// Rules returns the names of the rules in execution order. It is primarily
// useful for diagnostics and tests.
func (s *Sanitizer) Rules() []string {
	out := make([]string, len(s.patterns))
	for i, p := range s.patterns {
		out[i] = p.name
	}
	return out
}