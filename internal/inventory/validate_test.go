package inventory

import (
	"context"
	"errors"
	"testing"

	"github.com/nexus/levee/internal/state"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type stubValidateStore struct {
	frozen map[string]bool
}

func (s *stubValidateStore) ListTargets(_ context.Context, f state.TargetFilter) ([]*state.Target, error) {
	if f.Status != state.StatusFrozen {
		return nil, nil
	}
	var out []*state.Target
	for h := range s.frozen {
		out = append(out, &state.Target{Hostname: h})
	}
	return out, nil
}

func TestValidateNotFrozen(t *testing.T) {
	st := &stubValidateStore{frozen: map[string]bool{"db-01": true}}
	ctx := context.Background()

	require.NoError(t, ValidateNotFrozen(ctx, st, []string{"web-01", "unknown-host"}),
		"active and unknown hosts pass")

	err := ValidateNotFrozen(ctx, st, []string{"web-01", "db-01"})
	require.Error(t, err)
	var fe *FrozenError
	require.True(t, errors.As(err, &fe))
	assert.Equal(t, []string{"db-01"}, fe.Hosts)

	require.NoError(t, ValidateNotFrozen(ctx, st, nil))
}
