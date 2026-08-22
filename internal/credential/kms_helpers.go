// Shared helpers for the KMS providers. Kept in a separate file so the
// provider implementations stay focused on their respective SDKs.

package credential

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"net/http"
	"os"
)

// newInsecureTransport returns an *http.Transport that skips certificate
// verification. It is intended for development and tests only; production
// callers must supply a real CA bundle.
//
//nolint:unused // test-only helper, referenced in _test.go files
func newInsecureTransport() *http.Transport {
	return &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
	}
}

// loadCACertPool loads a PEM-encoded CA certificate file into a
// *x509.CertPool. The file is read once and the pool is returned; the
// file is not kept open.
func loadCACertPool(path string) (*x509.CertPool, error) {
	pem, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pem) {
		return nil, errInvalidPEM
	}
	return pool, nil
}

// errInvalidPEM is returned by loadCACertPool when the PEM file does not
// contain any parseable certificates.
var errInvalidPEM = jsonInvalidPEMError{}

type jsonInvalidPEMError struct{}

func (jsonInvalidPEMError) Error() string { return "credential: no certificates parsed from PEM file" }

// jsonMarshalImpl is the concrete implementation behind jsonMarshal. It
// exists so vault_provider.go can keep its encoding/json import localised
// to the fallback path.
func jsonMarshalImpl(v any) ([]byte, error) {
	return json.Marshal(v)
}
