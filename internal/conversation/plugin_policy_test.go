package conversation

// plugin_policy_test.go pins the draft-promotion gate's half of E-3: a
// model-authored draft that steps on a plugin action the manifest left
// unclassified (or declared destructive) must be refused before a Change row
// exists, while a declared reversible action passes. The same
// dsl.PluginActionPolicy is what compile and plan time read, so the three gates
// cannot disagree about the draft.

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nexus/levee/internal/dsl"
)

func pluginDraft(action string) string {
	return `
name: plugin-policy-draft
version: "1.0"
target:
  type: host
  query: "os=linux"
steps:
  - name: s
    action: cloud.` + action + `
`
}

func cloudPolicy() dsl.PluginActionPolicy {
	return dsl.NewPluginActionPolicy(
		[]string{"cloud.destroy"},
		[]string{"cloud.apply"},
		[]string{"cloud"},
	)
}

// TestBridgeJudgesPluginActionsByPolicy walks the three states through the
// promotion gate.
func TestBridgeJudgesPluginActionsByPolicy(t *testing.T) {
	cases := []struct {
		action   string
		wantPass bool
	}{
		{action: "apply", wantPass: true},
		{action: "destroy", wantPass: false},
		{action: "probe", wantPass: false},
	}

	for _, tc := range cases {
		t.Run(tc.action, func(t *testing.T) {
			creator := &fakeChangeCreator{}
			e := NewConversationEngine(ConversationEngineConfig{
				ChangeCreator:      creator,
				PluginActionPolicy: cloudPolicy(),
			})
			sess := newBridgeSession(pluginDraft(tc.action))

			reply, err := e.promoteRecommendation(context.Background(), sess, sess.GetRecommendation())
			require.NoError(t, err)
			require.NotNil(t, reply)

			if tc.wantPass {
				require.Len(t, creator.calls, 1)
				require.NotNil(t, reply.Action)
				return
			}
			assert.Empty(t, creator.calls, "a refused draft must leave no Change row behind")
			assert.Nil(t, reply.Action)
			assert.Contains(t, reply.Text, "未通过校验")
			assert.Contains(t, reply.Text, "cloud."+tc.action)
		})
	}
}

// TestBridgeLegacyInherentIrreversibleStillGates pins the compatibility path:
// a caller that only knows the destructive declarations still gets the
// destructive step refused.
func TestBridgeLegacyInherentIrreversibleStillGates(t *testing.T) {
	creator := &fakeChangeCreator{}
	e := NewConversationEngine(ConversationEngineConfig{
		ChangeCreator:        creator,
		InherentIrreversible: []string{"cloud.destroy"},
	})
	sess := newBridgeSession(pluginDraft("destroy"))

	reply, err := e.promoteRecommendation(context.Background(), sess, sess.GetRecommendation())
	require.NoError(t, err)
	require.NotNil(t, reply)
	assert.Empty(t, creator.calls)
	assert.Contains(t, reply.Text, "未通过校验")
}
