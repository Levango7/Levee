// AuditService implementation for the LEVEE gRPC API.
//
// AuditService exposes the tamper-evident audit subsystem: querying audit
// log entries and trace records, verifying hash-chain integrity, and
// producing per-run execution reports. All data is read from state.Store;
// hash-chain verification delegates to audit.ChainVerifier.
package grpc

import (
	"context"
	"errors"
	"fmt"

	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/nexus/levee/internal/audit"
	"github.com/nexus/levee/internal/grpc/pb"
	"github.com/nexus/levee/internal/state"
)

// AuditService implements pb.AuditServiceServer. It reads audit data from
// state.Store and verifies hash chains via audit.ChainVerifier.
type AuditService struct {
	pb.UnimplementedAuditServiceServer

	store state.Store
}

// NewAuditService returns an AuditService backed by the given store. The
// store must be non-nil.
func NewAuditService(store state.Store) *AuditService {
	return &AuditService{store: store}
}

// maxAuditFetchRows is retained as an import-compatibility constant for
// callers that referenced the old single-fetch cap; GetAuditLog no longer
// uses it (see auditStorePageSize below).
const maxAuditFetchRows = 10000

// auditStorePageSize is the number of audit rows fetched per store round
// trip while walking results for one GetAuditLog response.
const auditStorePageSize = 1000

// verifyRunPageSize is the page size used when VerifyHashChain walks all
// runs in the store (offset stepping until a short page).
const verifyRunPageSize = 1000

// GetAuditLog returns audit log entries matching the given filters, with
// pagination. Entries are sourced from state.Store.ListAudits and converted
// to pb.TraceEntry messages.
//
// Pagination semantics: the page token encodes how many FILTERED rows have
// already been returned. Each request re-walks the store from the start in
// auditStorePageSize-row fetches, skipping the first `skip` matching rows
// and collecting the next pageSize of them, while counting the global
// filtered total along the way. This keeps TotalSize stable across pages
// and never loses or duplicates a row at filter boundaries; the O(store)
// walk per request is an accepted MVP trade-off (audit volumes are small,
// and the alternative — offset tokens into a filtered stream — cannot be
// expressed by the store's row-offset pagination).
func (s *AuditService) GetAuditLog(ctx context.Context, req *pb.GetAuditLogRequest) (*pb.GetAuditLogResponse, error) {
	if req == nil {
		req = &pb.GetAuditLogRequest{}
	}
	if s.store == nil {
		return nil, status.Error(codes.FailedPrecondition, "store not configured")
	}

	pageSize := int(req.PageSize)
	if pageSize <= 0 {
		pageSize = defaultPageSize
	}
	if pageSize > maxPageSize {
		pageSize = maxPageSize
	}
	skip, err := parsePageToken(req.PageToken)
	if err != nil {
		return nil, err
	}

	entries := make([]*pb.TraceEntry, 0, pageSize)
	total := 0 // global count of filtered rows

	storeOffset := 0
	for {
		audits, ferr := s.store.ListAudits(ctx, state.AuditFilter{
			RunID:  req.RunId,
			Action: req.Action,
			Actor:  req.Actor,
			Limit:  auditStorePageSize,
			Offset: storeOffset,
		})
		if ferr != nil {
			return nil, status.Errorf(codes.Internal, "list audits: %v", ferr)
		}

		for _, a := range audits {
			if req.ChangeId != "" && a.RunID != req.ChangeId {
				continue
			}
			ts := a.Timestamp.Unix()
			if req.Since > 0 && ts < req.Since {
				continue
			}
			if req.Until > 0 && ts > req.Until {
				continue
			}
			total++
			switch {
			case total <= skip:
				// Returned by an earlier page.
			case len(entries) < pageSize:
				entries = append(entries, auditToTraceEntry(a))
			}
		}

		if len(audits) < auditStorePageSize {
			break // store exhausted — global count is now exact
		}
		storeOffset += len(audits)
	}

	hasMore := total > skip+len(entries)
	nextToken := ""
	if hasMore {
		nextToken = fmt.Sprintf("%d", skip+len(entries))
	}

	return &pb.GetAuditLogResponse{
		Entries: entries,
		// TotalSize is the GLOBAL filtered count, stable across pages so
		// UI pagers can render a trustworthy total.
		TotalSize:     int32(total),
		NextPageToken: nextToken,
	}, nil
}

// ListAuditTraces returns trace records matching the given filters, with
// pagination. When RunIds is non-empty, traces for all matching runs are
// returned; otherwise ChangeId is used as a run-id filter.
func (s *AuditService) ListAuditTraces(ctx context.Context, req *pb.ListAuditTracesRequest) (*pb.ListAuditTracesResponse, error) {
	if req == nil {
		req = &pb.ListAuditTracesRequest{}
	}
	if s.store == nil {
		return nil, status.Error(codes.FailedPrecondition, "store not configured")
	}

	runIDs := req.RunIds
	if len(runIDs) == 0 && req.ChangeId != "" {
		runIDs = []string{req.ChangeId}
	}

	var allTraces []*state.Trace
	if len(runIDs) > 0 {
		for _, rid := range runIDs {
			traces, err := s.store.ListTraces(ctx, state.TraceFilter{RunID: rid, Limit: maxPageSize})
			if err != nil {
				return nil, status.Errorf(codes.Internal, "list traces for run %q: %v", rid, err)
			}
			allTraces = append(allTraces, traces...)
		}
	} else {
		traces, err := s.store.ListTraces(ctx, state.TraceFilter{Limit: maxPageSize})
		if err != nil {
			return nil, status.Errorf(codes.Internal, "list traces: %v", err)
		}
		allTraces = traces
	}

	// Convert and apply time-range filter.
	entries := make([]*pb.TraceEntry, 0, len(allTraces))
	for _, t := range allTraces {
		ts := t.Timestamp.Unix()
		if req.Since > 0 && ts < req.Since {
			continue
		}
		if req.Until > 0 && ts > req.Until {
			continue
		}
		entries = append(entries, traceToPB(t))
	}

	// Pagination.
	pageSize := int(req.PageSize)
	if pageSize <= 0 {
		pageSize = defaultPageSize
	}
	if pageSize > maxPageSize {
		pageSize = maxPageSize
	}
	offset, err := parsePageToken(req.PageToken)
	if err != nil {
		return nil, err
	}

	total := len(entries)
	if offset > total {
		offset = total
	}
	end := offset + pageSize
	if end > total {
		end = total
	}
	page := entries[offset:end]

	return &pb.ListAuditTracesResponse{
		Entries:       page,
		TotalSize:     int32(total),
		NextPageToken: buildPageToken(end, total),
	}, nil
}

// VerifyHashChain verifies the integrity of the hash chain for the given run
// (or all runs when RunId is empty). It delegates to audit.ChainVerifier.
func (s *AuditService) VerifyHashChain(ctx context.Context, req *pb.VerifyHashChainRequest) (*pb.VerifyHashChainResponse, error) {
	if req == nil {
		req = &pb.VerifyHashChainRequest{}
	}
	if s.store == nil {
		return nil, status.Error(codes.FailedPrecondition, "store not configured")
	}

	verifier, err := audit.NewChainVerifier(s.store)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "create verifier: %v", err)
	}

	// Determine the set of run IDs to verify.
	runIDs := []string{}
	if req.RunId != "" {
		runIDs = append(runIDs, req.RunId)
	} else if req.ChangeId != "" {
		runIDs = append(runIDs, req.ChangeId)
	} else {
		// Verify all runs, paging through the store with offset stepping
		// until a page comes back short (RunFilter supports Offset,
		// unlike AuditFilter). The previous single maxPageSize fetch
		// silently skipped runs beyond the first 1000 rows.
		for offset := 0; ; offset += verifyRunPageSize {
			runs, err := s.store.ListRuns(ctx, state.RunFilter{Limit: verifyRunPageSize, Offset: offset})
			if err != nil {
				return nil, status.Errorf(codes.Internal, "list runs: %v", err)
			}
			for _, r := range runs {
				runIDs = append(runIDs, r.ID)
			}
			if len(runs) < verifyRunPageSize {
				break
			}
		}
	}

	resp := &pb.VerifyHashChainResponse{
		Valid:           true,
		Runs:            make([]*pb.RunVerification, 0, len(runIDs)),
		EntriesVerified: 0,
	}

	for _, rid := range runIDs {
		result, err := verifier.Verify(ctx, rid)
		if err != nil {
			if errors.Is(err, audit.ErrNoTraces) || errors.Is(err, audit.ErrEmptyRunID) {
				// Skip runs with no traces.
				continue
			}
			return nil, status.Errorf(codes.Internal, "verify run %q: %v", rid, err)
		}

		rv := &pb.RunVerification{
			RunId:           rid,
			Valid:           result.Valid,
			EntriesVerified: int64(result.Count),
		}
		if !result.Valid && len(result.Failures) > 0 {
			f := result.Failures[0]
			rv.BrokenEntryId = f.TraceID
			rv.BrokenReason = f.Type.String()
			resp.Valid = false
			if resp.BrokenEntryId == "" {
				resp.BrokenEntryId = f.TraceID
				resp.BrokenReason = f.Type.String()
			}
		}
		resp.Runs = append(resp.Runs, rv)
		resp.EntriesVerified += rv.EntriesVerified
	}

	return resp, nil
}

// GetRunReport produces a summary report for a single run: status, timing,
// per-host results and optionally embedded logs.
func (s *AuditService) GetRunReport(ctx context.Context, req *pb.GetRunReportRequest) (*pb.RunReport, error) {
	if req == nil {
		return nil, status.Error(codes.InvalidArgument, "nil request")
	}
	if req.ChangeId == "" && req.RunId == "" {
		return nil, status.Error(codes.InvalidArgument, "change_id or run_id is required")
	}
	if s.store == nil {
		return nil, status.Error(codes.FailedPrecondition, "store not configured")
	}

	// Resolve the run. When ChangeId is given, treat it as the run ID.
	runID := req.RunId
	if runID == "" {
		runID = req.ChangeId
	}
	run, err := s.store.GetRun(ctx, runID)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "get run: %v", err)
	}
	if run == nil {
		return nil, status.Errorf(codes.NotFound, "run %q not found", runID)
	}

	report := &pb.RunReport{
		ChangeId:   req.ChangeId,
		RunId:      run.ID,
		Status:     run.Status,
		StartedAt:  run.CreatedAt.Unix(),
		FinishedAt: run.UpdatedAt.Unix(),
	}
	if !run.UpdatedAt.IsZero() {
		report.DurationMs = run.UpdatedAt.Sub(run.CreatedAt).Milliseconds()
	}

	// Aggregate per-host results from steps.
	batches, err := s.store.ListBatches(ctx, state.BatchFilter{RunID: run.ID, Limit: maxPageSize})
	if err != nil {
		return nil, status.Errorf(codes.Internal, "list batches: %v", err)
	}

	hostResults := make([]*pb.HostResult, 0)
	var totalHosts, successHosts, failedHosts, skippedHosts int32

	for batchIdx, batch := range batches {
		steps, err := s.store.ListSteps(ctx, state.StepFilter{RunID: run.ID, BatchID: batch.ID, Limit: maxPageSize})
		if err != nil {
			return nil, status.Errorf(codes.Internal, "list steps for batch %q: %v", batch.ID, err)
		}
		for _, step := range steps {
			hr := &pb.HostResult{
				Host:       step.Host,
				Status:     step.Status,
				DurationMs: int64(step.DurationMs),
				BatchIndex: int32(batchIdx),
			}
			if step.Status == "failed" && step.Stderr != "" {
				hr.Error = step.Stderr
			}
			hostResults = append(hostResults, hr)
			totalHosts++
			switch step.Status {
			case "success", "succeeded":
				successHosts++
			case "failed":
				failedHosts++
			case "skipped":
				skippedHosts++
			}
		}
	}

	report.HostResults = hostResults
	report.TotalHosts = totalHosts
	report.SuccessHosts = successHosts
	report.FailedHosts = failedHosts
	report.SkippedHosts = skippedHosts

	// Optionally embed logs (trace entries).
	if req.IncludeLogs {
		traces, err := s.store.ListTraces(ctx, state.TraceFilter{RunID: run.ID, Limit: maxPageSize})
		if err != nil {
			return nil, status.Errorf(codes.Internal, "list traces: %v", err)
		}
		logs := make([]*pb.LogEntry, 0, len(traces))
		for _, t := range traces {
			logs = append(logs, &pb.LogEntry{
				RunId:     run.ID,
				Timestamp: t.Timestamp.UTC().Format(time.RFC3339),
				Level:     "INFO",
				Message:   t.Event,
				Source:    t.Actor,
			})
		}
		report.Logs = logs
	}

	return report, nil
}

// --- conversion helpers ------------------------------------------------------

// auditToTraceEntry converts a state.Audit to a pb.TraceEntry.
func auditToTraceEntry(a *state.Audit) *pb.TraceEntry {
	if a == nil {
		return nil
	}
	return &pb.TraceEntry{
		Id:         a.ID,
		RunId:      a.RunID,
		Action:     a.Action,
		TargetHost: a.Target,
		Timestamp:  a.Timestamp.Unix(),
	}
}

// traceToPB converts a state.Trace to a pb.TraceEntry.
func traceToPB(t *state.Trace) *pb.TraceEntry {
	if t == nil {
		return nil
	}
	return &pb.TraceEntry{
		Id:        t.ID,
		RunId:     t.RunID,
		Action:    t.Event,
		PrevHash:  t.PrevHash,
		CurrHash:  t.CurrHash,
		Timestamp: t.Timestamp.Unix(),
	}
}
