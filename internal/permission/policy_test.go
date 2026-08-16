package permission

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPolicyValidate(t *testing.T) {
	cases := []struct {
		name    string
		policy  Policy
		wantErr error
	}{
		{
			name:   "valid allow",
			policy: Policy{Effect: EffectAllow, Resource: "change:*", Action: "apply"},
		},
		{
			name:   "valid deny with condition",
			policy: Policy{Effect: EffectDeny, Resource: "target:*", Action: "apply", Condition: "target.env = prod"},
		},
		{
			name:    "unknown effect",
			policy:  Policy{Effect: "maybe", Resource: "x", Action: "y"},
			wantErr: ErrUnknownEffect,
		},
		{
			name:    "empty resource",
			policy:  Policy{Effect: EffectAllow, Resource: "", Action: "y"},
			wantErr: ErrEmptyResource,
		},
		{
			name:    "empty action",
			policy:  Policy{Effect: EffectAllow, Resource: "x", Action: ""},
			wantErr: ErrEmptyPolicyAction,
		},
		{
			name:    "invalid condition",
			policy:  Policy{Effect: EffectAllow, Resource: "x", Action: "y", Condition: "target.env ="},
			wantErr: ErrInvalidCondition,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.policy.Validate()
			if tc.wantErr != nil {
				require.Error(t, err)
				assert.ErrorIs(t, err, tc.wantErr)
			} else {
				assert.NoError(t, err)
			}
		})
	}
}

func TestPolicyMatchesResource(t *testing.T) {
	cases := []struct {
		name     string
		pattern  string
		request  string
		want     bool
	}{
		{"exact", "change:abc", "change:abc", true},
		{"exact mismatch", "change:abc", "change:def", false},
		{"wildcard all", "*", "anything", true},
		{"kind wildcard", "change:*", "change:abc", true},
		{"kind wildcard mismatch", "change:*", "target:abc", false},
		{"label selector matches kind", "target:env=prod", "target:abc", true},
		{"label selector mismatch kind", "target:env=prod", "change:abc", false},
		{"no kind exact", "abc", "abc", true},
		{"no kind mismatch", "abc", "def", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := &Policy{Resource: tc.pattern}
			assert.Equal(t, tc.want, p.matchesResource(tc.request))
		})
	}
}

func TestPolicyMatchesAction(t *testing.T) {
	p := &Policy{Action: "apply"}
	assert.True(t, p.matchesAction("apply"))
	assert.False(t, p.matchesAction("view"))

	pWild := &Policy{Action: Wildcard}
	assert.True(t, pWild.matchesAction("anything"))
}

func TestPolicyMatchesCondition(t *testing.T) {
	p := &Policy{Condition: "target.env = prod"}
	ok, err := p.matchesCondition(map[string]string{"target.env": "prod"})
	require.NoError(t, err)
	assert.True(t, ok)

	ok, err = p.matchesCondition(map[string]string{"target.env": "dev"})
	require.NoError(t, err)
	assert.False(t, ok)

	// Empty condition always matches.
	pEmpty := &Policy{}
	ok, err = pEmpty.matchesCondition(nil)
	require.NoError(t, err)
	assert.True(t, ok)
}

func TestPolicyMatches(t *testing.T) {
	p := &Policy{
		Effect:    EffectAllow,
		Resource:  "change:*",
		Action:    "apply",
		Condition: "change.risk != high",
	}
	ok, err := p.Matches("change:abc", "apply", map[string]string{"change.risk": "low"})
	require.NoError(t, err)
	assert.True(t, ok)

	ok, err = p.Matches("change:abc", "apply", map[string]string{"change.risk": "high"})
	require.NoError(t, err)
	assert.False(t, ok)

	ok, err = p.Matches("change:abc", "view", map[string]string{"change.risk": "low"})
	require.NoError(t, err)
	assert.False(t, ok)
}

func TestPolicySetAddRemove(t *testing.T) {
	ps := NewPolicySet()
	require.NoError(t, ps.Add(&Policy{ID: "p1", Effect: EffectAllow, Resource: "*", Action: "view"}))
	assert.Equal(t, 1, ps.Len())

	assert.True(t, ps.Remove("p1"))
	assert.Equal(t, 0, ps.Len())
	assert.False(t, ps.Remove("p1"))
}

func TestPolicySetAddInvalid(t *testing.T) {
	ps := NewPolicySet()
	err := ps.Add(&Policy{Effect: "maybe", Resource: "*", Action: "view"})
	require.Error(t, err)
	assert.Equal(t, 0, ps.Len())
}

func TestPolicySetAddNil(t *testing.T) {
	ps := NewPolicySet()
	err := ps.Add(nil)
	require.Error(t, err)
}

func TestPolicySetEvaluateAllow(t *testing.T) {
	ps := NewPolicySet()
	require.NoError(t, ps.Add(&Policy{ID: "a1", Effect: EffectAllow, Resource: "change:*", Action: "apply"}))

	allowed, err := ps.Evaluate(EvaluationContext{Subject: "u", Action: "apply", Resource: "change:123"})
	require.NoError(t, err)
	assert.True(t, allowed)
}

func TestPolicySetEvaluateDenyWins(t *testing.T) {
	ps := NewPolicySet()
	require.NoError(t, ps.Add(&Policy{ID: "a1", Effect: EffectAllow, Resource: "change:*", Action: "apply"}))
	require.NoError(t, ps.Add(&Policy{ID: "d1", Effect: EffectDeny, Resource: "change:*", Action: "apply", Condition: "change.risk = high"}))

	// Low risk: allow.
	allowed, err := ps.Evaluate(EvaluationContext{
		Subject:  "u",
		Action:   "apply",
		Resource: "change:123",
		Labels:   map[string]string{"change.risk": "low"},
	})
	require.NoError(t, err)
	assert.True(t, allowed)

	// High risk: deny wins.
	allowed, err = ps.Evaluate(EvaluationContext{
		Subject:  "u",
		Action:   "apply",
		Resource: "change:123",
		Labels:   map[string]string{"change.risk": "high"},
	})
	require.NoError(t, err)
	assert.False(t, allowed)
}

func TestPolicySetEvaluateNoMatch(t *testing.T) {
	ps := NewPolicySet()
	require.NoError(t, ps.Add(&Policy{ID: "a1", Effect: EffectAllow, Resource: "change:*", Action: "apply"}))

	allowed, err := ps.Evaluate(EvaluationContext{Subject: "u", Action: "view", Resource: "change:123"})
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrNoMatch)
	assert.False(t, allowed)
}

func TestPolicySetEvaluateEmpty(t *testing.T) {
	ps := NewPolicySet()
	allowed, err := ps.Evaluate(EvaluationContext{Subject: "u", Action: "view", Resource: "x"})
	require.Error(t, err)
	assert.False(t, allowed)
}

func TestPolicySetList(t *testing.T) {
	ps := NewPolicySet()
	require.NoError(t, ps.Add(&Policy{ID: "b", Effect: EffectAllow, Resource: "x", Action: "y"}))
	require.NoError(t, ps.Add(&Policy{ID: "a", Effect: EffectAllow, Resource: "x", Action: "y"}))

	list := ps.List()
	require.Len(t, list, 2)
	assert.Equal(t, "a", list[0].ID)
	assert.Equal(t, "b", list[1].ID)
}

func TestPolicySetLoadFromConfig(t *testing.T) {
	cfg := PolicyConfig{
		Policies: []Policy{
			{ID: "p1", Effect: EffectAllow, Resource: "*", Action: "view"},
			{ID: "p2", Effect: EffectDeny, Resource: "change:*", Action: "apply", Condition: "change.risk = high"},
		},
	}
	ps := NewPolicySet()
	require.NoError(t, ps.LoadFromConfig(cfg))
	assert.Equal(t, 2, ps.Len())
}

func TestPolicySetLoadFromConfigInvalid(t *testing.T) {
	cfg := PolicyConfig{
		Policies: []Policy{
			{ID: "p1", Effect: "maybe", Resource: "*", Action: "view"},
		},
	}
	ps := NewPolicySet()
	err := ps.LoadFromConfig(cfg)
	require.Error(t, err)
	assert.Equal(t, 0, ps.Len())
}

func TestPolicySetLoadFromYAML(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "policies.yaml")
	data := []byte("policies:\n  - id: p1\n    effect: allow\n    resource: \"*\"\n    action: view\n")
	require.NoError(t, os.WriteFile(path, data, 0o644))

	ps := NewPolicySet()
	require.NoError(t, ps.LoadFromYAML(path))
	assert.Equal(t, 1, ps.Len())
}

func TestPolicySetMarshalYAML(t *testing.T) {
	ps := NewPolicySet()
	require.NoError(t, ps.Add(&Policy{ID: "p1", Effect: EffectAllow, Resource: "*", Action: "view"}))

	data, err := ps.MarshalYAML()
	require.NoError(t, err)
	assert.Contains(t, string(data), "p1")

	// Round-trip.
	ps2 := NewPolicySet()
	require.NoError(t, ps2.LoadFromYAML(writeTemp(t, data)))
	assert.Equal(t, 1, ps2.Len())
}

// writeTemp writes data to a temp file and returns its path.
func writeTemp(t *testing.T, data []byte) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "policies.yaml")
	require.NoError(t, os.WriteFile(path, data, 0o644))
	return path
}

func TestPolicySetEvaluateWildcardAction(t *testing.T) {
	ps := NewPolicySet()
	require.NoError(t, ps.Add(&Policy{ID: "a1", Effect: EffectAllow, Resource: "*", Action: "*"}))

	allowed, err := ps.Evaluate(EvaluationContext{Subject: "u", Action: "anything", Resource: "anywhere"})
	require.NoError(t, err)
	assert.True(t, allowed)
}