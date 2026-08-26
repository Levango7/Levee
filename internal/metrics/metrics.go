// Package metrics provides a lightweight, dependency-free metrics
// collector for LEVEE self-observability. It aggregates change
// lifecycle counters, gauges and a simplified histogram (sum + count)
// and renders them in the Prometheus text exposition format (version
// 0.0.4), so `levee serve` can expose a scrape endpoint without
// pulling in the full Prometheus client library.
//
// All exported types are safe for concurrent use: counters are backed
// by sync/atomic values and dynamic label sets grow under a mutex.
package metrics

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Label values exported alongside the metric families below. Call
// sites should reference these constants instead of hard-coding the
// strings.
const (
	// Change lifecycle statuses for levee_changes_total.
	StatusCreated    = "created"
	StatusApproved   = "approved"
	StatusRunning    = "running"
	StatusSucceeded  = "succeeded"
	StatusFailed     = "failed"
	StatusRolledBack = "rolled_back"

	// Gate results for levee_gates_total.
	GateResultPass = "pass"
	GateResultFail = "fail"

	// Approval actions for levee_approvals_total.
	ApprovalActionApprove = "approve"
	ApprovalActionReject  = "reject"
	ApprovalActionTimeout = "timeout"

	// Channel names for levee_channel_acquire_total.
	ChannelSSH   = "ssh"
	ChannelWinRM = "winrm"

	// Backup results for levee_backup_total.
	BackupResultOK   = "ok"
	BackupResultFail = "fail"
)

// ExposedContentType is the Content-Type served by Handler, as required
// by the Prometheus text exposition format version 0.0.4.
const ExposedContentType = "text/plain; version=0.0.4; charset=utf-8"

// Metric family names. Collected here so exposition code and tests can
// reference them by name.
const (
	familyChanges         = "levee_changes_total"
	familyBatchDurSum     = "levee_batch_duration_seconds_sum"
	familyBatchDurCount   = "levee_batch_duration_seconds_count"
	familyGates           = "levee_gates_total"
	familyApprovals       = "levee_approvals_total"
	familyChannelAcquire  = "levee_channel_acquire_total"
	familyLocksHeld       = "levee_locks_held"
	familyRollbacks       = "levee_rollbacks_total"
	familyBackups         = "levee_backup_total"
	familyAlertsProcessed = "levee_alerts_processed_total"
)

// changeStatuses lists the lifecycle statuses always exported for
// levee_changes_total, even while their counter is still zero, so
// dashboards can rely on a stable label set.
var changeStatuses = []string{
	StatusCreated, StatusApproved, StatusRunning,
	StatusSucceeded, StatusFailed, StatusRolledBack,
}

// Default is the process-wide collector instance. LEVEE subsystems
// record into it and the serve command exposes it at /metrics.
var Default = New()

// labeledValue is one exposition row of a single-label counter family.
type labeledValue struct {
	label string
	value int64
}

// labeledCounters is a set of int64 counters keyed by one label value.
// The zero value is not usable; construct instances with
// newLabeledCounters. Concurrent use is safe: map growth is guarded by
// a mutex while increments go through atomics.
type labeledCounters struct {
	mu       sync.RWMutex
	counters map[string]*atomic.Int64
}

// newLabeledCounters pre-creates a counter for every preset label so
// the exposition output contains the full expected label set from the
// first scrape.
func newLabeledCounters(presets ...string) *labeledCounters {
	c := &labeledCounters{counters: make(map[string]*atomic.Int64, len(presets))}
	for _, p := range presets {
		c.counterFor(p)
	}
	return c
}

// counterFor returns (creating on first use) the counter for key.
func (c *labeledCounters) counterFor(key string) *atomic.Int64 {
	c.mu.RLock()
	v, ok := c.counters[key]
	c.mu.RUnlock()
	if ok {
		return v
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if v, ok := c.counters[key]; ok {
		return v
	}
	v = &atomic.Int64{}
	c.counters[key] = v
	return v
}

// inc increments the counter for key by one.
func (c *labeledCounters) inc(key string) { c.counterFor(key).Add(1) }

// value returns the current counter value for key (0 when absent).
func (c *labeledCounters) value(key string) int64 {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if v, ok := c.counters[key]; ok {
		return v.Load()
	}
	return 0
}

// snapshot returns all counters sorted by label so exposition output is
// deterministic across scrapes.
func (c *labeledCounters) snapshot() []labeledValue {
	c.mu.RLock()
	defer c.mu.RUnlock()
	out := make([]labeledValue, 0, len(c.counters))
	for k, v := range c.counters {
		out = append(out, labeledValue{label: k, value: v.Load()})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].label < out[j].label })
	return out
}

// matrixKey identifies one cell of a two-label counter family.
type matrixKey struct {
	first  string
	second string
}

// matrixValue is one exposition row of a two-label counter family.
type matrixValue struct {
	first  string
	second string
	value  int64
}

// matrixCounters is a set of int64 counters keyed by a pair of label
// values (e.g. channel + acquire result). Concurrent use is safe.
type matrixCounters struct {
	mu       sync.RWMutex
	counters map[matrixKey]*atomic.Int64
}

func newMatrixCounters() *matrixCounters {
	return &matrixCounters{counters: make(map[matrixKey]*atomic.Int64)}
}

func (c *matrixCounters) counterFor(first, second string) *atomic.Int64 {
	key := matrixKey{first: first, second: second}
	c.mu.RLock()
	v, ok := c.counters[key]
	c.mu.RUnlock()
	if ok {
		return v
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if v, ok := c.counters[key]; ok {
		return v
	}
	v = &atomic.Int64{}
	c.counters[key] = v
	return v
}

func (c *matrixCounters) inc(first, second string) { c.counterFor(first, second).Add(1) }

func (c *matrixCounters) value(first, second string) int64 {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if v, ok := c.counters[matrixKey{first: first, second: second}]; ok {
		return v.Load()
	}
	return 0
}

// snapshot returns all counters sorted by both labels for deterministic
// exposition output.
func (c *matrixCounters) snapshot() []matrixValue {
	c.mu.RLock()
	defer c.mu.RUnlock()
	out := make([]matrixValue, 0, len(c.counters))
	for k, v := range c.counters {
		out = append(out, matrixValue{first: k.first, second: k.second, value: v.Load()})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].first != out[j].first {
			return out[i].first < out[j].first
		}
		return out[i].second < out[j].second
	})
	return out
}

// Metrics aggregates all LEVEE self-observability counters and gauges.
// Construct instances with New; all methods are safe for concurrent
// use.
type Metrics struct {
	changes        *labeledCounters
	gates          *labeledCounters
	approvals      *labeledCounters
	backups        *labeledCounters
	alerts         *labeledCounters
	channelAcquire *matrixCounters

	// Batch duration is a simplified histogram: only sum and count are
	// tracked, exported as the conventional _sum/_count series.
	batchSumNanos atomic.Int64
	batchCount    atomic.Int64

	locksHeld atomic.Int64
	rollbacks atomic.Int64
}

// New constructs an empty collector with the fixed label sets
// pre-created.
func New() *Metrics {
	return &Metrics{
		changes:        newLabeledCounters(changeStatuses...),
		gates:          newLabeledCounters(GateResultPass, GateResultFail),
		approvals:      newLabeledCounters(ApprovalActionApprove, ApprovalActionReject, ApprovalActionTimeout),
		backups:        newLabeledCounters(BackupResultOK, BackupResultFail),
		alerts:         newLabeledCounters(),
		channelAcquire: newMatrixCounters(),
	}
}

// IncChange records one change lifecycle transition (e.g.
// metrics.StatusCreated). Unknown statuses are recorded as-is rather
// than dropped, so instrumentation bugs surface in the exposition
// instead of vanishing.
func (m *Metrics) IncChange(status string) { m.changes.inc(status) }

// ObserveBatchDuration records the wall-clock duration of one batch
// execution into the simplified sum+count histogram.
func (m *Metrics) ObserveBatchDuration(d time.Duration) {
	m.batchSumNanos.Add(int64(d))
	m.batchCount.Add(1)
}

// IncGate records one verification gate outcome; result should be
// GateResultPass or GateResultFail.
func (m *Metrics) IncGate(result string) { m.gates.inc(result) }

// IncApproval records one approval decision; action should be
// ApprovalActionApprove, ApprovalActionReject or
// ApprovalActionTimeout.
func (m *Metrics) IncApproval(action string) { m.approvals.inc(action) }

// IncChannelAcquire records one channel acquisition attempt for the
// given channel (e.g. ChannelSSH) and outcome result.
func (m *Metrics) IncChannelAcquire(channel, result string) {
	m.channelAcquire.inc(channel, result)
}

// IncLocksHeld raises the held-lock gauge by one.
func (m *Metrics) IncLocksHeld() { m.locksHeld.Add(1) }

// DecLocksHeld lowers the held-lock gauge by one. Callers must keep
// Inc/Dec balanced; the gauge can go negative if they do not, which is
// intentionally visible in the exposition as an instrumentation bug.
func (m *Metrics) DecLocksHeld() { m.locksHeld.Add(-1) }

// SetLocksHeld overwrites the held-lock gauge with an absolute value,
// for reconciliation-style updates.
func (m *Metrics) SetLocksHeld(n int64) { m.locksHeld.Store(n) }

// LocksHeld returns the current held-lock gauge value.
func (m *Metrics) LocksHeld() int64 { return m.locksHeld.Load() }

// IncRollback records one rollback run.
func (m *Metrics) IncRollback() { m.rollbacks.Add(1) }

// IncBackup records one backup attempt; result should be BackupResultOK
// or BackupResultFail.
func (m *Metrics) IncBackup(result string) { m.backups.inc(result) }

// IncAlertsProcessed records one processed alert from the given source
// (e.g. "prometheus").
func (m *Metrics) IncAlertsProcessed(source string) { m.alerts.inc(source) }

// ChangesTotal returns the counter value for one change status.
func (m *Metrics) ChangesTotal(status string) int64 { return m.changes.value(status) }

// BatchDurationSecondsSum returns the accumulated batch duration in
// seconds.
func (m *Metrics) BatchDurationSecondsSum() float64 {
	return float64(m.batchSumNanos.Load()) / float64(time.Second)
}

// BatchDurationCount returns the number of observed batch executions.
func (m *Metrics) BatchDurationCount() int64 { return m.batchCount.Load() }

// GatesTotal returns the counter value for one gate result.
func (m *Metrics) GatesTotal(result string) int64 { return m.gates.value(result) }

// ApprovalsTotal returns the counter value for one approval action.
func (m *Metrics) ApprovalsTotal(action string) int64 { return m.approvals.value(action) }

// ChannelAcquireTotal returns the counter value for one channel/result
// pair.
func (m *Metrics) ChannelAcquireTotal(channel, result string) int64 {
	return m.channelAcquire.value(channel, result)
}

// RollbacksTotal returns the rollback counter value.
func (m *Metrics) RollbacksTotal() int64 { return m.rollbacks.Load() }

// BackupsTotal returns the counter value for one backup result.
func (m *Metrics) BackupsTotal(result string) int64 { return m.backups.value(result) }

// AlertsProcessedTotal returns the counter value for one alert source.
func (m *Metrics) AlertsProcessedTotal(source string) int64 { return m.alerts.value(source) }

// Handler returns an http.Handler that serves all collected metrics in
// the Prometheus text exposition format (version 0.0.4), including
// # HELP and # TYPE annotation lines. Register it on the serve
// gateway, typically at /metrics.
func (m *Metrics) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", ExposedContentType)
		// A write failure means the client went away; the headers are
		// already sent so there is nothing else to report.
		_ = m.Render(w)
	})
}

// Render writes the full exposition document to w. It returns an error
// only when writing to w fails. The method is deliberately not named
// WriteTo so it cannot be mistaken for an io.WriterTo implementation.
func (m *Metrics) Render(w io.Writer) error {
	var b strings.Builder
	m.render(&b)
	if _, err := io.WriteString(w, b.String()); err != nil {
		return fmt.Errorf("metrics: render exposition: %w", err)
	}
	return nil
}

// render builds the exposition document into b.
func (m *Metrics) render(b *strings.Builder) {
	writeCounterFamily(b, familyChanges,
		"Total number of changes observed by LEVEE, partitioned by lifecycle status.",
		"status", m.changes.snapshot())

	fmt.Fprintf(b, "# HELP %s Total time spent executing batches, in seconds.\n", familyBatchDurSum)
	fmt.Fprintf(b, "# TYPE %s counter\n", familyBatchDurSum)
	fmt.Fprintf(b, "%s %s\n", familyBatchDurSum, formatFloat(m.BatchDurationSecondsSum()))
	fmt.Fprintf(b, "# HELP %s Number of batch executions observed by LEVEE.\n", familyBatchDurCount)
	fmt.Fprintf(b, "# TYPE %s counter\n", familyBatchDurCount)
	fmt.Fprintf(b, "%s %d\n", familyBatchDurCount, m.BatchDurationCount())

	writeCounterFamily(b, familyGates,
		"Total number of verification gates evaluated by LEVEE, partitioned by result.",
		"result", m.gates.snapshot())

	writeCounterFamily(b, familyApprovals,
		"Total number of approval decisions processed by LEVEE, partitioned by action.",
		"action", m.approvals.snapshot())

	fmt.Fprintf(b, "# HELP %s Total number of channel acquisition attempts, partitioned by channel and result.\n", familyChannelAcquire)
	fmt.Fprintf(b, "# TYPE %s counter\n", familyChannelAcquire)
	for _, s := range m.channelAcquire.snapshot() {
		fmt.Fprintf(b, "%s%s %d\n", familyChannelAcquire,
			labelPairs("channel", s.first, "result", s.second), s.value)
	}

	writeGauge(b, familyLocksHeld,
		"Number of target locks currently held by LEVEE.",
		m.LocksHeld())

	fmt.Fprintf(b, "# HELP %s Total number of rollback runs executed by LEVEE.\n", familyRollbacks)
	fmt.Fprintf(b, "# TYPE %s counter\n", familyRollbacks)
	fmt.Fprintf(b, "%s %d\n", familyRollbacks, m.RollbacksTotal())

	writeCounterFamily(b, familyBackups,
		"Total number of backup attempts performed by LEVEE, partitioned by result.",
		"result", m.backups.snapshot())

	writeCounterFamily(b, familyAlertsProcessed,
		"Total number of alerts processed by LEVEE, partitioned by source.",
		"source", m.alerts.snapshot())
}

// writeCounterFamily renders one single-label counter family with its
// HELP/TYPE annotations.
func writeCounterFamily(b *strings.Builder, name, help, labelName string, samples []labeledValue) {
	fmt.Fprintf(b, "# HELP %s %s\n", name, help)
	fmt.Fprintf(b, "# TYPE %s counter\n", name)
	for _, s := range samples {
		fmt.Fprintf(b, "%s%s %d\n", name, labelPairs(labelName, s.label), s.value)
	}
}

// writeGauge renders one unlabeled gauge with its HELP/TYPE
// annotations.
func writeGauge(b *strings.Builder, name, help string, value int64) {
	fmt.Fprintf(b, "# HELP %s %s\n", name, help)
	fmt.Fprintf(b, "# TYPE %s gauge\n", name)
	fmt.Fprintf(b, "%s %d\n", name, value)
}

// labelPairs formats an even list of key/value pairs as a Prometheus
// label set, e.g. `{"channel="ssh",result="ok"}`. Label values are
// escaped per the text format. An empty list renders as "".
func labelPairs(pairs ...string) string {
	if len(pairs) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteByte('{')
	for i := 0; i+1 < len(pairs); i += 2 {
		if i > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, "%s=\"%s\"", pairs[i], formatLabelValue(pairs[i+1]))
	}
	b.WriteByte('}')
	return b.String()
}

// formatLabelValue escapes a label value per the Prometheus text
// exposition format: backslash, double quote and line feed sequences
// are escaped.
func formatLabelValue(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `"`, `\"`)
	s = strings.ReplaceAll(s, "\n", `\n`)
	return s
}

// formatFloat renders a float for exposition using the shortest
// round-trippable decimal representation (no exponent), which matches
// how existing Prometheus client libraries format sums.
func formatFloat(v float64) string {
	return strconv.FormatFloat(v, 'f', -1, 64)
}

// Registry keeps named Metrics instances so subsystems or tests can
// maintain isolated collector sets. The zero value is not usable;
// construct instances with NewRegistry. Concurrent use is safe.
type Registry struct {
	mu      sync.RWMutex
	metrics map[string]*Metrics
}

// NewRegistry constructs an empty registry.
func NewRegistry() *Registry {
	return &Registry{metrics: make(map[string]*Metrics)}
}

// Register adds m under name. Registering a nil collector or a name
// that is already taken returns an error.
func (r *Registry) Register(name string, m *Metrics) error {
	if m == nil {
		return errors.New("metrics: cannot register nil Metrics")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.metrics[name]; exists {
		return fmt.Errorf("metrics: duplicate registry entry %q", name)
	}
	r.metrics[name] = m
	return nil
}

// Get returns the collector registered under name.
func (r *Registry) Get(name string) (*Metrics, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	m, ok := r.metrics[name]
	return m, ok
}

// Unregister removes the collector registered under name, reporting
// whether an entry was removed.
func (r *Registry) Unregister(name string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.metrics[name]; !ok {
		return false
	}
	delete(r.metrics, name)
	return true
}

// Names returns the registered collector names in sorted order.
func (r *Registry) Names() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	names := make([]string, 0, len(r.metrics))
	for name := range r.metrics {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}
