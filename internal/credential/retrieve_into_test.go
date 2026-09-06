package credential

// SA-011: RetrieveInto must hand the plaintext to fn and zero the buffer on
// every exit path — normal return, error return, and panic.

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRetrieveInto_ZeroesAfterCallbackAndPropagates(t *testing.T) {
	st := newTestStore(t)
	cs := newTestCredentialStore(t, st, "master-pw")
	ctx := context.Background()

	_, err := cs.Store(ctx, CredentialSpec{
		Name: "into-1", Type: "api_token", Plaintext: []byte("hunter7-secret"),
	})
	require.NoError(t, err)

	var seen []byte
	require.NoError(t, cs.RetrieveInto(ctx, "into-1", func(secret []byte) error {
		seen = secret // same backing array; readable inside the callback
		assert.Equal(t, "hunter7-secret", string(secret))
		return nil
	}))
	assert.True(t, allZero(seen), "buffer must be zeroed once fn returns")
}

func TestRetrieveInto_ErrorReturnStillZeroes(t *testing.T) {
	st := newTestStore(t)
	cs := newTestCredentialStore(t, st, "master-pw")
	ctx := context.Background()
	_, err := cs.Store(ctx, CredentialSpec{Name: "into-2", Type: "api_token", Plaintext: []byte("pw")})
	require.NoError(t, err)

	sentinel := errors.New("callback failed")
	var seen []byte
	err = cs.RetrieveInto(ctx, "into-2", func(secret []byte) error {
		seen = secret
		return sentinel
	})
	require.ErrorIs(t, err, sentinel)
	assert.True(t, allZero(seen))
}

func TestRetrieveInto_PanicPathStillZeroes(t *testing.T) {
	st := newTestStore(t)
	cs := newTestCredentialStore(t, st, "master-pw")
	ctx := context.Background()
	_, err := cs.Store(ctx, CredentialSpec{Name: "into-3", Type: "api_token", Plaintext: []byte("pw")})
	require.NoError(t, err)

	var seen []byte
	assert.Panics(t, func() {
		_ = cs.RetrieveInto(ctx, "into-3", func(secret []byte) error {
			seen = secret
			panic("boom")
		})
	})
	assert.True(t, allZero(seen), "deferred zeroing must run while the panic unwinds")
}

func TestRetrieveInto_MissingCredentialDoesNotCallFn(t *testing.T) {
	st := newTestStore(t)
	cs := newTestCredentialStore(t, st, "master-pw")

	called := false
	err := cs.RetrieveInto(context.Background(), "ghost", func(secret []byte) error {
		called = true
		return nil
	})
	require.ErrorIs(t, err, ErrNotFound)
	assert.False(t, called)
}
