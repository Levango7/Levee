// jira_test.go pins the outbound ITSM Jira bridge: disabled config is
// a complete no-op, kickoff creates an issue carrying the approval
// linkage label, decisions comment the mirrored issue (found by label
// via JQL), errors are contained (never propagate to the caller), and
// the HTTP wire format matches Jira REST API v3 (ADF description,
// project/issuetype, Basic vs Bearer auth).

package jira

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nexus/levee/internal/approval"
	"github.com/nexus/levee/internal/config"
)

// jiraStub scripts the Jira REST endpoints and records requests.
type jiraStub struct {
	mu         sync.Mutex
	issues     []map[string]any // create-issue bodies
	comments   []commentRecord  // issueKey + body
	searches   []string         // JQL queries
	basicAuth  string
	bearerSeen string

	issueKey string
}

type commentRecord struct {
	issueKey string
	body     string
}

// newJiraServer returns a stub server plus the URL to point a config at.
func newJiraServer(t *testing.T) (*jiraStub, string) {
	t.Helper()
	stub := &jiraStub{issueKey: "OPS-42"}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		stub.mu.Lock()
		defer stub.mu.Unlock()
		if u, p, ok := r.BasicAuth(); ok {
			stub.basicAuth = u + ":" + p
		}
		if b := r.Header.Get("Authorization"); strings.HasPrefix(b, "Bearer ") {
			stub.bearerSeen = strings.TrimPrefix(b, "Bearer ")
		}
		switch {
		case r.URL.Path == "/rest/api/3/issue" && r.Method == http.MethodPost:
			var body map[string]any
			require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
			stub.issues = append(stub.issues, body)
			_ = json.NewEncoder(w).Encode(map[string]any{"id": "10001", "key": stub.issueKey})

		case strings.HasPrefix(r.URL.Path, "/rest/api/3/issue/") && strings.HasSuffix(r.URL.Path, "/comment"):
			key := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/rest/api/3/issue/"), "/comment")
			var body map[string]any
			require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
			stub.comments = append(stub.comments, commentRecord{issueKey: key, body: compactADF(body)})
			w.WriteHeader(http.StatusCreated)

		case r.URL.Path == "/rest/api/3/search" && r.Method == http.MethodPost:
			var body struct {
				JQL string `json:"jql"`
			}
			require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
			stub.searches = append(stub.searches, body.JQL)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"issues": []map[string]any{{"key": stub.issueKey}},
			})

		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return stub, srv.URL
}

// compactADF flattens an ADF payload ({"body":{...}} or a raw doc) into
// one string for assertions.
func compactADF(payload map[string]any) string {
	doc := payload
	if b, ok := payload["body"].(map[string]any); ok {
		doc = b
	}
	var sb strings.Builder
	if content, ok := doc["content"].([]any); ok {
		for _, c := range content {
			para, ok := c.(map[string]any)
			if !ok {
				continue
			}
			runs, _ := para["content"].([]any)
			for _, run := range runs {
				if m, ok := run.(map[string]any); ok {
					if txt, ok := m["text"].(string); ok {
						sb.WriteString(txt)
						sb.WriteString("\n")
					}
				}
			}
		}
	}
	return sb.String()
}

func testCfg(url string) config.JiraConfig {
	return config.JiraConfig{
		Enabled:    true,
		URL:        url,
		APIToken:   "tok-123",
		Email:      "ops@example.com",
		ProjectKey: "OPS",
		IssueType:  "Change",
		Timeout:    5 * time.Second,
	}
}

func TestDisabledConfigIsNilBridge(t *testing.T) {
	// The disabled bridge is nil — the composition root installs
	// nothing and nothing dials out.
	assert.Nil(t, NewApprovalBridge(config.JiraConfig{Enabled: false}))
}

func TestOnApprovalCreatedFilesIssue(t *testing.T) {
	stub, url := newJiraServer(t)
	bridge := NewApprovalBridge(testCfg(url))
	require.NotNil(t, bridge)

	bridge.OnApprovalCreated(&approval.Approval{
		ID: "ap-1", RunID: "run-1", Level: approval.LevelHigh,
		Approvers: []string{"alice"}, MinApprovers: 1,
	})

	stub.mu.Lock()
	defer stub.mu.Unlock()
	require.Len(t, stub.issues, 1)
	issue := stub.issues[0]
	fields := issue["fields"].(map[string]any)

	// Project + issue type from config.
	project := fields["project"].(map[string]any)
	assert.Equal(t, "OPS", project["key"])
	issuetype := fields["issuetype"].(map[string]any)
	assert.Equal(t, "Change", issuetype["name"])

	// Summary carries level + run.
	assert.Contains(t, fields["summary"], "run-1")

	// Label carries the approval linkage the decision path looks up.
	labels, _ := fields["labels"].([]any)
	var linkage, mirror bool
	for _, l := range labels {
		switch l {
		case "approval:ap-1":
			linkage = true
		case "levee-approval":
			mirror = true
		}
	}
	assert.True(t, linkage, "issue must carry the approval:ID label")
	assert.True(t, mirror, "issue must carry the levee-approval label")

	// Jira Cloud basic auth.
	assert.Equal(t, "ops@example.com:tok-123", stub.basicAuth)
}

func TestOnDecisionCommentsMirroredIssue(t *testing.T) {
	stub, url := newJiraServer(t)
	bridge := NewApprovalBridge(testCfg(url))
	require.NotNil(t, bridge)

	now := time.Now().UTC()
	bridge.OnDecision(&approval.Approval{
		ID: "ap-9", RunID: "run-9", Level: approval.LevelStandard,
		Status: approval.StatusApproved, MinApprovers: 1,
		Decisions: []approval.Decision{{Approver: "alice", Action: approval.ActionApprove, At: now}},
	}, approval.ActionApprove)

	stub.mu.Lock()
	defer stub.mu.Unlock()
	require.Len(t, stub.searches, 1)
	assert.Contains(t, stub.searches[0], "approval:ap-9", "JQL must find the mirror by label")
	require.Len(t, stub.comments, 1)
	assert.Equal(t, "OPS-42", stub.comments[0].issueKey)
	assert.Contains(t, stub.comments[0].body, "ap-9")
	assert.Contains(t, stub.comments[0].body, "alice")
}

func TestOnDecisionNoMirrorIssueIsSilent(t *testing.T) {
	// A decision for an approval that never got an issue (Jira was
	// down at kickoff): log-only, never an error path for the caller.
	stub, url := newJiraServer(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	_ = srv
	_ = stub

	bridge := NewApprovalBridge(testCfg(url))
	require.NotNil(t, bridge)
	// Point at a dead server to force the lookup failure.
	bridge.client.baseURL = "http://127.0.0.1:1"
	bridge.OnDecision(&approval.Approval{ID: "ap-x", RunID: "r", MinApprovers: 1}, approval.ActionApprove)
	// No panic, no error surfaced — containment holds.
}

func TestOnApprovalCreatedServerErrorContained(t *testing.T) {
	// Jira down at kickoff: the bridge logs and drops; the caller
	// (kickoffApproval) must never fail.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	t.Cleanup(srv.Close)
	bridge := NewApprovalBridge(testCfg(srv.URL))
	require.NotNil(t, bridge)
	bridge.OnApprovalCreated(&approval.Approval{ID: "ap-2", RunID: "run-2"})
}

func TestBearerAuthWhenNoEmail(t *testing.T) {
	stub, url := newJiraServer(t)
	cfg := testCfg(url)
	cfg.Email = "" // Jira Server/DC: bearer-only
	bridge := NewApprovalBridge(cfg)
	require.NotNil(t, bridge)
	bridge.OnApprovalCreated(&approval.Approval{ID: "ap-3", RunID: "run-3"})

	stub.mu.Lock()
	defer stub.mu.Unlock()
	assert.Empty(t, stub.basicAuth)
	assert.Equal(t, "tok-123", stub.bearerSeen)
}

func TestNilApprovalAndNilReceiverNoOps(t *testing.T) {
	_, url := newJiraServer(t)
	bridge := NewApprovalBridge(testCfg(url))

	var nilBridge *ApprovalBridge
	nilBridge.OnApprovalCreated(&approval.Approval{ID: "x"})
	nilBridge.OnDecision(&approval.Approval{ID: "x"}, approval.ActionApprove)

	bridge.OnApprovalCreated(nil)
	bridge.OnDecision(nil, approval.ActionApprove)
}

func TestClientDoRejectsBadJSON(t *testing.T) {
	// A 200 with garbage JSON must surface a decode error, not a
	// silent pass.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("not json"))
	}))
	t.Cleanup(srv.Close)
	c := NewClient(testCfg(srv.URL))
	var out CreateIssueResponse
	err := c.do(context.Background(), "/rest/api/3/issue", map[string]any{}, &out)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "decode")
}
