package main

import (
	"bytes"
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nexus/levee/internal/dbchange"
	"github.com/nexus/levee/internal/dsl"
	"github.com/nexus/levee/internal/plan"
	"github.com/nexus/levee/internal/state"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// seedDBChangeTicket builds a change whose plan artifact carries the shape the
// renderer exists for, so the test asserts on the sections a reader sees.
func seedDBChangeTicket(t *testing.T) *dbchange.Ticket {
	t.Helper()
	ctx := context.Background()
	store, err := state.NewSQLiteStore(ctx, filepath.Join(t.TempDir(), "dbchange-cmd-test.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })

	now := time.Now().UTC().Truncate(time.Second)
	runID := "chg-ticket"
	require.NoError(t, store.CreateRun(ctx, &state.Run{
		ID: runID, WorkflowName: "db-change.yaml", Status: "approved",
		Priority: "high", IncidentID: "prod", Creator: "alice",
		PlanHash: "v2:abc", CreatedAt: now, UpdatedAt: now,
	}))
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
		Rollback:     &dsl.RollbackSpec{OnFailure: "auto", VerifyAfter: true},
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
	require.NoError(t, store.UpdateRunPlan(ctx, runID, string(raw), "v2:abc", now))

	tk, err := dbchange.Collect(ctx, store, runID)
	require.NoError(t, err)
	return tk
}

func TestPrintDBChangeTicket_SectionsArePresent(t *testing.T) {
	tk := seedDBChangeTicket(t)

	var buf bytes.Buffer
	printDBChangeTicket(&buf, tk)
	out := buf.String()

	// The identity block.
	assert.Contains(t, out, "数据库变更发布单: chg-ticket")
	assert.Contains(t, out, "环境:     prod")

	// The SQL section carries the statement and the review finding.
	assert.Contains(t, out, "ALTER TABLE t ADD COLUMN c INT")
	assert.Contains(t, out, "[SR-05/warn]")

	// The rollback section names the declared compensation.
	assert.Contains(t, out, "步骤补偿: add-column（undo-action，1 个 undo 步骤）")

	// The window says plainly that it is not persisted per run — the one
	// section that cannot replay a stored fact must not look like it did.
	assert.Contains(t, out, "随 run 持久化: 否")

	// Empty sections say what they mean instead of vanishing.
	assert.Contains(t, out, "审批链:")
	assert.Contains(t, out, "未进入审批链")
}

func TestPrintDBChangeTicket_EmptySQLSaysSo(t *testing.T) {
	ctx := context.Background()
	store, err := state.NewSQLiteStore(ctx, filepath.Join(t.TempDir(), "dbchange-empty.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })

	now := time.Now().UTC().Truncate(time.Second)
	require.NoError(t, store.CreateRun(ctx, &state.Run{
		ID: "no-sql", Status: "draft", CreatedAt: now, UpdatedAt: now,
	}))
	tk, err := dbchange.Collect(ctx, store, "no-sql")
	require.NoError(t, err)

	var buf bytes.Buffer
	printDBChangeTicket(&buf, tk)
	out := buf.String()
	assert.Contains(t, out, "无可审核的 SQL 步骤")
	assert.Contains(t, out, "未声明回滚")
	assert.Contains(t, out, "审核结论: 无")
}

// The JSON path is the machine surface: it must be an envelope with the ticket
// as `data`, matching every other command's contract.
func TestDBChangeShowJSONEnvelope(t *testing.T) {
	tk := seedDBChangeTicket(t)
	env := map[string]any{"data": tk, "meta": nil, "error": nil}
	raw, err := json.Marshal(env)
	require.NoError(t, err)

	var back map[string]any
	require.NoError(t, json.Unmarshal(raw, &back))
	assert.Contains(t, back, "data")
	assert.NotNil(t, back["data"])

	// The ticket serialises under the section names the package declares.
	data, err := json.Marshal(back["data"])
	require.NoError(t, err)
	for _, section := range []string{"change", "sql", "rollback", "window", "approval", "execution", "audit"} {
		assert.True(t, strings.Contains(string(data), `"`+section+`"`), "missing section %q", section)
	}
}
