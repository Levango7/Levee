package grpc

// change_service_getchange_ticket_test.go pins the include_ticket behaviour,
// which follows the include_plan precedent exactly: pb.Change gained a Ticket
// field, filled only when the caller asks for it.
//
// Properties worth pinning:
//   - include_ticket=true returns the composed read model, including the SQL
//     steps and the review verdict the plan artifact carries.
//   - include_ticket=false must NOT leak the ticket. The wire default is
//     absent; making the opt-in the ONLY way the ticket appears is what keeps
//     every existing caller's payload and the wire snapshot stable.
//   - a run with NO stored plan still composes a ticket: the change's identity,
//     lifecycle status, approval state and evidence verdict are real facts even
//     before a plan exists. Nil here would mean "could not compose", which is a
//     different answer from "nothing to show in these sections".
//   - the composed ticket reports an honest evidence verdict rather than a
//     fabricated one (a planned-but-never-applied change is not_started).

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nexus/levee/internal/dsl"
	"github.com/nexus/levee/internal/evidence"
	"github.com/nexus/levee/internal/grpc/pb"
	"github.com/nexus/levee/internal/plan"
	"github.com/nexus/levee/internal/state"
)

func TestGetChange_IncludeTicket(t *testing.T) {
	ctx := context.Background()

	seed := func(t *testing.T) (*ChangeService, state.Store, string) {
		t.Helper()
		svc, store := newTestChangeService(t)
		id := "run-ticket-test"
		require.NoError(t, store.CreateRun(ctx, &state.Run{
			ID: id, WorkflowName: "db-change.yaml", Status: "approved",
			Priority: "high", IncidentID: "prod", Creator: "alice",
		}))
		return svc, store, id
	}

	persistDBPlan := func(t *testing.T, store state.Store, runID string) {
		t.Helper()
		p := &plan.Plan{
			ID: runID,
			Batches: []plan.Batch{{
				Index:   0,
				Targets: []string{"db-1"},
				Steps: []plan.PlanStep{{
					Name: "add-column", Module: "mysql", Action: "query",
					Args: map[string]any{"sql": "ALTER TABLE t ADD COLUMN c INT"},
					Rollback: &dsl.RollbackSpec{
						Strategy: "undo-action",
						Steps:    []dsl.Step{{Name: "drop-column", Module: "mysql", Action: "query"}},
					},
				}},
			}},
			TotalTargets: 1,
			SQLReview: &plan.SQLReviewSummary{
				Verdict: "warn", WarnCount: 1,
				Findings: []plan.SQLReviewFinding{{
					Step: "add-column", Rule: "SR-05", Severity: "warn", Statement: 1,
					Message: "ADD COLUMN without a default rewrites the table",
				}},
			},
		}
		raw, err := json.Marshal(p)
		require.NoError(t, err)
		run, err := store.GetRun(ctx, runID)
		require.NoError(t, err)
		run.PlanJSON = string(raw)
		run.PlanHash = plan.ComputeHash(p)
		require.NoError(t, store.UpdateRun(ctx, run))
	}

	t.Run("include_ticket=true returns the composed ticket", func(t *testing.T) {
		svc, store, id := seed(t)
		persistDBPlan(t, store, id)
		resp, err := svc.GetChange(ctx, &pb.GetChangeRequest{Id: id, IncludeTicket: true})
		require.NoError(t, err)
		tk := resp.GetTicket()
		require.NotNil(t, tk, "include_ticket=true must carry the ticket")

		assert.Equal(t, id, tk.GetChange().GetId())
		assert.Equal(t, "approved", tk.GetChange().GetStatus())
		assert.Equal(t, "prod", tk.GetChange().GetEnvironment())

		require.True(t, tk.GetSql().GetHasSql())
		require.Len(t, tk.GetSql().GetSteps(), 1)
		assert.Equal(t, "ALTER TABLE t ADD COLUMN c INT", tk.GetSql().GetSteps()[0].GetSql())

		require.NotNil(t, tk.GetSql().GetReview())
		assert.Equal(t, "warn", tk.GetSql().GetReview().GetVerdict())
		require.Len(t, tk.GetSql().GetReview().GetFindings(), 1)
		assert.Equal(t, "SR-05", tk.GetSql().GetReview().GetFindings()[0].GetRule())

		// The rollback section carries the declared compensation.
		assert.True(t, tk.GetRollback().GetHasUndo())
		require.Len(t, tk.GetRollback().GetStepUndos(), 1)
		assert.Equal(t, "add-column", tk.GetRollback().GetStepUndos()[0].GetStep())

		// Window honesty: it says plainly where the declaration lives. A
		// string rather than a bool, so the answer survives protojson (which
		// omits a false boolean).
		assert.Equal(t, "workflow-declaration", tk.GetWindow().GetPersistence())
		assert.NotEmpty(t, tk.GetWindow().GetNote())

		// No batches yet: the evidence verdict must be honest about it.
		assert.Equal(t, string(evidence.NotStarted), tk.GetExecution().GetVerdict())
		assert.False(t, tk.GetExecution().GetHasEvidence())
	})

	t.Run("include_ticket=false does not leak the ticket", func(t *testing.T) {
		svc, store, id := seed(t)
		persistDBPlan(t, store, id)
		resp, err := svc.GetChange(ctx, &pb.GetChangeRequest{Id: id})
		require.NoError(t, err)
		assert.Nil(t, resp.GetTicket(),
			"the ticket must only appear when include_ticket is explicitly true")
	})

	t.Run("include_ticket=true without a plan still composes", func(t *testing.T) {
		svc, _, id := seed(t)
		resp, err := svc.GetChange(ctx, &pb.GetChangeRequest{Id: id, IncludeTicket: true})
		require.NoError(t, err)
		tk := resp.GetTicket()
		require.NotNil(t, tk, "a draft with no plan still has identity/status/evidence facts")
		assert.Equal(t, id, tk.GetChange().GetId())
		assert.False(t, tk.GetSql().GetHasSql(), "no plan means no SQL surface, said plainly")
		assert.False(t, tk.GetRollback().GetHasUndo())
	})
}
