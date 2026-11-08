package evidence

// evidence_test.go pins the one decision this package makes: given what the
// store holds, may an operator read the empty step list as "nothing ran"?
//
// Every case names the operator action it separates. A test that only asserted
// Kind would let the reason drift into something that reads as reassurance
// ("no steps") while the kind says otherwise — the exact failure the package
// exists to remove — so the reasons are checked to mention the hosts.

import (
	"strings"
	"testing"

	"github.com/nexus/levee/internal/runstatus"
	"github.com/nexus/levee/internal/state"

	"github.com/stretchr/testify/assert"
)

func TestClassify(t *testing.T) {
	cases := []struct {
		name  string
		facts Facts
		want  Kind
	}{
		{
			name:  "draft is not started",
			facts: Facts{Status: runstatus.StatusDraft},
			want:  NotStarted,
		},
		{
			name:  "approved without a claimed assignment is not started",
			facts: Facts{Status: runstatus.StatusApproved},
			want:  NotStarted,
		},
		{
			name:  "approved and claimed is in flight",
			facts: Facts{Status: runstatus.StatusApproved, Assignment: true},
			want:  InFlight,
		},
		{
			name:  "running is in flight, not missing",
			facts: Facts{Status: runstatus.StatusRunning},
			want:  InFlight,
		},
		{
			name:  "paused is in flight",
			facts: Facts{Status: runstatus.StatusPaused, Assignment: true},
			want:  InFlight,
		},
		{
			name:  "completed with rows is complete",
			facts: Facts{Status: runstatus.StatusCompleted, Batches: 2, Steps: 8, Assignment: true},
			want:  Complete,
		},
		{
			name:  "failed with rows is complete: the outcome is documented even though it failed",
			facts: Facts{Status: runstatus.StatusFailed, Batches: 1, Steps: 3},
			want:  Complete,
		},
		{
			name:  "interrupted with no rows may have run",
			facts: Facts{Status: runstatus.StatusInterrupted},
			want:  Missing,
		},
		{
			name:  "interrupted with earlier attempts' rows is partial, never complete",
			facts: Facts{Status: runstatus.StatusInterrupted, Batches: 1, Steps: 4},
			want:  Partial,
		},
		{
			name: "a recorded discard beats rows: the discarded attempt was the one that counted",
			facts: Facts{
				Status: runstatus.StatusInterrupted, Batches: 1, Steps: 4,
				Events: []string{state.EventEvidenceDiscarded},
			},
			want: Missing,
		},
		{
			name: "persist failure is partial",
			facts: Facts{
				Status: runstatus.StatusFailed,
				Events: []string{state.EventEvidenceIncomplete},
			},
			want: Partial,
		},
		{
			name: "discard outranks incomplete",
			facts: Facts{
				Status: runstatus.StatusFailed,
				Events: []string{state.EventEvidenceIncomplete, state.EventEvidenceDiscarded},
			},
			want: Missing,
		},
		{
			name:  "a dispatched cancelled run with no rows may have run",
			facts: Facts{Status: runstatus.StatusCancelled, Assignment: true},
			want:  Missing,
		},
		{
			name:  "cancelled before dispatch cannot be called dispatched",
			facts: Facts{Status: runstatus.StatusCancelled},
			want:  Unrecorded,
		},
		{
			name:  "a terminal run with no rows and no dispatch record is unrecorded, not missing",
			facts: Facts{Status: runstatus.StatusCompleted},
			want:  Unrecorded,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Classify(tc.facts)
			assert.Equal(t, tc.want, got.Kind)
			assert.NotEmpty(t, got.Reason, "a verdict without a reason is a label, not an answer")
		})
	}
}

// TestGapReasonsDoNotReadAsReassurance: the verdict exists because "no steps"
// reads as "nothing happened". Any reason attached to a verdict that admits
// uncertainty must say so, and must mention the hosts the operator has to check.
func TestGapReasonsDoNotReadAsReassurance(t *testing.T) {
	for _, facts := range []Facts{
		{Status: runstatus.StatusInterrupted},
		{Status: runstatus.StatusInterrupted, Events: []string{state.EventEvidenceDiscarded}},
		{Status: runstatus.StatusFailed, Events: []string{state.EventEvidenceIncomplete}},
		{Status: runstatus.StatusCancelled, Assignment: true},
		{Status: runstatus.StatusCompleted},
	} {
		v := Classify(facts)
		assert.True(t, v.Kind.UnsafeToBlindRetry(), "%s must not read as safe to retry", v.Kind)
		reason := strings.ToLower(v.Reason)
		assert.True(t,
			strings.Contains(reason, "host") || strings.Contains(reason, "changed") ||
				strings.Contains(reason, "executed"),
			"%s reason %q must say what is unknown, not merely that evidence is absent", v.Kind, v.Reason)
	}
}

func TestUnsafeToBlindRetry(t *testing.T) {
	assert.False(t, NotStarted.UnsafeToBlindRetry(), "starting something that never ran is not a risk")
	assert.False(t, Complete.UnsafeToBlindRetry(), "documented outcomes are what retries rely on")
	for _, k := range []Kind{InFlight, Partial, Missing, Unrecorded} {
		assert.True(t, k.UnsafeToBlindRetry(), "%s must require an explicit decision", k)
	}
}
