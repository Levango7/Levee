package drift

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// --- NewScheduler ----------------------------------------------------------

func TestNewScheduler(t *testing.T) {
	d := NewDetector(&mockStateProber{})
	bm := NewBaselineManager()
	s := NewScheduler(d, bm)
	assert.NotNil(t, s)
	assert.Empty(t, s.ListJobs())
}

// --- AddJob ----------------------------------------------------------------

func TestAddJob(t *testing.T) {
	d := NewDetector(&mockStateProber{})
	bm := NewBaselineManager()
	s := NewScheduler(d, bm)

	job := DriftJob{
		Name:     "daily-check",
		CronExpr: "0 2 * * *",
		Hosts:    []string{"web-01", "web-02"},
		Enabled:  true,
	}

	err := s.AddJob(job)
	require.NoError(t, err)

	// The ID is generated inside AddJob; retrieve it via ListJobs.
	jobs := s.ListJobs()
	require.Len(t, jobs, 1)
	assert.NotEmpty(t, jobs[0].ID)

	got, err := s.GetJob(jobs[0].ID)
	require.NoError(t, err)
	assert.Equal(t, "daily-check", got.Name)
	assert.Equal(t, "0 2 * * *", got.CronExpr)
	assert.Len(t, got.Hosts, 2)
	assert.False(t, got.NextRun.IsZero())
}

func TestAddJob_AutoID(t *testing.T) {
	d := NewDetector(&mockStateProber{})
	bm := NewBaselineManager()
	s := NewScheduler(d, bm)

	job := DriftJob{
		Name:     "test",
		CronExpr: "0 * * * *",
		Hosts:    []string{"web-01"},
	}
	err := s.AddJob(job)
	require.NoError(t, err)

	jobs := s.ListJobs()
	require.Len(t, jobs, 1)
	assert.NotEmpty(t, jobs[0].ID)
}

func TestAddJob_DuplicateID(t *testing.T) {
	d := NewDetector(&mockStateProber{})
	bm := NewBaselineManager()
	s := NewScheduler(d, bm)

	job := DriftJob{
		ID:       "job-custom",
		Name:     "test",
		CronExpr: "0 * * * *",
		Hosts:    []string{"web-01"},
	}
	err := s.AddJob(job)
	require.NoError(t, err)

	err = s.AddJob(job)
	assert.ErrorIs(t, err, ErrJobExists)
}

func TestAddJob_InvalidCron(t *testing.T) {
	d := NewDetector(&mockStateProber{})
	bm := NewBaselineManager()
	s := NewScheduler(d, bm)

	job := DriftJob{
		Name:     "test",
		CronExpr: "invalid",
		Hosts:    []string{"web-01"},
	}
	err := s.AddJob(job)
	assert.Error(t, err)
}

func TestAddJob_EmptyCron(t *testing.T) {
	d := NewDetector(&mockStateProber{})
	bm := NewBaselineManager()
	s := NewScheduler(d, bm)

	job := DriftJob{
		Name:     "test",
		CronExpr: "",
		Hosts:    []string{"web-01"},
	}
	err := s.AddJob(job)
	assert.Error(t, err)
}

func TestAddJob_EmptyHosts(t *testing.T) {
	d := NewDetector(&mockStateProber{})
	bm := NewBaselineManager()
	s := NewScheduler(d, bm)

	job := DriftJob{
		Name:     "test",
		CronExpr: "0 * * * *",
		Hosts:    nil,
	}
	err := s.AddJob(job)
	assert.Error(t, err)
}

// --- RemoveJob -------------------------------------------------------------

func TestRemoveJob(t *testing.T) {
	d := NewDetector(&mockStateProber{})
	bm := NewBaselineManager()
	s := NewScheduler(d, bm)

	job := DriftJob{
		Name:     "test",
		CronExpr: "0 * * * *",
		Hosts:    []string{"web-01"},
	}
	err := s.AddJob(job)
	require.NoError(t, err)

	jobs := s.ListJobs()
	require.Len(t, jobs, 1)
	jobID := jobs[0].ID

	err = s.RemoveJob(jobID)
	require.NoError(t, err)

	_, err = s.GetJob(jobID)
	assert.ErrorIs(t, err, ErrJobNotFound)
}

func TestRemoveJob_NotFound(t *testing.T) {
	d := NewDetector(&mockStateProber{})
	bm := NewBaselineManager()
	s := NewScheduler(d, bm)

	err := s.RemoveJob("nonexistent")
	assert.ErrorIs(t, err, ErrJobNotFound)
}

// --- ListJobs --------------------------------------------------------------

func TestListJobs(t *testing.T) {
	d := NewDetector(&mockStateProber{})
	bm := NewBaselineManager()
	s := NewScheduler(d, bm)

	for i := 0; i < 3; i++ {
		job := DriftJob{
			Name:     "test",
			CronExpr: "0 * * * *",
			Hosts:    []string{"web-01"},
		}
		err := s.AddJob(job)
		require.NoError(t, err)
	}

	jobs := s.ListJobs()
	assert.Len(t, jobs, 3)
}

func TestListJobs_Sorted(t *testing.T) {
	d := NewDetector(&mockStateProber{})
	bm := NewBaselineManager()
	s := NewScheduler(d, bm)

	// Add jobs with known IDs.
	for _, id := range []string{"job-c", "job-a", "job-b"} {
		job := DriftJob{
			ID:       id,
			Name:     "test",
			CronExpr: "0 * * * *",
			Hosts:    []string{"web-01"},
		}
		err := s.AddJob(job)
		require.NoError(t, err)
	}

	jobs := s.ListJobs()
	assert.Equal(t, "job-a", jobs[0].ID)
	assert.Equal(t, "job-b", jobs[1].ID)
	assert.Equal(t, "job-c", jobs[2].ID)
}

// --- GetJob ----------------------------------------------------------------

func TestGetJob_NotFound(t *testing.T) {
	d := NewDetector(&mockStateProber{})
	bm := NewBaselineManager()
	s := NewScheduler(d, bm)

	_, err := s.GetJob("nonexistent")
	assert.ErrorIs(t, err, ErrJobNotFound)
}

// --- RunOnce ---------------------------------------------------------------

func TestRunOnce(t *testing.T) {
	// Set up a baseline for the host.
	bm := NewBaselineManager()
	items := []BaselineItem{
		{CheckName: "a", Type: CheckTypeFile, Path: "/a", ExpectedValue: "1"},
	}
	_, err := bm.GenerateFromSnapshot("web-01", "run-1", items)
	require.NoError(t, err)

	prober := &mockStateProber{}
	d := NewDetector(prober)
	s := NewScheduler(d, bm)

	jobID := addTestJob(s, DriftJob{
		Name:     "test",
		CronExpr: "0 * * * *",
		Hosts:    []string{"web-01"},
	})

	report, err := s.RunOnce(context.Background(), jobID)
	require.NoError(t, err)
	assert.NotNil(t, report)
	assert.Equal(t, 1, report.TotalHosts)
	assert.Equal(t, 0, report.TotalDrifts) // no drift
}


func TestRunOnce_WithDrift(t *testing.T) {
	bm := NewBaselineManager()
	items := []BaselineItem{
		{CheckName: "a", Type: CheckTypeFile, Path: "/a", ExpectedValue: "1"},
	}
	_, err := bm.GenerateFromSnapshot("web-01", "run-1", items)
	require.NoError(t, err)

	prober := &mockStateProber{
		items: []StateItem{
			{CheckName: "a", ActualValue: "changed", ExpectedValue: "1"},
		},
	}
	d := NewDetector(prober)
	s := NewScheduler(d, bm)

	jobID := addTestJob(s, DriftJob{
		Name:     "test",
		CronExpr: "0 * * * *",
		Hosts:    []string{"web-01"},
	})

	report, err := s.RunOnce(context.Background(), jobID)
	// RunOnce returns nil error even on drift (drift is in the report).
	require.NoError(t, err)
	assert.NotNil(t, report)
	assert.Equal(t, 1, report.TotalDrifts)
}

func TestRunOnce_JobNotFound(t *testing.T) {
	d := NewDetector(&mockStateProber{})
	bm := NewBaselineManager()
	s := NewScheduler(d, bm)

	_, err := s.RunOnce(context.Background(), "nonexistent")
	assert.ErrorIs(t, err, ErrJobNotFound)
}

func TestRunOnce_NoBaseline(t *testing.T) {
	d := NewDetector(&mockStateProber{})
	bm := NewBaselineManager()
	s := NewScheduler(d, bm)

	jobID := addTestJob(s, DriftJob{
		Name:     "test",
		CronExpr: "0 * * * *",
		Hosts:    []string{"web-01"},
	})

	// No baseline set for web-01; RunOnce should still return a report
	// (with no results).
	report, err := s.RunOnce(context.Background(), jobID)
	require.NoError(t, err)
	assert.NotNil(t, report)
}

// --- RunOnce updates LastRun/NextRun ---------------------------------------

func TestRunOnce_UpdatesTimestamps(t *testing.T) {
	bm := NewBaselineManager()
	items := []BaselineItem{{CheckName: "a", Type: CheckTypeFile, Path: "/a", ExpectedValue: "1"}}
	_, err := bm.GenerateFromSnapshot("web-01", "run-1", items)
	require.NoError(t, err)

	d := NewDetector(&mockStateProber{})
	s := NewScheduler(d, bm)

	jobID := addTestJob(s, DriftJob{
		Name:     "test",
		CronExpr: "0 * * * *",
		Hosts:    []string{"web-01"},
	})

	before, _ := s.GetJob(jobID)
	assert.True(t, before.LastRun.IsZero())

	_, err = s.RunOnce(context.Background(), jobID)
	require.NoError(t, err)

	after, _ := s.GetJob(jobID)
	assert.False(t, after.LastRun.IsZero())
	assert.False(t, after.NextRun.IsZero())
}

// --- History ---------------------------------------------------------------

func TestGetHistory(t *testing.T) {
	bm := NewBaselineManager()
	items := []BaselineItem{{CheckName: "a", Type: CheckTypeFile, Path: "/a", ExpectedValue: "1"}}
	_, err := bm.GenerateFromSnapshot("web-01", "run-1", items)
	require.NoError(t, err)

	d := NewDetector(&mockStateProber{})
	s := NewScheduler(d, bm)

	jobID := addTestJob(s, DriftJob{
		Name:     "test",
		CronExpr: "0 * * * *",
		Hosts:    []string{"web-01"},
	})

	// Initially no history.
	hist := s.GetHistory(jobID)
	assert.Empty(t, hist)

	// After running once, history should have one entry.
	_, err = s.RunOnce(context.Background(), jobID)
	require.NoError(t, err)

	hist = s.GetHistory(jobID)
	assert.Len(t, hist, 1)

	// Run again.
	_, err = s.RunOnce(context.Background(), jobID)
	require.NoError(t, err)

	hist = s.GetHistory(jobID)
	assert.Len(t, hist, 2)
}

// addTestJob is a helper that adds a job to the scheduler and returns the
// generated job ID. It panics on error (test-only helper).
func addTestJob(s *DriftScheduler, job DriftJob) string {
	if err := s.AddJob(job); err != nil {
		panic(err)
	}
	jobs := s.ListJobs()
	if len(jobs) == 0 {
		panic("no jobs after add")
	}
	return jobs[len(jobs)-1].ID
}

// --- Start / Stop ----------------------------------------------------------

func TestStartStop(t *testing.T) {
	d := NewDetector(&mockStateProber{})
	bm := NewBaselineManager()
	s := NewScheduler(d, bm)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	err := s.Start(ctx)
	require.NoError(t, err)
	assert.True(t, s.IsRunning())

	// Starting again should fail.
	err = s.Start(ctx)
	assert.ErrorIs(t, err, ErrSchedulerRunning)

	err = s.Stop()
	require.NoError(t, err)
	assert.False(t, s.IsRunning())
}

func TestStop_NotRunning(t *testing.T) {
	d := NewDetector(&mockStateProber{})
	bm := NewBaselineManager()
	s := NewScheduler(d, bm)

	// Stop when not running should be a no-op.
	err := s.Stop()
	require.NoError(t, err)
}

func TestStart_CancelContext(t *testing.T) {
	d := NewDetector(&mockStateProber{})
	bm := NewBaselineManager()
	s := NewScheduler(d, bm)

	ctx, cancel := context.WithCancel(context.Background())

	err := s.Start(ctx)
	require.NoError(t, err)

	// Cancel the context; the scheduler should stop.
	cancel()
	time.Sleep(100 * time.Millisecond)
	assert.False(t, s.IsRunning())
}

// --- parseCron / nextOccurrence --------------------------------------------

func TestParseCron(t *testing.T) {
	tests := []struct {
		expr string
		ok   bool
	}{
		{"0 * * * *", true},
		{"0 2 * * *", true},
		{"*/5 * * * *", true},
		{"0 0 1 * *", true},
		{"0 0 * * 0", true},
		{"invalid", false},
		{"* * * * * *", false}, // 6 fields
		{"* * *", false},       // 3 fields
	}

	for _, tt := range tests {
		_, err := parseCron(tt.expr)
		if tt.ok {
			assert.NoError(t, err, "expected %q to parse", tt.expr)
		} else {
			assert.Error(t, err, "expected %q to fail", tt.expr)
		}
	}
}

func TestNextOccurrence(t *testing.T) {
	now := time.Date(2026, 8, 16, 10, 30, 0, 0, time.UTC)

	next, err := nextOccurrence("0 * * * *", now)
	require.NoError(t, err)
	assert.Equal(t, 11, next.Hour())
	assert.Equal(t, 0, next.Minute())

	next2, err := nextOccurrence("0 2 * * *", now)
	require.NoError(t, err)
	assert.Equal(t, 2, next2.Hour())
	assert.Equal(t, 17, next2.Day()) // next day
}

// --- Error wrapping --------------------------------------------------------

func TestSchedulerErrors_Wrapping(t *testing.T) {
	d := NewDetector(&mockStateProber{})
	bm := NewBaselineManager()
	s := NewScheduler(d, bm)

	_, err := s.GetJob("nonexistent")
	assert.True(t, errors.Is(err, ErrJobNotFound))

	err = s.RemoveJob("nonexistent")
	assert.True(t, errors.Is(err, ErrJobNotFound))
}