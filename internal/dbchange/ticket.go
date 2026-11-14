// Package dbchange composes the reader-facing 数据库变更发布单 (DB change
// ticket): one change's SQL batch, its review verdict, its rollback plan, the
// window it was admitted under, its approval chain, its execution evidence and
// its audit-chain integrity — the seven things a reviewer, an auditor or a
// customer asks for when they point at a change id.
//
// It is a READ MODEL, not a new source of truth. Every field is derived from
// artifacts the engine already persists: the run row (`state.Run`), the plan
// artifact (`run.PlanJSON`, which already carries SQLReview, Rollback,
// Approval, RunSnapshot and the batches), the approval rows, the trace/audit
// chains and the step evidence. Nothing here is written back, and no field is
// re-derived from a live probe that the plan-time gate did not already make —
// a ticket that recomputes risk or re-scores SQL would be a second opinion
// about an approved artifact, which is exactly the drift the plan_hash exists
// to prevent.
//
// What it adds is the composition: today those facts are spread across
// `levee show`, `levee audit report`, `levee approvals`-style reads and the
// console's change detail, so "show me the ticket for this change" means
// opening four surfaces and mentally joining them. The ticket is that join,
// named and testable.
package dbchange

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/nexus/levee/internal/approval"
	"github.com/nexus/levee/internal/audit"
	"github.com/nexus/levee/internal/dsl"
	"github.com/nexus/levee/internal/evidence"
	"github.com/nexus/levee/internal/plan"
	"github.com/nexus/levee/internal/state"
)

// ErrNotFound reports that no run exists with the requested id. It is a
// sentinel (not a bare nil) so a caller can distinguish "no such change" from
// "the change exists but has no plan yet" — the two send the reader to
// different fixes.
var ErrNotFound = errors.New("dbchange: change not found")

// Plan artifact states. They are three facts, not two: "the change declares
// no SQL" and "nobody could read the plan" must not render the same way, or
// a ticket tells a reader the change has no SQL when the truth is that the
// plan is unknown.
const (
	// planStateNone means no plan artifact is persisted on the run yet.
	planStateNone = "none"
	// planStateReadable means the persisted plan artifact parsed.
	planStateReadable = "readable"
	// planStateUnreadable means an artifact exists but could not be parsed.
	planStateUnreadable = "unreadable"
)

// Ticket is one change, composed for a reader.
type Ticket struct {
	Change    ChangeSection    `json:"change"`
	SQL       SQLSection       `json:"sql"`
	Rollback  RollbackSection  `json:"rollback"`
	Window    WindowSection    `json:"window"`
	Approval  ApprovalSection  `json:"approval"`
	Execution ExecutionSection `json:"execution"`
	Audit     AuditSection     `json:"audit"`
}

// ChangeSection is the change's identity and lifecycle state.
type ChangeSection struct {
	ID          string `json:"id"`
	Label       string `json:"label,omitempty"`
	Status      string `json:"status"`
	Priority    string `json:"priority,omitempty"`
	Environment string `json:"environment,omitempty"`
	Creator     string `json:"creator,omitempty"`
	Workflow    string `json:"workflow,omitempty"`
	Template    string `json:"template,omitempty"`
	PlanHash    string `json:"plan_hash,omitempty"`
	// PlanState says what the run's plan artifact IS: "none" (no plan
	// persisted yet), "readable" (parsed) or "unreadable" (an artifact exists
	// but does not parse). It exists because the SQL and rollback sections
	// can only be empty in two very different ways, and a reader must be able
	// to tell them apart: "this change declares no SQL" is a fact about the
	// change, "this change's plan could not be read" is a fact about the
	// store, and the second must never be rendered as the first.
	PlanState string    `json:"plan_state"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// SQLSection is the reviewed SQL surface: the statements the plan will run,
// plus the review verdict that was stamped onto the artifact at plan time.
//
// Steps is empty and HasSQL is false when the change carries no `mysql.query`
// step — which is a legitimate state for a non-database change, and the
// caller can branch on HasSQL rather than on an error.
type SQLSection struct {
	Steps  []SQLStep              `json:"steps"`
	Review *plan.SQLReviewSummary `json:"review,omitempty"`
	HasSQL bool                   `json:"has_sql"`
}

// SQLStep is one reviewable statement: the step that carries it and the SQL.
type SQLStep struct {
	Step string `json:"step"`
	SQL  string `json:"sql"`
}

// RollbackSection is the rollback plan the change was approved under: the
// run-level policy (strategy, on_failure, verify_after, snapshot paths) and
// the per-step undo declarations that actually carry the compensation.
//
// HasUndo is false when nothing declared a compensation — an honest "no
// rollback plan", not an empty one that reads like one.
type RollbackSection struct {
	Strategy      string     `json:"strategy,omitempty"`
	OnFailure     string     `json:"on_failure,omitempty"`
	VerifyAfter   bool       `json:"verify_after"`
	SnapshotPaths []string   `json:"snapshot_paths,omitempty"`
	StepUndos     []StepUndo `json:"step_undos,omitempty"`
	HasUndo       bool       `json:"has_undo"`
}

// StepUndo is one step's declared compensation.
type StepUndo struct {
	Step         string `json:"step"`
	Strategy     string `json:"strategy,omitempty"`
	UndoSteps    int    `json:"undo_steps"`
	Irreversible bool   `json:"irreversible"`
}

// WindowSection records where the change window is enforced.
//
// The declared window is a WORKFLOW fact, re-checked when the plan is
// generated; neither `state.Run` nor the plan artifact carries it. A ticket
// therefore cannot replay "which window was this change admitted under" from
// the store alone, and says so rather than inventing a value. PersistedPerRun
// stays false until the run row grows a window column — see the note.
type WindowSection struct {
	// EnforcedAt names the funnel stage that checks the window.
	EnforcedAt string `json:"enforced_at"`
	// PersistedPerRun is false: the run row has no window column.
	PersistedPerRun bool `json:"persisted_per_run"`
	// Note states, in one line, what that means for a reader.
	Note string `json:"note"`
}

// ApprovalSection is the approval chain state at read time.
type ApprovalSection struct {
	Level     string             `json:"level,omitempty"`
	Status    string             `json:"status,omitempty"`
	Approvers []string           `json:"approvers,omitempty"`
	Decisions []ApprovalDecision `json:"decisions,omitempty"`
	// PendingCount counts approval rows still pending; a change can carry
	// more than one level (standard → high escalation).
	PendingCount int `json:"pending_count"`
	// ApprovedCount counts rows whose outcome was approval.
	ApprovedCount int `json:"approved_count"`
	// RejectedCount counts rows whose outcome was rejection.
	RejectedCount int `json:"rejected_count"`
	// MatchesPlan reports whether an approval row is bound to the run's
	// current plan_hash — a stale approval is one that blessed a plan the
	// change no longer carries.
	//
	// It is deliberately NOT true for a legacy row (one carrying no plan hash
	// at all). state.Approval.MatchesPlan reports legacy rows as matching any
	// plan, because that is the compatibility rule the engine needs; but a
	// legacy row blesses no particular plan, and reporting it as "bound to
	// the current plan" would claim a binding it does not have.
	MatchesPlan bool `json:"matches_plan"`
	// LegacyApproval is true when at least one approval row carries no plan
	// hash at all — such a row blessed no particular plan.
	LegacyApproval bool `json:"legacy_approval"`
	// StaleApproval is true when at least one approval row is bound to a
	// plan hash this run no longer carries.
	StaleApproval bool `json:"stale_approval"`
	// HasApproval is false when the change never entered the chain.
	HasApproval bool `json:"has_approval"`
}

// ApprovalDecision is one approver's recorded decision.
type ApprovalDecision struct {
	Approver string    `json:"approver"`
	Action   string    `json:"action"`
	Reason   string    `json:"reason,omitempty"`
	At       time.Time `json:"at"`
}

// ExecutionSection summarises what the run actually did, using the same
// verdict the console shows (`evidence.Classify`), so the ticket cannot
// disagree with the execution surface about whether a run is complete.
type ExecutionSection struct {
	// Verdict is the evidence Kind (not_started / in_flight / complete /
	// partial / missing / unrecorded) — the same vocabulary the console and
	// `levee show` render, so the ticket cannot disagree with them.
	Verdict string `json:"verdict"`
	// Reason is the operator sentence evidence.Classify produced for it.
	Reason       string `json:"reason,omitempty"`
	Batches      int    `json:"batches"`
	Steps        int    `json:"steps"`
	TotalTargets int    `json:"total_targets"`
	// HasEvidence is false when no batch rows exist yet (a planned-but-never-
	// applied change) — distinct from a run that executed and left no trace.
	HasEvidence bool `json:"has_evidence"`
}

// AuditSection is the integrity answer for this change's trace chain.
type AuditSection struct {
	Valid       bool `json:"valid"`
	Count       int  `json:"count"`
	LegacyCount int  `json:"legacy_count"`
	// Verified reports whether a verification was attempted. A change with
	// no trace rows yet has nothing to verify, and Valid stays false with
	// Unverifiable set — an honest "no evidence" rather than a silent pass.
	Verified     bool `json:"verified"`
	Unverifiable bool `json:"unverifiable"`
	// VerifyError carries the reason a verification could not be performed
	// (a store failure, a broken chain reader). It is empty when there was
	// simply nothing to verify — a change with no trace rows is a state, not
	// an error — so a reader is never told "no trace records" when the truth
	// is "the audit store could not be read".
	VerifyError string `json:"verify_error,omitempty"`
}

// Collect composes the ticket for one change id.
//
// Errors: ErrNotFound when the id names no run; a wrapped error when a
// dependency read fails. An unparsable plan artifact does NOT fail the call —
// it degrades the SQL/Rollback sections and is reported through
// ChangeSection.PlanHash staying set while the sections stay empty (a reader
// can see a plan was blessed without being able to read it). A trace chain
// that cannot be verified (no rows yet) is recorded as Unverifiable, not
// returned as an error.
func Collect(ctx context.Context, store state.Store, runID string) (*Ticket, error) {
	if store == nil {
		return nil, errors.New("dbchange: nil store")
	}
	run, err := store.GetRun(ctx, runID)
	if err != nil {
		return nil, fmt.Errorf("dbchange: get run %q: %w", runID, err)
	}
	if run == nil {
		return nil, fmt.Errorf("%w: %q", ErrNotFound, runID)
	}

	t := &Ticket{
		Change: ChangeSection{
			ID: run.ID,
			// The label is the workflow's display name — the same derivation
			// runToPB uses, so the ticket and the API cannot disagree about
			// what this change is called.
			Label:       dsl.DisplayName(run.WorkflowName),
			Status:      run.Status,
			Priority:    run.Priority,
			Environment: run.IncidentID,
			Creator:     run.Creator,
			Workflow:    run.WorkflowName,
			Template:    run.TemplateName,
			PlanHash:    run.PlanHash,
			CreatedAt:   run.CreatedAt,
			UpdatedAt:   run.UpdatedAt,
		},
		Window: WindowSection{
			EnforcedAt:      "plan",
			PersistedPerRun: false,
			Note:            "变更窗口是工作流声明、plan 生成时校验，run 行不持久化该事实——本单无法从库里回放「这次变更是按哪个窗口放行的」。",
		},
	}

	t.Change.PlanState = planStateNone
	if run.PlanJSON != "" {
		var p plan.Plan
		if err := json.Unmarshal([]byte(run.PlanJSON), &p); err == nil {
			t.Change.PlanState = planStateReadable
			fillFromPlan(t, &p)
		} else {
			// The artifact exists but cannot be read. The sections stay
			// empty, and PlanState says why — so the reader is told "unknown",
			// not "this change declares no SQL".
			t.Change.PlanState = planStateUnreadable
		}
	}

	if err := fillApproval(ctx, store, t, run); err != nil {
		return nil, err
	}
	if err := fillExecution(ctx, store, t, run); err != nil {
		return nil, err
	}
	fillAudit(ctx, store, t, run)
	return t, nil
}

// fillFromPlan copies the plan-artifact facts into the ticket. It is called
// only when the artifact parsed; a plan that cannot be read leaves the
// sections empty, which the reader sees as "no SQL / no rollback declared"
// rather than as an error the ticket refuses to render.
func fillFromPlan(t *Ticket, p *plan.Plan) {
	for _, b := range p.Batches {
		for _, s := range b.Steps {
			if s.Module == "mysql" && s.Action == "query" {
				if sql, ok := s.Args["sql"].(string); ok && strings.TrimSpace(sql) != "" {
					t.SQL.Steps = append(t.SQL.Steps, SQLStep{Step: s.Name, SQL: sql})
				}
			}
			if s.Rollback != nil {
				u := StepUndo{
					Step:         s.Name,
					Strategy:     s.Rollback.Strategy,
					UndoSteps:    len(s.Rollback.Steps),
					Irreversible: s.Irreversible,
				}
				t.Rollback.StepUndos = append(t.Rollback.StepUndos, u)
				t.Rollback.HasUndo = true
			}
		}
	}
	t.SQL.HasSQL = len(t.SQL.Steps) > 0
	t.SQL.Review = p.SQLReview

	if p.Rollback != nil {
		t.Rollback.Strategy = p.Rollback.Strategy
		t.Rollback.OnFailure = p.Rollback.OnFailure
		t.Rollback.VerifyAfter = p.Rollback.VerifyAfter
		t.Rollback.SnapshotPaths = p.Rollback.SnapshotPaths
		if p.Rollback.Strategy != "" {
			t.Rollback.HasUndo = true
		}
	}
}

// fillApproval reads the approval chain. A change that never entered the
// chain is a valid state (draft, or auto-approved low risk) and yields
// HasApproval=false rather than an error.
func fillApproval(ctx context.Context, store state.Store, t *Ticket, run *state.Run) error {
	rows, err := store.ListApprovals(ctx, state.ApprovalFilter{RunID: run.ID})
	if err != nil {
		return fmt.Errorf("dbchange: list approvals for %q: %w", run.ID, err)
	}
	if len(rows) == 0 {
		return nil
	}
	t.Approval.HasApproval = true
	for i := range rows {
		sa := rows[i]
		// The row's own columns are read first and unconditionally: a metadata
		// blob this package cannot decode is a defect, but the level, the
		// status and the plan binding on the row are still facts a reader
		// should see. Only the richer chain detail (approvers, decisions)
		// needs the decoded payload.
		if sa.Level != "" {
			t.Approval.Level = sa.Level
		}
		if sa.Status != "" {
			t.Approval.Status = sa.Status
		}
		switch sa.Status {
		case string(approval.StatusPending):
			t.Approval.PendingCount++
		case string(approval.StatusApproved):
			t.Approval.ApprovedCount++
		case string(approval.StatusRejected):
			t.Approval.RejectedCount++
		}
		matched, legacy := sa.MatchesPlan(run.PlanHash)
		switch {
		case legacy:
			// A row with no plan hash blesses no plan at all. It matches
			// every plan for compatibility (state.Approval.MatchesPlan), but
			// calling that "bound to the current plan" would be a claim the
			// row does not support.
			t.Approval.LegacyApproval = true
		case matched:
			t.Approval.MatchesPlan = true
		default:
			t.Approval.StaleApproval = true
		}
		a, err := approval.ApprovalFromState(sa)
		if err != nil {
			continue
		}
		t.Approval.Approvers = append(t.Approval.Approvers, a.Approvers...)
		for _, d := range a.Decisions {
			t.Approval.Decisions = append(t.Approval.Decisions, ApprovalDecision{
				Approver: d.Approver, Action: d.Action, Reason: d.Reason, At: d.At,
			})
		}
	}
	return nil
}

// fillExecution summarises the run's execution evidence.
func fillExecution(ctx context.Context, store state.Store, t *Ticket, run *state.Run) error {
	verdict, err := evidence.ForRun(ctx, store, run.ID, run.Status)
	if err != nil {
		return fmt.Errorf("dbchange: evidence for %q: %w", run.ID, err)
	}
	t.Execution.Verdict = string(verdict.Kind)
	t.Execution.Reason = verdict.Reason

	batches, err := store.ListBatches(ctx, state.BatchFilter{RunID: run.ID})
	if err != nil {
		return fmt.Errorf("dbchange: list batches for %q: %w", run.ID, err)
	}
	steps, err := store.ListSteps(ctx, state.StepFilter{RunID: run.ID})
	if err != nil {
		return fmt.Errorf("dbchange: list steps for %q: %w", run.ID, err)
	}
	t.Execution.Batches = len(batches)
	t.Execution.Steps = len(steps)
	for _, b := range batches {
		t.Execution.TotalTargets += b.TotalHosts
	}
	t.Execution.HasEvidence = len(batches) > 0 || len(steps) > 0
	return nil
}

// fillAudit verifies the change's trace chain. A change with no trace rows
// has nothing to verify: Verified stays false and Unverifiable is set, so a
// reader never reads an unverified ticket as a passed one.
func fillAudit(ctx context.Context, store state.Store, t *Ticket, run *state.Run) {
	verifier, err := audit.NewChainVerifier(store)
	if err != nil {
		t.Audit.Unverifiable = true
		return
	}
	res, err := verifier.Verify(ctx, run.ID)
	if err != nil {
		t.Audit.Unverifiable = true
		// "no trace rows yet" is a state, not a failure; anything else is a
		// reason the chain could not be checked, and the reader is told which.
		if !errors.Is(err, audit.ErrNoTraces) {
			t.Audit.VerifyError = err.Error()
		}
		return
	}
	t.Audit.Verified = true
	t.Audit.Valid = res.Valid
	t.Audit.Count = res.Count
	t.Audit.LegacyCount = res.LegacyCount
}
