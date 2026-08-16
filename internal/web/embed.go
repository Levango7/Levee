// Package web embeds the LEVEE Web UI static assets into the Go binary and
// exposes them through an http.Handler. The handler serves the SPA with
// proper fallback: any path that does not match a real asset and does not
// start with /api/ or /events/ is rewritten to index.html so that client-
// side routing works on a fresh page load.
//
// The embedded directory is internal/web/dist/. A committed placeholder
// index.html keeps `go build` working in a fresh checkout; the Makefile
// `web` target overwrites dist/ with the real `npm run build` output before
// release builds.
package web

import (
	"embed"
	"errors"
	"io"
	"io/fs"
	"net/http"
	"strings"
)

// errNotSeekable is returned by readSeeker.Seek when the underlying fs.File
// does not implement io.Seeker. http.ServeContent tolerates this by skipping
// range requests, which is fine for the SPA shell.
var errNotSeekable = errors.New("file is not seekable")

// distFS holds the embedded static files. The `all:` prefix includes files
// starting with `_` or `.`, which Vite may emit. The directive requires
// internal/web/dist/ to exist at compile time; the committed placeholder
// index.html guarantees that.
//
//go:embed all:dist
var distFS embed.FS

// Handler returns an http.Handler that serves the embedded Web UI. The
// returned handler performs SPA fallback: requests that do not match an
// embedded file and do not start with /api/ or /events/ are rewritten to
// /index.html so the Vue router can take over.
//
// Passing a non-nil override replaces the embedded file system. This is
// intended for tests that want to serve a synthetic fixture; production
// callers should pass nil.
func Handler(override ...fs.FS) http.Handler {
	fsys, err := fs.Sub(distFS, "dist")
	if err != nil {
		// Should never happen: the directive above guarantees the
		// directory exists at compile time.
		panic(err)
	}
	if len(override) > 0 && override[0] != nil {
		fsys = override[0]
	}
	return spaHandler{fsys: fsys}
}

// spaHandler is the SPA-aware file server. It tries to serve the requested
// path from the embedded FS; if the path is missing and is not an API or
// asset path, it serves index.html so the Vue router can take over.
type spaHandler struct {
	fsys fs.FS
}

// ServeHTTP implements http.Handler.
func (h spaHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// API and event streams are handled by other mux entries; never fall
	// through to the SPA for them.
	if strings.HasPrefix(r.URL.Path, "/api/") || strings.HasPrefix(r.URL.Path, "/events/") {
		http.NotFound(w, r)
		return
	}

	clean := strings.TrimPrefix(r.URL.Path, "/")
	if clean == "" {
		clean = "index.html"
	}

	// Try the requested file first.
	if f, err := h.fsys.Open(clean); err == nil {
		defer f.Close()
		stat, err := f.Stat()
		if err == nil && !stat.IsDir() {
			http.ServeContent(w, r, stat.Name(), stat.ModTime(), readSeeker{f})
			return
		}
	}

	// SPA fallback: serve index.html for any unknown non-asset path.
	if !looksLikeAsset(r.URL.Path) {
		if f, err := h.fsys.Open("index.html"); err == nil {
			defer f.Close()
			stat, err := f.Stat()
			if err == nil {
				http.ServeContent(w, r, "index.html", stat.ModTime(), readSeeker{f})
				return
			}
		}
	}

	http.NotFound(w, r)
}

// looksLikeAsset reports whether the path has a static-asset extension. We
// use this to decide between SPA fallback (return index.html) and a hard 404
// (a missing CSS/JS file should not return the HTML shell).
func looksLikeAsset(path string) bool {
	for _, suffix := range []string{
		".js", ".css", ".svg", ".png", ".jpg", ".jpeg", ".gif",
		".ico", ".woff", ".woff2", ".ttf", ".eot", ".map",
	} {
		if strings.HasSuffix(path, suffix) {
			return true
		}
	}
	return false
}

// readSeeker adapts an fs.File to an io.ReadSeeker for http.ServeContent.
// Most embed.FS files already implement ReadSeeker; this wrapper exists so
// the call site compiles even for stub file systems that only expose
// io.Reader. If the underlying file does not implement Seek, ServeContent
// will skip range requests — acceptable for the SPA shell.
type readSeeker struct {
	f fs.File
}

func (r readSeeker) Read(p []byte) (int, error) { return r.f.Read(p) }

func (r readSeeker) Seek(offset int64, whence int) (int64, error) {
	s, ok := r.f.(io.Seeker)
	if !ok {
		return 0, errNotSeekable
	}
	return s.Seek(offset, whence)
}
