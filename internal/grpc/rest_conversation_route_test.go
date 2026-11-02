// rest_conversation_route_test.go pins the CONVERSATION session routes in three
// places at once: the dispatch code, the §13.7 documentation, and the wire.
//
// Why this exists: these five endpoints are REST-only (not ConversationService
// RPCs), so the proto-driven guards that cover the rest of the surface —
// rest_exposure_matrix_test.go enumerates RPCs from the proto descriptors, and
// rest_endpoint_table_test.go reconciles the §13.2 table against
// restResourceHandlers — never see them. The nearest existing coverage
// (rest_conversation_owner_test.go) asserts OWNERSHIP outcomes: it goes red when
// ownership rules break, but it stays green when a route quietly stops being
// routed, and it never reads the docs.
//
// The three legs are deliberately independent:
//
//   - source: the dispatcher must contain the literals that make each shape
//     routable (dropping one turns a documented endpoint into a 404 nothing);
//   - docs: §13.7 must describe each shape (an endpoint nobody wrote down is
//     an endpoint only its author knows about);
//   - wire: each legal shape must produce its success code against a REAL
//     session, and shapes the dispatcher does not model must be distinguishable
//     from method errors — see TestConversationRoutesAreWired for why "not 404"
//     is not the assertion.
package grpc

import (
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// conversationRoutes are the five (method, path) pairs §13.7 documents. The
// dispatchAs column is the literal dispatchConversation must contain for that
// shape to be routable at all; docAnchor is the backticked invocation §13.7
// uses. Keeping all three in one row means an editor who adds, removes or
// renames a shape updates the code, the docs and this table together, or the
// test says which one was forgotten.
type convRoute struct {
	method     string
	pathShape  string // as written in prose: {id} is the placeholder
	dispatchAs string // the Go string literal inside dispatchConversation
	docAnchor  string // the backticked invocation §13.7 uses
	why        string
}

var conversationRoutes = []convRoute{
	// docAnchor is the whole backticked invocation as §13.7 writes it — the
	// method INSIDE the backticks, because that is how the section lists them.
	// (An earlier version put the method outside the backticks and every row
	// failed against the real prose.)
	{http.MethodPost, "/conversation/sessions", `"/conversation/sessions"`,
		"`POST /conversation/sessions`", "create a session"},
	{http.MethodGet, "/conversation/sessions", `"/conversation/sessions"`,
		"`GET /conversation/sessions`", "list the caller's sessions"},
	{http.MethodGet, "/conversation/sessions/{id}", `"/conversation/sessions/"`,
		"`GET /conversation/sessions/{id}`", "read one session"},
	{http.MethodDelete, "/conversation/sessions/{id}", `"/conversation/sessions/"`,
		"`DELETE /conversation/sessions/{id}`", "close a session (204)"},
	{http.MethodPost, "/conversation/sessions/{id}/messages", `"/messages"`,
		"`POST /conversation/sessions/{id}/messages`", "post a message, get the {reply} envelope"},
}

// TestConversationRoutesAreDispatchable reads the dispatcher's own source and
// requires every shape's literal to be there. A source scan rather than a route
// table because the dispatcher is string arithmetic over the path — there is no
// registry to enumerate, which is exactly why a deleted case is invisible to
// every test that drives the wire with a valid request (it would just 404).
func TestConversationRoutesAreDispatchable(t *testing.T) {
	body := dispatchConversationSource(t)
	for _, r := range conversationRoutes {
		assert.Contains(t, body, r.dispatchAs,
			"%s %s must stay routable: dispatchConversation no longer contains %s (%s)",
			r.method, r.pathShape, r.dispatchAs, r.why)
	}
}

// TestConversationRoutesAreDocumented requires §13.7 to name each shape. The
// anchors are the backticked invocations the section already uses, so this
// fails when prose is dropped rather than when wording changes.
func TestConversationRoutesAreDocumented(t *testing.T) {
	section := docSection(t, "docs/levee-api.md", "### 13.7", "### 13.8")
	require.NotEmpty(t, section, "§13.7 must exist and be non-empty")
	for _, r := range conversationRoutes {
		assert.Contains(t, section, r.docAnchor,
			"§13.7 must describe %s %s — these are REST-only endpoints, nothing else records them",
			r.method, r.pathShape)
	}
}

// TestConversationRoutesAreWired drives the real middleware chain.
//
// Why it creates a session first, and why "not 404" would have been wrong: the
// dispatcher's answer to an unknown path is 404, but so is a handler's answer to
// a session id it cannot find ("session not found", rest.go) — and
// DELETE /conversation/sessions/{nonexistent} is a routed request that 404s.
// The first version of this test asserted NotEqual(404) on an arbitrary id and
// went red on the DELETE row for exactly that reason. So the positive assertion
// is the success code against a REAL session, and the negative cases are
// separated by what they answer: an unmodelled sub-path 404s from the dispatcher
// (and its message names the path), while a wrong verb on a real path 405s.
func TestConversationRoutesAreWired(t *testing.T) {
	cfg := ServeGatewayConfig{Addr: "127.0.0.1:0"}
	_, srv := startConversationGateway(t, cfg)

	// A fresh session per case that consumes it, so the order of the cases
	// cannot make a later one fail for lack of a predecessor.
	newID := func(t *testing.T) string {
		t.Helper()
		dto := createConvSession(t, srv, "", `{"user_id":"route-probe"}`)
		require.NotEmpty(t, dto.ID)
		return dto.ID
	}

	// POST /conversation/sessions — create.
	resp := doConvReq(t, http.MethodPost, srv.URL+"/conversation/sessions", "", `{"user_id":"route-probe"}`)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.NoError(t, resp.Body.Close())

	// GET /conversation/sessions — list. The query carries the caller: with no
	// authenticated subject (development mode) the list endpoint requires
	// ?user_id= and answers 400 without it, so the probe names one. That is a
	// documented contract of the section (§13.7 P2-2 fallback), not a workaround.
	resp = doConvReq(t, http.MethodGet, srv.URL+"/conversation/sessions?user_id=route-probe", "", "")
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.NoError(t, resp.Body.Close())

	// GET /conversation/sessions/{id} — read.
	readID := newID(t)
	resp = doConvReq(t, http.MethodGet, srv.URL+"/conversation/sessions/"+readID, "", "")
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.NoError(t, resp.Body.Close())

	// POST /conversation/sessions/{id}/messages — reply envelope.
	msgID := newID(t)
	resp = doConvReq(t, http.MethodPost, srv.URL+"/conversation/sessions/"+msgID+"/messages", "",
		`{"user_id":"route-probe","text":"/help"}`)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.NoError(t, resp.Body.Close())

	// DELETE /conversation/sessions/{id} — close, 204.
	delID := newID(t)
	resp = doConvReq(t, http.MethodDelete, srv.URL+"/conversation/sessions/"+delID, "", "")
	require.Equal(t, http.StatusNoContent, resp.StatusCode)
	require.NoError(t, resp.Body.Close())

	// Negative 1: a sub-path the dispatcher does not model. Its 404 comes from
	// dispatchConversation itself and the message names the path — that is how
	// a reader tells "route absent" from "session absent".
	resp = doConvReq(t, http.MethodGet, srv.URL+"/conversation/sessions/"+readID+"/bogus", "", "")
	body, readErr := io.ReadAll(resp.Body)
	code := resp.StatusCode
	require.NoError(t, resp.Body.Close())
	require.Equal(t, http.StatusNotFound, code)
	assert.Contains(t, string(body), "not found: /conversation",
		"an unmodelled sub-path must be answered by the dispatcher, whose 404 names the path")

	// Negative 2: a real path with a verb it does not take. 405, not 404 — a
	// caller who used the wrong verb must not be sent looking for a missing
	// endpoint. This pair with negative 1 is what makes "routed" meaningful.
	resp = doConvReq(t, http.MethodPut, srv.URL+"/conversation/sessions/"+readID, "", "")
	code = resp.StatusCode
	require.NoError(t, resp.Body.Close())
	assert.Equal(t, http.StatusMethodNotAllowed, code,
		"PUT on a real session path is a verb problem and must be reported as one")
	_ = readErr
}

// dispatchConversationSource returns the body of dispatchConversation. Reading
// the function (not the whole file) keeps the scan honest: a literal that moved
// into a handler the dispatcher no longer calls would satisfy a file-wide
// Contains while the route is still dead.
func dispatchConversationSource(t *testing.T) string {
	t.Helper()
	src, err := os.ReadFile(filepath.Join(repoRootForTest(t), "internal", "grpc", "rest.go"))
	require.NoError(t, err, "rest.go must be readable")
	body := string(src)
	start := strings.Index(body, "func (gw *Gateway) dispatchConversation(")
	require.GreaterOrEqual(t, start, 0, "dispatchConversation must exist")
	end := strings.Index(body[start:], "\n}\n")
	require.Greater(t, end, 0, "dispatchConversation must have a body")
	return body[start : start+end]
}

// docSection returns the text of one `### ` section of a repo doc, up to the
// next same-level heading. It fails loudly rather than returning "" when the
// heading moved, so an empty section cannot read as "documented".
func docSection(t *testing.T, rel, startHeading, endHeading string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(repoRootForTest(t), filepath.FromSlash(rel)))
	require.NoError(t, err, "%s must be readable", rel)
	body := string(b)
	start := strings.Index(body, startHeading)
	require.GreaterOrEqual(t, start, 0, "%s must contain %s", rel, startHeading)
	rest := body[start:]
	if end := strings.Index(rest, endHeading); end >= 0 {
		rest = rest[:end]
	}
	return rest
}

// repoRootForTest walks up to the module root, the same way the endpoint-table
// guard and internal/chatops' producer guard do. Duplicated rather than shared
// because a test helper in one package cannot be called from another.
func repoRootForTest(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	require.NoError(t, err)
	for i := 0; i < 10; i++ {
		internalOK := false
		if st, serr := os.Stat(filepath.Join(dir, "internal")); serr == nil && st.IsDir() {
			internalOK = true
		}
		cmdOK := false
		if st, serr := os.Stat(filepath.Join(dir, "cmd")); serr == nil && st.IsDir() {
			cmdOK = true
		}
		if internalOK && cmdOK {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	t.Fatalf("cannot locate the module root from %s", dir)
	return ""
}

// TestConversationRouteTableIsWellFormed is a cheap structural check on the
// table itself: the doc anchors are backticked invocations, so an anchor that
// stops being one would otherwise satisfy the Contains assertion by accident.
func TestConversationRouteTableIsWellFormed(t *testing.T) {
	for _, r := range conversationRoutes {
		require.NotEmpty(t, r.dispatchAs, "%s %s must name a dispatcher literal", r.method, r.pathShape)
		require.True(t, strings.HasPrefix(r.dispatchAs, `"`),
			"%s %s: dispatchAs must be a Go string literal (%q)", r.method, r.pathShape, r.dispatchAs)
		assert.Contains(t, r.docAnchor, r.pathShape,
			"%s %s: docAnchor must mention the path shape it stands for", r.method, r.pathShape)
		require.NotEmpty(t, r.why, "%s %s must say why it exists", r.method, r.pathShape)
	}
	// Duplicate (method, path) pairs would make one of them untested while both
	// look covered.
	seen := make(map[string]bool, len(conversationRoutes))
	for _, r := range conversationRoutes {
		key := r.method + " " + r.pathShape
		require.False(t, seen[key], "%s appears twice in conversationRoutes", key)
		seen[key] = true
	}
}
