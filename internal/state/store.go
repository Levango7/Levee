package state

import (
	"context"
	"errors"
	"time"
)

// DefaultTenantID is the owning tenant assigned to rows that predate
// multi-tenancy (or that have no tenant context when it is enabled). It is
// also the tenant a deployment runs as in single-tenant mode, so an
// unconfigured LEVEE behaves exactly as it did before tenant_id existed:
// every row carries this value and the isolation predicate always matches.
const DefaultTenantID = "default"

// Run is a top-level change execution unit. One Run owns multiple Batches
// executed serially; each Batch owns multiple Steps executed concurrently
// across hosts.
type Run struct {
	ID           string `json:"id"`
	WorkflowName string `json:"workflow_name"`
	TemplateName string `json:"template_name"`
	Params       string `json:"params"` // JSON encoded
	PlanHash     string `json:"plan_hash"`
	// PlanJSON is the canonical JSON encoding of the generated plan.Plan
	// ('' when no plan has been persisted). Apply refuses to execute
	// without it and re-verifies PlanHash against it to detect drift.
	PlanJSON       string    `json:"plan_json"`
	Status         string    `json:"status"`
	ApprovalStatus string    `json:"approval_status"`
	ApprovalLevel  string    `json:"approval_level"`
	CreatedAt      time.Time `json:"created_at"`
	UpdatedAt      time.Time `json:"updated_at"`
	Creator        string    `json:"creator"`
	IncidentID     string    `json:"incident_id"`
	// TenantID is the owning tenant. It is denormalised onto every
	// tenant-owned table (rather than derived through runs) so that every
	// read is a plain indexed `AND tenant_id = ?` with no join or subquery —
	// a single missed predicate would be a cross-tenant read, so the cheap
	// shape is the safe one. internal/state/tenant_consistency_test.go
	// asserts a child row's tenant_id always equals its parent run's.
	//
	// Empty means "unowned/legacy" and is only ever written by the migration
	// backfill; once multi-tenancy is enabled every create path stamps it
	// from the request context. See internal/tenant for the enforcement.
	TenantID string `json:"tenant_id,omitempty"`
}

// Batch is a single batch within a run. Batches are numbered sequentially
// starting at 1 and execute serially.
type Batch struct {
	ID          string     `json:"id"`
	RunID       string     `json:"run_id"`
	BatchNo     int        `json:"batch_no"`
	Status      string     `json:"status"`
	TotalHosts  int        `json:"total_hosts"`
	Succeeded   int        `json:"succeeded"`
	Failed      int        `json:"failed"`
	StartedAt   *time.Time `json:"started_at,omitempty"`
	CompletedAt *time.Time `json:"completed_at,omitempty"`
	TenantID    string     `json:"tenant_id,omitempty"`
}

// Step is a per-host step execution record. One Step corresponds to one
// action executed on one host within one batch.
type Step struct {
	ID          string     `json:"id"`
	RunID       string     `json:"run_id"`
	BatchID     string     `json:"batch_id"`
	Host        string     `json:"host"`
	StepName    string     `json:"step_name"`
	Action      string     `json:"action"`
	Status      string     `json:"status"`
	ExitCode    *int       `json:"exit_code,omitempty"`
	Stdout      string     `json:"stdout"`
	Stderr      string     `json:"stderr"`
	DurationMs  int        `json:"duration_ms"`
	StartedAt   *time.Time `json:"started_at,omitempty"`
	CompletedAt *time.Time `json:"completed_at,omitempty"`
	TenantID    string     `json:"tenant_id,omitempty"`
}

// Trace is an audit trace record. Traces form a hash chain per run: each
// record's CurrHash depends on PrevHash, making tampering detectable.
type Trace struct {
	ID        string    `json:"id"`
	RunID     string    `json:"run_id"`
	Event     string    `json:"event"`
	Actor     string    `json:"actor"`
	Detail    string    `json:"detail"` // JSON encoded
	PrevHash  string    `json:"prev_hash"`
	CurrHash  string    `json:"curr_hash"`
	Timestamp time.Time `json:"timestamp"`
	TenantID  string    `json:"tenant_id,omitempty"`
}

// Approval is a single approval record within a multi-level approval chain.
type Approval struct {
	ID        string     `json:"id"`
	RunID     string     `json:"run_id"`
	Level     string     `json:"level"`
	Approver  string     `json:"approver"`
	Status    string     `json:"status"`
	Comment   string     `json:"comment"`
	TimeoutAt *time.Time `json:"timeout_at,omitempty"`
	ActedAt   *time.Time `json:"acted_at,omitempty"`
	// PlanHash is the plan artifact the approval attests to. Empty means
	// "legacy record": it still settles/authorises runs regardless of plan,
	// preserving behaviour for rows created before D-1 v2.
	PlanHash string `json:"plan_hash"`
	// Revision is the optimistic-lock version used by the concurrent decision
	// CAS (UpdateApprovalIfPending). It guards against a stale writer
	// overwriting a partial vote that another actor just recorded.
	Revision int64 `json:"revision"`
	// TenantID mirrors the owning run's tenant (see Run.TenantID).
	TenantID string `json:"tenant_id,omitempty"`
}

// MatchesPlan reports whether this approval row authorises the plan version
// identified by planHash, and whether it matched only through the legacy
// (empty PlanHash) rule.
//
// It lives here, next to the struct, because THREE gates depend on it and
// they must never disagree about which approval authorises which revision:
// settlement, the apply gate, and the retry re-plan gate. When a fourth gate
// appears it must call this too rather than re-implement the rule.
//
//   - a row with an empty PlanHash is a pre-binding (legacy) record: it
//     matches any plan, preserving behaviour for databases written before
//     plan binding existed;
//   - a row bound to a revision matches only that exact revision, and never
//     a run whose plan hash is itself empty.
func (a *Approval) MatchesPlan(planHash string) (matched, legacy bool) {
	if a == nil {
		return false, false
	}
	if a.PlanHash == "" {
		return true, true
	}
	return planHash != "" && a.PlanHash == planHash, false
}

// Lock is a mutex lock with a TTL. Locks are scoped (e.g. host:<name>) and
// enforce mutual exclusion across concurrent runs.
type Lock struct {
	ID         string    `json:"id"`
	Scope      string    `json:"scope"`
	Owner      string    `json:"owner"`
	TTLSeconds int       `json:"ttl_seconds"`
	AcquiredAt time.Time `json:"acquired_at"`
	ExpiresAt  time.Time `json:"expires_at"`
}

// Credential is an encrypted credential reference. The plaintext never enters
// the database, logs or trace; only AES-GCM ciphertext is stored.
//
// Tags carries operator metadata (e.g. target/env) as the JSON encoding of a
// map[string]string, or "" when the credential has no tags. The state layer
// stores it verbatim; the credential package owns encode/decode.
type Credential struct {
	ID            string     `json:"id"`
	Name          string     `json:"name"`
	Type          string     `json:"type"`
	EncryptedData []byte     `json:"encrypted_data"`
	CreatedAt     time.Time  `json:"created_at"`
	RotatedAt     *time.Time `json:"rotated_at,omitempty"`
	Tags          string     `json:"tags,omitempty"`
	// TenantID owns the credential. Credentials are tenant-scoped: a secret
	// readable by one tenant must never be resolvable by another.
	TenantID string `json:"tenant_id,omitempty"`
}

// Audit is a high-level audit log entry. Audit entries describe who did what
// to which target and when; they complement the fine-grained Trace chain.
type Audit struct {
	ID     string `json:"id"`
	RunID  string `json:"run_id"`
	Action string `json:"action"`
	Actor  string `json:"actor"`
	Target string `json:"target"`
	Result string `json:"result"`
	// TenantID is carried on the row rather than derived from RunID because
	// audit entries exist without a run (login, config, credential actions —
	// RunID is '' for those), so there is no parent to derive from.
	TenantID string `json:"tenant_id,omitempty"`
	// PrevHash / CurrHash form a GLOBAL chain over the audit log, in
	// (Timestamp, ID) order. Global rather than per-run because a run-less
	// entry is exactly the security-relevant kind and a per-run chain cannot
	// reach it. Empty means "not yet chained"; internal/audit's chain builder
	// fills them, after which the WORM triggers make every other column
	// immutable.
	PrevHash string `json:"prev_hash,omitempty"`
	CurrHash string `json:"curr_hash,omitempty"`
	// Timestamp participates in the chain ORDER, so it is part of the hashed
	// content, not just a sort key.
	Timestamp time.Time `json:"timestamp"`
}

// RunFilter narrows ListRuns results. Empty fields are ignored; non-empty
// fields are combined with AND semantics.
type RunFilter struct {
	Status       string
	WorkflowName string
	TemplateName string
	Creator      string
	IncidentID   string
	// Limit caps the number of returned runs. <= 0 means no cap (callers
	// should set a sane limit to avoid loading the whole table).
	Limit int
	// Offset skips the first Offset matching rows (for keyset-free
	// pagination). Negative values are treated as 0.
	Offset int
	// TenantID restricts results to one owning tenant. Empty means "no
	// tenant predicate" — that is the single-tenant / migration path and is
	// only reachable when multi-tenancy is disabled. internal/tenant always
	// sets it.
	TenantID string
}

// BatchFilter narrows ListBatches results within a run.
type BatchFilter struct {
	RunID    string
	Status   string
	Limit    int
	TenantID string
}

// StepFilter narrows ListSteps results.
type StepFilter struct {
	RunID    string
	BatchID  string
	Host     string
	Status   string
	Limit    int
	TenantID string
}

// TraceFilter narrows ListTraces results.
type TraceFilter struct {
	RunID    string
	Event    string
	Limit    int
	TenantID string
}

// ApprovalFilter narrows ListApprovals results.
type ApprovalFilter struct {
	RunID    string
	Level    string
	Status   string
	Limit    int
	TenantID string
}

// AuditFilter narrows ListAudits results.
type AuditFilter struct {
	RunID  string
	Action string
	Actor  string
	Limit  int
	// Offset skips the first Offset matching rows (timestamp DESC order).
	// Negative values are treated as 0. Use with Limit for pagination.
	//
	// Paging with it is only safe against a read-only table: a row inserted
	// mid-walk shifts every later offset. For a walk that must survive
	// concurrent writers, use ListAuditChainPage.
	Offset int
	// TenantID restricts results to one owning tenant (see RunFilter).
	TenantID string
}

// AuditCursor is a position in the global audit chain order — the total order
// the audit hash chain is built over: ascending by (timestamp, id).
//
// It exists for ListAuditChainPage's keyset paging. An OFFSET would be simpler,
// but ListAudits reads newest-first, so a row inserted while a walk is in
// flight shifts every later offset by one: the walk re-reads a row it already
// sealed and silently skips the oldest one, which for a hash chain means
// writing a row's hash against a predecessor that is no longer its own.
// Comparing against the last row actually read cannot do that — the walk only
// ever moves forward, whatever arrives at the head.
type AuditCursor struct {
	Timestamp time.Time
	ID        string
}

// DefaultAuditChainPageSize is how many rows one ListAuditChainPage call
// returns when the caller does not choose. It bounds what a chain walk holds
// in memory: the walk keeps only the running predecessor hash, so a page of
// rows is the entire footprint regardless of how long the audit log gets.
const DefaultAuditChainPageSize = 1000

// Target is a managed inventory host. It persists across daemon restarts,
// unlike the earlier in-memory TargetService registry.
type Target struct {
	ID            string            `json:"id"`
	Hostname      string            `json:"hostname"` // address: IP or DNS name
	Port          int               `json:"port"`
	ChannelType   string            `json:"channel_type"` // ssh|winrm
	CredentialRef string            `json:"credential_ref,omitempty"`
	Labels        map[string]string `json:"labels,omitempty"`
	GroupID       string            `json:"group_id,omitempty"`
	Status        string            `json:"status"` // active|frozen|retired
	Reachable     bool              `json:"reachable"`
	LastCheckedAt *time.Time        `json:"last_checked_at,omitempty"`
	CreatedAt     time.Time         `json:"created_at"`
	// TenantID owns the target host. Host inventory is tenant-scoped, but
	// deliberately NOT the lock that guards it: `locks` stays global so two
	// tenants changing the same physical host still mutually exclude.
	TenantID string `json:"tenant_id,omitempty"`
}

// InventoryGroup is a hierarchical grouping of targets. Names are unique
// and path-style ("prod/db"); ParentID allows tree structures.
type InventoryGroup struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	ParentID  string    `json:"parent_id,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	// TenantID owns the group. Group names are UNIQUE table-wide, which is
	// intentional: two tenants may not both claim "prod/db" even though
	// neither can see the other's row.
	TenantID string `json:"tenant_id,omitempty"`
}

// ErrDuplicateTarget is wrapped by UpsertTarget when the (hostname, port)
// pair is already owned by a different target ID.
var ErrDuplicateTarget = errors.New("state: target address already in use")

// Target lifecycle statuses. active targets may be selected for changes;
// frozen targets are rejected at change creation AND apply time; retired
// targets are kept for history queries only.
const (
	StatusActive  = "active"
	StatusFrozen  = "frozen"
	StatusRetired = "retired"
)

// TargetFilter narrows ListTargets results.
type TargetFilter struct {
	GroupID string
	Status  string
	// Labels requires EVERY listed label to match exactly (AND semantics).
	Labels map[string]string
	Limit  int
	Offset int
	// TenantID restricts results to one owning tenant (see RunFilter).
	TenantID string
}

// WORMStore is a restricted subset of Store that only allows append-only
// operations on trace records, consistent with Write-Once-Read-Many semantics.
// Use this interface in audit/WORM contexts to prevent accidental or malicious
// modification of trace data. SQLiteStore implements WORMStore implicitly.
type WORMStore interface {
	// Trace append-only operations.
	CreateTrace(ctx context.Context, trace *Trace) error
	GetTrace(ctx context.Context, id string) (*Trace, error)
	ListTraces(ctx context.Context, filter TraceFilter) ([]*Trace, error)

	// Run operations needed for trace context (FK constraint).
	GetRun(ctx context.Context, id string) (*Run, error)
	CreateRun(ctx context.Context, run *Run) error

	// Close releases resources.
	Close() error
}

// Store is the persistence abstraction used by every LEVEE subsystem.
// Implementations must be safe for concurrent use; the SQLite implementation
// achieves this by relying on database/sql's connection pool and serialising
// writes through a single writer connection (WAL mode).
//
// Methods follow a consistent convention:
//   - Create* inserts a new row; the ID must be set by the caller.
//   - Get* returns (nil, nil) when the row does not exist.
//   - Update* overwrites all mutable columns; the ID is used as the key.
//   - List* applies the given filter and returns a slice (possibly empty).
//   - Delete* removes a row by ID and returns nil if it did not exist.
type Store interface {
	// Run CRUD.
	CreateRun(ctx context.Context, run *Run) error
	GetRun(ctx context.Context, id string) (*Run, error)
	UpdateRun(ctx context.Context, run *Run) error
	// UpdateRunStatusIf atomically transitions a run's status from `from`
	// to `to` (WHERE id=? AND status=?), stamping updated_at with the given
	// time. It returns (true, nil) when the transition was applied and
	// (false, nil) when the run does not exist or its current status is not
	// `from` — the row is left untouched in that case. Callers use it to
	// guard state-machine transitions (e.g. only "approved" runs may
	// become "running") against concurrent racers.
	UpdateRunStatusIf(ctx context.Context, id string, from string, to string, updatedAt time.Time) (bool, error)
	// UpdateRunApprovalStatusIf atomically transitions a run's status from
	// `from` to `to` AND sets approval_status, stamping updated_at. It
	// returns (true, nil) when the row matched and was updated, (false, nil)
	// when the run does not exist or its current status is not `from`. It is
	// the single-CAS write behind approval settlement so a settled outcome
	// (status + approval status) is written atomically instead of a read-then
	// full-row UpdateRun that could clobber a concurrent state transition.
	UpdateRunApprovalStatusIf(ctx context.Context, id string, from string, to string, approvalStatus string, updatedAt time.Time) (bool, error)
	// UpdateRunPlan overwrites only the plan artifact (plan_json, plan_hash)
	// on an existing run, touching updated_at. Unlike a full-row UpdateRun it
	// never writes status/approval_status, so persisting a plan can never
	// clobber a concurrent state transition (e.g. settlement or apply that
	// runs between a caller's GetRun and this write).
	UpdateRunPlan(ctx context.Context, id string, planJSON string, planHash string, updatedAt time.Time) error
	// MarkNonTerminalSteps flips every step row of the run whose status
	// is a non-terminal vocabulary (running/pending) to the given
	// terminal marker, and returns the number of rows flipped. It is the
	// defensive terminal-marking pass of the failover takeover: with the
	// current persist-once evidence model it matches 0 rows by
	// construction (step rows only land with terminal statuses), but a
	// future incremental-persistence model cannot silently reintroduce
	// the "step stuck in running" failure mode while this exists.
	MarkNonTerminalSteps(ctx context.Context, runID string, marker string) (int64, error)
	ListRuns(ctx context.Context, filter RunFilter) ([]*Run, error)
	DeleteRun(ctx context.Context, id string) error

	// Batch CRUD.
	CreateBatch(ctx context.Context, batch *Batch) error
	GetBatch(ctx context.Context, id string) (*Batch, error)
	UpdateBatch(ctx context.Context, batch *Batch) error
	ListBatches(ctx context.Context, filter BatchFilter) ([]*Batch, error)
	DeleteBatch(ctx context.Context, id string) error

	// Step CRUD.
	CreateStep(ctx context.Context, step *Step) error
	GetStep(ctx context.Context, id string) (*Step, error)
	UpdateStep(ctx context.Context, step *Step) error
	ListSteps(ctx context.Context, filter StepFilter) ([]*Step, error)
	DeleteStep(ctx context.Context, id string) error

	// Trace CRUD.
	CreateTrace(ctx context.Context, trace *Trace) error
	GetTrace(ctx context.Context, id string) (*Trace, error)
	UpdateTrace(ctx context.Context, trace *Trace) error
	// UpdateTraceChain stamps the chain hashes onto a trace row. It is the
	// trace-side twin of UpdateAuditChain and exists for the same reason: the
	// row is inserted before its position in the chain is known, so the builder
	// writes the hashes afterwards.
	//
	// It writes NOTHING else. UpdateTrace would also work — the WORM trigger
	// compares column values, not the statement — but a whole-row write makes
	// the chain seal the only thing in the codebase that can touch every
	// content column at once, and its safety would rest on the values happening
	// to be identical. Naming the write surface keeps the seal provably
	// chain-only, and keeps it working if the trigger's comparison ever
	// tightens.
	//
	// It returns an error when no row matched — a silent no-op here would
	// produce a chain with a hole in it.
	UpdateTraceChain(ctx context.Context, id string, prevHash string, currHash string) error
	// UpdateTraceChecksum writes checksum into the curr_hash column of the
	// trace identified by id, but only when curr_hash is still empty
	// (WHERE id=? AND curr_hash=''). It exists so the archiver can stamp a
	// WORM content checksum onto pre-existing, unchecksummed traces
	// without ever overwriting an already-computed hash (checksum or chain
	// hash). It returns an error when no row was updated.
	UpdateTraceChecksum(ctx context.Context, id string, checksum string) error
	ListTraces(ctx context.Context, filter TraceFilter) ([]*Trace, error)
	DeleteTrace(ctx context.Context, id string) error

	// Approval CRUD.
	CreateApproval(ctx context.Context, approval *Approval) error
	GetApproval(ctx context.Context, id string) (*Approval, error)
	UpdateApproval(ctx context.Context, approval *Approval) error
	// UpdateApprovalIfPending is a compare-and-set variant of
	// UpdateApproval: it applies the update only when the stored row is
	// still in status "pending" (WHERE id=? AND status='pending'). It
	// returns true when the update was applied and false when the row was
	// concurrently decided by another actor (or does not exist), so the
	// caller can retry or report a conflict without ever overwriting a
	// terminal decision.
	UpdateApprovalIfPending(ctx context.Context, approval *Approval) (bool, error)
	ListApprovals(ctx context.Context, filter ApprovalFilter) ([]*Approval, error)
	DeleteApproval(ctx context.Context, id string) error

	// Lock CRUD.
	CreateLock(ctx context.Context, lock *Lock) error
	GetLock(ctx context.Context, id string) (*Lock, error)
	GetLockByScope(ctx context.Context, scope string) (*Lock, error)
	UpdateLock(ctx context.Context, lock *Lock) error
	// UpdateLockOwnedBy atomically updates the owner (and TTL/acquired/
	// expires) of a lock identified by id, but only when the lock has
	// expired (expires_at <= now). It returns the number of rows
	// affected so callers can detect concurrent races: 0 means another
	// actor won the update and the caller should retry.
	UpdateLockOwnedBy(ctx context.Context, id string, owner string, ttlSeconds int, now time.Time) (int64, error)
	ListLocks(ctx context.Context) ([]*Lock, error)
	DeleteLock(ctx context.Context, id string) error
	// DeleteLockByIDAndOwner deletes the lock identified by id but only
	// when it is still owned by owner. The ownership check and the delete
	// happen inside a single statement so a concurrent ForceAcquire
	// takeover (which reuses the same row id) cannot be deleted by the
	// stale owner: rowsAffected==0 means the lock was taken over by
	// another owner or is already gone.
	DeleteLockByIDAndOwner(ctx context.Context, id string, owner string) (bool, error)
	DeleteExpiredLocks(ctx context.Context, now time.Time) (int64, error)

	// Credential CRUD.
	CreateCredential(ctx context.Context, cred *Credential) error
	GetCredential(ctx context.Context, id string) (*Credential, error)
	GetCredentialByName(ctx context.Context, name string) (*Credential, error)
	UpdateCredential(ctx context.Context, cred *Credential) error
	ListCredentials(ctx context.Context) ([]*Credential, error)
	DeleteCredential(ctx context.Context, id string) error

	// Audit CRUD.
	CreateAudit(ctx context.Context, audit *Audit) error
	GetAudit(ctx context.Context, id string) (*Audit, error)
	// UpdateAuditChain stamps the chain hashes onto an audit row. It is the
	// ONLY update the audit WORM triggers permit (they compare every content
	// column and ignore these two), and it exists for the same reason as
	// UpdateTraceChecksum: the row is inserted before its position in the
	// chain is known, so the builder has to write the hashes afterwards.
	//
	// It writes nothing else, and it returns an error when no row matched —
	// a silent no-op here would produce a chain with a hole in it.
	UpdateAuditChain(ctx context.Context, id string, prevHash string, currHash string) error
	ListAudits(ctx context.Context, filter AuditFilter) ([]*Audit, error)

	// ListAuditChainPage returns up to limit audit rows in ASCENDING chain order
	// — (timestamp, id), the total order the global audit hash chain is built
	// over — starting strictly after the given cursor, and restricted to one
	// tenant when tenantID is set. A nil cursor starts at the oldest row; a page
	// shorter than limit is the end of the chain.
	//
	// This is ListAudits' counterpart for chain work, and the two differ in
	// direction, in paging and in what tenantID means: the chain must be walked
	// oldest-first (each row's hash covers its predecessor's) and must keep
	// advancing past whatever is being appended to the log meanwhile, so it
	// pages by cursor rather than by offset. It is a separate method rather than
	// a filter flag because the read has to be exactly the total order the
	// hashes were computed over — a tie broken differently here and there would
	// make every row after the tie look tampered.
	ListAuditChainPage(ctx context.Context, after *AuditCursor, limit int, tenantID string) ([]*Audit, error)

	// Dispatch assignment CRUD (design-cluster-dispatch.md).
	CreateAssignment(ctx context.Context, a *Assignment) error
	GetAssignment(ctx context.Context, runID string) (*Assignment, error)
	// UpdateAssignmentStateIf is a compare-and-set on (run_id, epoch, state):
	// it applies expected→next only when the row's current state is expected.
	// It reports (true, nil) when the transition was applied and (false, nil)
	// when the row does not exist or its state/epoch no longer matches (a
	// concurrent actor won the race). Callers use it to serialise assignment
	// state transitions without a distributed lock.
	UpdateAssignmentStateIf(ctx context.Context, runID string, epoch int64, expected, next string) (bool, error)
	// UpdateAssignmentState transitions an assignment to next if its current
	// state is one of the active states (pending, executing). It is a
	// takeover-path helper: dispatch's Reassign can reclaim interrupted rows,
	// so the takeover must not CAS against a fixed expected state — instead
	// it stands down when the row is already terminal (done/interrupted).
	// Returns (true, nil) when the transition was applied.
	UpdateAssignmentState(ctx context.Context, runID, next string) (bool, error)
	// Reassign bumps the epoch and resets state to pending for a run, used by
	// the dispatch loop when a worker dies and the run must be given to a
	// fresh node. It returns (true, nil) when the existing row matched
	// (runID, prevEpoch) and was bumped.
	//
	// Callers must only reassign a row whose transition is already over
	// (terminal) or whose owner is known dead: the CAS compares the epoch
	// alone, so an executing row keeps its epoch and would be dragged back to
	// pending. Stale-pending reclaim uses ReclaimAssignment instead.
	Reassign(ctx context.Context, runID string, prevEpoch int64, newNode string) (bool, error)
	// ReclaimAssignment is the stale-assignment counterpart of Reassign: it
	// bumps the epoch and re-points a PENDING assignment at newNode, CASing on
	// (runID, epoch, state=pending). The state guard is what makes reclaim
	// safe where Reassign is not — a worker that claimed the row in the
	// meantime (pending→executing keeps the epoch) wins the race and the
	// reclaim stands down, so a claimed assignment is never dragged back to
	// pending (which would let two nodes execute the same run). The epoch bump
	// is the fencing half: the previous owner's late claim CAS still compares
	// its stale epoch and fails. Returns (true, nil) when the reclaim applied.
	ReclaimAssignment(ctx context.Context, runID string, epoch int64, newNode string) (bool, error)
	// SetAssignmentResult writes the terminal result onto an assignment row
	// identified by (runID, epoch). Best-evidence: a store failure is logged
	// by the caller but never aborts the run's terminal transition.
	SetAssignmentResult(ctx context.Context, runID string, epoch int64, result string) error
	ListAssignments(ctx context.Context, filter AssignmentFilter) ([]*Assignment, error)
	// DeleteAssignment removes the assignment for a run. Idempotent.
	DeleteAssignment(ctx context.Context, runID string) error

	// ListClusterNodes returns every registered cluster node, ordered by ID.
	// Single-node (SQLite) deployments have no cluster_nodes table and
	// simply return (nil, nil).
	ListClusterNodes(ctx context.Context) ([]ClusterNode, error)

	// AssignmentSummary aggregates the run_assignment rows for the cluster
	// status view: counts per state and per-node active load.
	AssignmentSummary(ctx context.Context) (*AssignmentSummary, error)

	// BatchSummary returns the per-batch progress of a run: the batch
	// list (ordered by batch_no) and the batch_no of the first non-terminal
	// batch (the one the executor should resume from, or 0 when all are
	// terminal). Returns (nil, nil, nil) when the run has no batches.
	BatchSummary(ctx context.Context, runID string) (*BatchSummary, error)

	// Inventory: managed target hosts and hierarchical groups.
	UpsertInventoryGroup(ctx context.Context, group *InventoryGroup) error
	GetInventoryGroup(ctx context.Context, id string) (*InventoryGroup, error)
	GetInventoryGroupByName(ctx context.Context, name string) (*InventoryGroup, error)
	ListInventoryGroups(ctx context.Context) ([]*InventoryGroup, error)
	DeleteInventoryGroup(ctx context.Context, id string) error

	// UpsertTarget inserts or updates by ID. The (hostname, port) pair is
	// UNIQUE across the table: inserting a different ID with an address
	// already owned by another row fails with a wrapped ErrDuplicateTarget.
	UpsertTarget(ctx context.Context, target *Target) error
	GetTarget(ctx context.Context, id string) (*Target, error)
	FindTargetByAddress(ctx context.Context, hostname string, port int) (*Target, error)
	ListTargets(ctx context.Context, filter TargetFilter) ([]*Target, error)
	UpdateTargetStatus(ctx context.Context, id string, status string) error
	SetTargetReachability(ctx context.Context, id string, reachable bool, at time.Time) error
	DeleteTarget(ctx context.Context, id string) error
	CountTargetsInGroup(ctx context.Context, groupID string) (int, error)

	// Close releases all underlying resources.
	Close() error
}

// Assignment is the cross-node dispatch assignment for a run
// (design-cluster-dispatch.md). One active assignment per run (run_id PK).
// The epoch increases on every reassignment so a stale scheduler can detect
// that it no longer owns the assignment it is about to write.
type Assignment struct {
	RunID      string
	OwnerNode  string
	Epoch      int64
	State      string // pending | executing | done | interrupted
	Result     string // completed | failed | rolled_back | '' (while not done)
	AssignedAt time.Time
	UpdatedAt  time.Time
	// TenantID mirrors the dispatched run's tenant (see Run.TenantID), so a
	// leader's cross-node dispatch view can be scoped per tenant.
	TenantID string
}

// AssignmentFilter narrows ListAssignments results. Empty fields are ignored;
// non-empty fields combine with AND.
type AssignmentFilter struct {
	RunID     string
	OwnerNode string
	State     string
	// States is the inverse of State: list assignments whose state is NOT in
	// this set. Used to find "active" assignments (state NOT IN (done,
	// interrupted)). Ignored when empty.
	ExcludeStates []string
	Limit         int
	// TenantID restricts results to one owning tenant (see RunFilter).
	TenantID string
}

// ClusterNode is the persisted view of a cluster member
// (design-cluster-dispatch.md). Single-node (SQLite) deployments have no
// cluster_nodes table; callers get (nil, nil) from ListClusterNodes.
type ClusterNode struct {
	ID            string
	Address       string
	Role          string // master | worker
	Status        string // active | offline
	LastHeartbeat time.Time
	JoinedAt      time.Time
}

// AssignmentSummary aggregates run_assignment rows for the cluster status view.
type AssignmentSummary struct {
	Counts      map[string]int // state -> count
	NodeLoad    map[string]int // owner_node -> active count
	TotalActive int
}

// BatchSummary is the per-batch progress of a run (cluster v2 observability).
//
// The json tags are load-bearing, not decoration: this struct is marshalled
// verbatim by the REST gateway (`GET /api/v1/system/batch-status`), and without
// them the wire shape is Go's PascalCase field names while BOTH consumers —
// web/src/api's BatchSummaryDTO and the two views that read it — spell the keys
// snake_case. Nothing failed loudly; the panels were just永远空. Tags align the
// wire with the declared DTO.
type BatchSummary struct {
	Batches        []BatchProgress `json:"batches"`
	CurrentBatchNo int             `json:"current_batch_no"` // first non-terminal batch_no, or 0 when all terminal
	TotalBatches   int             `json:"total_batches"`
	DoneBatches    int             `json:"done_batches"`
}

// BatchProgress is a single batch's execution status.
//
// Status carries the values the execution path writes — completed | failed |
// rolled_back (see internal/wiring/persist.go) — plus the older spellings
// declared as BatchState* constants. See batchDoneStates for what counts as
// done, and internal/wiring/batch_summary_seam_test.go for the seam test that
// keeps writer and reader using one vocabulary.
type BatchProgress struct {
	BatchNo    int    `json:"batch_no"` // 1-based sequence number
	Status     string `json:"status"`
	TotalHosts int    `json:"total_hosts"`
	Succeeded  int    `json:"succeeded"`
	Failed     int    `json:"failed"`
}

// batchDoneStates are the batch statuses that count as fully completed for
// the cluster v2 observability view. Note: "failed" and "interrupted" are
// NOT terminal in the execution sense — they need re-execution/resumption, so
// they are excluded here. CurrentBatchNo points at the first batch NOT in
// this set, i.e. the one the executor should resume from.
//
// BatchStateCompleted is in this set because that is the string the execution
// path actually writes (internal/wiring/persist.go). Before it was added, this
// map only held BatchStateDone — and nothing ever wrote "done": every batch row
// the engine persisted said "completed". The visible consequence was operator
// facing: DoneBatches stayed 0 on a fully finished run, and CurrentBatchNo kept
// naming batch #1 as "the one to resume from". Both spellings are listed so a
// row written under either is read correctly.
var batchDoneStates = map[string]bool{
	BatchStateCompleted: true,
	BatchStateDone:      true,
}

const (
	// Batch states (run-level batches within a run).
	//
	// Writer and reader must use THESE constants rather than literals: the
	// execution path writes `completed` / `failed` / `rolled_back` (see
	// internal/wiring/persist.go), while this list also carries the older
	// `done` / `interrupted` / `pending` / `running` spelling that the schema
	// comment documents. A literal on one side and a constant on the other is
	// exactly how the two vocabularies stopped describing the same row.
	BatchStateCompleted   = "completed"
	BatchStateRolledBack  = "rolled_back"
	BatchStatePending     = "pending"
	BatchStateRunning     = "running"
	BatchStateDone        = "done"
	BatchStateFailed      = "failed"
	BatchStateInterrupted = "interrupted"

	// Step states (one row per (run, batch, host, step)).
	//
	// These are the strings the steps.status schema comment documents and the
	// execution path writes. They are deliberately NOT the same set as
	// BatchState*: a step's success is `success`, a batch's is `completed`, and
	// reusing one name for the other is how the two vocabularies stop
	// describing the rows they claim to (see the BatchState* note above for
	// that exact failure, already once).
	//
	// Pending and running have no writers today: the schema comment lists them
	// because the state machine's shape anticipates them, and internal/lock's
	// busy-host probe reads them. They stay in the set so a future writer
	// cannot invent a third spelling for a state that is already named.
	StepStatusPending = "pending"
	StepStatusRunning = "running"
	StepStatusSuccess = "success"
	StepStatusFailed  = "failed"
	StepStatusSkipped = "skipped"

	// Run approval states: the approval verdict carried on a RUN row, settled
	// by the approval service independently of the run's execution status.
	//
	// This set is exactly what the writers write — verified by sweeping the writers
	// rather than by copying the schema comment, which is not the same list.
	// The schema comment on runs.approval_status reads
	// `pending|approved|rejected|timeout|skipped`, but `timeout` and `skipped`
	// belong to the APPROVALS table's status column (documented one column
	// over) and have no writer here; the run-side initial value is set by
	// CreateChange / InstantiateTemplate / CloneChange / cmd new. Both columns
	// are called "status" in prose, which is exactly why the constants are
	// named apart — see the BatchState* note above for what happens when one
	// vocabulary is assumed to cover the other's rows.
	ApprovalStatusPending  = "pending"
	ApprovalStatusApproved = "approved"
	ApprovalStatusRejected = "rejected"

	// Assignment states.
	AssignStatePending         = "pending"
	AssignmentStateExecuting   = "executing"
	AssignmentStateDone        = "done"
	AssignmentStateInterrupted = "interrupted"

	// Assignment results (mirrors the run status vocabulary).
	AssignResultCompleted  = "completed"
	AssignResultFailed     = "failed"
	AssignResultRolledBack = "rolled_back"
)
