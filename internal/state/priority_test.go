// priority_test.go — the priority vocabulary, its accepted set, and the one
// spelling it shares with the approval tier, all pinned against the owners
// rather than against a restated list.
//
// Why the intersection test matters: "the two vocabularies overlap only at high"
// was for a long time only a comment (internal/grpc/change_service.go's NOTE and
// risk.go's vocabulary block both relied on it). If a fifth priority or a fourth
// tier is ever added, that comment silently becomes false — and the value that
// made it false is exactly the one a reader of runs.approval_level could not
// attribute. Asserting the intersection turns the comment into a checked fact.
package state

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nexus/levee/internal/dsl"
)

func TestPriorityVocabularyIsExactlyTheFourDeclaredNames(t *testing.T) {
	names := PriorityNames()
	assert.Equal(t, []string{"low", "normal", "high", "urgent"}, names,
		"the priority vocabulary changed shape; docs, the UI select and any stored rows must follow")

	// Values must equal the constant names: a value that differs from its
	// identifier is how a reader starts matching on the wrong string.
	assert.Equal(t, PriorityLow, "low")
	assert.Equal(t, PriorityNormal, "normal")
	assert.Equal(t, PriorityHigh, "high")
	assert.Equal(t, PriorityUrgent, "urgent")

	// No duplicates — a repeated entry makes PriorityNamesJoined lie.
	seen := map[string]bool{}
	for _, n := range names {
		assert.False(t, seen[n], "priority %q declared twice", n)
		seen[n] = true
	}
}

func TestPriorityNamesReturnsACopy(t *testing.T) {
	first := PriorityNames()
	require.Len(t, first, 4)
	first[0] = "tampered"
	assert.Equal(t, PriorityLow, PriorityNames()[0],
		"PriorityNames exposed its backing slice: one caller's edit would change the accepted set for everyone")
}

func TestIsPriorityAcceptsExactlyTheDeclaredSet(t *testing.T) {
	for _, name := range PriorityNames() {
		assert.True(t, IsPriority(name), "%q is declared but rejected", name)
	}

	// Rejected: the empty value ("" is the create path's "unspecified" marker,
	// never a priority), spelling/case/space variants, and every foreign
	// vocabulary that could otherwise be mistaken for one.
	for _, bad := range []string{
		"", "High", "HIGH", " normal", "urgent ", "none", "auto",
		"medium", "critical", // plan.RiskLevel / alert severity — never a priority
		"standard", "emergency", // tiers — never a priority
	} {
		assert.False(t, IsPriority(bad), "IsPriority accepted %q, which no constant declares", bad)
	}
}

func TestPriorityAndTierVocabulariesShareOnlyHigh(t *testing.T) {
	shared := []string{}
	for _, p := range PriorityNames() {
		if dsl.IsApprovalLevel(p) {
			shared = append(shared, p)
		}
	}
	assert.Equal(t, []string{PriorityHigh}, shared,
		"priority and approval tier now share %v; the single-column history in runs.approval_level is only decidable while exactly one spelling overlaps",
		strings.Join(shared, "|"))

	// And neither list is a subset of the other, which is what made the shared
	// column ambiguous in the first place.
	tiers := dsl.ApprovalLevels
	onlyTier := false
	for _, tname := range tiers {
		if !IsPriority(tname) {
			onlyTier = true
		}
	}
	assert.True(t, onlyTier, "every tier is also a priority — the two vocabularies have collapsed into one")
}

func TestPriorityNamesJoinedIsUsableInErrorText(t *testing.T) {
	joined := PriorityNamesJoined()
	for _, name := range PriorityNames() {
		assert.Contains(t, joined, name, "error text omits %q while the predicate accepts it", name)
	}
	// A caller comparing the rejection message against the accepted set must not
	// get a list that includes something the predicate refuses.
	assert.NotContains(t, joined, "medium")
	assert.NotContains(t, joined, "critical")
}
