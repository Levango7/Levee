// persist.go writes the operator-visible evidence rows for engine
// executions: batch rows are reused across retries (the DDL enforces
// UNIQUE(run_id, batch_no)), step rows are append-only so every attempt
// keeps its own audit trail. Status vocabularies mirror the state layer:
// batches pending|running|completed|failed|skipped, steps
// pending|running|success|failed|skipped.

package wiring

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/nexus/levee/internal/engine"
	"github.com/nexus/levee/internal/executor"
	"github.com/nexus/levee/internal/plan"
	"github.com/nexus/levee/internal/rollback"
	"github.com/nexus/levee/internal/state"
)

// utcNow is the single clock source for engine-written rows (the state
// layer stores UTC everywhere).
func utcNow() time.Time { return time.Now().UTC() }

// batchesByNo snapshots the batch rows already persisted for a run, keyed
// by batch_no (1-based).
func (e *Engine) batchesByNo(ctx context.Context, changeID string) (map[int]*state.Batch, error) {
	rows, err := e.store.ListBatches(ctx, state.BatchFilter{RunID: changeID})
	if err != nil {
		return nil, fmt.Errorf("wiring: list batches for %q: %w", changeID, err)
	}
	byNo := make(map[int]*state.Batch, len(rows))
	for _, b := range rows {
		byNo[b.BatchNo] = b
	}
	return byNo, nil
}

// stepAction maps a step name back to its "module.action" using the plan
// batch it came from. Returns "" when the name is unknown (should not
// happen — results are produced from this same plan).
func stepAction(pb plan.Batch, stepName string) string {
	for _, ps := range pb.Steps {
		if ps.Name == stepName {
			return ps.Module + "." + ps.Action
		}
	}
	return ""
}

// persistClosureResults writes the batch and step evidence for one closure
// execution. It never returns a partial-success illusion: every store
// failure is collected and joined.
func (e *Engine) persistClosureResults(
	ctx context.Context,
	changeID string,
	p *plan.Plan,
	res *engine.ClosureResult,
	outputs map[string]stepOutput,
) error {
	planByIndex := make(map[int]plan.Batch, len(p.Batches))
	for _, b := range p.Batches {
		planByIndex[b.Index] = b
	}

	byNo, err := e.batchesByNo(ctx, changeID)
	if err != nil {
		return err
	}

	now := utcNow()
	var errs []error
	for _, br := range res.BatchResults {
		batchNo := br.BatchIndex + 1
		failed := 0
		for _, tr := range br.TargetResults {
			if tr.Error != nil {
				failed++
			}
		}
		succeeded := len(br.TargetResults) - failed
		status := "completed"
		if br.Error != nil {
			status = "failed"
		}
		started := now.Add(-br.Duration)
		completed := now

		row, exists := byNo[batchNo]
		if !exists {
			row = &state.Batch{
				ID:          newID("bat-"),
				RunID:       changeID,
				BatchNo:     batchNo,
				Status:      status,
				TotalHosts:  len(br.TargetResults),
				Succeeded:   succeeded,
				Failed:      failed,
				StartedAt:   &started,
				CompletedAt: &completed,
			}
			if err := e.store.CreateBatch(ctx, row); err != nil {
				errs = append(errs, fmt.Errorf("create batch %d: %w", batchNo, err))
				continue
			}
			byNo[batchNo] = row
		} else {
			row.Status = status
			row.TotalHosts = len(br.TargetResults)
			row.Succeeded = succeeded
			row.Failed = failed
			row.StartedAt = &started
			row.CompletedAt = &completed
			if err := e.store.UpdateBatch(ctx, row); err != nil {
				errs = append(errs, fmt.Errorf("update batch %d: %w", batchNo, err))
				continue
			}
		}

		pb, havePlan := planByIndex[br.BatchIndex]
		for _, tr := range br.TargetResults {
			for _, sr := range tr.StepResults {
				action := ""
				if havePlan {
					action = stepAction(pb, sr.StepName)
				}
				stepStatus := "success"
				if sr.Error != nil {
					stepStatus = "failed"
				}
				step := &state.Step{
					ID:          newID("stp-"),
					RunID:       changeID,
					BatchID:     row.ID,
					Host:        tr.Target,
					StepName:    sr.StepName,
					Action:      action,
					Status:      stepStatus,
					DurationMs:  int(sr.Duration.Milliseconds()),
					StartedAt:   ptrTo(now.Add(-sr.Duration)),
					CompletedAt: ptrTo(now),
				}
				attachOutput(step, outputs, outputKey(tr.Target, sr.StepName))
				if sr.Error != nil && step.Stderr == "" {
					step.Stderr = tail(sr.Error.Error(), maxErrOutputLen)
				}
				if err := e.store.CreateStep(ctx, step); err != nil {
					errs = append(errs, fmt.Errorf("create step %q on %q: %w", sr.StepName, tr.Target, err))
				}
			}
		}
	}

	// Automatic rollback evidence: when the closure unwound executed
	// batches after a failure, the undo steps live in res.RollbackResult,
	// keyed to the same batch_no rows just written above.
	if res.RollbackResult != nil {
		if err := e.persistRollbackResults(ctx, changeID, res.RollbackResult, outputs); err != nil {
			errs = append(errs, fmt.Errorf("auto-rollback evidence: %w", err))
		}
	}
	return errors.Join(errs...)
}

// persistRollbackResults writes undo-step evidence for a manual rollback
// (and for the closure's automatic rollback). Existing batch rows are NOT
// re-labelled (their status records the forward execution; the undo is
// evidenced by its own step rows). When a batch row is missing entirely
// (pre-engine run, or an earlier persist failure), a placeholder row with
// status "rolled_back" is created so the FK holds.
func (e *Engine) persistRollbackResults(
	ctx context.Context,
	changeID string,
	res *rollback.RollbackResult,
	outputs map[string]stepOutput,
) error {
	if res == nil {
		return nil
	}
	byNo, err := e.batchesByNo(ctx, changeID)
	if err != nil {
		return err
	}

	now := utcNow()
	var errs []error
	for _, br := range res.BatchResults {
		batchNo := br.BatchIndex + 1
		row, ok := byNo[batchNo]
		if !ok {
			row = &state.Batch{
				ID:          newID("bat-"),
				RunID:       changeID,
				BatchNo:     batchNo,
				Status:      "rolled_back",
				TotalHosts:  len(br.TargetResults),
				StartedAt:   ptrTo(now),
				CompletedAt: ptrTo(now),
			}
			if err := e.store.CreateBatch(ctx, row); err != nil {
				errs = append(errs, fmt.Errorf("create rollback batch row %d: %w", batchNo, err))
				continue
			}
			byNo[batchNo] = row
		}

		for _, tr := range br.TargetResults {
			for _, sr := range tr.StepResults {
				name := sr.RollbackStepName
				if name == "" {
					name = sr.OrigStepName
				}
				action := ""
				if sr.Module != "" {
					action = sr.Module + "." + sr.Action
				}
				stepStatus := "success"
				switch {
				case sr.Skipped:
					stepStatus = "skipped"
				case sr.Error != nil:
					stepStatus = "failed"
				}
				step := &state.Step{
					ID:          newID("stp-"),
					RunID:       changeID,
					BatchID:     row.ID,
					Host:        tr.Target,
					StepName:    name,
					Action:      action,
					Status:      stepStatus,
					DurationMs:  int(sr.Duration.Milliseconds()),
					StartedAt:   ptrTo(now.Add(-sr.Duration)),
					CompletedAt: ptrTo(now),
				}
				if !sr.Skipped {
					attachOutput(step, outputs, outputKey(tr.Target, sr.RollbackStepName))
				}
				switch {
				case sr.Skipped && sr.SkipReason != "":
					step.Stderr = sr.SkipReason
				case sr.Error != nil && step.Stderr == "":
					step.Stderr = tail(sr.Error.Error(), maxErrOutputLen)
				}
				if err := e.store.CreateStep(ctx, step); err != nil {
					errs = append(errs, fmt.Errorf("create rollback step %q on %q: %w", name, tr.Target, err))
				}
			}
		}
	}
	return errors.Join(errs...)
}

// attachOutput copies captured channel output (last call wins per
// host+step, see stepOutput) onto a step row.
func attachOutput(step *state.Step, outputs map[string]stepOutput, key string) {
	out, ok := outputs[key]
	if !ok {
		return
	}
	code := out.ExitCode
	step.ExitCode = &code
	step.Stdout = out.Stdout
	step.Stderr = out.Stderr
}

func ptrTo[T any](v T) *T { return &v }

// --- resumable retry -------------------------------------------------------

// resumeEvidence is the persisted evidence for one skipped step of a
// resumable retry — it records that the step was intentionally not
// re-dispatched because it had already completed via an idempotent module.
type resumeEvidence struct {
	BatchNo  int
	Host     string
	StepName string
	Action   string
}

// completedIdempotentBatches returns the batch numbers (1-based) whose every
// (step, host) combination already has a persisted "success" row AND whose
// module declares itself idempotent. These batches can be skipped on a
// resumable retry without changing target state.
//
// The check is conservative: any missing row, any non-success status, any
// non-idempotent module, or any step with no declared module leaves the batch
// OUT of the result (it will be re-run). Batches with zero steps are never
// reported as completable — re-running an empty batch is harmless and avoids
// a surprising "everything skipped" outcome when evidence is absent.
func (e *Engine) completedIdempotentBatches(ctx context.Context, changeID string, p *plan.Plan) (map[int]bool, error) {
	batches, err := e.store.ListBatches(ctx, state.BatchFilter{RunID: changeID})
	if err != nil {
		return nil, fmt.Errorf("wiring: list batches for %q: %w", changeID, err)
	}
	noToID := make(map[int]string, len(batches))
	for _, b := range batches {
		noToID[b.BatchNo] = b.ID
	}

	steps, err := e.store.ListSteps(ctx, state.StepFilter{RunID: changeID, Limit: 10000})
	if err != nil {
		return nil, fmt.Errorf("wiring: list steps for %q: %w", changeID, err)
	}

	// key (batch_no, step_name, host) -> succeeded.
	type stepKey struct {
		batchNo  int
		stepName string
		host     string
	}
	succeeded := make(map[stepKey]bool, len(steps))
	for _, s := range steps {
		batchNo := batchNoForID(noToID, s.BatchID)
		if batchNo == 0 {
			continue
		}
		if s.Status != "success" {
			continue
		}
		succeeded[stepKey{batchNo: batchNo, stepName: s.StepName, host: s.Host}] = true
	}

	completable := make(map[int]bool)
	for _, b := range p.Batches {
		batchNo := b.Index + 1
		if len(b.Steps) == 0 || len(b.Targets) == 0 {
			continue
		}
		allDone := true
		for _, ps := range b.Steps {
			if !executor.DefaultExecutor().IsIdempotent(ps.Module) {
				allDone = false
				break
			}
			for _, host := range b.Targets {
				if !succeeded[stepKey{batchNo: batchNo, stepName: ps.Name, host: host}] {
					allDone = false
					break
				}
			}
			if !allDone {
				break
			}
		}
		if allDone {
			completable[batchNo] = true
		}
	}
	return completable, nil
}

// batchNoForID inverts the batchNo->ID map produced by ListBatches. Returns 0
// when the id is unknown (a step whose batch row somehow vanished).
func batchNoForID(noToID map[int]string, id string) int {
	for no, bid := range noToID {
		if bid == id {
			return no
		}
	}
	return 0
}

// buildResumePlan returns a plan identical to p but with every batch whose
// number is in skip removed, plus the list of skipped (batch, step, host)
// evidence triples that callers must persist for audit completeness. Batches
// are renumbered so their Index values remain contiguous (the plan is a fresh
// copy; the input is not mutated). BatchNo in the evidence uses the ORIGINAL
// 1-based numbering so it stays aligned with already-persisted rows.
func buildResumePlan(p *plan.Plan, skip map[int]bool) (*plan.Plan, []resumeEvidence) {
	resume := &plan.Plan{
		ID:           p.ID,
		WorkflowName: p.WorkflowName,
		TotalTargets: 0,
		CreatedAt:    p.CreatedAt,
	}
	var skipped []resumeEvidence
	newIndex := 0
	for _, b := range p.Batches {
		originalBatchNo := b.Index + 1
		// A batch is skippable only when the caller flagged it AND it
		// actually has work (steps and targets). An empty batch is never
		// skipped — completedIdempotentBatches already excludes those,
		// but this guards buildResumePlan when called directly.
		if skip[originalBatchNo] && len(b.Steps) > 0 && len(b.Targets) > 0 {
			for _, ps := range b.Steps {
				for _, host := range b.Targets {
					skipped = append(skipped, resumeEvidence{
						BatchNo:  originalBatchNo,
						Host:     host,
						StepName: ps.Name,
						Action:   ps.Module + "." + ps.Action,
					})
				}
			}
			continue
		}
		b.Index = newIndex
		newIndex++
		resume.TotalTargets += len(b.Targets)
		resume.Batches = append(resume.Batches, b)
	}
	return resume, skipped
}

// persistResumeEvidence writes the skipped-step evidence rows for a resumable
// retry. Each skipped (batch, step, host) triple becomes a "skipped" step row
// with a reason that names the idempotency guarantee. Best-evidence: a store
// failure is logged but does not abort the run (the operator already has the
// original success rows for these steps).
func (e *Engine) persistResumeEvidence(ctx context.Context, changeID string, skipped []resumeEvidence) {
	now := utcNow()
	for _, sk := range skipped {
		_ = e.store.CreateStep(ctx, &state.Step{
			ID:          newID("stp-"),
			RunID:       changeID,
			BatchID:     batchIDForBatchNo(e, ctx, changeID, sk.BatchNo),
			Host:        sk.Host,
			StepName:    sk.StepName,
			Action:      sk.Action,
			Status:      "skipped",
			Stderr:      "skipped on resume: already completed by idempotent module",
			DurationMs:  0,
			StartedAt:   &now,
			CompletedAt: &now,
		})
	}
}

// batchIDForBatchNo resolves the persisted batch row id for a batch number, so
// skipped step rows link to the correct batch. Returns "" on miss (the step
// row will fail the batch FK; that is caught by CreateStep and logged by
// persistResumeEvidence's caller convention).
func batchIDForBatchNo(e *Engine, ctx context.Context, changeID string, batchNo int) string {
	batches, err := e.store.ListBatches(ctx, state.BatchFilter{RunID: changeID})
	if err != nil {
		return ""
	}
	for _, b := range batches {
		if b.BatchNo == batchNo {
			return b.ID
		}
	}
	return ""
}
