package dsl

// run_snapshot.go implements the run-level baseline primitive: a single
// capture of the declared paths on every target, taken once before the
// first batch and restored once if the run rolls back.
//
// Why this is a separate primitive rather than a workflow-level
// `rollback.snapshot_paths` (which LE097 rejects outright): the compensation
// ledger attributes compensations per (host, forward step). A workflow-level
// path list has no step to hang on, and projecting it onto every step would
// mean N pre-images for one run, restored from whichever step ran last. The
// honest shape for "the state before this whole run" is one field with one
// capture and one restore.
//
// Validation is fail-closed on purpose. Every rule below rejects a document
// that would compile into a baseline nobody restores, or a baseline whose
// scope is not what the operator thinks it is. A silently accepted baseline
// is worse than a rejected document: the operator would read "snapshot:
// scope: run" as "rollback will put my files back" and be wrong.

import (
	"fmt"
	"strings"
)

// run snapshot validation codes (LE098–LE102). LE097 remains the
// workflow-level rollback rejection this primitive exists alongside.
const (
	codeRunSnapshotScope    = "LE098"
	codeRunSnapshotNoPaths  = "LE099"
	codeRunSnapshotType     = "LE100"
	codeRunSnapshotPathForm = "LE101"
	codeRunSnapshotNoUndo   = "LE102"
)

// RunSnapshotScopeRun is the only accepted scope.
const RunSnapshotScopeRun = "run"

// RunSnapshotTypes is the accepted capture vocabulary. Empty means the
// default (SnapshotTypeFile) and is resolved by the planner, not here —
// the validator's job is to reject nonsense, not to materialise defaults.
var RunSnapshotTypes = []string{"file", "config"}

// ValidateRunSnapshot checks the run-level baseline declaration. spec may be
// nil (no baseline declared), which is always valid.
func ValidateRunSnapshot(spec *RunSnapshotSpec, rollback *RollbackSpec, field string) []ValidationError {
	if spec == nil {
		return nil
	}
	var errs []ValidationError

	// Scope must say "run" explicitly. Omitting it is an error rather than
	// a default: the entire point of the field is that the blast radius is
	// stated, and an omitted scope is exactly the workflow-level ambiguity
	// LE097 exists to prevent.
	if spec.Scope != RunSnapshotScopeRun {
		errs = append(errs, ValidationError{
			Code:  codeRunSnapshotScope,
			Field: field + ".scope",
			Message: fmt.Sprintf("run snapshot scope must be %q (allowed: %s); "+
				"per-step and per-batch scopes are rejected because a path list "+
				"without a step to attach to has no compensation basis (LE097)",
				RunSnapshotScopeRun, RunSnapshotScopeRun),
		})
	}

	if len(spec.Paths) == 0 {
		errs = append(errs, ValidationError{
			Code:    codeRunSnapshotNoPaths,
			Field:   field + ".paths",
			Message: "run snapshot declares no paths; there is nothing to capture",
		})
	}
	for i, p := range spec.Paths {
		if strings.TrimSpace(p) == "" {
			errs = append(errs, ValidationError{
				Code:    codeRunSnapshotNoPaths,
				Field:   fmt.Sprintf("%s.paths[%d]", field, i),
				Message: "run snapshot path is empty",
			})
			continue
		}
		// Absolute only. A relative path is resolved against whatever the
		// executor's working directory happens to be at capture time, which
		// is a different directory at restore time — the baseline would be
		// written somewhere and restored from somewhere else.
		if !strings.HasPrefix(p, "/") {
			errs = append(errs, ValidationError{
				Code:  codeRunSnapshotPathForm,
				Field: fmt.Sprintf("%s.paths[%d]", field, i),
				Message: fmt.Sprintf("run snapshot path %q must be absolute; a relative "+
					"path resolves against the executor's working directory, which is "+
					"not the same at capture and restore time", p),
			})
		}
	}

	if spec.Type != "" && !containsString(RunSnapshotTypes, spec.Type) {
		errs = append(errs, ValidationError{
			Code:  codeRunSnapshotType,
			Field: field + ".type",
			// The refusal names the full accepted set, the same way the batch
			// strategy validator does: a help string that can contradict the
			// gate is worse than no help string.
			Message: fmt.Sprintf("invalid run snapshot type %q (allowed: %s)",
				spec.Type, strings.Join(RunSnapshotTypes, ", ")),
		})
	}

	// A baseline is only worth capturing if something will restore it.
	// on_failure: manual suppresses automatic compensation entirely and hands
	// the decision to an operator, whose rollback path does not restore the
	// run baseline yet. Accepting that combination would capture a baseline
	// that nothing consumes, so it is rejected here rather than shipped as a
	// promise. Wiring the manual path is a follow-up, not a caveat.
	if rollback != nil && rollback.OnFailure == RollbackOnFailureManual {
		errs = append(errs, ValidationError{
			Code:  codeRunSnapshotNoUndo,
			Field: "rollback.on_failure",
			Message: "run snapshot is declared together with on_failure: manual, but " +
				"the operator-triggered rollback path does not restore the run baseline; " +
				"drop one of the two declarations until it does",
		})
	}

	return errs
}

func containsString(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}
