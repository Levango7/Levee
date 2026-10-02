package grpc

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The gateway accepts two URL shapes and the frontend only ever sends one of
// them with the /api/v1 prefix. Before this file, the prefixed shape was
// mounted on a router that demanded /<Service>/<Method>, so every dashboard
// read answered 400 while the tests stayed green — the test harness hand-copied
// the two mounts instead of mounting production's pipeline.
//
// startTestGateway now mounts gw.dataHandler(), so these tests exercise the
// same wiring that answers real traffic.

func TestBothURLShapesResolveToTheSameHandler(t *testing.T) {
	_, srv, _ := startTestGateway(t, ServeGatewayConfig{})

	cases := []struct {
		bare     string
		prefixed string
		want     int
	}{
		{"/changes", "/api/v1/changes", http.StatusOK},
		{"/system/status", "/api/v1/system/status", http.StatusOK},
		{"/ChangeService/ListChanges", "/api/v1/ChangeService/ListChanges", http.StatusOK},
		{"/SystemService/GetVersion", "/api/v1/SystemService/GetVersion", http.StatusOK},
	}
	for _, tc := range cases {
		t.Run(tc.bare, func(t *testing.T) {
			bareStatus := getStatus(t, srv.URL+tc.bare)
			prefixedStatus := getStatus(t, srv.URL+tc.prefixed)
			assert.Equal(t, tc.want, bareStatus, "bare path %s", tc.bare)
			assert.Equal(t, tc.want, prefixedStatus, "prefixed path %s", tc.prefixed)
		})
	}

	// POST-only routes through the prefix too (the SPA's apply/rollback buttons
	// hit /api/v1/changes/<id>/apply).
	resp, err := http.Post(srv.URL+"/api/v1/changes/missing/apply", "application/json", strings.NewReader("{}"))
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	assert.NotEqual(t, http.StatusBadRequest, resp.StatusCode,
		"a prefixed RESTful POST must not be rejected as an unroutable path")
}

func TestUnroutableShapesKeepGivingActionableErrors(t *testing.T) {
	_, srv, _ := startTestGateway(t, ServeGatewayConfig{})

	// A segment that looks like a service but is not registered is a bad API
	// path, so it must say "unknown service" rather than masquerading as a
	// missing resource.
	for _, p := range []string{"/BogusService/Method", "/api/v1/BogusService/Method"} {
		status, body := get(t, srv.URL+p)
		assert.Equal(t, http.StatusBadRequest, status, p)
		assert.Contains(t, body, "unknown service", p)
	}

	// An unknown RESTful resource is a missing path.
	for _, p := range []string{"/nonsense", "/api/v1/nonsense"} {
		status, body := get(t, srv.URL+p)
		assert.Equal(t, http.StatusNotFound, status, p)
		assert.Contains(t, body, "not found", p)
	}
}

// TestPublicPathsArePublicUnderBothShapes covers the second half of the same
// bug: the bearer middleware compares bare paths (/system/auth-info,
// /changes/deeplink/*) to decide what is public, so with the prefix left in
// place the login page could not even ask the server what login methods it
// offers — it was told to authenticate first.
func TestPublicPathsArePublicUnderBothShapes(t *testing.T) {
	_, srv, _ := startTestGateway(t, ServeGatewayConfig{AuthToken: "public-path-test"})

	for _, p := range []string{
		"/system/auth-info", "/api/v1/system/auth-info",
		"/auth/github", "/api/v1/auth/github",
	} {
		req, err := http.NewRequest(http.MethodGet, srv.URL+p, nil)
		require.NoError(t, err)
		resp, err := http.DefaultClient.Do(req) // no Authorization header on purpose
		require.NoError(t, err)
		defer func() { _ = resp.Body.Close() }()
		data, rerr := io.ReadAll(resp.Body)
		require.NoError(t, rerr)
		assert.NotEqual(t, http.StatusUnauthorized, resp.StatusCode,
			"%s must reach the handler without a credential (body: %s)", p, string(data))
	}
}

// TestFrontendApiPathsAreAllRoutable is the gate that was missing. It reads the
// paths the SPA actually requests out of web/src/api and pins that every one of
// them is recognized by the gateway's own tables.
//
// Checking against the router's tables (rather than a hand-written list) is
// deliberate: a list in the test would be a fifth copy of the vocabulary, and a
// copy is how the two sides disagreed in the first place.
func TestFrontendApiPathsAreAllRoutable(t *testing.T) {
	apiDir := filepath.Join("..", "..", "web", "src", "api")
	if _, err := os.Stat(apiDir); err != nil {
		t.Skipf("no frontend sources at %s; nothing to cross-check", apiDir)
	}

	routable := map[string]bool{}
	for name := range (&Gateway{}).grpcServiceHandlers() {
		routable[name] = true
	}
	for name := range (&Gateway{}).restResourceHandlers() {
		routable[name] = true
	}

	// The frontend's wrappers are generic (`get<Template>('/templates/x')`), so
	// the verb is optionally followed by a type argument before the paren.
	// A regex that only matched `get('/x')` silently missed every typed call
	// site — which is the same failure mode this whole gate exists to catch.
	callRE := regexp.MustCompile(`(?m)\b(get|post|put|patch|del)(?:<[^()\n]*>)?\(\s*[` + "`" + `']([^` + "`" + `']+)[` + "`" + `']`)
	var checked int
	var unrouted []string
	found := map[string]bool{}

	require.NoError(t, filepath.Walk(apiDir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || !strings.HasSuffix(path, ".ts") ||
			strings.HasSuffix(path, ".spec.ts") {
			return err
		}
		src, rerr := os.ReadFile(path)
		if rerr != nil {
			return nil
		}
		for _, m := range callRE.FindAllStringSubmatch(string(src), -1) {
			p := m[2]
			if !strings.HasPrefix(p, "/") {
				continue // relative to the axios baseURL but not a path root
			}
			checked++
			rel := apiRelativePath(p)
			first := strings.Trim(rel, "/")
			if i := strings.Index(first, "/"); i >= 0 {
				first = first[:i]
			}
			found[first] = true
			if first == "" || !routable[first] {
				unrouted = append(unrouted, fmt.Sprintf("%s: %s (%s)", filepath.Base(path), p, m[1]))
			}
		}
		return nil
	}))

	assert.Empty(t, unrouted, "frontend requests the gateway cannot route")

	// The extraction has to keep reading the frontend's call sites, or the
	// assertion above would be vacuously green. Measured on the current tree:
	// 44 call sites in web/src/api (the wrappers are generic, `get<T>('/x')`,
	// so the regex must allow a type argument — matching only `get('/x')` found
	// 10 and silently skipped the typed majority). The floor sits below the
	// observation with room for refactors but far above "the regex broke".
	assert.GreaterOrEqual(t, checked, 30,
		"the regex must keep finding the frontend's API calls (found %d)", checked)
	for _, critical := range []string{"changes", "targets", "templates", "audit", "system", "conversation"} {
		assert.True(t, found[critical],
			"the extraction no longer sees any frontend call under /%s — check web/src/api and this regex together",
			critical)
	}
	t.Logf("cross-checked %d frontend call sites against the gateway tables", checked)
}

// TestRelativePathHelperIsPrefixAgnostic pins the one function every router now
// depends on.
func TestRelativePathHelperIsPrefixAgnostic(t *testing.T) {
	cases := map[string]string{
		"/api/v1/changes":                   "changes",
		"/changes":                          "changes",
		"/api/v1/changes/run-1/logs":        "changes/run-1/logs",
		"/api/v1/ChangeService/ListChanges": "ChangeService/ListChanges",
		"/api/v1":                           "",
		"/api/v1/":                          "",
		"/api/v1x/changes":                  "api/v1x/changes", // not a prefix, not stripped
		"/api/v1/system/auth-info":          "system/auth-info",
		"/changes/deeplink/approve":         "changes/deeplink/approve",
		"/api/v1/api/v1/changes":            "api/v1/changes", // strips once, deliberately
	}
	for in, want := range cases {
		assert.Equal(t, want, apiRelativePath(in), "apiRelativePath(%q)", in)
	}
}

func TestGatewayRouterTablesCoverTheDocumentedServices(t *testing.T) {
	gw := &Gateway{}
	services := make([]string, 0, len(gw.grpcServiceHandlers()))
	for name := range gw.grpcServiceHandlers() {
		services = append(services, name)
	}
	sort.Strings(services)
	assert.Equal(t, []string{
		"AlertService", "AuditService", "ChangeService", "ConversationService",
		"DiagnosisService", "SystemService", "TargetService", "TemplateService",
	}, services, "the gateway must route every service serve registers; check cmd_serve.go "+
		"and RegisterServices when this list changes")

	resources := make([]string, 0, len(gw.restResourceHandlers()))
	for name := range gw.restResourceHandlers() {
		resources = append(resources, name)
	}
	sort.Strings(resources)
	assert.Equal(t, []string{
		"audit", "auth", "changes", "conversation", "gates", "system", "targets", "templates",
	}, resources, "RESTful resource families")
}

// --- helpers ----------------------------------------------------------------

func get(t *testing.T, url string) (int, string) {
	t.Helper()
	resp, err := http.Get(url)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	return resp.StatusCode, string(data)
}

func getStatus(t *testing.T, url string) int {
	t.Helper()
	status, _ := get(t, url)
	return status
}
