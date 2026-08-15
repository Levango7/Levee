package state

import (
	"context"
	"time"
)

// Run is a top-level change execution unit. One Run owns multiple Batches
// executed serially; each Batch owns multiple Steps executed concurrently
// across hosts.
type Run struct {
	ID             string    `json:"id"`
	WorkflowName   string    `json:"workflow_name"`
	TemplateName   string    `json:"template_name"`
	Params         string    `json:"params"` // JSON encoded
	PlanHash       string    `json:"plan_hash"`
	Status         string    `json:"status"`
	ApprovalStatus string    `json:"approval_status"`
	ApprovalLevel  string    `json:"approval_level"`
	CreatedAt      time.Time `json:"created_at"`
	UpdatedAt      time.Time `json:"updated_at"`
	Creator        string    `json:"creator"`
	IncidentID     string    `json:"incident_id"`
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
type Credential struct {
	ID            string     `json:"id"`
	Name          string     `json:"name"`
	Type          string     `json:"type"`
	EncryptedData []byte     `json:"encrypted_data"`
	CreatedAt     time.Time  `json:"created_at"`
	RotatedAt     *time.Time `json:"rotated_at,omitempty"`
}

// Audit is a high-level audit log entry. Audit entries describe who did what
// to which target and when; they complement the fine-grained Trace chain.
type Audit struct {
	ID        string    `json:"id"`
	RunID     string    `json:"run_id"`
	Action    string    `json:"action"`
	Actor     string    `json:"actor"`
	Target    string    `json:"target"`
	Result    string    `json:"result"`
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
}

// BatchFilter narrows ListBatches results within a run.
type BatchFilter struct {
	RunID  string
	Status string
	Limit  int
}

// StepFilter narrows ListSteps results.
type StepFilter struct {
	RunID   string
	BatchID string
	Host    string
	Status  string
	Limit   int
}

// TraceFilter narrows ListTraces results.
type TraceFilter struct {
	RunID string
	Event string
	Limit int
}

// ApprovalFilter narrows ListApprovals results.
type ApprovalFilter struct {
	RunID  string
	Level  string
	Status string
	Limit  int
}

// AuditFilter narrows ListAudits results.
type AuditFilter struct {
	RunID  string
	Action string
	Actor  string
	Limit  int
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
	ListTraces(ctx context.Context, filter TraceFilter) ([]*Trace, error)
	DeleteTrace(ctx context.Context, id string) error

	// Approval CRUD.
	CreateApproval(ctx context.Context, approval *Approval) error
	GetApproval(ctx context.Context, id string) (*Approval, error)
	UpdateApproval(ctx context.Context, approval *Approval) error
	ListApprovals(ctx context.Context, filter ApprovalFilter) ([]*Approval, error)
	DeleteApproval(ctx context.Context, id string) error

	// Lock CRUD.
	CreateLock(ctx context.Context, lock *Lock) error
	GetLock(ctx context.Context, id string) (*Lock, error)
	GetLockByScope(ctx context.Context, scope string) (*Lock, error)
	UpdateLock(ctx context.Context, lock *Lock) error
	ListLocks(ctx context.Context) ([]*Lock, error)
	DeleteLock(ctx context.Context, id string) error
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
	ListAudits(ctx context.Context, filter AuditFilter) ([]*Audit, error)

	// Close releases all underlying resources.
	Close() error
}
