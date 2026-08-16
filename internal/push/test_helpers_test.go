package push

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"testing"
)

// generateTestECDSAKey produces a fresh P-256 key and returns its PEM-encoded
// PKCS8 form. Used by APNs tests to avoid committing a long-lived key.
func generateTestECDSAKey(t *testing.T) ([]byte, *ecdsa.PrivateKey) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate ecdsa key: %v", err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatalf("marshal pkcs8: %v", err)
	}
	block := &pem.Block{Type: "PRIVATE KEY", Bytes: der}
	return pem.EncodeToMemory(block), key
}

// generateTestRSAKey produces a fresh 2048-bit RSA key and returns its
// PEM-encoded PKCS1 form, suitable for the FCM service-account JSON.
func generateTestRSAKey(t *testing.T) ([]byte, *rsa.PrivateKey) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate rsa key: %v", err)
	}
	der := x509.MarshalPKCS1PrivateKey(key)
	block := &pem.Block{Type: "RSA PRIVATE KEY", Bytes: der}
	return pem.EncodeToMemory(block), key
}

// buildServiceAccountJSON builds a minimal Google service-account JSON document
// using the supplied PEM-encoded RSA key. The token_uri is set to the supplied
// endpoint so tests can point at an httptest.Server.
func buildServiceAccountJSON(t *testing.T, rsaPEM []byte, projectID, tokenURI string) []byte {
	t.Helper()
	// Marshal the PEM string as a proper JSON string to escape newlines.
	pemJSON, err := json.Marshal(string(rsaPEM))
	if err != nil {
		t.Fatalf("marshal pem: %v", err)
	}
	return []byte(`{
  "type": "service_account",
  "project_id": "` + projectID + `",
  "private_key_id": "test-key-id",
  "private_key": ` + string(pemJSON) + `,
  "client_email": "levee-test@` + projectID + `.iam.gserviceaccount.com",
  "client_id": "test-client-id",
  "auth_uri": "https://accounts.google.com/o/oauth2/auth",
  "token_uri": "` + tokenURI + `",
  "auth_provider_x509_cert_url": "https://www.googleapis.com/oauth2/v1/certs",
  "client_x509_cert_url": "https://www.googleapis.com/robot/v1/metadata/x509/levee-test%40` + projectID + `.iam.gserviceaccount.com"
}`)
}
