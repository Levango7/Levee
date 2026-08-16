package permission

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseLabelConditionEmpty(t *testing.T) {
	c, err := ParseLabelCondition("")
	require.NoError(t, err)
	ok, err := c.Evaluate(nil)
	require.NoError(t, err)
	assert.True(t, ok)
}

func TestParseLabelConditionEq(t *testing.T) {
	c, err := ParseLabelCondition("target.env = prod")
	require.NoError(t, err)

	ok, err := c.Evaluate(map[string]string{"target.env": "prod"})
	require.NoError(t, err)
	assert.True(t, ok)

	ok, err = c.Evaluate(map[string]string{"target.env": "dev"})
	require.NoError(t, err)
	assert.False(t, ok)
}

func TestParseLabelConditionNeq(t *testing.T) {
	c, err := ParseLabelCondition("target.env != prod")
	require.NoError(t, err)

	ok, err := c.Evaluate(map[string]string{"target.env": "dev"})
	require.NoError(t, err)
	assert.True(t, ok)

	ok, err = c.Evaluate(map[string]string{"target.env": "prod"})
	require.NoError(t, err)
	assert.False(t, ok)
}

func TestParseLabelConditionIn(t *testing.T) {
	c, err := ParseLabelCondition("target.env in [prod, staging]")
	require.NoError(t, err)

	ok, err := c.Evaluate(map[string]string{"target.env": "prod"})
	require.NoError(t, err)
	assert.True(t, ok)

	ok, err = c.Evaluate(map[string]string{"target.env": "staging"})
	require.NoError(t, err)
	assert.True(t, ok)

	ok, err = c.Evaluate(map[string]string{"target.env": "dev"})
	require.NoError(t, err)
	assert.False(t, ok)
}

func TestParseLabelConditionNotIn(t *testing.T) {
	c, err := ParseLabelCondition("target.env not_in [prod, staging]")
	require.NoError(t, err)

	ok, err := c.Evaluate(map[string]string{"target.env": "dev"})
	require.NoError(t, err)
	assert.True(t, ok)

	ok, err = c.Evaluate(map[string]string{"target.env": "prod"})
	require.NoError(t, err)
	assert.False(t, ok)
}

func TestParseLabelConditionAnd(t *testing.T) {
	c, err := ParseLabelCondition("target.env = prod AND change.risk = high")
	require.NoError(t, err)

	ok, err := c.Evaluate(map[string]string{"target.env": "prod", "change.risk": "high"})
	require.NoError(t, err)
	assert.True(t, ok)

	ok, err = c.Evaluate(map[string]string{"target.env": "prod", "change.risk": "low"})
	require.NoError(t, err)
	assert.False(t, ok)

	ok, err = c.Evaluate(map[string]string{"target.env": "dev", "change.risk": "high"})
	require.NoError(t, err)
	assert.False(t, ok)
}

func TestParseLabelConditionOr(t *testing.T) {
	c, err := ParseLabelCondition("target.env = prod OR target.env = staging")
	require.NoError(t, err)

	ok, err := c.Evaluate(map[string]string{"target.env": "prod"})
	require.NoError(t, err)
	assert.True(t, ok)

	ok, err = c.Evaluate(map[string]string{"target.env": "staging"})
	require.NoError(t, err)
	assert.True(t, ok)

	ok, err = c.Evaluate(map[string]string{"target.env": "dev"})
	require.NoError(t, err)
	assert.False(t, ok)
}

func TestParseLabelConditionGrouped(t *testing.T) {
	c, err := ParseLabelCondition("(target.env = prod AND change.risk = high) OR target.env = dev")
	require.NoError(t, err)

	ok, err := c.Evaluate(map[string]string{"target.env": "prod", "change.risk": "high"})
	require.NoError(t, err)
	assert.True(t, ok)

	ok, err = c.Evaluate(map[string]string{"target.env": "dev"})
	require.NoError(t, err)
	assert.True(t, ok)

	ok, err = c.Evaluate(map[string]string{"target.env": "prod", "change.risk": "low"})
	require.NoError(t, err)
	assert.False(t, ok)
}

func TestParseLabelConditionQuotedValue(t *testing.T) {
	c, err := ParseLabelCondition(`target.name = "my target"`)
	require.NoError(t, err)

	ok, err := c.Evaluate(map[string]string{"target.name": "my target"})
	require.NoError(t, err)
	assert.True(t, ok)
}

func TestParseLabelConditionInvalid(t *testing.T) {
	cases := []string{
		"this is not valid",
		"= prod",
		"target.env =",
		"target.env in",
		"target.env in []",
		"target.env in [a",
		"(target.env = prod",
		"target.env = prod)",
	}
	for _, s := range cases {
		t.Run(s, func(t *testing.T) {
			_, err := ParseLabelCondition(s)
			require.Error(t, err, "expected error for %q", s)
		})
	}
}

func TestLabelConditionString(t *testing.T) {
	c, err := ParseLabelCondition("target.env = prod AND change.risk = high")
	require.NoError(t, err)
	s := c.String()
	assert.Contains(t, s, "target.env")
	assert.Contains(t, s, "prod")
	assert.Contains(t, s, "AND")
	assert.Contains(t, s, "change.risk")
	assert.Contains(t, s, "high")
}

func TestABACEngineEvaluate(t *testing.T) {
	ps := NewPolicySet()
	require.NoError(t, ps.Add(&Policy{ID: "allow-prod", Effect: EffectAllow, Resource: "target:*", Action: "apply", Condition: "target.env = prod"}))
	require.NoError(t, ps.Add(&Policy{ID: "deny-high-risk", Effect: EffectDeny, Resource: "target:*", Action: "apply", Condition: "change.risk = high"}))

	engine := NewABACEngine(ps)

	// prod, low risk -> allow.
	allowed, reason := engine.Evaluate("alice", "apply", "target:abc", map[string]string{"target.env": "prod", "change.risk": "low"})
	assert.True(t, allowed)
	assert.Contains(t, reason, "allow-prod")

	// prod, high risk -> deny wins.
	allowed, reason = engine.Evaluate("alice", "apply", "target:abc", map[string]string{"target.env": "prod", "change.risk": "high"})
	assert.False(t, allowed)
	assert.Contains(t, reason, "deny-high-risk")

	// dev -> no matching allow.
	allowed, reason = engine.Evaluate("alice", "apply", "target:abc", map[string]string{"target.env": "dev"})
	assert.False(t, allowed)
	assert.Contains(t, reason, "no matching policy")
}

func TestABACEngineNilPolicies(t *testing.T) {
	engine := NewABACEngine(nil)
	allowed, reason := engine.Evaluate("u", "a", "r", nil)
	assert.False(t, allowed)
	assert.Contains(t, reason, "no matching policy")
}

func TestABACEngineSetPolicies(t *testing.T) {
	engine := NewABACEngine(nil)
	ps := NewPolicySet()
	require.NoError(t, ps.Add(&Policy{ID: "a1", Effect: EffectAllow, Resource: "*", Action: "*"}))
	engine.SetPolicies(ps)

	allowed, _ := engine.Evaluate("u", "a", "r", nil)
	assert.True(t, allowed)
}

func TestABACEngineExplain(t *testing.T) {
	ps := NewPolicySet()
	require.NoError(t, ps.Add(&Policy{ID: "a1", Effect: EffectAllow, Resource: "*", Action: "view"}))
	engine := NewABACEngine(ps)

	out := engine.Explain("alice", "view", "change:1", map[string]string{"target.env": "prod"})
	assert.Contains(t, out, "ALLOW")
	assert.Contains(t, out, "alice")
	assert.Contains(t, out, "view")
	assert.Contains(t, out, "change:1")
	assert.Contains(t, out, "target.env")
	assert.Contains(t, out, "prod")
}

func TestABACEngineExplainDeny(t *testing.T) {
	ps := NewPolicySet()
	engine := NewABACEngine(ps)

	out := engine.Explain("alice", "view", "change:1", nil)
	assert.Contains(t, out, "DENY")
}
