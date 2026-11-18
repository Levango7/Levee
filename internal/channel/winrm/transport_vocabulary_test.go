package winrm

// transport_vocabulary_test.go — 配置接受的 transport 必须覆盖通道实现的，
// 通道实现的必须被配置接受。与 internal/channel/ssh/config_vocabulary_test.go
// 同构，只是这里的词表只有两个拼写、且不需要密钥探针。
//
// Why this guard exists: internal/config validated
// channel.winrm.transport against negotiate|kerberos, while
// internal/channel/winrm implements negotiate|ntlm (Config.withDefaults +
// Create's transport switch). Two lists, two owners, nothing comparing them.
// The consequence was not a crash: it was a promise the config layer made and
// the transport layer could not keep — an operator who wrote `transport:
// kerberos` passed validation, booted fine, and then failed every connection
// with `winrm: unsupported transport "kerberos"`; meanwhile `transport: ntlm`,
// which the channel runs, was refused at validation time. The channel's own
// comment says DefaultTransport is negotiate and TransportNTLM forces NTLMv2 —
// kerberos is not implemented anywhere in this package.
//
// config.WinRMTransports() is now the single list; this test pins the channel
// to it from the other side, so a future transport added here (or a spelling
// removed from the config) turns red instead of shipping as a silent
// mismatch. The dispatch check goes through Create (not just the constants)
// because the drift that matters is runtime behaviour, not naming.

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nexus/levee/internal/config"
)

// channelTransports is the set of non-empty transports the channel's Create
// actually dispatches on. The empty string is not a spelling of its own:
// Config.withDefaults normalises it to DefaultTransport (negotiate), so the
// config list must contain exactly these two.
func channelTransports() []string {
	return []string{TransportNegotiate, TransportNTLM}
}

// fakeTarget is provided by winrm_test.go (newFakeTarget); it is the minimum
// Create needs: a host, a username and a password. Create never connects here
// — it fails closed on the transport switch before any network IO — so the
// fake is enough to make the probe measure dispatch, not dialing.

// TestConfigAcceptsEveryTransportTheChannelImplements pins implementation ⊆
// config: a transport the channel can run must be selectable from the config
// file, or it is an unreachable capability.
func TestConfigAcceptsEveryTransportTheChannelImplements(t *testing.T) {
	accepted := config.WinRMTransports()
	for _, tr := range channelTransports() {
		assert.Containsf(t, accepted, tr,
			"the channel implements transport %q but the config vocabulary does not "+
				"accept it — unreachable capability; add it to config.WinRMTransports()", tr)
	}
}

// TestConfigRejectsWhatTheChannelCannotDispatch pins config ⊆ implementation:
// a spelling the channel's Create switch refuses must fail validation, or a
// typo becomes a boot-time-ok / connect-time-error time bomb.
func TestConfigRejectsWhatTheChannelCannotDispatch(t *testing.T) {
	// kerberos is the historical offender: the old config accepted it, the
	// channel never implemented it. It must stay refused.
	for _, tr := range []string{"kerberos", "Kerberos", " KERBEROS ", "ssl", "raw", " negotiate "} {
		f := NewFactory(Config{Transport: tr})
		_, err := f.Create(newFakeTarget("winrm-guard-host"))
		assert.Errorf(t, err, "transport %q must not dispatch on the channel side", tr)
	}
	// The empty string is the one value the channel's Create switch accepts
	// and the config vocabulary does not list: Create normalises it to
	// negotiate via withDefaults. That asymmetry is unreachable from the Load
	// path (SetDefault fills "negotiate"), is pinned by
	// TestWithDefaultsNormalisesEmptyTransport below, and is deliberately NOT
	// a config spelling — the vocabulary stays closed.
}

// TestConfigAndChannelSpellingsAreTheSameList pins the two lists to the same
// words in the same order — the same guard shape as chatops and ssh, because
// order matters for the error message a rejected operator reads.
func TestConfigAndChannelSpellingsAreTheSameList(t *testing.T) {
	assert.Equal(t, channelTransports(), config.WinRMTransports(),
		"the config vocabulary and the channel's dispatch set must be the same "+
			"list in the same order; drift here is exactly the defect this guard exists for")
}

// TestChannelDispatchesEveryConfigAcceptedTransport is the behavioural half
// of config ⊆ implementation: for every spelling the config accepts, Create
// must return a channel, not an error.
func TestChannelDispatchesEveryConfigAcceptedTransport(t *testing.T) {
	for _, tr := range config.WinRMTransports() {
		f := NewFactory(Config{Transport: tr})
		c, err := f.Create(newFakeTarget("winrm-guard-host"))
		require.NoErrorf(t, err, "transport %q is accepted by the config but refused "+
			"by the channel — the validator is promising something the transport "+
			"cannot deliver", tr)
		require.NotNil(t, c)
	}
}

// TestWithDefaultsNormalisesEmptyTransport pins the one non-spelling the
// channel treats specially: the empty transport means "negotiate", it is not a
// third vocabulary member. If this ever stops holding, the config side must
// decide explicitly whether "" is an accepted value.
func TestWithDefaultsNormalisesEmptyTransport(t *testing.T) {
	got := Config{Transport: ""}.withDefaults()
	assert.Equal(t, DefaultTransport, got.Transport,
		"empty transport must normalise to negotiate, not stay empty — the config "+
			"vocabulary is closed and does not include \"\"")
}
