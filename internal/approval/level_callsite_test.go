// Call-site tests for the approval tier vocabulary.
//
// The tier list is now defined once (dsl.ApprovalLevels, re-exported here as
// levelOrder), which is only worth something if the places that *judge* it keep
// judging it. A table nobody consults is the same bug in a tidier box, so each
// judgement site gets its own accept/refuse pair here — including the error
// text, which used to be a fourth hand-typed copy of the list.
package approval

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestServiceCreate_RefusesTierOutsideVocabulary(t *testing.T) {
	store := newMockStore()
	svc := NewService(store)
	ctx := context.Background()

	// Acceptance half: everything the vocabulary offers is usable at this call
	// site, with the tier recorded as asked.
	for _, level := range LevelNames() {
		a, err := svc.Create(ctx, CreateRequest{
			RunID: "run-" + level, Level: level, MinApprovers: 1,
		})
		require.NoError(t, err, "tier %q comes from the vocabulary, so Create must accept it", level)
		require.NotNil(t, a)
		assert.Equal(t, level, a.Level)
	}

	before := len(store.approvals)
	for _, bad := range []string{"", "platinum", "Standard", "high ", "urgent"} {
		a, err := svc.Create(ctx, CreateRequest{RunID: "run-bad", Level: bad})
		require.Error(t, err, "level %q is not in the vocabulary", bad)
		assert.ErrorIs(t, err, ErrInvalidLevel)
		assert.Nil(t, a, "a refused request must not return an approval")
		assert.Contains(t, err.Error(), LevelNamesJoined(),
			"the refusal must list what is accepted, assembled from the table it judged by")
	}
	assert.Equal(t, before, len(store.approvals),
		"refused requests must not reach the store")
}

func TestTemplateLibraryRegister_RefusesTierOutsideVocabulary(t *testing.T) {
	lib := NewTemplateLibrary()

	for _, level := range LevelNames() {
		require.NoError(t, lib.RegisterTemplate(Template{
			Name: "ok-" + level, RequiredLevel: level,
		}), "tier %q must be registerable", level)
	}

	err := lib.RegisterTemplate(Template{Name: "bad", RequiredLevel: "critical"})
	require.ErrorIs(t, err, ErrInvalidLevel)
	assert.Contains(t, err.Error(), LevelNamesJoined())
	assert.Contains(t, err.Error(), `"critical"`, "the offending value must be named, not just the accepted set")
}

// TestLevelTableIsTheOnlyDefinition is the guard the equality assertions cannot
// be: a refusal message typed out as "allowed: standard, high, emergency" passes
// them all, because that is what the table renders today. So the table is
// extended here and every consumer must follow — which is the only way to know
// they read it rather than restated it.
func TestLevelTableIsTheOnlyDefinition(t *testing.T) {
	saved := append([]string(nil), levelOrder...)
	t.Cleanup(func() { levelOrder = append([]string(nil), saved...) })
	levelOrder = append(append([]string(nil), saved...), "beta-tier")

	require.Contains(t, LevelNamesJoined(), "beta-tier")

	_, err := NewService(newMockStore()).Create(context.Background(),
		CreateRequest{RunID: "run-table", Level: "no-such-tier"})
	require.ErrorIs(t, err, ErrInvalidLevel)
	assert.Contains(t, err.Error(), "beta-tier",
		"Create's accepted list is assembled from the table, not typed out beside it")

	err = NewTemplateLibrary().RegisterTemplate(Template{Name: "t", RequiredLevel: "no-such-tier"})
	require.ErrorIs(t, err, ErrInvalidLevel)
	assert.Contains(t, err.Error(), "beta-tier",
		"template registration's accepted list follows the same table")

	m := NewLevelManager()
	_, err = m.Get("no-such-tier")
	require.ErrorIs(t, err, ErrInvalidLevel)
	assert.Contains(t, err.Error(), "beta-tier", "LevelManager.Get follows the same table")

	err = m.SetConfig(LevelConfig{Level: "no-such-tier"})
	require.ErrorIs(t, err, ErrInvalidLevel)
	assert.Contains(t, err.Error(), "beta-tier", "LevelManager.SetConfig follows the same table")

	assert.Len(t, NewLevelManager().All(), len(levelOrder),
		"All() enumerates the table instead of restating three names")
}
