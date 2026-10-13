package state

import "strings"

// Change priority is its own vocabulary, and the proof it was never one with the
// approval tier is how it was stored: until the split in schema v9, CreateChange
// wrote these strings into runs.approval_level, which approval kickoff also
// overwrites with a tier (standard/high/emergency, owner internal/dsl
// ApprovalLevel*). The two sets share only the spelling "high", so a value read
// out of that column cannot be attributed without knowing which writer produced
// it — which is precisely the ambiguity the split removes.
//
// The vocabulary is declared here and nowhere else. Validation, error text and
// any "allowed: …" list read through PriorityNames; a hand-copied list is how the
// message ends up promising something the predicate rejects.
const (
	// PriorityLow is a routine change; it never raises the approval floor.
	PriorityLow = "low"
	// PriorityNormal is the default for an unspecified priority. It is the
	// value the create path substitutes for "" — the substitution is why
	// "absent" and "explicitly normal" are indistinguishable in storage, and
	// that is deliberate: both mean "nothing special about this change".
	PriorityNormal = "normal"
	// PriorityHigh is the only spelling shared with the tier vocabulary.
	PriorityHigh = "high"
	// PriorityUrgent is a priority claim, NOT a tier: it does not by itself
	// forbid auto-approval (see risk.AutoApproveForbidden's documented reading
	// of the priority vocabulary), which surprises readers who assume
	// "urgent" escalates. Escalation comes from the derived tier only.
	PriorityUrgent = "urgent"
)

// priorityOrder lists the priorities from least to most urgent. It is the single
// source for the accepted set, the display order and the error text, so the three
// can never disagree.
var priorityOrder = []string{PriorityLow, PriorityNormal, PriorityHigh, PriorityUrgent}

// PriorityNames returns the legal priorities in urgency order. The result is a
// copy and may be modified by the caller.
func PriorityNames() []string {
	return append([]string(nil), priorityOrder...)
}

// PriorityNamesJoined renders the vocabulary for error text and CLI help.
func PriorityNamesJoined() string {
	return strings.Join(priorityOrder, ", ")
}

// IsPriority reports whether p names a legal priority. Case and surrounding
// spaces are not tolerated: the column is compared against these literals all
// the way down to the auto-approval gate, so accepting "High" here would create
// a second spelling that every reader must learn.
func IsPriority(p string) bool {
	for _, name := range priorityOrder {
		if name == p {
			return true
		}
	}
	return false
}
