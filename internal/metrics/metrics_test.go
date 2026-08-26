package metrics

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNewPrecreatesFixedLabelSets(t *testing.T) {
	m := New()

	for _, status := range changeStatuses {
		assert.Zero(t, m.ChangesTotal(status), "status %s should start at 0", status)
	}
	assert.Zero(t, m.GatesTotal(GateResultPass))
	assert.Zero(t, m.GatesTotal(GateResultFail))
	assert.Zero(t, m.ApprovalsTotal(ApprovalActionApprove))
	assert.Zero(t, m.ApprovalsTotal(ApprovalActionReject))
	assert.Zero(t, m.ApprovalsTotal(ApprovalActionTimeout))
	assert.Zero(t, m.BackupsTotal(BackupResultOK))
	assert.Zero(t, m.BackupsTotal(BackupResultFail))
	assert.Zero(t, m.RollbacksTotal())
	assert.Zero(t, m.LocksHeld())
	assert.Zero(t, m.BatchDurationCount())
	assert.InDelta(t, 0, m.BatchDurationSecondsSum(), 1e-9)
}

func TestIncChange(t *testing.T) {
	m := New()
	m.IncChange(StatusCreated)
	m.IncChange(StatusCreated)
	m.IncChange(StatusApproved)
	m.IncChange(StatusRunning)
	m.IncChange(StatusSucceeded)
	m.IncChange(StatusFailed)
	m.IncChange(StatusRolledBack)

	assert.Equal(t, int64(2), m.ChangesTotal(StatusCreated))
	assert.Equal(t, int64(1), m.ChangesTotal(StatusApproved))
	assert.Equal(t, int64(1), m.ChangesTotal(StatusRunning))
	assert.Equal(t, int64(1), m.ChangesTotal(StatusSucceeded))
	assert.Equal(t, int64(1), m.ChangesTotal(StatusFailed))
	assert.Equal(t, int64(1), m.ChangesTotal(StatusRolledBack))
	assert.Zero(t, m.ChangesTotal("unknown-status"), "unset labels read as 0")
}

func TestObserveBatchDuration(t *testing.T) {
	m := New()
	m.ObserveBatchDuration(1500 * time.Millisecond)
	m.ObserveBatchDuration(500 * time.Millisecond)

	assert.Equal(t, int64(2), m.BatchDurationCount())
	assert.InDelta(t, 2.0, m.BatchDurationSecondsSum(), 1e-9)
}

func TestIncGate(t *testing.T) {
	m := New()
	m.IncGate(GateResultPass)
	m.IncGate(GateResultPass)
	m.IncGate(GateResultPass)
	m.IncGate(GateResultFail)

	assert.Equal(t, int64(3), m.GatesTotal(GateResultPass))
	assert.Equal(t, int64(1), m.GatesTotal(GateResultFail))
}

func TestIncApproval(t *testing.T) {
	m := New()
	m.IncApproval(ApprovalActionApprove)
	m.IncApproval(ApprovalActionReject)
	m.IncApproval(ApprovalActionTimeout)
	m.IncApproval(ApprovalActionTimeout)

	assert.Equal(t, int64(1), m.ApprovalsTotal(ApprovalActionApprove))
	assert.Equal(t, int64(1), m.ApprovalsTotal(ApprovalActionReject))
	assert.Equal(t, int64(2), m.ApprovalsTotal(ApprovalActionTimeout))
}

func TestIncChannelAcquire(t *testing.T) {
	m := New()
	m.IncChannelAcquire(ChannelSSH, "ok")
	m.IncChannelAcquire(ChannelSSH, "ok")
	m.IncChannelAcquire(ChannelSSH, "fail")
	m.IncChannelAcquire(ChannelWinRM, "ok")

	assert.Equal(t, int64(2), m.ChannelAcquireTotal(ChannelSSH, "ok"))
	assert.Equal(t, int64(1), m.ChannelAcquireTotal(ChannelSSH, "fail"))
	assert.Equal(t, int64(1), m.ChannelAcquireTotal(ChannelWinRM, "ok"))
	assert.Zero(t, m.ChannelAcquireTotal(ChannelWinRM, "fail"))
}

func TestLocksHeldGauge(t *testing.T) {
	m := New()
	m.IncLocksHeld()
	m.IncLocksHeld()
	m.IncLocksHeld()
	m.DecLocksHeld()
	assert.Equal(t, int64(2), m.LocksHeld())

	m.SetLocksHeld(7)
	assert.Equal(t, int64(7), m.LocksHeld())
}

func TestIncRollbackAndBackupAndAlerts(t *testing.T) {
	m := New()
	m.IncRollback()
	m.IncRollback()
	m.IncBackup(BackupResultOK)
	m.IncBackup(BackupResultFail)
	m.IncAlertsProcessed("prometheus")
	m.IncAlertsProcessed("prometheus")
	m.IncAlertsProcessed("custom")

	assert.Equal(t, int64(2), m.RollbacksTotal())
	assert.Equal(t, int64(1), m.BackupsTotal(BackupResultOK))
	assert.Equal(t, int64(1), m.BackupsTotal(BackupResultFail))
	assert.Equal(t, int64(2), m.AlertsProcessedTotal("prometheus"))
	assert.Equal(t, int64(1), m.AlertsProcessedTotal("custom"))
}

func TestHandlerContentTypeAndFormat(t *testing.T) {
	m := New()
	m.IncChange(StatusCreated)
	m.ObserveBatchDuration(2500 * time.Millisecond)
	m.IncGate(GateResultPass)
	m.IncApproval(ApprovalActionApprove)
	m.IncChannelAcquire(ChannelSSH, "ok")
	m.IncLocksHeld()
	m.IncRollback()
	m.IncBackup(BackupResultOK)
	m.IncAlertsProcessed("prometheus")

	rec := httptest.NewRecorder()
	m.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))

	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, ExposedContentType, rec.Header().Get("Content-Type"))

	body := rec.Body.String()

	// HELP/TYPE annotations for every family.
	expectedAnnotations := []string{
		"# HELP levee_changes_total ", "# TYPE levee_changes_total counter",
		"# HELP levee_batch_duration_seconds_sum ", "# TYPE levee_batch_duration_seconds_sum counter",
		"# HELP levee_batch_duration_seconds_count ", "# TYPE levee_batch_duration_seconds_count counter",
		"# HELP levee_gates_total ", "# TYPE levee_gates_total counter",
		"# HELP levee_approvals_total ", "# TYPE levee_approvals_total counter",
		"# HELP levee_channel_acquire_total ", "# TYPE levee_channel_acquire_total counter",
		"# HELP levee_locks_held ", "# TYPE levee_locks_held gauge",
		"# HELP levee_rollbacks_total ", "# TYPE levee_rollbacks_total counter",
		"# HELP levee_backup_total ", "# TYPE levee_backup_total counter",
		"# HELP levee_alerts_processed_total ", "# TYPE levee_alerts_processed_total counter",
	}
	for _, want := range expectedAnnotations {
		assert.Contains(t, body, want)
	}

	// Sample lines with expected values.
	expectedSamples := []string{
		`levee_changes_total{status="created"} 1`,
		`levee_batch_duration_seconds_sum 2.5`,
		`levee_batch_duration_seconds_count 1`,
		`levee_gates_total{result="pass"} 1`,
		`levee_approvals_total{action="approve"} 1`,
		`levee_channel_acquire_total{channel="ssh",result="ok"} 1`,
		`levee_locks_held 1`,
		`levee_rollbacks_total 1`,
		`levee_backup_total{result="ok"} 1`,
		`levee_alerts_processed_total{source="prometheus"} 1`,
	}
	for _, want := range expectedSamples {
		assert.Contains(t, body, want)
	}

	// Every fixed change status must be present even at zero.
	for _, status := range changeStatuses {
		assert.Contains(t, body, `levee_changes_total{status="`+status+`"}`)
	}
}

func TestRenderIsDeterministic(t *testing.T) {
	m := New()
	m.IncChange(StatusFailed)
	m.IncChannelAcquire(ChannelWinRM, "ok")
	m.IncChannelAcquire(ChannelSSH, "fail")
	m.IncAlertsProcessed("b-source")
	m.IncAlertsProcessed("a-source")

	var first, second strings.Builder
	require.NoError(t, m.Render(&first))
	require.NoError(t, m.Render(&second))
	assert.Equal(t, first.String(), second.String())

	// Matrix rows sort by channel then result.
	body := first.String()
	sshIdx := strings.Index(body, `levee_channel_acquire_total{channel="ssh",result="fail"}`)
	winrmIdx := strings.Index(body, `levee_channel_acquire_total{channel="winrm",result="ok"}`)
	assert.NotEqual(t, -1, sshIdx)
	assert.NotEqual(t, -1, winrmIdx)
	assert.Less(t, sshIdx, winrmIdx)
}

func TestFormatLabelValue(t *testing.T) {
	m := New()
	m.IncAlertsProcessed(`weird"source\with` + "\n" + `newline`)

	var b strings.Builder
	require.NoError(t, m.Render(&b))
	assert.Contains(t, b.String(),
		`levee_alerts_processed_total{source="weird\"source\\with\nnewline"} 1`)
}

func TestFormatFloat(t *testing.T) {
	assert.Equal(t, "0", formatFloat(0))
	assert.Equal(t, "2.5", formatFloat(2.5))
	assert.Equal(t, "0.000001", formatFloat(1e-6))
	assert.Equal(t, "1234567.891", formatFloat(1234567.891))
}

func TestLabelPairs(t *testing.T) {
	assert.Equal(t, "", labelPairs())
	assert.Equal(t, `{a="1"}`, labelPairs("a", "1"))
	assert.Equal(t, `{a="1",b="2"}`, labelPairs("a", "1", "b", "2"))
}

// errWriter fails on every write, exercising Render's error path.
type errWriter struct{}

func (errWriter) Write(p []byte) (int, error) { return 0, errors.New("boom") }

func TestRenderPropagatesWriteError(t *testing.T) {
	m := New()
	err := m.Render(errWriter{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "metrics: render exposition")
	assert.Contains(t, err.Error(), "boom")
}

func TestDefaultSingleton(t *testing.T) {
	require.NotNil(t, Default)
	require.NotNil(t, Default.Handler())

	rec := httptest.NewRecorder()
	Default.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), "# TYPE levee_changes_total counter")
}

func TestRegistry(t *testing.T) {
	r := NewRegistry()
	m1, m2 := New(), New()

	require.NoError(t, r.Register("engine", m1))
	require.NoError(t, r.Register("gateway", m2))

	got, ok := r.Get("engine")
	require.True(t, ok)
	assert.Same(t, m1, got)

	_, ok = r.Get("missing")
	assert.False(t, ok)

	assert.Equal(t, []string{"engine", "gateway"}, r.Names())

	// Duplicate name is rejected.
	err := r.Register("engine", New())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "duplicate")

	// nil collector is rejected.
	err = r.Register("nil-collector", nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "nil")

	assert.True(t, r.Unregister("engine"))
	assert.False(t, r.Unregister("engine"), "second unregister finds nothing")
	assert.Equal(t, []string{"gateway"}, r.Names())
}

// TestConcurrentUse hammers every collector method from many goroutines
// to prove the atomics + mutex design is race-free. Run with -race.
func TestConcurrentUse(t *testing.T) {
	m := New()
	const goroutines = 16
	const iterations = 500

	var wg sync.WaitGroup
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < iterations; j++ {
				m.IncChange(StatusRunning)
				m.ObserveBatchDuration(time.Millisecond)
				m.IncGate(GateResultPass)
				m.IncApproval(ApprovalActionApprove)
				m.IncChannelAcquire(ChannelSSH, "ok")
				m.IncLocksHeld()
				m.DecLocksHeld()
				m.IncRollback()
				m.IncBackup(BackupResultOK)
				m.IncAlertsProcessed("prometheus")
				// Concurrent scrape while writers are active.
				_ = m.Render(&strings.Builder{})
			}
		}()
	}
	wg.Wait()

	total := int64(goroutines * iterations)
	assert.Equal(t, total, m.ChangesTotal(StatusRunning))
	assert.Equal(t, total, m.BatchDurationCount())
	assert.InDelta(t, float64(total)/1000, m.BatchDurationSecondsSum(), 1e-6)
	assert.Equal(t, total, m.GatesTotal(GateResultPass))
	assert.Equal(t, total, m.ApprovalsTotal(ApprovalActionApprove))
	assert.Equal(t, total, m.ChannelAcquireTotal(ChannelSSH, "ok"))
	assert.Zero(t, m.LocksHeld(), "inc/dec must balance")
	assert.Equal(t, total, m.RollbacksTotal())
	assert.Equal(t, total, m.BackupsTotal(BackupResultOK))
	assert.Equal(t, total, m.AlertsProcessedTotal("prometheus"))
}

// TestConcurrentNewLabelGrowth exercises the map-growth path of the
// labeled and matrix counters under contention.
func TestConcurrentNewLabelGrowth(t *testing.T) {
	m := New()
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				m.IncAlertsProcessed("src-a")
				m.IncAlertsProcessed("src-b")
				m.IncChannelAcquire(ChannelSSH, "ok")
				m.IncChannelAcquire(ChannelWinRM, "timeout")
			}
		}(i)
	}
	wg.Wait()

	assert.Equal(t, int64(1600), m.AlertsProcessedTotal("src-a"))
	assert.Equal(t, int64(1600), m.AlertsProcessedTotal("src-b"))
	assert.Equal(t, int64(1600), m.ChannelAcquireTotal(ChannelSSH, "ok"))
	assert.Equal(t, int64(1600), m.ChannelAcquireTotal(ChannelWinRM, "timeout"))
}
