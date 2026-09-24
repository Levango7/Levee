// Package jira implements the outbound ITSM Jira approval bridge.
//
// Design (design doc: 审批留痕必须可对外追溯). The approval chain
// already leaves a durable, hash-auditable trail in LEVEE's own store;
// this bridge MIRRORS that trail into Jira so organisations whose change
// process lives in ITSM see approvals where they work — without LEVEE
// ever depending on Jira for a decision:
//
//   - OUTBOUND ONLY. Jira issues are informational mirrors. The
//     authoritative approval state stays in LEVEE; nothing in the apply
//     path reads from Jira. A Jira outage can delay a mirror, never
//     block or fake an approval.
//   - config-driven no-op. Disabled (the default) installs nothing and
//     dials nothing. Enabled requires URL + project + token; anything
//     missing fails wiring with an explicit error rather than a
//     half-bridge.
//   - failure containment. Every outbound call is best-effort: errors
//     are logged and dropped. The bridge must never make the approval
//     path itself fail — the local chain is the system of record.
//
// The bridge hooks the same two lifecycle moments the ChatOps bridge
// does: OnApprovalCreated (kickoff ⇒ create issue) and OnDecision
// (DecisionObserver ⇒ comment). Issue↔approval linkage is carried in
// the issue's description and the local approval ID; a round-trip
// mapping table would add state and sync problems without changing
// what the mirror is for (visibility).
package jira

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/nexus/levee/internal/approval"
	"github.com/nexus/levee/internal/config"
	"github.com/nexus/levee/internal/log"
)

// Client talks to the Jira REST API v3. It is safe for concurrent use;
// the http.Client carries the timeout. projectKey/issueType ride on the
// client so construction is total and concurrent bridges never share
// mutable state.
type Client struct {
	baseURL    string
	token      string
	email      string
	projectKey string
	issueType  string
	http       *http.Client
}

// NewClient builds a Jira client. cfg supplies the URL, token (the
// LEVEE_NOTIFY_JIRA_TOKEN / config api_token), email (optional, for
// Jira Cloud Basic auth), project, issue type and timeout.
func NewClient(cfg config.JiraConfig) *Client {
	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	issueType := cfg.IssueType
	if issueType == "" {
		issueType = "Task"
	}
	return &Client{
		baseURL:    strings.TrimRight(cfg.URL, "/"),
		token:      cfg.APIToken,
		email:      cfg.Email,
		projectKey: cfg.ProjectKey,
		issueType:  issueType,
		http:       &http.Client{Timeout: timeout},
	}
}

// do issues one authenticated JSON request. out (when non-nil) receives
// the decoded response body. All Jira REST calls in this client use POST.
func (c *Client) do(ctx context.Context, path string, in any, out any) error {
	var body io.Reader
	if in != nil {
		raw, err := json.Marshal(in)
		if err != nil {
			return fmt.Errorf("jira: marshal request: %w", err)
		}
		body = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+path, body)
	if err != nil {
		return fmt.Errorf("jira: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	if c.email != "" {
		// Jira Cloud: Basic auth with email:api_token.
		req.SetBasicAuth(c.email, c.token)
	} else {
		// Jira Server/DC or PAT: bearer.
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("jira: POST %s: %w", path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode >= 300 {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("jira: POST %s: status %d: %s", path, resp.StatusCode, strings.TrimSpace(string(snippet)))
	}
	if out != nil {
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			return fmt.Errorf("jira: decode response: %w", err)
		}
	}
	return nil
}

// CreateIssueRequest is the API v3 issue creation payload (only the
// fields the bridge sets).
type CreateIssueRequest struct {
	Fields struct {
		Project struct {
			Key string `json:"key"`
		} `json:"project"`
		Summary   string `json:"summary"`
		Issuetype struct {
			Name string `json:"name"`
		} `json:"issuetype"`
		Description struct {
			Type    string            `json:"type"`
			Content []descriptionNode `json:"content"`
		} `json:"description"`
		Labels []string `json:"labels,omitempty"`
	} `json:"fields"`
}

// descriptionNode is one Atlassian Document Format paragraph node.
type descriptionNode struct {
	Type string `json:"type"`
	// Content holds the text runs of a paragraph.
	Content []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content,omitempty"`
}

// CreateIssueResponse carries the fields of the created issue the
// bridge cares about.
type CreateIssueResponse struct {
	ID  string `json:"id"`
	Key string `json:"key"`
}

// CreateIssue files one issue in the configured project.
func (c *Client) CreateIssue(ctx context.Context, summary, description string, labels []string) (*CreateIssueResponse, error) {
	var req CreateIssueRequest
	req.Fields.Project.Key = c.projectKey
	req.Fields.Summary = summary
	req.Fields.Issuetype.Name = c.issueType
	req.Fields.Description.Type = "doc"
	para := descriptionNode{Type: "paragraph"}
	for _, line := range strings.Split(description, "\n") {
		para.Content = append(para.Content, struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}{Type: "text", Text: line})
	}
	req.Fields.Description.Content = []descriptionNode{para}
	req.Fields.Labels = append(labels, "levee-approval")

	var out CreateIssueResponse
	if err := c.do(ctx, "/rest/api/3/issue", &req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// AddComment appends one comment to the issue (ADF paragraph).
func (c *Client) AddComment(ctx context.Context, issueKey, body string) error {
	var payload struct {
		Body struct {
			Type    string            `json:"type"`
			Content []descriptionNode `json:"content"`
		} `json:"body"`
	}
	payload.Body.Type = "doc"
	para := descriptionNode{Type: "paragraph"}
	for _, line := range strings.Split(body, "\n") {
		para.Content = append(para.Content, struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}{Type: "text", Text: line})
	}
	payload.Body.Content = []descriptionNode{para}
	return c.do(ctx, "/rest/api/3/issue/"+issueKey+"/comment", &payload, nil)
}

// ApprovalBridge mirrors approval lifecycle moments into Jira. A nil
// client turns every method into a no-op. The bridge is stateless
// (issue linkage rides on the mirrored content, not a local map) and
// safe for concurrent use.
type ApprovalBridge struct {
	client *Client
}

// NewApprovalBridge builds the bridge from config. When cfg is
// disabled the constructor returns nil — the composition root checks
// for nil and simply does not install the observer (a disabled bridge
// must be a complete no-op, not a silent error).
func NewApprovalBridge(cfg config.JiraConfig) *ApprovalBridge {
	if !cfg.Enabled {
		return nil
	}
	return &ApprovalBridge{client: NewClient(cfg)}
}

// OnApprovalCreated files a Jira issue for a freshly created approval.
// Failures are logged and dropped: the local chain is the system of
// record and a Jira outage must not break the approval path.
func (b *ApprovalBridge) OnApprovalCreated(a *approval.Approval) {
	if b == nil || b.client == nil || a == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	summary := fmt.Sprintf("[levee] Approval required: %s change %s", a.Level, a.RunID)
	desc := fmt.Sprintf("LEVEE approval %s (level %s) for change %s is pending.\n"+
		"Approvers: %s (min %d). One reject vetoes.\n"+
		"This issue is an informational mirror; the authoritative approval state lives in LEVEE.",
		a.ID, a.Level, a.RunID, strings.Join(a.Approvers, ", "), a.MinApprovers)
	issue, err := b.client.CreateIssue(ctx, summary, desc, []string{"approval:" + a.ID})
	if err != nil {
		log.Warn("jira: create approval mirror issue failed",
			"run_id", a.RunID, "approval_id", a.ID, "error", err)
		return
	}
	log.Info("jira: approval mirror issue created",
		"run_id", a.RunID, "approval_id", a.ID,
		"issue_key", issue.Key, "issue_id", issue.ID)
}

// OnDecision comments the decision outcome onto the mirrored issue.
// The issue key is unknown here (the bridge is stateless by design),
// so the comment lands on the issue found via the approval label —
// implemented pragmatically as a JQL lookup by label.
func (b *ApprovalBridge) OnDecision(a *approval.Approval, action string) {
	if b == nil || b.client == nil || a == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Find the mirror issue by the approval label filed at creation.
	issues, err := b.client.searchByLabel(ctx, "approval:"+a.ID)
	if err != nil || len(issues) == 0 {
		if err != nil {
			log.Warn("jira: decision mirror lookup failed",
				"run_id", a.RunID, "approval_id", a.ID, "error", err)
		}
		return
	}
	approved := 0
	var last string
	for _, d := range a.Decisions {
		if d.Action == approval.ActionApprove {
			approved++
			last = d.Approver
		}
	}
	body := fmt.Sprintf("LEVEE decision on approval %s: %s by %s (%d/%d approved).\nStatus: %s.",
		a.ID, action, last, approved, a.MinApprovers, a.Status)
	if err := b.client.AddComment(ctx, issues[0], body); err != nil {
		log.Warn("jira: decision mirror comment failed",
			"run_id", a.RunID, "approval_id", a.ID, "issue_key", issues[0], "error", err)
		return
	}
	log.Info("jira: decision mirrored",
		"run_id", a.RunID, "approval_id", a.ID, "issue_key", issues[0], "action", action)
}

// searchByLabel runs the JQL label search and returns the issue keys.
func (c *Client) searchByLabel(ctx context.Context, label string) ([]string, error) {
	jql := fmt.Sprintf("labels = %q ORDER BY created DESC", label)
	var payload struct {
		JQL string `json:"jql"`
	}
	payload.JQL = jql
	var out struct {
		Issues []struct {
			Key string `json:"key"`
		} `json:"issues"`
	}
	if err := c.do(ctx, "/rest/api/3/search", &payload, &out); err != nil {
		return nil, err
	}
	keys := make([]string, 0, len(out.Issues))
	for _, i := range out.Issues {
		keys = append(keys, i.Key)
	}
	return keys, nil
}
