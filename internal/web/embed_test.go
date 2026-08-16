package web

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
)

// TestHandler_ServesIndex verifies that the root path serves index.html.
func TestHandler_ServesIndex(t *testing.T) {
	fsys := fstest.MapFS{
		"index.html": &fstest.MapFile{
			Data: []byte("<html><body>SPA</body></html>"),
		},
	}
	srv := httptest.NewServer(Handler(fsys))
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "SPA") {
		t.Errorf("expected SPA shell, got %q", body)
	}
	if resp.Header.Get("Content-Type") == "" {
		t.Errorf("missing content-type")
	}
}

// TestHandler_SpaFallback verifies that an unknown non-asset path falls back
// to index.html so client-side routing works on refresh.
func TestHandler_SpaFallback(t *testing.T) {
	fsys := fstest.MapFS{
		"index.html": &fstest.MapFile{Data: []byte("<html>SPA</html>")},
	}
	srv := httptest.NewServer(Handler(fsys))
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/changes/abc-123")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("expected 200 for SPA route, got %d", resp.StatusCode)
	}
}

// TestHandler_AssetNotFound verifies that a missing asset returns 404 rather
// than the SPA shell, so the browser does not try to parse HTML as JS.
func TestHandler_AssetNotFound(t *testing.T) {
	fsys := fstest.MapFS{
		"index.html": &fstest.MapFile{Data: []byte("<html>SPA</html>")},
	}
	srv := httptest.NewServer(Handler(fsys))
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/assets/missing.js")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("expected 404 for missing asset, got %d", resp.StatusCode)
	}
}

// TestHandler_ApiPathNotServed verifies that /api/* is never handled by the
// SPA handler; the caller is expected to mount the gateway on the same mux.
func TestHandler_ApiPathNotServed(t *testing.T) {
	fsys := fstest.MapFS{
		"index.html": &fstest.MapFile{Data: []byte("<html>SPA</html>")},
	}
	srv := httptest.NewServer(Handler(fsys))
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/api/v1/changes")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("expected 404 for /api path, got %d", resp.StatusCode)
	}
}

// TestHandler_ServesRealAsset verifies that a real asset is served with the
// correct content.
func TestHandler_ServesRealAsset(t *testing.T) {
	fsys := fstest.MapFS{
		"index.html":    &fstest.MapFile{Data: []byte("<html>SPA</html>")},
		"assets/app.js": &fstest.MapFile{Data: []byte("console.log('app')")},
	}
	srv := httptest.NewServer(Handler(fsys))
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/assets/app.js")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "console.log") {
		t.Errorf("expected asset content, got %q", body)
	}
}

// TestLooksLikeAsset covers the asset classification heuristic.
func TestLooksLikeAsset(t *testing.T) {
	cases := []struct {
		path string
		want bool
	}{
		{"/assets/app.js", true},
		{"/style.css", true},
		{"/logo.svg", true},
		{"/favicon.ico", true},
		{"/changes/123", false},
		{"/api/v1/changes", false},
		{"/", false},
	}
	for _, c := range cases {
		if got := looksLikeAsset(c.path); got != c.want {
			t.Errorf("looksLikeAsset(%q) = %v, want %v", c.path, got, c.want)
		}
	}
}