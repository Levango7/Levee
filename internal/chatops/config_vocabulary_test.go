package chatops

// config_vocabulary_test.go — 配置接受的 platform 拼写必须与适配器能解析的完全一致。
//
// Why this guard exists: the `platform` field of notify.chatops.bots[] had
// two vocabularies at once. The CLI flag went through ParsePlatform — which
// has always accepted "lark" / "dingding" / "dd", case-insensitively and
// whitespace-tolerantly — while the serve config path switched on the raw
// string and accepted exactly three spellings ("slack", "dingtalk",
// "feishu"). A bot configured as `platform: lark` therefore worked from the
// CLI and refused to boot the server, and the refusal message listed a
// hand-written platform set that nothing compared against the adapters.
//
// The direction of the fix matters: the config side may only accept what the
// implementation can parse, and the implementation may only accept what the
// config can name. Each direction is asserted below, plus the stronger
// statement that the two lists are the SAME list (same entries, same order)
// so the error message and the acceptance cannot disagree either.
//
// This test lives on the side that IMPLEMENTS the vocabulary, and reads the
// config side's list from internal/config — the same arrangement as
// internal/channel/ssh/config_vocabulary_test.go. internal/config must not
// import internal/chatops: the guard has to point the other way.

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nexus/levee/internal/config"
)

// TestConfigAcceptsEveryPlatformTheAdapterImplements — implementation ⊆ config.
//
// A platform that exists in the adapter set but that the configuration
// cannot name is a capability nobody can reach: the operator has no way to
// select it, so it is also never exercised.
func TestConfigAcceptsEveryPlatformTheAdapterImplements(t *testing.T) {
	accepted := map[string]bool{}
	for _, s := range config.ChatOpsPlatforms() {
		accepted[s] = true
	}
	require.NotEmpty(t, accepted, "the probe reads an empty accepted set")

	for _, p := range SupportedPlatforms() {
		found := false
		for s := range accepted {
			got, err := ParsePlatform(s)
			if err == nil && got == p {
				found = true
				break
			}
		}
		assert.Truef(t, found,
			"the adapter set declares platform %q but internal/config accepts no spelling for it: "+
				"the platform is unreachable (and untested) through configuration", p)
	}
}

// TestConfigRejectsWhatTheAdapterCannotParse — config ⊆ implementation.
//
// The other direction: widening the config to a spelling the adapter cannot
// parse would turn a typo into a boot-time failure deep inside the wiring
// instead of a named validation error.
func TestConfigRejectsWhatTheAdapterCannotParse(t *testing.T) {
	require.NotEmpty(t, config.ChatOpsPlatforms(), "the probe reads an empty accepted set")

	for _, s := range config.ChatOpsPlatforms() {
		p, err := ParsePlatform(s)
		require.NoErrorf(t, err,
			"internal/config accepts platform %q but internal/chatops cannot parse it", s)
		assert.Containsf(t, SupportedPlatforms(), p,
			"internal/config accepts %q but it maps to platform %q, which has no adapter", s, p)
	}
}

// TestConfigAndAdapterSpellingsAreTheSameList pins the two lists together.
//
// Set equality would already catch a missing spelling; comparing the lists
// themselves additionally pins the order, which is what every error message
// prints. A vocabulary that is accepted in one place and merely mentioned in
// the other is exactly the drift this file exists to stop.
func TestConfigAndAdapterSpellingsAreTheSameList(t *testing.T) {
	assert.Equal(t, PlatformSpellings(), config.ChatOpsPlatforms(),
		"the config side and the adapter side must carry the same vocabulary "+
			"(same entries, same order) — otherwise the acceptance and the error message disagree")
}

// TestEveryDeclaredPlatformHasASpelling — no orphan platform, no stray alias.
//
// Two failure modes in one assertion pair: a declared platform that no
// spelling maps to (unreachable), and a spelling that maps to a platform
// that is not declared (a name for an adapter that does not exist).
func TestEveryDeclaredPlatformHasASpelling(t *testing.T) {
	declared := map[Platform]bool{}
	for _, p := range SupportedPlatforms() {
		declared[p] = true
	}
	require.NotEmpty(t, declared, "the probe reads an empty declared set")

	seen := map[Platform]bool{}
	for _, e := range platformSpellings {
		require.Truef(t, declared[e.Platform],
			"spelling %q maps to platform %q, which is not declared in supportedPlatforms", e.Spelling, e.Platform)
		seen[e.Platform] = true
	}
	for p := range declared {
		assert.Truef(t, seen[p],
			"platform %q is declared but no accepted spelling maps to it", p)
	}
}

// TestParsePlatformIsCaseAndWhitespaceTolerant pins the normalisation the
// config side assumes. The serve path and the CLI path both resolve through
// this function, so this is what makes "Feishu" and " feishu " mean the same
// thing on both.
func TestParsePlatformIsCaseAndWhitespaceTolerant(t *testing.T) {
	cases := map[string]Platform{
		"Feishu":   PlatformFeishu,
		" feishu ": PlatformFeishu,
		"LARK":     PlatformFeishu,
		"DingTalk": PlatformDingtalk,
		" DD ":     PlatformDingtalk,
		"dingding": PlatformDingtalk,
		"SLACK":    PlatformSlack,
	}
	for spelling, want := range cases {
		got, err := ParsePlatform(spelling)
		require.NoErrorf(t, err, "spelling %q must parse", spelling)
		assert.Equalf(t, want, got, "spelling %q resolved to the wrong platform", spelling)
	}
}

// TestParsePlatformRefusesAnUnknownPlatform — the fail-closed half.
func TestParsePlatformRefusesAnUnknownPlatform(t *testing.T) {
	_, err := ParsePlatform("teams")
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrUnknownPlatform)
}
