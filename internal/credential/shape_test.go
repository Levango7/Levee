package credential

import (
	"errors"
	"strings"
	"testing"
)

// TestValidatePlaintextShapeKeepsTheBareSecretConvention: a value that does
// not open a JSON container is a bare secret — the resolver's documented
// fallback. api_token and every other type keeps that shape.
func TestValidatePlaintextShapeKeepsTheBareSecretConvention(t *testing.T) {
	ok := []struct{ label, credType, value string }{
		{"bare password", "ssh_password", "test-password-not-a-secret"},
		{"bare token", "api_token", "tok_abcdef1234567890"},
		{"password with braces but no JSON punctuation", "ssh_password", "{hunter2"},
		{"password with brackets but no JSON punctuation", "ssh_password", "[hunter2"},
		{"json-ish token for a non-transport type", "api_token", `{"account":"acme","scope":"read"}`},
		{"trimmed surrounding whitespace", "ssh_password", "   bare-with-space   "},
	}
	for _, tc := range ok {
		if err := validatePlaintextShape(tc.credType, []byte(tc.value)); err != nil {
			t.Errorf("%s: must be accepted, got %v", tc.label, err)
		}
	}
}

// TestValidatePlaintextShapeAcceptsTransportJSON: the documented JSON forms for
// the three transport credential types, including a key-carrying ssh_key.
func TestValidatePlaintextShapeAcceptsTransportJSON(t *testing.T) {
	ok := []struct{ label, credType, value string }{
		{"username+password", "ssh_password", `{"username":"root","password":"test-password-not-a-secret"}`},
		{"password only (bare-password fallback stays JSON-visible)", "ssh_password", `{"password":"x"}`},
		{"username+key_path", "ssh_key", `{"username":"root","key_path":"/root/.ssh/id_ed25519"}`},
		{"key with passphrase", "ssh_key", `{"username":"u","key_path":"/k","key_passphrase":"p"}`},
		{"winrm username+password", "winrm_password", `{"username":"Administrator","password":"p"}`},
		{"case-insensitive type", "SSH_Password", `{"username":"u","password":"p"}`},
		{"unrecognised extra field alongside a recognised one", "ssh_password", `{"username":"u","note":"prod"}`},
	}
	for _, tc := range ok {
		if err := validatePlaintextShape(tc.credType, []byte(tc.value)); err != nil {
			t.Errorf("%s: must be accepted, got %v", tc.label, err)
		}
	}
}

// TestValidatePlaintextShapeRefusesMangledJSON is the regression this file
// exists for: a quoting pass ate the double quotes. The store used to accept
// it silently, and the failure surfaced later as an SSH dial with an empty
// username. It must be refused at write time.
func TestValidatePlaintextShapeRefusesMangledJSON(t *testing.T) {
	bad := []struct{ label, credType, value string }{
		{"quotes eaten entirely (PowerShell in-line --value)", "ssh_password", `{username:root,password:test-password-not-a-secret}`},
		{"quotes eaten on some keys", "ssh_password", `{username:"root",password:x}`},
		{"trailing comma left behind", "ssh_password", `{"username":"root",}`},
		{"single quotes substituted", "ssh_password", `{'username':'root','password':'x'}`},
		{"unterminated object", "ssh_password", `{"username":"root","password":"x"`},
	}
	for _, tc := range bad {
		err := validatePlaintextShape(tc.credType, []byte(tc.value))
		if err == nil {
			t.Errorf("%s: must be refused", tc.label)
			continue
		}
		if !errors.Is(err, ErrMalformedCredentialJSON) {
			t.Errorf("%s: must wrap ErrMalformedCredentialJSON, got %v", tc.label, err)
		}
	}
}

// TestValidatePlaintextShapeRefusesUnusableJSONObjects: a value that parses as
// JSON but cannot serve the credential type. Without this rule a key typo
// becomes a bare secret and fails at dial time as an empty username.
//
// A JSON SCALAR that does not open a container (e.g. `7` or `"x"`) is not in
// this table: it is a bare secret by the first rule, and the resolver's
// fallback treats it as one, which is the documented convention.
func TestValidatePlaintextShapeRefusesUnusableJSONObjects(t *testing.T) {
	bad := []struct{ label, credType, value string }{
		{"typo'd key", "ssh_password", `{"user":"root","passwd":"x"}`},
		{"empty object", "ssh_password", `{}`},
		{"json array", "ssh_password", `["root","x"]`},
	}
	for _, tc := range bad {
		err := validatePlaintextShape(tc.credType, []byte(tc.value))
		if err == nil {
			t.Errorf("%s: must be refused", tc.label)
			continue
		}
		if !errors.Is(err, ErrCredentialJSONShape) {
			t.Errorf("%s: must wrap ErrCredentialJSONShape, got %v", tc.label, err)
		}
	}
}

// TestShapeErrorsNeverLeakTheSecret: the error text must not carry the value,
// because an error string is exactly where a secret gets copied into logs and
// tickets. The commands that print these errors write them to a terminal.
func TestShapeErrorsNeverLeakTheSecret(t *testing.T) {
	secrets := []string{"test-password-not-a-secret", "SuperSecretValue9", "tok_abcdef1234567890"}
	// Every entry here must be REFUSED — that is what makes the assertion
	// meaningful. The last one is a well-formed credential with its quotes
	// eaten, i.e. exactly the shape an operator produces by mistake.
	values := []string{
		`{username:root,password:test-password-not-a-secret}`,
		`{"user":"root","passwd":"SuperSecretValue9"}`,
		`{"username":"root","password":`,
		`{username:root,key_path:/k,key_passphrase:tok_abcdef1234567890}`,
	}
	for _, v := range values {
		err := validatePlaintextShape("ssh_password", []byte(v))
		if err == nil {
			t.Fatalf("expected a refusal for %q", v)
		}
		for _, s := range secrets {
			if strings.Contains(err.Error(), s) {
				t.Errorf("error for %q leaked %q: %v", v, s, err)
			}
		}
	}
}

// TestShapeErrorMessageNamesTheFix: the operator's next action has to be in
// the message — that is the whole point of refusing at write time.
func TestShapeErrorMessageNamesTheFix(t *testing.T) {
	err := validatePlaintextShape("ssh_password", []byte(`{username:root,password:x}`))
	if err == nil {
		t.Fatal("expected a refusal")
	}
	for _, want := range []string{"quoting", "username", "bare secret"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("message must name %q: %v", want, err)
		}
	}
}
