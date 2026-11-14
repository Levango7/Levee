package grpc

// ticket_convert.go maps the dbchange read model onto its proto shape.
//
// The conversion is deliberately total and lossless in one direction: every
// field the ticket carries has a home in the message, and no field is derived
// here. A converter that computed anything would make the API's answer depend
// on when it was asked rather than on what was persisted.

import (
	"time"

	"github.com/nexus/levee/internal/dbchange"
	"github.com/nexus/levee/internal/grpc/pb"
)

// DBChangeTicketToPB converts a composed ticket. A nil ticket yields nil, so
// "not requested" and "could not compose" stay distinguishable from an empty
// ticket by the caller.
func DBChangeTicketToPB(t *dbchange.Ticket) *pb.DBChangeTicket {
	if t == nil {
		return nil
	}
	return &pb.DBChangeTicket{
		Change:    ticketChangeToPB(t.Change),
		Sql:       ticketSQLToPB(t.SQL),
		Rollback:  ticketRollbackToPB(t.Rollback),
		Window:    ticketWindowToPB(t.Window),
		Approval:  ticketApprovalToPB(t.Approval),
		Execution: ticketExecutionToPB(t.Execution),
		Audit:     ticketAuditToPB(t.Audit),
	}
}

func ticketChangeToPB(c dbchange.ChangeSection) *pb.TicketChange {
	return &pb.TicketChange{
		Id:          c.ID,
		Label:       c.Label,
		Status:      c.Status,
		Priority:    c.Priority,
		Environment: c.Environment,
		Creator:     c.Creator,
		Workflow:    c.Workflow,
		Template:    c.Template,
		PlanHash:    c.PlanHash,
		CreatedAt:   unixSeconds(c.CreatedAt),
		UpdatedAt:   unixSeconds(c.UpdatedAt),
	}
}

func ticketSQLToPB(s dbchange.SQLSection) *pb.TicketSQL {
	out := &pb.TicketSQL{
		HasSql: s.HasSQL,
	}
	for _, step := range s.Steps {
		out.Steps = append(out.Steps, &pb.TicketSQLStep{Step: step.Step, Sql: step.SQL})
	}
	if r := s.Review; r != nil {
		rr := &pb.TicketSQLReview{
			Verdict:   r.Verdict,
			FailCount: int32(r.FailCount),
			WarnCount: int32(r.WarnCount),
		}
		for _, f := range r.Findings {
			rr.Findings = append(rr.Findings, &pb.TicketFinding{
				Step: f.Step, Rule: f.Rule, Severity: f.Severity,
				Statement: int32(f.Statement), Message: f.Message,
			})
		}
		out.Review = rr
	}
	return out
}

func ticketRollbackToPB(r dbchange.RollbackSection) *pb.TicketRollback {
	return &pb.TicketRollback{
		Strategy:      r.Strategy,
		OnFailure:     r.OnFailure,
		VerifyAfter:   r.VerifyAfter,
		SnapshotPaths: r.SnapshotPaths,
		StepUndos:     ticketStepUndosToPB(r.StepUndos),
		HasUndo:       r.HasUndo,
	}
}

func ticketStepUndosToPB(in []dbchange.StepUndo) []*pb.TicketStepUndo {
	if len(in) == 0 {
		return nil
	}
	out := make([]*pb.TicketStepUndo, 0, len(in))
	for _, u := range in {
		out = append(out, &pb.TicketStepUndo{
			Step: u.Step, Strategy: u.Strategy,
			UndoSteps: int32(u.UndoSteps), Irreversible: u.Irreversible,
		})
	}
	return out
}

func ticketWindowToPB(w dbchange.WindowSection) *pb.TicketWindow {
	return &pb.TicketWindow{
		EnforcedAt:  w.EnforcedAt,
		Persistence: w.Persistence,
		Note:        w.Note,
	}
}

func ticketApprovalToPB(a dbchange.ApprovalSection) *pb.TicketApproval {
	out := &pb.TicketApproval{
		Level:         a.Level,
		Status:        a.Status,
		Approvers:     a.Approvers,
		PendingCount:  int32(a.PendingCount),
		ApprovedCount: int32(a.ApprovedCount),
		RejectedCount: int32(a.RejectedCount),
		MatchesPlan:   a.MatchesPlan,
		HasApproval:   a.HasApproval,
	}
	for _, d := range a.Decisions {
		out.Decisions = append(out.Decisions, &pb.TicketDecision{
			Approver: d.Approver, Action: d.Action, Reason: d.Reason,
			At: unixSeconds(d.At),
		})
	}
	return out
}

func ticketExecutionToPB(e dbchange.ExecutionSection) *pb.TicketExecution {
	return &pb.TicketExecution{
		Verdict:      e.Verdict,
		Reason:       e.Reason,
		Batches:      int32(e.Batches),
		Steps:        int32(e.Steps),
		TotalTargets: int32(e.TotalTargets),
		HasEvidence:  e.HasEvidence,
	}
}

func ticketAuditToPB(a dbchange.AuditSection) *pb.TicketAudit {
	out := &pb.TicketAudit{
		Valid:        a.Valid,
		Count:        int32(a.Count),
		LegacyCount:  int32(a.LegacyCount),
		Verified:     a.Verified,
		Unverifiable: a.Unverifiable,
	}
	for _, f := range a.Failures {
		out.Failures = append(out.Failures, &pb.TicketChainFailure{
			EntryId:     f.EntryID,
			Index:       int32(f.Index),
			FailureType: f.Type,
			Expected:    f.Expected,
			Actual:      f.Actual,
		})
	}
	return out
}

// unixSeconds renders a timestamp the way the rest of the API does: whole
// seconds since the epoch. A zero time renders as 0 (absent), matching the
// other messages' convention.
func unixSeconds(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.Unix()
}
