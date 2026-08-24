package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/nexus/levee/internal/approval"
	"github.com/nexus/levee/internal/config"
	"github.com/nexus/levee/internal/state"
)

// openStore loads the LEVEE configuration and opens a SQLite store. The caller
// is responsible for calling Close on the returned store when done.
func openStore(ctx context.Context) (*state.SQLiteStore, error) {
	cfg, err := config.Load(optConfigPath)
	if err != nil {
		return nil, fmt.Errorf("load config: %w", err)
	}
	store, err := state.NewSQLiteStore(ctx, cfg.Database.Path)
	if err != nil {
		return nil, fmt.Errorf("open store: %w", err)
	}
	return store, nil
}

// currentActor returns the identity of the CLI user for audit purposes. It
// checks the LEVEE_ACTOR environment variable first, then falls back to
// "cli-user".
func currentActor() string {
	if actor := os.Getenv("LEVEE_ACTOR"); actor != "" {
		return actor
	}
	return "cli-user"
}

// templateDir returns the base directory for the template library. It is
// derived from the LEVEE data directory: ~/.levee/templates/.
func templateDir(cfg *config.Config) string {
	// The data dir is typically ~/.levee/data; templates live one level up
	// under ~/.levee/templates.
	parent := filepath.Dir(cfg.Server.DataDir)
	return filepath.Join(parent, "templates")
}

// generateRunID creates a new random run identifier using crypto/rand.
func generateRunID() (string, error) {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate run id: %w", err)
	}
	return "run-" + hex.EncodeToString(b), nil
}

// --- approval.Store adapter --------------------------------------------------
//
// approvalStoreAdapter bridges state.Store to the approval.Store interface
// expected by approval.Service. The two Approval types have different
// structures: state.Approval is a per-approver record while approval.Approval
// is a multi-approver record. The adapter serialises the extra fields
// (Approvers, MinApprovers, Decisions, CreatedAt, ExpiresAt) as a JSON blob
// in state.Approval.Comment so that they survive round-trips through the
// state store.

// approvalStoreAdapter adapts state.Store to the approval.Store interface.
type approvalStoreAdapter struct {
	store state.Store
}

// newApprovalStoreAdapter wraps a state.Store as an approval.Store.
func newApprovalStoreAdapter(store state.Store) *approvalStoreAdapter {
	return &approvalStoreAdapter{store: store}
}

func (a *approvalStoreAdapter) Create(ctx context.Context, ap *approval.Approval) error {
	sa, err := approvalToState(ap)
	if err != nil {
		return fmt.Errorf("convert approval: %w", err)
	}
	return a.store.CreateApproval(ctx, sa)
}

func (a *approvalStoreAdapter) Get(ctx context.Context, id string) (*approval.Approval, error) {
	sa, err := a.store.GetApproval(ctx, id)
	if err != nil {
		return nil, err
	}
	if sa == nil {
		return nil, nil
	}
	return stateToApproval(sa)
}

func (a *approvalStoreAdapter) Update(ctx context.Context, ap *approval.Approval) error {
	sa, err := approvalToState(ap)
	if err != nil {
		return fmt.Errorf("convert approval: %w", err)
	}
	return a.store.UpdateApproval(ctx, sa)
}

// UpdateIfPending implements the compare-and-set half of approval.Store:
// it maps to state.Store.UpdateApprovalIfPending, which applies the update
// only while the stored row is still in status "pending" and reports
// whether it won. This is what makes Approve/Reject exactly-once under
// concurrent decisions.
func (a *approvalStoreAdapter) UpdateIfPending(ctx context.Context, ap *approval.Approval) (bool, error) {
	sa, err := approvalToState(ap)
	if err != nil {
		return false, fmt.Errorf("convert approval: %w", err)
	}
	return a.store.UpdateApprovalIfPending(ctx, sa)
}

func (a *approvalStoreAdapter) ListPending(ctx context.Context) ([]*approval.Approval, error) {
	sas, err := a.store.ListApprovals(ctx, state.ApprovalFilter{
		Status: string(approval.StatusPending),
	})
	if err != nil {
		return nil, err
	}
	var result []*approval.Approval
	for _, sa := range sas {
		ap, err := stateToApproval(sa)
		if err != nil {
			continue // skip malformed records
		}
		result = append(result, ap)
	}
	return result, nil
}

// approvalExtra holds the fields that do not map directly to state.Approval
// columns. They are serialised as JSON and stored in state.Approval.Comment.
type approvalExtra struct {
	Approvers    []string            `json:"approvers,omitempty"`
	MinApprovers int                 `json:"min_approvers,omitempty"`
	Decisions    []approval.Decision `json:"decisions,omitempty"`
	CreatedAt    time.Time           `json:"created_at"`
	ExpiresAt    time.Time           `json:"expires_at"`
}

// approvalToState converts an approval.Approval to a state.Approval.
func approvalToState(ap *approval.Approval) (*state.Approval, error) {
	extra := approvalExtra{
		Approvers:    ap.Approvers,
		MinApprovers: ap.MinApprovers,
		Decisions:    ap.Decisions,
		CreatedAt:    ap.CreatedAt,
		ExpiresAt:    ap.ExpiresAt,
	}
	extraJSON, err := json.Marshal(extra)
	if err != nil {
		return nil, fmt.Errorf("marshal approval extra: %w", err)
	}

	sa := &state.Approval{
		ID:      ap.ID,
		RunID:   ap.RunID,
		Level:   ap.Level,
		Status:  string(ap.Status),
		Comment: string(extraJSON),
	}

	if !ap.ExpiresAt.IsZero() {
		t := ap.ExpiresAt
		sa.TimeoutAt = &t
	}

	// Set Approver and ActedAt from the latest decision, if any.
	if len(ap.Decisions) > 0 {
		last := ap.Decisions[len(ap.Decisions)-1]
		sa.Approver = last.Approver
		sa.ActedAt = &last.At
	}

	return sa, nil
}

// stateToApproval converts a state.Approval back to an approval.Approval.
func stateToApproval(sa *state.Approval) (*approval.Approval, error) {
	ap := &approval.Approval{
		ID:     sa.ID,
		RunID:  sa.RunID,
		Level:  sa.Level,
		Status: approval.Status(sa.Status),
	}

	// Restore extra fields from the JSON blob in Comment.
	if sa.Comment != "" {
		var extra approvalExtra
		if err := json.Unmarshal([]byte(sa.Comment), &extra); err == nil {
			ap.Approvers = extra.Approvers
			ap.MinApprovers = extra.MinApprovers
			ap.Decisions = extra.Decisions
			ap.CreatedAt = extra.CreatedAt
			ap.ExpiresAt = extra.ExpiresAt
		}
		// If unmarshal fails, the extra fields are left at zero values.
		// This is defensive: the record may have been created by a different path.
	}

	if sa.TimeoutAt != nil && ap.ExpiresAt.IsZero() {
		ap.ExpiresAt = *sa.TimeoutAt
	}

	return ap, nil
}
