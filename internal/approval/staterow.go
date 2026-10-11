// StateRow maps an approval chain (this package's domain type) to and from the
// `approvals` table's row shape.
//
// The chain does not fit the row, so the part that does not fit — who was
// asked, the quorum, the decisions, the initiator — travels inside the row's
// `comment` column as JSON (the schema's `comment` is the only free-text column
// the approvals table has). That is a fact of the schema, and this file is the
// only place allowed to know it: the conversion previously lived in
// cmd/levee/helpers.go with a near-identical copy in internal/grpc's tests, so
// anything learned by one was invisible to the other.
//
// Decoding tolerates the empty/blank comment: a row written before this
// encoding existed (or by a writer that left it empty) decodes to a chain with
// no approvers and no decisions, which the read path reports as what it is
// rather than failing the whole request.
package approval

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/nexus/levee/internal/state"
)

// StateApproval writes the fields the row's own columns carry (level, status,
// plan hash, expiry, revision) and encodes the rest into `comment`. Approver
// and ActedAt are derived from the latest decision, so a reader that only looks
// at the columns still sees "who decided last, and when".
func StateApproval(a *Approval) (*state.Approval, error) {
	extra := approvalExtra{
		Approvers:        a.Approvers,
		MinApprovers:     a.MinApprovers,
		Decisions:        a.Decisions,
		CreatedAt:        a.CreatedAt,
		ExpiresAt:        a.ExpiresAt,
		Initiator:        a.Initiator,
		ExcludeInitiator: a.ExcludeInitiator,
	}
	blob, err := json.Marshal(extra)
	if err != nil {
		return nil, fmt.Errorf("marshal approval extra: %w", err)
	}
	sa := &state.Approval{
		ID:       a.ID,
		RunID:    a.RunID,
		Level:    a.Level,
		Status:   string(a.Status),
		Comment:  string(blob),
		PlanHash: a.PlanHash,
		Revision: a.Revision,
	}
	// Derived columns: the latest decision is what a column-only reader wants.
	if n := len(a.Decisions); n > 0 {
		last := a.Decisions[n-1]
		sa.Approver = last.Approver
		if !last.At.IsZero() {
			t := last.At
			sa.ActedAt = &t
		}
	}
	if !a.ExpiresAt.IsZero() {
		t := a.ExpiresAt
		sa.TimeoutAt = &t
	}
	return sa, nil
}

// ApprovalFromState reverses StateApproval. A chain whose `comment` is not the
// JSON this package writes (legacy row, or a comment a human typed) decodes to
// an empty chain instead of an error: refusing the whole read would hide every
// other chain on the change.
func ApprovalFromState(sa *state.Approval) (*Approval, error) {
	a := &Approval{
		ID:       sa.ID,
		RunID:    sa.RunID,
		Level:    sa.Level,
		Status:   Status(sa.Status),
		PlanHash: sa.PlanHash,
		Revision: sa.Revision,
	}
	if sa.Comment != "" {
		var extra approvalExtra
		if err := json.Unmarshal([]byte(sa.Comment), &extra); err != nil {
			return nil, fmt.Errorf("approval %q has unparsable metadata (comment %q): %w", sa.ID, sa.Comment, err)
		}
		a.Approvers = extra.Approvers
		a.MinApprovers = extra.MinApprovers
		a.Decisions = extra.Decisions
		a.CreatedAt = extra.CreatedAt
		a.ExpiresAt = extra.ExpiresAt
		a.Initiator = extra.Initiator
		a.ExcludeInitiator = extra.ExcludeInitiator
	}
	if sa.TimeoutAt != nil && a.ExpiresAt.IsZero() {
		a.ExpiresAt = *sa.TimeoutAt
	}
	return a, nil
}

// approvalExtra is the JSON payload stored in state.Approval.Comment. Field
// names are part of the stored format: rows already in the field use these.
type approvalExtra struct {
	Approvers        []string   `json:"approvers"`
	MinApprovers     int        `json:"min_approvers"`
	Decisions        []Decision `json:"decisions"`
	CreatedAt        time.Time  `json:"created_at"`
	ExpiresAt        time.Time  `json:"expires_at"`
	Initiator        string     `json:"initiator,omitempty"`
	ExcludeInitiator bool       `json:"exclude_initiator,omitempty"`
}
