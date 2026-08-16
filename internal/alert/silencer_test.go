package alert

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestSilenceRuleMatches covers every matching branch.
func TestSilenceRuleMatches(t *testing.T) {
	now := time.Unix(0, 0)
	alert := &Alert{Source: "prom", Severity: SeverityCritical, Labels: map[string]string{"host": "n1", "env": "prod"}}

	cases := []struct {
		name string
		rule SilenceRule
		want bool
	}{
		{"empty match", SilenceRule{}, true},
		{"label match", SilenceRule{Match: map[string]string{"host": "n1"}}, true},
		{"label mismatch", SilenceRule{Match: map[string]string{"host": "other"}}, false},
		{"source match", SilenceRule{Source: "prom"}, true},
		{"source mismatch", SilenceRule{Source: "custom"}, false},
		{"min sev match", SilenceRule{MinSeverity: SeverityWarning}, true},
		{"min sev mismatch", SilenceRule{MinSeverity: SeverityCritical + 1}, false},
		{"expired", SilenceRule{Expires: now.Add(-time.Second)}, false},
		{"combined", SilenceRule{Match: map[string]string{"env": "prod"}, Source: "prom", MinSeverity: SeverityCritical}, true},
	}
	for _, c := range cases {
		assert.Equal(t, c.want, c.rule.Matches(alert, now), c.name)
	}
}

// TestSilencerAddRemove covers rule lifecycle.
func TestSilencerAddRemove(t *testing.T) {
	s := NewSilencer()
	id := s.AddRule(SilenceRule{Match: map[string]string{"host": "n1"}, Reason: "maintenance"})
	assert.NotEmpty(t, id)
	assert.Equal(t, 1, s.Size())

	rule, err := s.GetRule(id)
	require.NoError(t, err)
	assert.Equal(t, "maintenance", rule.Reason)

	assert.True(t, s.RemoveRule(id))
	assert.False(t, s.RemoveRule(id))
	assert.Equal(t, 0, s.Size())

	_, err = s.GetRule(id)
	assert.Error(t, err)
}

// TestSilencerIsSilenced returns the first matching rule.
func TestSilencerIsSilenced(t *testing.T) {
	s := NewSilencer()
	id1 := s.AddRule(SilenceRule{Match: map[string]string{"env": "prod"}})
	id2 := s.AddRule(SilenceRule{Match: map[string]string{"host": "n1"}})

	a := &Alert{Source: "x", Severity: SeverityInfo, Labels: map[string]string{"env": "prod", "host": "n1"}}
	silenced, ruleID, _ := s.IsSilenced(a)
	assert.True(t, silenced)
	assert.Equal(t, id1, ruleID, "first matching rule in sorted order wins")

	b := &Alert{Source: "x", Severity: SeverityInfo, Labels: map[string]string{"host": "n1"}}
	silenced, ruleID, _ = s.IsSilenced(b)
	assert.True(t, silenced)
	assert.Equal(t, id2, ruleID)

	c := &Alert{Source: "x", Severity: SeverityInfo, Labels: map[string]string{"foo": "bar"}}
	silenced, _, _ = s.IsSilenced(c)
	assert.False(t, silenced)
}

// TestSilencerListRules returns a sorted snapshot.
func TestSilencerListRules(t *testing.T) {
	s := NewSilencer()
	s.AddRule(SilenceRule{ID: "b"})
	s.AddRule(SilenceRule{ID: "a"})
	rules := s.ListRules()
	require.Len(t, rules, 2)
	assert.Equal(t, "a", rules[0].ID)
	assert.Equal(t, "b", rules[1].ID)
}

// TestSilencerCleanup removes expired rules.
func TestSilencerCleanup(t *testing.T) {
	s := NewSilencer()
	clock := &fakeClock{t: time.Unix(0, 0)}
	s.now = clock.now

	s.AddRule(SilenceRule{ID: "expired", Duration: time.Minute})
	s.AddRule(SilenceRule{ID: "forever"})
	clock.advance(2 * time.Minute)
	removed := s.Cleanup()
	assert.Equal(t, 1, removed)
	assert.Equal(t, 1, s.Size())
}

// TestSilencerAutoID generates sequential IDs.
func TestSilencerAutoID(t *testing.T) {
	s := NewSilencer()
	id1 := s.AddRule(SilenceRule{})
	id2 := s.AddRule(SilenceRule{})
	assert.NotEqual(t, id1, id2)
	assert.Contains(t, id1, "silence-")
}

// TestMatchLabels helper.
func TestMatchLabels(t *testing.T) {
	m := matchLabels("a=1, b=2")
	assert.Equal(t, "1", m["a"])
	assert.Equal(t, "2", m["b"])
	assert.Nil(t, matchLabels(""))
}
