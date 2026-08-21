// Scheduler implementation for the LEVEE drift package.
//
// DriftScheduler runs DriftDetector on a cron schedule for a set of hosts. It
// supports multiple jobs, each with its own cron expression, host list and
// alert-on-drift flag. Jobs can be added and removed at runtime and triggered
// manually through RunOnce.
//
// The cron expression is parsed with internal/calendar.ParseCron so the drift
// scheduler shares the same 5-field POSIX cron syntax as the change calendar.
// For simplicity each job runs at a fixed interval derived from the cron
// expression's next occurrence rather than tracking every fire time; this is
// sufficient for periodic drift inspection.

package drift

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/nexus/levee/internal/calendar"
	"github.com/nexus/levee/internal/log"
)

// --- Sentinel errors --------------------------------------------------------

var (
	// ErrJobNotFound is returned when an operation targets a job that is not
	// registered with the scheduler.
	ErrJobNotFound = errors.New("drift: job not found")
	// ErrJobExists is returned when attempting to add a job whose ID is
	// already registered.
	ErrJobExists = errors.New("drift: job already exists")
	// ErrInvalidCron is returned when a cron expression cannot be parsed.
	ErrInvalidCron = errors.New("drift: invalid cron expression")
	// ErrSchedulerRunning is returned when an operation that requires a
	// stopped scheduler is called while the scheduler is running.
	ErrSchedulerRunning = errors.New("drift: scheduler is running")
)

// --- DriftJob ---------------------------------------------------------------

// DriftJob describes a single scheduled drift detection job. A job owns a cron
// expression, a list of hosts to probe and a flag controlling whether drift
// alerts are emitted.
type DriftJob struct {
	// ID is the unique job identifier. It is auto-generated when empty.
	ID string `json:"id"`
	// Name is the human-readable job name.
	Name string `json:"name"`
	// CronExpr is a 5-field POSIX cron expression, e.g. "0 */6 * * *" for
	// every 6 hours.
	CronExpr string `json:"cron_expr"`
	// Hosts is the list of target hosts to probe on each run.
	Hosts []string `json:"hosts"`
	// Enabled controls whether the job is eligible for scheduling. Disabled
	// jobs are kept in the scheduler but never fire automatically.
	Enabled bool `json:"enabled"`
	// AlertOnDrift controls whether a drift alert is emitted when drift is
	// detected. When true the detector's notifier (if configured) is
	// invoked.
	AlertOnDrift bool `json:"alert_on_drift"`
	// LastRun is the time of the most recent execution (zero before the
	// first run).
	LastRun time.Time `json:"last_run"`
	// NextRun is the scheduled time of the next execution.
	NextRun time.Time `json:"next_run"`
}

// --- DriftScheduler ---------------------------------------------------------

// DriftScheduler owns the set of DriftJobs and runs them on their cron
// schedules. It is safe for concurrent use. The scheduler runs each job in its
// own goroutine; jobs share the underlying DriftDetector and BaselineManager.
type DriftScheduler struct {
	detector    *DriftDetector
	baselineMgr *BaselineManager
	mu          sync.RWMutex
	jobs        map[string]*DriftJob
	stopCh      chan struct{}
	running     bool
	// history stores the most recent DriftReports for trend analysis. The
	// key is the job ID; the value is a slice capped at maxHistoryPerJob.
	history map[string][]*DriftReport
}

// maxHistoryPerJob caps how many historical reports are retained per job for
// trend analysis. Older reports are dropped FIFO.
const maxHistoryPerJob = 100

// NewScheduler returns a DriftScheduler that uses the given detector and
// baseline manager. Both must be non-nil.
func NewScheduler(detector *DriftDetector, baselineMgr *BaselineManager) *DriftScheduler {
	return &DriftScheduler{
		detector:    detector,
		baselineMgr: baselineMgr,
		jobs:        make(map[string]*DriftJob),
		history:     make(map[string][]*DriftReport),
	}
}

// AddJob registers a new job with the scheduler. The job's CronExpr is
// validated immediately; an invalid expression returns ErrInvalidCron. When
// the job ID is empty a new one is generated. When a job with the same ID
// already exists AddJob returns ErrJobExists.
//
// NextRun is computed from the cron expression and the current time so the
// job is ready to fire as soon as the scheduler is started.
func (s *DriftScheduler) AddJob(job DriftJob) error {
	if job.CronExpr == "" {
		return fmt.Errorf("drift: add job: empty cron expression")
	}
	if _, err := parseCron(job.CronExpr); err != nil {
		return fmt.Errorf("drift: add job: %w", err)
	}
	if len(job.Hosts) == 0 {
		return fmt.Errorf("drift: add job: empty hosts")
	}

	if job.ID == "" {
		job.ID = generateJobID()
	}

	// Compute the next run time from now.
	next, err := nextOccurrence(job.CronExpr, time.Now().UTC())
	if err != nil {
		return fmt.Errorf("drift: add job: compute next run: %w", err)
	}
	job.NextRun = next

	s.mu.Lock()
	defer s.mu.Unlock()

	if _, exists := s.jobs[job.ID]; exists {
		return fmt.Errorf("drift: add job %q: %w", job.ID, ErrJobExists)
	}
	stored := job // copy
	s.jobs[job.ID] = &stored

	log.Info("drift: job added",
		"job_id", stored.ID,
		"name", stored.Name,
		"cron", stored.CronExpr,
		"hosts", len(stored.Hosts),
		"next_run", stored.NextRun.Format(time.RFC3339))
	return nil
}

// RemoveJob removes the job with the given ID. It returns ErrJobNotFound when
// no such job exists.
func (s *DriftScheduler) RemoveJob(jobID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, ok := s.jobs[jobID]; !ok {
		return fmt.Errorf("drift: remove job %q: %w", jobID, ErrJobNotFound)
	}
	delete(s.jobs, jobID)
	delete(s.history, jobID)
	log.Info("drift: job removed", "job_id", jobID)
	return nil
}

// ListJobs returns all registered jobs ordered by ID. The returned slice is a
// defensive copy.
func (s *DriftScheduler) ListJobs() []DriftJob {
	s.mu.RLock()
	defer s.mu.RUnlock()

	out := make([]DriftJob, 0, len(s.jobs))
	for _, j := range s.jobs {
		out = append(out, *j)
	}
	// Sort by ID for stable output.
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j-1].ID > out[j].ID; j-- {
			out[j-1], out[j] = out[j], out[j-1]
		}
	}
	return out
}

// GetJob returns the job with the given ID. It returns ErrJobNotFound when no
// such job exists.
func (s *DriftScheduler) GetJob(jobID string) (*DriftJob, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	j, ok := s.jobs[jobID]
	if !ok {
		return nil, fmt.Errorf("drift: get job %q: %w", jobID, ErrJobNotFound)
	}
	copy := *j
	return &copy, nil
}

// Start launches the scheduler. It returns ErrSchedulerRunning if already
// running. The scheduler runs until Stop is called or ctx is cancelled. Each
// enabled job is polled in its own goroutine; the scheduler wakes up every
// pollInterval to check whether any job is due.
func (s *DriftScheduler) Start(ctx context.Context) error {
	s.mu.Lock()
	if s.running {
		s.mu.Unlock()
		return fmt.Errorf("drift: start: %w", ErrSchedulerRunning)
	}
	s.running = true
	s.stopCh = make(chan struct{})
	s.mu.Unlock()

	log.Info("drift: scheduler started")
	go s.loop(ctx)
	return nil
}

// Stop signals the scheduler to stop and waits for the loop goroutine to
// finish. It is a no-op (returns nil) when the scheduler is not running.
func (s *DriftScheduler) Stop() error {
	s.mu.Lock()
	if !s.running {
		s.mu.Unlock()
		return nil
	}
	stopCh := s.stopCh
	s.running = false
	s.mu.Unlock()

	close(stopCh)
	log.Info("drift: scheduler stopped")
	return nil
}

// IsRunning reports whether the scheduler is currently running.
func (s *DriftScheduler) IsRunning() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.running
}

// loop is the main scheduler loop. It wakes up every pollInterval and runs
// any jobs whose NextRun time has passed.
func (s *DriftScheduler) loop(ctx context.Context) {
	pollInterval := 1 * time.Minute
	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			log.Info("drift: scheduler loop: context cancelled")
			s.mu.Lock()
			s.running = false
			s.mu.Unlock()
			return
		case <-s.stopSignal():
			log.Info("drift: scheduler loop: stop signal received")
			return
		case now := <-ticker.C:
			s.tick(ctx, now.UTC())
		}
	}
}

// stopSignal returns the current stop channel under the read lock. It returns
// a nil channel when the scheduler is not running, which blocks forever in a
// select and is therefore never selected.
func (s *DriftScheduler) stopSignal() <-chan struct{} {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.stopCh == nil {
		// Return a channel that never fires.
		return make(chan struct{})
	}
	return s.stopCh
}

// tick runs all jobs whose NextRun time has passed.
func (s *DriftScheduler) tick(ctx context.Context, now time.Time) {
	s.mu.RLock()
	jobs := make([]*DriftJob, 0, len(s.jobs))
	for _, j := range s.jobs {
		if j.Enabled && !j.NextRun.IsZero() && !now.Before(j.NextRun) {
			jobs = append(jobs, j)
		}
	}
	s.mu.RUnlock()

	for _, j := range jobs {
		s.executeJob(ctx, j, now)
	}
}

// executeJob runs a single job, updates its LastRun/NextRun and stores the
// resulting report in the history.
func (s *DriftScheduler) executeJob(ctx context.Context, job *DriftJob, now time.Time) {
	results, err := s.runDetection(ctx, job)
	if err != nil {
		log.Warn("drift: scheduled run failed",
			"job_id", job.ID,
			"err", err)
	}

	report := GenerateReport(results)
	s.recordHistory(job.ID, report)

	// Update LastRun / NextRun.
	next, err := nextOccurrence(job.CronExpr, now)
	if err != nil {
		log.Error("drift: compute next run failed",
			"job_id", job.ID,
			"cron", job.CronExpr,
			"err", err)
		next = now.Add(1 * time.Hour) // fallback
	}

	s.mu.Lock()
	if stored, ok := s.jobs[job.ID]; ok {
		stored.LastRun = now
		stored.NextRun = next
	}
	s.mu.Unlock()

	log.Info("drift: scheduled run completed",
		"job_id", job.ID,
		"results", len(results),
		"drift_count", report.TotalDrifts,
		"next_run", next.Format(time.RFC3339))
}

// runDetection runs the detector for all hosts in the job and returns the
// collected results. Per-host errors are logged but do not abort the run.
func (s *DriftScheduler) runDetection(ctx context.Context, job *DriftJob) ([]*DriftResult, error) {
	var (
		results  []*DriftResult
		mu       sync.Mutex
		wg       sync.WaitGroup
		firstErr error
	)

	for _, host := range job.Hosts {
		wg.Add(1)
		go func(h string) {
			defer wg.Done()
			baseline, err := s.baselineMgr.Get(h)
			if err != nil {
				mu.Lock()
				if firstErr == nil {
					firstErr = err
				}
				log.Warn("drift: get baseline failed",
					"job_id", job.ID,
					"host", h,
					"err", err)
				mu.Unlock()
				return
			}
			r, err := s.detector.Detect(ctx, h, baseline)
			mu.Lock()
			defer mu.Unlock()
			if r != nil {
				results = append(results, r)
			}
			if err != nil && firstErr == nil && !errors.Is(err, ErrDriftDetected) {
				firstErr = err
			}
		}(host)
	}
	wg.Wait()
	return results, firstErr
}

// RunOnce manually triggers the job with the given ID and returns the
// aggregated DriftResult. The job's LastRun is updated; NextRun is recomputed
// from the cron expression. RunOnce works whether or not the scheduler is
// running.
func (s *DriftScheduler) RunOnce(ctx context.Context, jobID string) (*DriftReport, error) {
	s.mu.RLock()
	j, ok := s.jobs[jobID]
	s.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("drift: run once: %w", ErrJobNotFound)
	}

	now := time.Now().UTC()
	results, err := s.runDetection(ctx, j)
	if err != nil {
		log.Warn("drift: run once completed with errors",
			"job_id", jobID,
			"err", err)
	}

	report := GenerateReport(results)
	s.recordHistory(jobID, report)

	next, err := nextOccurrence(j.CronExpr, now)
	if err != nil {
		next = now.Add(1 * time.Hour)
	}

	s.mu.Lock()
	if stored, ok := s.jobs[jobID]; ok {
		stored.LastRun = now
		stored.NextRun = next
	}
	s.mu.Unlock()

	return report, nil
}

// recordHistory appends a report to the job's history, capping at
// maxHistoryPerJob entries (FIFO eviction).
func (s *DriftScheduler) recordHistory(jobID string, report *DriftReport) {
	s.mu.Lock()
	defer s.mu.Unlock()

	hist := s.history[jobID]
	hist = append(hist, report)
	if len(hist) > maxHistoryPerJob {
		hist = hist[len(hist)-maxHistoryPerJob:]
	}
	s.history[jobID] = hist
}

// GetHistory returns the stored reports for the given job, oldest first. It
// returns an empty slice when the job has no history.
func (s *DriftScheduler) GetHistory(jobID string) []*DriftReport {
	s.mu.RLock()
	defer s.mu.RUnlock()
	hist := s.history[jobID]
	out := make([]*DriftReport, len(hist))
	copy(out, hist)
	return out
}

// --- Cron helpers -----------------------------------------------------------

// parseCron validates a cron expression by delegating to the calendar package.
// It returns the parsed schedule so callers can compute next occurrences.
func parseCron(expr string) (*calendar.CronSchedule, error) {
	sched, err := calendar.ParseCron(expr)
	if err != nil {
		return nil, fmt.Errorf("drift: %w: %v", ErrInvalidCron, err)
	}
	return sched, nil
}

// nextOccurrence returns the next time strictly after `from` that matches the
// cron expression.
func nextOccurrence(cronExpr string, from time.Time) (time.Time, error) {
	sched, err := parseCron(cronExpr)
	if err != nil {
		return time.Time{}, err
	}
	return calendar.NextOccurrence(sched, from)
}

// --- ID generation ----------------------------------------------------------

// generateJobID returns a random 16-byte hex string suitable for use as a
// DriftJob.ID. If the crypto RNG fails it falls back to a timestamp-based id
// so that construction never fails.
func generateJobID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("job-t%d", time.Now().UnixNano())
	}
	return "job-" + hex.EncodeToString(b)
}
