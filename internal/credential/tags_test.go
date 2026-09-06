package credential

// SA-018: CredentialSpec.Tags round-trips through the credentials.tags column
// as a JSON map; pre-SA-018 rows (empty column) decode to a nil map.

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nexus/levee/internal/state"
)

func TestStore_TagsRoundTrip(t *testing.T) {
	st := newTestStore(t)
	cs := newTestCredentialStore(t, st, "master-pw")
	ctx := context.Background()

	cred, err := cs.Store(ctx, CredentialSpec{
		Name:      "web-ssh",
		Type:      "ssh_key",
		Plaintext: []byte("secret-material"),
		Tags:      map[string]string{"env": "prod", "target": "host-a"},
	})
	require.NoError(t, err)

	// The stored column is a JSON map encoding (deterministic: Go sorts map
	// keys), never a Go map rendering.
	assert.JSONEq(t, `{"env":"prod","target":"host-a"}`, cred.Tags)

	got, err := st.GetCredentialByName(ctx, "web-ssh")
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, cred.Tags, got.Tags)

	tags, err := ParseCredentialTags(got)
	require.NoError(t, err)
	assert.Equal(t, map[string]string{"env": "prod", "target": "host-a"}, tags)

	// List path passes tags through.
	list, err := cs.List(ctx)
	require.NoError(t, err)
	require.Len(t, list, 1)
	assert.Equal(t, cred.Tags, list[0].Tags)

	// Rotate preserves tags (the row is rewritten as loaded).
	rotated, err := cs.Rotate(ctx, "web-ssh", []byte("new-material"))
	require.NoError(t, err)
	assert.Equal(t, cred.Tags, rotated.Tags)
}

func TestStore_TagsEmptyStoredAsZeroValue(t *testing.T) {
	st := newTestStore(t)
	cs := newTestCredentialStore(t, st, "master-pw")
	ctx := context.Background()

	// nil and empty maps both store the column's zero value "", so rows
	// written before SA-018 are indistinguishable from tag-less rows.
	for _, tc := range []struct {
		name string
		tags map[string]string
	}{
		{"nil", nil},
		{"empty", map[string]string{}},
	} {
		cred, err := cs.Store(ctx, CredentialSpec{
			Name: "plain-" + tc.name, Type: "api_token",
			Plaintext: []byte("x"), Tags: tc.tags,
		})
		require.NoError(t, err)
		assert.Empty(t, cred.Tags, tc.name)
	}
}

func TestParseCredentialTags_LegacyRowAndMalformed(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "legacy-tags.db")
	ctx := context.Background()
	st, err := state.NewSQLiteStore(ctx, dbPath)
	require.NoError(t, err)
	defer func() { _ = st.Close() }()

	// Simulate a pre-SA-018 row: tags column never written (stays '').
	now := time.Now().UTC()
	require.NoError(t, st.CreateCredential(ctx, &state.Credential{
		ID: "c-legacy", Name: "legacy", Type: "ssh_key",
		EncryptedData: []byte("not-real-ciphertext-but-tags-parse-does-not-touch-it"),
		CreatedAt:     now,
	}))

	got, err := st.GetCredential(ctx, "c-legacy")
	require.NoError(t, err)
	tags, err := ParseCredentialTags(got)
	require.NoError(t, err, "legacy empty column must parse cleanly")
	assert.Nil(t, tags)

	assert.Nil(t, mustParse(t, &state.Credential{Tags: ""}))
	assert.Nil(t, mustParse(t, nil))
	_, err = ParseCredentialTags(&state.Credential{Name: "broken", Tags: "{not json"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "broken")
}

func mustParse(t *testing.T, c *state.Credential) map[string]string {
	t.Helper()
	m, err := ParseCredentialTags(c)
	require.NoError(t, err)
	return m
}
