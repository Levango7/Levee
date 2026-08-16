package chatops

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// --- CardBuilder ----------------------------------------------------------

func TestCardBuilder_FluentChain(t *testing.T) {
	c := NewCardBuilder().
		WithKind(CardKindApproval).
		WithTitle("T").
		WithSummary("S").
		WithDetailURL("http://x").
		WithChangeID("ch-1").
		WithRunID("run-1").
		WithLevel("high").
		AddField("k", "v", true).
		AddAction("approve", "通过", "/levee approve ch-1", "primary").
		Build()

	require.NotNil(t, c)
	assert.Equal(t, CardKindApproval, c.Kind)
	assert.Equal(t, "T", c.Title)
	assert.Equal(t, "S", c.Summary)
	assert.Equal(t, "http://x", c.DetailURL)
	assert.Equal(t, "ch-1", c.ChangeID)
	assert.Equal(t, "run-1", c.RunID)
	assert.Equal(t, "high", c.Level)
	require.Len(t, c.Fields, 1)
	assert.Equal(t, "k", c.Fields[0].Label)
	require.Len(t, c.Actions, 1)
	assert.Equal(t, "approve", c.Actions[0].Type)
}

// --- BuildApprovalCard ----------------------------------------------------

func TestBuildApprovalCard(t *testing.T) {
	evt := Event{
		Type:      EventApprovalRequested,
		RunID:     "run-1",
		ChangeID:  "ch-1",
		Title:     "升级 nginx",
		Summary:   "升级 nginx 1.24 -> 1.26",
		Level:     "high",
		Approver:  "alice",
		DetailURL: "http://levee/changes/ch-1",
		Timestamp: time.Date(2026, 8, 16, 10, 0, 0, 0, time.UTC),
	}
	c := BuildApprovalCard(evt)
	require.NotNil(t, c)
	assert.Equal(t, CardKindApproval, c.Kind)
	assert.Contains(t, c.Title, "审批请求")
	assert.Contains(t, c.Title, "升级 nginx")
	assert.Equal(t, "ch-1", c.ChangeID)

	// Approve / Reject buttons present.
	require.Len(t, c.Actions, 2)
	assert.Equal(t, "通过", c.Actions[0].Text)
	assert.Equal(t, "primary", c.Actions[0].Style)
	assert.Contains(t, c.Actions[0].Value, "/levee approve ch-1")
	assert.Equal(t, "驳回", c.Actions[1].Text)
	assert.Equal(t, "danger", c.Actions[1].Style)
	assert.Contains(t, c.Actions[1].Value, "/levee reject ch-1")
}

// --- BuildStatusCard ------------------------------------------------------

func TestBuildStatusCard(t *testing.T) {
	evt := Event{
		Type:      EventStateChange,
		RunID:     "run-2",
		ChangeID:  "ch-2",
		Title:     "rolling update",
		Summary:   "batch 2/3 completed",
		Status:    "running",
		GateName:  "canary",
		GatePass:  true,
		DetailURL: "http://levee/changes/ch-2",
		Timestamp: time.Date(2026, 8, 16, 11, 0, 0, 0, time.UTC),
	}
	c := BuildStatusCard(evt)
	require.NotNil(t, c)
	assert.Equal(t, CardKindStatus, c.Kind)
	assert.Contains(t, c.Title, "变更状态")

	// Should have a link action to detail URL.
	foundLink := false
	for _, a := range c.Actions {
		if a.Type == "link" {
			foundLink = true
			assert.Equal(t, evt.DetailURL, a.Value)
		}
	}
	assert.True(t, foundLink, "status card should have a link action")

	// Gate result field rendered.
	foundGate := false
	for _, f := range c.Fields {
		if f.Label == "门禁结果" {
			foundGate = true
			assert.Equal(t, "通过", f.Value)
		}
	}
	assert.True(t, foundGate)
}

func TestBuildStatusCard_GateFailed(t *testing.T) {
	evt := Event{
		Type:     EventGateResult,
		RunID:    "run-3",
		ChangeID: "ch-3",
		Title:    "gate failed",
		GateName: "smoke",
		GatePass: false,
	}
	c := BuildStatusCard(evt)
	for _, f := range c.Fields {
		if f.Label == "门禁结果" {
			assert.Equal(t, "失败", f.Value)
		}
	}
}

// --- BuildNotificationCard ------------------------------------------------

func TestBuildNotificationCard(t *testing.T) {
	evt := Event{
		Type:      EventApprovalDecision,
		RunID:     "run-4",
		ChangeID:  "ch-4",
		Title:     "审批通过",
		Summary:   "alice approved",
		Status:    "approved",
		DetailURL: "http://levee/changes/ch-4",
		Timestamp: time.Date(2026, 8, 16, 12, 0, 0, 0, time.UTC),
	}
	c := BuildNotificationCard(evt)
	require.NotNil(t, c)
	assert.Equal(t, CardKindNotification, c.Kind)
	assert.Equal(t, "审批通过", c.Title)
}

// --- BuildCardForEvent dispatch -------------------------------------------

func TestBuildCardForEvent_Dispatch(t *testing.T) {
	cases := []struct {
		evt  EventType
		want CardKind
	}{
		{EventApprovalRequested, CardKindApproval},
		{EventStateChange, CardKindStatus},
		{EventRunStarted, CardKindStatus},
		{EventRunCompleted, CardKindStatus},
		{EventRunFailed, CardKindStatus},
		{EventGateResult, CardKindStatus},
		{EventApprovalDecision, CardKindNotification},
		{EventType("unknown"), CardKindNotification},
	}
	for _, c := range cases {
		got := BuildCardForEvent(Event{Type: c.evt, Title: "x"})
		require.NotNil(t, got, "event %q", c.evt)
		assert.Equal(t, c.want, got.Kind, "event %q", c.evt)
	}
}

// --- ToFeishu -------------------------------------------------------------

func TestCard_ToFeishu(t *testing.T) {
	c := BuildApprovalCard(Event{
		Type:     EventApprovalRequested,
		RunID:    "run-1",
		ChangeID: "ch-1",
		Title:    "T",
		Summary:  "S",
		Level:    "high",
	})
	payload := c.ToFeishu()
	assert.Equal(t, true, payload["config"].(map[string]any)["wide_screen_mode"])

	header := payload["header"].(map[string]any)
	title := header["title"].(map[string]any)
	assert.Contains(t, title["content"], "T")
	assert.Equal(t, "turquoise", header["template"])

	elements := payload["elements"].([]any)
	// Should contain at least: summary div, column_set, action.
	assert.GreaterOrEqual(t, len(elements), 2)
}

func TestCard_ToFeishu_HeaderColorByKind(t *testing.T) {
	cases := []struct {
		kind CardKind
		want string
	}{
		{CardKindApproval, "turquoise"},
		{CardKindStatus, "blue"},
		{CardKindNotification, "grey"},
	}
	for _, c := range cases {
		card := NewCardBuilder().WithKind(c.kind).WithTitle("t").Build()
		payload := card.ToFeishu()
		header := payload["header"].(map[string]any)
		assert.Equal(t, c.want, header["template"], "kind %q", c.kind)
	}
}

// --- ToDingtalk -----------------------------------------------------------

func TestCard_ToDingtalk(t *testing.T) {
	c := BuildApprovalCard(Event{
		Type:     EventApprovalRequested,
		RunID:    "run-1",
		ChangeID: "ch-1",
		Title:    "T",
		Summary:  "S",
		Level:    "high",
	})
	payload := c.ToDingtalk()
	assert.Equal(t, "actionCard", payload["msgtype"])
	ac := payload["actionCard"].(map[string]any)
	assert.Contains(t, ac["title"].(string), "T")
	assert.Contains(t, ac["text"].(string), "S")
	btns, ok := ac["btns"].([]map[string]any)
	require.True(t, ok)
	require.Len(t, btns, 2)
	assert.Equal(t, "通过", btns[0]["title"])
	assert.Equal(t, "驳回", btns[1]["title"])
}

// --- ToSlack --------------------------------------------------------------

func TestCard_ToSlack(t *testing.T) {
	c := BuildApprovalCard(Event{
		Type:     EventApprovalRequested,
		RunID:    "run-1",
		ChangeID: "ch-1",
		Title:    "T",
		Summary:  "S",
		Level:    "high",
	})
	payload := c.ToSlack()
	blocks := payload["blocks"].([]any)
	require.GreaterOrEqual(t, len(blocks), 2)

	// First block is a header.
	header := blocks[0].(map[string]any)
	assert.Equal(t, "header", header["type"])
	text := header["text"].(map[string]any)
	assert.Contains(t, text["text"], "T")

	// Last block is an actions block with two buttons.
	actions := blocks[len(blocks)-1].(map[string]any)
	assert.Equal(t, "actions", actions["type"])
	elements := actions["elements"].([]map[string]any)
	require.Len(t, elements, 2)
	assert.Equal(t, "primary", elements[0]["style"])
	assert.Equal(t, "danger", elements[1]["style"])
}

func TestCard_ToSlack_LinkButtonIsURL(t *testing.T) {
	c := NewCardBuilder().
		WithKind(CardKindStatus).
		WithTitle("t").
		AddAction("link", "查看详情", "http://levee/x", "default").
		Build()
	payload := c.ToSlack()
	blocks := payload["blocks"].([]any)
	actions := blocks[len(blocks)-1].(map[string]any)
	elements := actions["elements"].([]map[string]any)
	require.Len(t, elements, 1)
	assert.Equal(t, "http://levee/x", elements[0]["url"])
}

// --- Marshal helpers ------------------------------------------------------

func TestCard_MarshalHelpers(t *testing.T) {
	c := BuildApprovalCard(Event{Type: EventApprovalRequested, ChangeID: "ch-1", Title: "T"})

	fb, err := c.MarshalFeishu()
	require.NoError(t, err)
	assert.True(t, strings.HasPrefix(string(fb), "{"))

	db, err := c.MarshalDingtalk()
	require.NoError(t, err)
	assert.True(t, strings.HasPrefix(string(db), "{"))

	sb, err := c.MarshalSlack()
	require.NoError(t, err)
	assert.True(t, strings.HasPrefix(string(sb), "{"))

	// Round-trip: the marshalled bytes should be valid JSON.
	var any map[string]any
	require.NoError(t, json.Unmarshal(fb, &any))
	require.NoError(t, json.Unmarshal(db, &any))
	require.NoError(t, json.Unmarshal(sb, &any))
}
