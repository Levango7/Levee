package push

import (
	"crypto/ecdsa"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"time"
)

// generateToken builds an ES256-signed JWT for APNs provider authentication.
// The header is {"alg":"ES256","kid":"<keyID>","typ":"JWT"} and the payload
// contains {"iss":"<teamID>","iat":<now>}. The token expires at now+apnsTokenTTL.
//
// We implement the JWS manually because the project deliberately avoids
// external JWT libraries; ES256 is small enough to inline.
func (c *APNSClient) generateToken() (string, time.Time, error) {
	now := time.Now()
	exp := now.Add(apnsTokenTTL)

	header := map[string]string{
		"alg": "ES256",
		"kid": c.keyID,
		"typ": "JWT",
	}
	payload := map[string]any{
		"iss": c.teamID,
		"iat": now.Unix(),
	}

	headerJSON, err := json.Marshal(header)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("push: apns: marshal jwt header: %w", err)
	}
	payloadJSON, err := json.Marshal(payload)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("push: apns: marshal jwt payload: %w", err)
	}

	headerB64 := base64.RawURLEncoding.EncodeToString(headerJSON)
	payloadB64 := base64.RawURLEncoding.EncodeToString(payloadJSON)
	signingInput := headerB64 + "." + payloadB64

	sig, err := ecdsa.SignASN1(rand.Reader, c.privateKey, []byte(signingInput))
	if err != nil {
		return "", time.Time{}, fmt.Errorf("push: apns: sign jwt: %w", err)
	}
	sigB64 := base64.RawURLEncoding.EncodeToString(sig)

	return signingInput + "." + sigB64, exp, nil
}

// verifyTokenForTest verifies a JWT signature using the public key. It is
// intended for unit tests that want to assert the token is well-formed without
// calling Apple. The function is exported only within the package (lowercase)
// to keep the public API clean.
func (c *APNSClient) verifyTokenForTest(token string) error {
	parts := splitJWT(token)
	if len(parts) != 3 {
		return fmt.Errorf("push: apns: token has %d parts, want 3", len(parts))
	}
	signingInput := parts[0] + "." + parts[1]
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return fmt.Errorf("push: apns: decode signature: %w", err)
	}
	if !ecdsa.VerifyASN1(&c.privateKey.PublicKey, []byte(signingInput), sig) {
		return fmt.Errorf("push: apns: signature verification failed")
	}
	return nil
}

// splitJWT splits a compact JWT into its three base64 parts. Empty parts are
// preserved so the caller can detect malformed input.
func splitJWT(token string) []string {
	var parts []string
	start := 0
	for i := 0; i < len(token); i++ {
		if token[i] == '.' {
			parts = append(parts, token[start:i])
			start = i + 1
		}
	}
	parts = append(parts, token[start:])
	return parts
}
