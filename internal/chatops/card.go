
// card.go implements the unified Card model and a CardBuilder that produces
// the three card kinds used by the ChatOps layer — approval, status and
// notification. Each card carries a platform-neutral description plus three
// pre-rendered platform representations (Feishu / DingTalk / Slack) computed
// by the To* methods. Adapters call the matching To* method at send time so
// that the wire payload is always consistent with the neutral model.
//
// The card schema is intentionally minimal: it captures exactly the fields
// LEVEE needs to render a change summary, an approval action surface and a
// deep link back to the LEVEE UI. Platform-specific styling lives in the
// To* methods, not in the model.

package chatops

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// --- CardKind --------------------------------------------------------------

// CardKind identifies the logical kind of a card. It is preserved on the
// neutral Card so that adapters can apply platform-specific defaults (e.g.
// Feishu template IDs) without re-inspecting the content.
type CardKind string

const (
	// CardKindApproval is a card asking the user to approve / reject a
	// change. It carries Approve/Reject action buttons.
	CardKindApproval CardKind = "approval"
	// CardKindStatus reports the current status of a change run together
	// with progress and gate results.
	CardKindStatus CardKind = "status"
	// CardKindNotification is a generic event notification with a deep link.
	CardKindNotification CardKind = "notification"
)

// --- CardAction ------------------------------------------------------------

// CardAction is a single interactive button on a card. Value is the
// platform-native callback payload; for LEVEE commands it is the literal
// command string (e.g. "/levee approve run-123").
type CardAction struct {
	Type  string `json:"type"`            // approve / reject / link / default
	Text  string `json:"text"`            // button label
	Value string `json:"value"`           // callback value or URL
	Style string `json:"style,omitempty"` // primary / danger / default
}

// --- CardField -------------------------------------------------------------

// CardField is a labelled key/value row displayed in the card body.
type CardField struct {
	Label string `json:"label"`
	Value string `json:"value"`
	Short bool   `json:"short,omitempty"` // hint to render two columns
}

// --- Card ------------------------------------------------------------------

// Card is the platform-neutral card model. Adapters call ToFeishu /
// ToDingtalk / ToSlack to obtain the wire payload.
type Card struct {
	Kind       CardKind     `json:"kind"`
	Title      string       `json:"title"`
	Summary    string       `json:"summary,omitempty"`
	Fields     []CardField  `json:"fields,omitempty"`
	Actions    []CardAction `json:"actions,omitempty"`
	DetailURL  string       `json:"detail_url,omitempty"`
	Footer     string       `json:"footer,omitempty"`
	Timestamp  time.Time    `json:"timestamp,omitempty"`
	ChangeID   string       `json:"change_id,omitempty"`
	RunID      string       `json:"run_id,omitempty"`
	Level      string       `json:"level,omitempty"`
}

// --- CardBuilder -----------------------------------------------------------

// CardBuilder is a fluent builder for the neutral Card model. It is the
// primary construction API; callers should not instantiate Card literals
// directly so that future schema changes stay localised to the builder.
type CardBuilder struct {
	card Card
}

// NewCardBuilder returns an empty builder.
func NewCardBuilder() *CardBuilder { return &CardBuilder{} }

// WithKind sets the card kind.
func (b *CardBuilder) WithKind(k CardKind) *CardBuilder { b.card.Kind = k; return b }

// WithTitle sets the title.
func (b *CardBuilder) WithTitle(t string) *CardBuilder { b.card.Title = t; return b }

// WithSummary sets the summary line.
func (b *CardBuilder) WithSummary(s string) *CardBuilder { b.card.Summary = s; return b }

// WithDetailURL sets the deep link back to the LEVEE UI.
func (b *CardBuilder) WithDetailURL(u string) *CardBuilder { b.card.DetailURL = u; return b }

// WithFooter sets the footer text.
func (b *CardBuilder) WithFooter(f string) *CardBuilder { b.card.Footer = f; return b }

// WithTimestamp sets the timestamp.
func (b *CardBuilder) WithTimestamp(t time.Time) *CardBuilder { b.card.Timestamp = t; return b }

// WithChangeID sets the change identifier.
func (b *CardBuilder) WithChangeID(id string) *CardBuilder { b.card.ChangeID = id; return b }

// WithRunID sets the run identifier.
func (b *CardBuilder) WithRunID(id string) *CardBuilder { b.card.RunID = id; return b }

// WithLevel sets the approval level.
func (b *CardBuilder) WithLevel(l string) *CardBuilder { b.card.Level = l; return b }

// AddField appends a labelled field row.
func (b *CardBuilder) AddField(label, value string, short bool) *CardBuilder {
	b.card.Fields = append(b.card.Fields, CardField{Label: label, Value: value, Short: short})
	return b
}

// AddAction appends an interactive button.
func (b *CardBuilder) AddAction(actionType, text, value, style string) *CardBuilder {
	b.card.Actions = append(b.card.Actions, CardAction{Type: actionType, Text: text, Value: value, Style: style})
	return b
}

// Build returns the constructed Card.
func (b *CardBuilder) Build() *Card {
	c := b.card
	return &c
}

// --- High-level builders ----------------------------------------------------

// BuildApprovalCard produces an approval card: change summary, level, approver
// list and Approve / Reject buttons wired to /levee approve|reject <change-id>.
func BuildApprovalCard(evt Event) *Card {
	b := NewCardBuilder().
		WithKind(CardKindApproval).
		WithTitle(fmt.Sprintf("审批请求: %s", evt.Title)).
		WithSummary(evt.Summary).
		WithChangeID(evt.ChangeID).
		WithRunID(evt.RunID).
		WithLevel(evt.Level).
		WithDetailURL(evt.DetailURL).
		WithTimestamp(evt.Timestamp).
		AddField("变更ID", evt.ChangeID, true).
		AddField("运行ID", evt.RunID, true).
		AddField("审批级别", evt.Level, true).
		AddField("发起时间", evt.Timestamp.Format("2006-01-02 15:04:05"), true)

	if evt.Approver != "" {
		b.AddField("审批人", evt.Approver, true)
	}

	// Action buttons: clicking sends the corresponding /levee command back.
	b.AddAction("approve", "通过", fmt.Sprintf("/levee approve %s", evt.ChangeID), "primary")
	b.AddAction("reject", "驳回", fmt.Sprintf("/levee reject %s", evt.ChangeID), "danger")

	return b.Build()
}

// BuildStatusCard produces a status card: current status, progress and the
// latest gate result.
func BuildStatusCard(evt Event) *Card {
	b := NewCardBuilder().
		WithKind(CardKindStatus).
		WithTitle(fmt.Sprintf("变更状态: %s", evt.Title)).
		WithSummary(evt.Summary).
		WithChangeID(evt.ChangeID).
		WithRunID(evt.RunID).
		WithDetailURL(evt.DetailURL).
		WithTimestamp(evt.Timestamp).
		AddField("变更ID", evt.ChangeID, true).
		AddField("运行ID", evt.RunID, true).
		AddField("状态", evt.Status, true).
		AddField("更新时间", evt.Timestamp.Format("2006-01-02 15:04:05"), true)

	if evt.GateName != "" {
		pass := "失败"
		if evt.GatePass {
			pass = "通过"
		}
		b.AddField("门禁", evt.GateName, true)
		b.AddField("门禁结果", pass, true)
	}

	if evt.DetailURL != "" {
		b.AddAction("link", "查看详情", evt.DetailURL, "default")
	}

	return b.Build()
}

// BuildNotificationCard produces a generic notification card with a deep link.
func BuildNotificationCard(evt Event) *Card {
	b := NewCardBuilder().
		WithKind(CardKindNotification).
		WithTitle(evt.Title).
		WithSummary(evt.Summary).
		WithChangeID(evt.ChangeID).
		WithRunID(evt.RunID).
		WithDetailURL(evt.DetailURL).
		WithTimestamp(evt.Timestamp).
		AddField("事件", string(evt.Type), true).
		AddField("时间", evt.Timestamp.Format("2006-01-02 15:04:05"), true)

	if evt.RunID != "" {
		b.AddField("运行ID", evt.RunID, true)
	}
	if evt.Status != "" {
		b.AddField("状态", evt.Status, true)
	}
	if evt.DetailURL != "" {
		b.AddAction("link", "查看详情", evt.DetailURL, "default")
	}
	return b.Build()
}

// BuildCardForEvent dispatches to the appropriate Build*Card based on the
// event type. Unknown events fall back to a notification card.
func BuildCardForEvent(evt Event) *Card {
	switch evt.Type {
	case EventApprovalRequested:
		return BuildApprovalCard(evt)
	case EventStateChange, EventRunStarted, EventRunCompleted, EventRunFailed, EventGateResult:
		return BuildStatusCard(evt)
	case EventApprovalDecision:
		return BuildNotificationCard(evt)
	default:
		return BuildNotificationCard(evt)
	}
}

// ===========================================================================
// Platform renderers
// ===========================================================================
//
// Each To* method returns the platform-native payload as a JSON-serialisable
// map[string]any. Adapters marshal the map with encoding/json before sending
// it over the wire. Keeping the renderers here (rather than in each adapter)
// makes it trivial to audit the cross-platform rendering in one place.

// --- Feishu ----------------------------------------------------------------

// ToFeishu renders the card as a Feishu interactive card payload.
// Reference: https://open.feishu.cn/document/uAjLw4CM/ukTMukTMukTM/feishu-cards/card-json-structure
func (c *Card) ToFeishu() map[string]any {
	elements := make([]any, 0, len(c.Fields)+len(c.Actions)+2)

	// Summary as a markdown element.
	if c.Summary != "" {
		elements = append(elements, map[string]any{
			"tag":     "div",
			"text":    map[string]any{"tag": "lark_md", "content": c.Summary},
		})
	}

	// Fields rendered as a column set.
	if len(c.Fields) > 0 {
		columns := make([]any, 0, len(c.Fields))
		for _, f := range c.Fields {
			columns = append(columns, map[string]any{
				"tag": "column",
				"elements": []any{
					map[string]any{
						"tag":  "div",
						"text": map[string]any{"tag": "lark_md", "content": fmt.Sprintf("**%s**\n%s", f.Label, f.Value)},
					},
				},
			})
		}
		elements = append(elements, map[string]any{
			"tag":      "column_set",
			"flex_mode": "none",
			"columns":  columns,
		})
	}

	// Action buttons.
	if len(c.Actions) > 0 {
		buttons := make([]any, 0, len(c.Actions))
		for _, a := range c.Actions {
			btn := map[string]any{
				"tag":  "button",
				"text": map[string]any{"tag": "plain_text", "content": a.Text},
				"value": map[string]any{"command": a.Value},
				"type":  feishuButtonType(a.Style),
			}
			buttons = append(buttons, btn)
		}
		elements = append(elements, map[string]any{
			"tag":      "action",
			"actions":  buttons,
		})
	}

	// Footer / note.
	if c.Footer != "" {
		elements = append(elements, map[string]any{
			"tag":  "note",
			"elements": []any{
				map[string]any{"tag": "plain_text", "content": c.Footer},
			},
		})
	}

	header := map[string]any{
		"title":    map[string]any{"tag": "plain_text", "content": c.Title},
		"template": feishuHeaderColor(c.Kind),
	}
	return map[string]any{
		"config": map[string]any{"wide_screen_mode": true},
		"header": header,
		"elements": elements,
	}
}

// feishuButtonType maps our style vocabulary to Feishu button types.
func feishuButtonType(style string) string {
	switch strings.ToLower(style) {
	case "primary":
		return "primary"
	case "danger":
		return "danger"
	default:
		return "default"
	}
}

// feishuHeaderColor maps a card kind to a Feishu header template colour.
func feishuHeaderColor(k CardKind) string {
	switch k {
	case CardKindApproval:
		return "turquoise"
	case CardKindStatus:
		return "blue"
	case CardKindNotification:
		return "grey"
	default:
		return "grey"
	}
}

// --- DingTalk --------------------------------------------------------------

// ToDingtalk renders the card as a DingTalk ActionCard payload.
// Reference: https://open.dingtalk.com/document/robots/custom-robot-access
func (c *Card) ToDingtalk() map[string]any {
	// Build the markdown body.
	var sb strings.Builder
	if c.Summary != "" {
		sb.WriteString(c.Summary)
		sb.WriteString("\n\n")
	}
	for _, f := range c.Fields {
		sb.WriteString(fmt.Sprintf("- **%s**: %s\n", f.Label, f.Value))
	}
	if c.DetailURL != "" {
		sb.WriteString(fmt.Sprintf("\n[查看详情](%s)\n", c.DetailURL))
	}

	// Action buttons become independent actionCard buttons.
	btns := make([]map[string]any, 0, len(c.Actions))
	for _, a := range c.Actions {
		btns = append(btns, map[string]any{
			"title":     a.Text,
			"actionURL": a.Value,
		})
	}

	payload := map[string]any{
		"msgtype": "actionCard",
		"actionCard": map[string]any{
			"title":          c.Title,
			"text":           sb.String(),
			"btnOrientation": "0",
		},
	}
	if len(btns) > 0 {
		payload["actionCard"].(map[string]any)["btns"] = btns
	}
	return payload
}

// --- Slack -----------------------------------------------------------------

// ToSlack renders the card as a Slack Block Kit payload.
// Reference: https://api.slack.com/reference/block-kit/blocks
func (c *Card) ToSlack() map[string]any {
	blocks := make([]any, 0, len(c.Fields)+len(c.Actions)+2)

	// Header block.
	blocks = append(blocks, map[string]any{
		"type": "header",
		"text": map[string]any{"type": "plain_text", "text": c.Title},
	})

	// Summary as a context / markdown section.
	if c.Summary != "" {
		blocks = append(blocks, map[string]any{
			"type": "section",
			"text": map[string]any{"type": "mrkdwn", "text": c.Summary},
		})
	}

	// Fields as a single section with two-column fields.
	if len(c.Fields) > 0 {
		fields := make([]map[string]any, 0, len(c.Fields))
		for _, f := range c.Fields {
			fields = append(fields, map[string]any{
				"type": "mrkdwn",
				"text": fmt.Sprintf("*%s*\n%s", f.Label, f.Value),
			})
		}
		blocks = append(blocks, map[string]any{
			"type":   "section",
			"fields": fields,
		})
	}

	// Action buttons in an actions block.
	if len(c.Actions) > 0 {
		elements := make([]map[string]any, 0, len(c.Actions))
		for _, a := range c.Actions {
			elm := map[string]any{
				"type":  "button",
				"text":  map[string]any{"type": "plain_text", "text": a.Text},
				"value": a.Value,
			}
			if a.Type == "link" && strings.HasPrefix(a.Value, "http") {
				elm["url"] = a.Value
			}
			if style := slackButtonStyle(a.Style); style != "" {
				elm["style"] = style
			}
			elements = append(elements, elm)
		}
		blocks = append(blocks, map[string]any{
			"type":     "actions",
			"elements": elements,
		})
	}

	// Footer as a context block.
	if c.Footer != "" {
		blocks = append(blocks, map[string]any{
			"type": "context",
			"elements": []map[string]any{
				{"type": "mrkdwn", "text": c.Footer},
			},
		})
	}

	return map[string]any{"blocks": blocks}
}

// slackButtonStyle maps our style vocabulary to Slack button styles.
func slackButtonStyle(style string) string {
	switch strings.ToLower(style) {
	case "primary":
		return "primary"
	case "danger":
		return "danger"
	default:
		return ""
	}
}

// --- JSON helpers ----------------------------------------------------------

// MarshalFeishu / MarshalDingtalk / MarshalSlack are convenience wrappers
// that render and JSON-encode a card in one step. Adapters use them to
// obtain the bytes they POST over the wire.
func (c *Card) MarshalFeishu() ([]byte, error) {
	return json.Marshal(c.ToFeishu())
}

func (c *Card) MarshalDingtalk() ([]byte, error) {
	return json.Marshal(c.ToDingtalk())
}

func (c *Card) MarshalSlack() ([]byte, error) {
	return json.Marshal(c.ToSlack())
}