// exec_target_namer_test.go pins the addressing boundary a runtime channel
// plugin depends on: the live target exposes the inventory row's credential
// reference NAME (channel.CredentialRefNamer) so the plugin channel can send
// that instead of the expanded, plaintext CredentialRef.
package wiring

import (
	"testing"

	"github.com/nexus/levee/internal/channel"
	"github.com/nexus/levee/internal/state"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLiveTargetExposesCredentialRefName(t *testing.T) {
	lt := &liveTarget{t: &state.Target{Hostname: "h1", CredentialRef: "acme-prod-token"}}

	namer, ok := any(lt).(channel.CredentialRefNamer)
	require.True(t, ok, "liveTarget must implement channel.CredentialRefNamer")
	assert.Equal(t, "acme-prod-token", namer.CredentialRefName())

	// A row without a reference is not an error: the plugin gets no
	// credential_ref key at all.
	empty := &liveTarget{t: &state.Target{Hostname: "h2"}}
	assert.Empty(t, empty.CredentialRefName())
}
