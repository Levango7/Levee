package main

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/nexus/levee/internal/approval"
	"github.com/nexus/levee/internal/audit"
	"github.com/nexus/levee/internal/calendar"
	"github.com/nexus/levee/internal/config"
	"github.com/nexus/levee/internal/dsl"
	"github.com/nexus/levee/internal/state"
)

// workflowDisplay renders a run's stored workflow source for human-facing
// output. run.WorkflowName holds the *source* — inline YAML for template and
// gRPC-instantiated runs, a file path for `change create` — because that is
// what wiring.resolveWorkflow parses; it is not something a one-line table
// cell can show. The workflow's own declared name is the honest rendering.
// Anything that does not parse (a path, or a corrupt document the operator
// still has to recognise) is returned exactly as stored, never guessed at.
func workflowDisplay(src string) string {
	src = strings.TrimSpace(src)
	if src == "" || !strings.ContainsAny(src, "\n\r") {
		return src
	}
	wf, err := dsl.NewParser().ParseBytes([]byte(src))
	if err != nil || wf == nil || wf.Meta.Name == "" {
		return src
	}
	return wf.Meta.Name
}

// applySecurityConfig propagates security-related configuration into the
// process-wide subsystem registries. Call it after every successful
// config.Load so audit redaction sees the full security.sensitive_fields
// vocabulary before any trace is recorded (SA-009/SA-010).
func applySecurityConfig(cfg *config.Config) {
	if cfg == nil {
		return
	}
	audit.SetSensitiveFields(cfg.Security.SensitiveFields)
}

// openStore loads the LEVEE configuration and opens the database it names. The
// caller is responsible for calling Close on the returned store when done.
func openStore(ctx context.Context) (state.Store, error) {
	cfg, err := config.Load(optConfigPath)
	if err != nil {
		return nil, fmt.Errorf("load config: %w", err)
	}
	applySecurityConfig(cfg)
	return openStoreFromConfig(ctx, cfg)
}

// openStoreFromConfig is the single place in the CLI that turns a configuration
// into a live store: `levee <command>`, `serve` without --cluster, `system
// status`/`doctor` and the change calendar all reach a database through it.
//
// Two properties are load-bearing here, and both are why this is a function
// rather than four copies of an if-statement.
//
// The first is that the calendar gates added in #46 only protect a deployment
// when the process that *writes* freeze periods and the process that *reads*
// them hold the same database. Choosing the backend in one place is what makes
// "the same database" a property of the config file instead of a coincidence of
// which sub-command happened to be invoked.
//
// The second is that an unrecognised driver must fail rather than default to
// SQLite. Silent fallback to a local file is the worst available outcome: the
// operator's runs, approvals and freeze periods stay in a database the server
// never reads, every command reports success, and nothing on any output surface
// says the deployment is split. config.Validate rejects unknown drivers, so
// this default arm only fires on a Config built in memory — and it says so.
func openStoreFromConfig(ctx context.Context, cfg *config.Config) (state.Store, error) {
	switch cfg.Database.Driver {
	case state.DriverSQLite:
		store, err := state.NewSQLiteStore(ctx, cfg.Database.Path,
			state.WithSynchronous(cfg.State.SQLiteSynchronous))
		if err != nil {
			return nil, fmt.Errorf("open sqlite store at %s: %w", cfg.Database.Path, err)
		}
		return store, nil
	case state.DriverPostgres:
		store, err := state.NewPGStore(ctx, cfg.Database.DSN, state.PGPoolConfig{
			MaxOpenConns:    cfg.Database.MaxOpenConns,
			MaxIdleConns:    cfg.Database.MaxIdleConns,
			ConnMaxLifetime: cfg.Database.ConnMaxLifetime,
		})
		if err != nil {
			// The DSN is deliberately not repeated: it carries credentials, and
			// an error string like this ends up on stderr and in audit records.
			return nil, fmt.Errorf("open postgres store at %s: %w",
				cfg.Database.StoreLocation(), err)
		}
		return store, nil
	default:
		return nil, fmt.Errorf("unknown database.driver %q (want %s or %s)",
			cfg.Database.Driver, state.DriverSQLite, state.DriverPostgres)
	}
}

// calendarFor attaches the change calendar to the database this process already
// holds. It takes the state store rather than a DSN on purpose: freeze periods
// have to be read from the same database the server executes against, and a
// second connection string is a second thing to get wrong.
//
// The store's concrete type decides the SQL dialect, because `internal/calendar`
// cannot tell a SQLite handle from a PostgreSQL one by looking at it, and
// guessing wrong fails at CREATE TABLE (`type "datetime" does not exist`) or at
// every `$n`/`?` placeholder. An unknown store implementation is an error, not a
// silent SQLite: a calendar nobody can open means freeze enforcement is inert,
// and the caller has to say so out loud rather than plan happily.
//
// Decorators are unwrapped before the type switch. This is not cosmetic: with
// `tenant.enabled: true` the handle every request-serving service holds is a
// tenant.TenantStore around the real store, and matching the decorator instead of
// the database behind it made the freeze gate refuse to open — a governance
// control that silently switches itself off in the deployment shape it exists
// for. See internal/tenant.TenantStore.Underlying.
func calendarFor(ctx context.Context, store state.Store) (*calendar.CalendarService, error) {
	if store == nil {
		return nil, fmt.Errorf("calendar unavailable: no state store to share")
	}
	var (
		db      *sql.DB
		dialect calendar.Dialect
	)
	switch s := state.Underlying(store).(type) {
	case *state.SQLiteStore:
		db, dialect = s.DB(), calendar.DialectSQLite
	case *state.PGStore:
		db, dialect = s.DB(), calendar.DialectPostgres
	default:
		return nil, fmt.Errorf("calendar unavailable: unknown state store type %T", store)
	}
	calStore, err := calendar.NewStore(ctx, db, dialect)
	if err != nil {
		return nil, fmt.Errorf("calendar unavailable: %w", err)
	}
	return calendar.NewCalendarService(calStore), nil
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
	// Initiator/ExcludeInitiator carry the independence requirement. A
	// record written before these fields existed decodes to "no
	// exclusion", which is the only safe reading of an absent value: the
	// alternative would retroactively void historical approvals.
	Initiator        string `json:"initiator,omitempty"`
	ExcludeInitiator bool   `json:"exclude_initiator,omitempty"`
}

// approvalToState converts an approval.Approval to a state.Approval.
func approvalToState(ap *approval.Approval) (*state.Approval, error) {
	extra := approvalExtra{
		Approvers:        ap.Approvers,
		MinApprovers:     ap.MinApprovers,
		Decisions:        ap.Decisions,
		CreatedAt:        ap.CreatedAt,
		ExpiresAt:        ap.ExpiresAt,
		Initiator:        ap.Initiator,
		ExcludeInitiator: ap.ExcludeInitiator,
	}
	extraJSON, err := json.Marshal(extra)
	if err != nil {
		return nil, fmt.Errorf("marshal approval extra: %w", err)
	}

	sa := &state.Approval{
		ID:       ap.ID,
		RunID:    ap.RunID,
		Level:    ap.Level,
		Status:   string(ap.Status),
		Comment:  string(extraJSON),
		PlanHash: ap.PlanHash,
		Revision: ap.Revision,
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
// A non-empty Comment that fails to parse is treated as a hard error rather
// than silently returning a zero-valued record: approver identity, quorum and
// existing decisions are governance data, and silently degrading them could
// let a record pass through with MinApprovers effectively 0 (which the
// service re-clamps to 1) or lose prior votes. ListPending callers already
// skip malformed records; decision paths surface this error to the operator.
func stateToApproval(sa *state.Approval) (*approval.Approval, error) {
	ap := &approval.Approval{
		ID:       sa.ID,
		RunID:    sa.RunID,
		Level:    sa.Level,
		Status:   approval.Status(sa.Status),
		PlanHash: sa.PlanHash,
		Revision: sa.Revision,
	}

	if sa.Comment != "" {
		var extra approvalExtra
		if err := json.Unmarshal([]byte(sa.Comment), &extra); err != nil {
			return nil, fmt.Errorf("approval %q has unparsable metadata (comment %q): %w", sa.ID, sa.Comment, err)
		}
		ap.Approvers = extra.Approvers
		ap.MinApprovers = extra.MinApprovers
		ap.Decisions = extra.Decisions
		ap.CreatedAt = extra.CreatedAt
		ap.ExpiresAt = extra.ExpiresAt
		ap.Initiator = extra.Initiator
		ap.ExcludeInitiator = extra.ExcludeInitiator
	}

	if sa.TimeoutAt != nil && ap.ExpiresAt.IsZero() {
		ap.ExpiresAt = *sa.TimeoutAt
	}

	return ap, nil
}
