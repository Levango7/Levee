package main

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/nexus/levee/internal/approval"
	"github.com/nexus/levee/internal/audit"
	"github.com/nexus/levee/internal/authz"
	"github.com/nexus/levee/internal/calendar"
	"github.com/nexus/levee/internal/config"
	"github.com/nexus/levee/internal/dsl"
	"github.com/nexus/levee/internal/grpc"
	"github.com/nexus/levee/internal/log"
	"github.com/nexus/levee/internal/state"
)

// workflowDisplay renders a run's stored workflow source for human-facing
// output. run.WorkflowName holds the *source* — inline YAML for template and
// gRPC-instantiated runs, a file path for `change create` — because that is
// what wiring.resolveWorkflow parses; it is not something a one-line table
// cell can show. The workflow's own declared name is the honest rendering.
// Anything that does not parse (a path, or a corrupt document the operator
// still has to recognise) is returned exactly as stored, never guessed at.
//
// The implementation moved to dsl.DisplayName when the gRPC change board
// needed the same rendering (it was mapping the raw source onto Change.Label):
// one implementation, two callers, because two would eventually disagree about
// what a run is called.
func workflowDisplay(src string) string {
	return dsl.DisplayName(src)
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
// checks --as first, then the LEVEE_ACTOR environment variable, then falls back
// to "cli-user".
//
// --as wins because it is the identity the policy is judging (see
// localAuthorizer): a governance action recorded under a different name than the
// one that authorised it is exactly the drift grpc.ContextWithSubject exists to
// prevent, and the same argument applies one layer up.
func currentActor() string {
	if optAs != "" {
		return optAs
	}
	if actor := os.Getenv("LEVEE_ACTOR"); actor != "" {
		return actor
	}
	return "cli-user"
}

// localAuthorizer returns the authorizer local commands should judge against, or
// nil for the historical posture (local commands run unjudged).
//
// It returns nil unless the operator asked otherwise, because enforcing local
// policy by default would start refusing commands in every deployment whose
// operator is not in the registry — a behaviour change that arrives as "the CLI
// stopped working", not as a policy decision. --as is the opt-in.
//
// What --as covers, stated here so it is not mistaken for "all of local mode":
// it reaches the actions that local commands perform THROUGH the gRPC service
// layer (plan, apply, and the approval settle that follows a CLI vote), because
// that is where the policy judgements live. Command paths that call a domain
// service directly do not pass a gate and are NOT covered -- approving is the
// one that matters, and it is documented on the command itself.
//
// A registry that cannot be loaded is an ERROR, not a silent nil: the operator
// explicitly asked to be judged, so quietly running unjudged is the one outcome
// they were trying to avoid.
func localAuthorizer() (*authz.Authorizer, error) {
	if optAs == "" {
		return nil, nil
	}
	a, _, err := loadAuthorizerForCmd()
	if err != nil {
		return nil, fmt.Errorf("--as %q: load policy registry: %w", optAs, err)
	}
	if !a.Enforced() {
		// A deployment with no matrix is a legitimate state (serve warns rather
		// than refusing there too), but the operator asked for something that is
		// not happening. Say so instead of leaving them to infer it.
		log.Warn("--as has no permission matrix to judge against; local commands stay unjudged", "as", optAs)
		return nil, nil
	}
	if !a.Knows(optAs) {
		log.Warn("--as names a subject the registry does not know; every judged call will be refused",
			"as", optAs, "registered", a.Registered())
	}
	return a.WithDefaultSubject(optAs), nil
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

// newApprovalService builds the local approval service for every CLI
// surface that casts approval votes (approve, reject, chatops approve and
// reject, gate approve and reject). When --as put a policy registry in
// effect, the service carries a DecisionGuard so the vote itself — not
// just the settle that follows it — is judged in the change's environment;
// without --as the historical unjudged posture is preserved.
//
// Single construction point on purpose: the vote and its gate must not be
// able to drift apart again, and they did exactly that when each command
// built its own service.
func newApprovalService(store state.Store) (*approval.Service, error) {
	authzSvc, err := localAuthorizer()
	if err != nil {
		return nil, err
	}
	svc := approval.NewService(newApprovalStoreAdapter(store))
	if authzSvc == nil {
		return svc, nil
	}
	return svc.WithDecisionGuard(&grpc.ApprovalDecisionGuard{
		Auth:  authzSvc,
		Store: store,
	}), nil
}

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

// ListByRun returns the run's records newest first. The state store orders by
// timeout (an approval's own TTL), which says nothing about which record is
// newest, so the total order is established here: created_at desc, then id
// desc. The evidence read on top of this ("the newest record is what has
// already happened") depends on that order being stable across backends.
func (a *approvalStoreAdapter) ListByRun(ctx context.Context, runID string) ([]*approval.Approval, error) {
	sas, err := a.store.ListApprovals(ctx, state.ApprovalFilter{RunID: runID})
	if err != nil {
		return nil, err
	}
	result := make([]*approval.Approval, 0, len(sas))
	for _, sa := range sas {
		ap, err := stateToApproval(sa)
		if err != nil {
			continue // skip malformed records
		}
		result = append(result, ap)
	}
	sort.Slice(result, func(i, j int) bool {
		if !result[i].CreatedAt.Equal(result[j].CreatedAt) {
			return result[i].CreatedAt.After(result[j].CreatedAt)
		}
		return result[i].ID > result[j].ID
	})
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
// approvalToState / stateToApproval delegate to internal/approval's staterow.go,
// the single owner of the row mapping. Two copies used to exist (this one and a
// near-identical one in internal/grpc's tests), so a change to the mapping
// rewritten how one surface read rows the other still wrote the old shape.
func approvalToState(ap *approval.Approval) (*state.Approval, error) {
	return approval.StateApproval(ap)
}

func stateToApproval(sa *state.Approval) (*approval.Approval, error) {
	return approval.ApprovalFromState(sa)
}

// approvalExpiryResolverFromConfig maps an approval tier to the expiry the
// kickoff record should carry, from the deployment's approval timeouts:
// standard → standard_timeout, high → high_timeout, emergency →
// emergency_supplement (the documented semantics, now enforceable through
// the serve-side expiry sweeper). An unknown level or a non-positive tier
// timeout yields the zero time: the record never expires, which
// approval.CheckExpiry treats as the documented skip — a deployment that
// wants no automatic expiry sets the tier to zero, it is not guessed.
func approvalExpiryResolverFromConfig(cfg *config.Config) func(level string) time.Time {
	return func(level string) time.Time {
		var d time.Duration
		switch level {
		case approval.LevelStandard:
			d = cfg.Approval.StandardTimeout
		case approval.LevelHigh:
			d = cfg.Approval.HighTimeout
		case approval.LevelEmergency:
			d = cfg.Approval.EmergencySupplement
		default:
			return time.Time{}
		}
		if d <= 0 {
			return time.Time{}
		}
		return time.Now().UTC().Add(d)
	}
}

// newID mints a prefixed random identifier (same shape as the gRPC layer's
// and wiring's local helpers, kept local so the command package does not
// import a service package for an id). Used by the serve-side background
// loops that write trace rows outside any RPC.
func newID(prefix string) string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand failure is extraordinary; fall back to a fixed
		// suffix — collision risk is not worth a panic.
		return prefix + "0000000000000000"
	}
	return prefix + hex.EncodeToString(b[:])
}
