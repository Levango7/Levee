package state

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCountRunsMatchesFiltersAndIgnoresPagination(t *testing.T) {
	for _, dialect := range []string{"sqlite", "postgres"} {
		t.Run(dialect, func(t *testing.T) {
			var store interface {
				Store
				RunCounter
			}
			switch dialect {
			case "sqlite":
				store = newTestStore(t)
			case "postgres":
				pg, cleanup := newPGTestStore(t)
				defer cleanup()
				store = pg
			}
			ctx := context.Background()
			now := time.Now().UTC()
			for i := 0; i < 5; i++ {
				status := "draft"
				if i >= 3 {
					status = "completed"
				}
				require.NoError(t, store.CreateRun(ctx, &Run{
					ID: fmt.Sprintf("count-%d", i), WorkflowName: "wf", TemplateName: "tpl",
					Status: status, Creator: "alice", IncidentID: "prod",
					CreatedAt: now.Add(time.Duration(i) * time.Second), UpdatedAt: now,
				}))
			}
			for _, tc := range []struct {
				name   string
				filter RunFilter
				want   int
			}{
				{name: "all ignores limit and offset", filter: RunFilter{Limit: 1, Offset: 3}, want: 5},
				{name: "status", filter: RunFilter{Status: "draft"}, want: 3},
				{name: "combined", filter: RunFilter{Status: "completed", Creator: "alice", IncidentID: "prod"}, want: 2},
				{name: "no match", filter: RunFilter{Status: "missing"}, want: 0},
			} {
				t.Run(tc.name, func(t *testing.T) {
					got, err := store.CountRuns(ctx, tc.filter)
					require.NoError(t, err)
					assert.Equal(t, tc.want, got)
				})
			}
		})
	}
}
