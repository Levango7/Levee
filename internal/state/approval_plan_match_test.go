package state

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestApprovalMatchesPlan pins the single rule three gates share
// (settlement, apply, retry re-plan). If this table needs a case added, the
// gate that motivated it is the one to name here.
func TestApprovalMatchesPlan(t *testing.T) {
	cases := []struct {
		name        string
		row         *Approval
		planHash    string
		wantMatched bool
		wantLegacy  bool
	}{
		{"nil row never matches", nil, "h1", false, false},
		{"legacy row matches any plan", &Approval{PlanHash: ""}, "h1", true, true},
		{"legacy row matches an empty run hash", &Approval{PlanHash: ""}, "", true, true},
		{"bound row matches its own version", &Approval{PlanHash: "h1"}, "h1", true, false},
		{"bound row rejects another version", &Approval{PlanHash: "h1"}, "h2", false, false},
		{"bound row never matches an empty run hash", &Approval{PlanHash: "h1"}, "", false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			matched, legacy := tc.row.MatchesPlan(tc.planHash)
			assert.Equal(t, tc.wantMatched, matched, "matched")
			assert.Equal(t, tc.wantLegacy, legacy, "legacy")
		})
	}
}
