package credential

// Credential value-shape validation.
//
// WHY THIS EXISTS: the store accepted any non-empty plaintext. When a caller's
// shell quoting ate the double quotes out of a JSON credential — which
// PowerShell does to in-line arguments, so `--value {"username":"root",…}`
// arrives as `{username:root,…}` — the store stored the mangled text silently.
// The failure then surfaced much later, at an SSH dial with an EMPTY username,
// because the resolver's documented fallback treats unparseable JSON as a bare
// password. The operator goes hunting a password problem that is really a
// quoting problem, at a point where the evidence is gone.
//
// The rules keep BOTH documented conventions and refuse only the ambiguous
// middle:
//
//   - a value that does not start with '{' or '[' is a bare secret — accepted
//     verbatim (that is the resolver's fallback convention, still the only
//     documented shape for api_token);
//   - a value that starts with '{' or '[' and IS valid JSON is checked for the
//     recognised credential fields when the type carries a transport credential;
//   - a value that starts with '{' or '[', is NOT valid JSON, but carries JSON
//     punctuation (a colon or a double quote) is REFUSED. It is far more likely
//     a mangled JSON payload than a secret that merely begins with '{'. A bare
//     secret that starts with '{' without any JSON punctuation stays accepted,
//     so the rules cannot lock out an ordinary password.
//
// No error message embeds any part of the plaintext: the caller already knows
// what they typed, and an error string is exactly where a secret gets copied.

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// ErrMalformedCredentialJSON marks a value that looks like JSON, carries JSON
// punctuation, and does not parse. It is a quoting bug, not a secret.
var ErrMalformedCredentialJSON = errors.New("credential: value looks like JSON but is not valid JSON")

// ErrCredentialJSONShape marks a value that parses as JSON but cannot serve the
// credential type: a transport credential got a JSON array, or an object with
// none of the recognised field names.
var ErrCredentialJSONShape = errors.New("credential: JSON value has no usable credential field")

// transportCredentialFields are the field names channel.CredentialRef
// unmarshals from — the single source of truth for "which keys a transport
// credential may carry". transportCredentialTypes are the types whose value is
// a transport credential (JSON object or bare secret); other types (api_token)
// keep accepting any shape, including structured payloads we do not interpret.
var transportCredentialFields = []string{"username", "password", "key_path", "key_passphrase"}

func isTransportCredentialType(credType string) bool {
	switch strings.ToLower(strings.TrimSpace(credType)) {
	case "ssh_password", "ssh_key", "winrm_password":
		return true
	default:
		return false
	}
}

// startsLikeJSONObject reports whether the trimmed value opens a JSON
// container — the only case where a shape rule applies at all.
func startsLikeJSONObject(b []byte) bool {
	return len(b) > 0 && (b[0] == '{' || b[0] == '[')
}

// carriesJSONPunctuation reports whether the value contains a colon or a double
// quote: the characters a JSON object cannot do without and a bare secret
// usually does not have. This is what separates "mangled JSON" from "a
// password that happens to start with '{'".
func carriesJSONPunctuation(b []byte) bool {
	return bytes.ContainsAny(b, ":\"")
}

// validatePlaintextShape enforces the rules above. It returns nil for every
// shape the resolver can serve, including the bare-secret convention.
func validatePlaintextShape(credType string, plaintext []byte) error {
	trimmed := bytes.TrimSpace(plaintext)
	if !startsLikeJSONObject(trimmed) {
		return nil
	}

	if !json.Valid(trimmed) {
		if carriesJSONPunctuation(trimmed) {
			return fmt.Errorf("%w (the value opens a JSON container and carries ':' or '\"', "+
				"so a shell or PowerShell quoting pass very likely ate the double quotes): "+
				"write it as a JSON object such as {\"username\":\"…\",\"password\":\"…\"}, "+
				"or use a bare secret that does not open with '{' or '['",
				ErrMalformedCredentialJSON)
		}
		// No JSON punctuation: an ordinary secret that merely begins with '{'.
		return nil
	}

	if !isTransportCredentialType(credType) {
		return nil
	}
	if trimmed[0] == '[' {
		return fmt.Errorf("%w: a %s credential must be a JSON object or a bare secret, not a JSON array",
			ErrCredentialJSONShape, credType)
	}

	var obj map[string]json.RawMessage
	if err := json.Unmarshal(trimmed, &obj); err != nil {
		// Defensive: a value that opens with '{' and passes json.Valid is an
		// object, so this is unreachable through the public API today. Kept as
		// a guard rather than a silent fall-through, because "it cannot happen"
		// is a claim, and this one is cheap to keep honest.
		return fmt.Errorf("%w: a %s credential must be a JSON object or a bare secret, not a JSON scalar",
			ErrCredentialJSONShape, credType)
	}
	for key := range obj {
		for _, want := range transportCredentialFields {
			if strings.EqualFold(strings.TrimSpace(key), want) {
				return nil
			}
		}
	}
	return fmt.Errorf("%w: a %s credential's JSON object must carry at least one of %v "+
		"(a key typo would otherwise silently become a bare secret and fail later as an empty username)",
		ErrCredentialJSONShape, credType, transportCredentialFields)
}
