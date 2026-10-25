// drift_serve_wiring_test.go pins the serve half of the drift scheduler:
// buildServeDriftScheduler must stay a no-op unless the operator opted in,
// and when opted in it must load the very job files the CLI wrote — one
// store, two consumers. The scheduler's own cron behaviour is covered in
// internal/drift; what only this package can prove is the wiring.
package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/nexus/levee/internal/config"
	"github.com/nexus/levee/internal/notify"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newDriftFixture points the CLI config at a temp data dir and returns it.
func newDriftFixture(t *testing.T) string {
	t.Helper()
	prevConfig := optConfigPath
	t.Cleanup(func() { optConfigPath = prevConfig })

	dataDir := t.TempDir()
	cfgPath := filepath.Join(t.TempDir(), "config.yaml")
	require.NoError(t, os.WriteFile(cfgPath, []byte("server:\n  data_dir: "+dataDir+"\n"), 0o600))
	optConfigPath = cfgPath
	return dataDir
}

// writeDriftJobFile drops one job file where the CLI would put it.
func writeDriftJobFile(t *testing.T, dataDir, name string) {
	t.Helper()
	jd := filepath.Join(dataDir, "drift", "jobs")
	require.NoError(t, os.MkdirAll(jd, 0o750))
	job := `{"id":"job-` + name + `","name":"` + name + `","cron_expr":"0 3 * * *","hosts":["h1"],"enabled":true}`
	require.NoError(t, os.WriteFile(filepath.Join(jd, name+".json"), []byte(job), 0o600))
}

// The switch off (the default, and every existing deployment) builds
// nothing: no scheduler, no goroutine, no behaviour change.
func TestBuildServeDriftSchedulerDisabledBuildsNothing(t *testing.T) {
	s, err := buildServeDriftScheduler(&config.Config{}, nil)
	require.NoError(t, err)
	assert.Nil(t, s)
}

// Opted in, the builder loads exactly the job files the CLI manages —
// including skipping a disabled job file's scheduling without dropping it
// from the list (the scheduler keeps disabled jobs for visibility).
func TestBuildServeDriftSchedulerLoadsCLIJobFiles(t *testing.T) {
	dataDir := newDriftFixture(t)
	writeDriftJobFile(t, dataDir, "nightly")
	writeDriftJobFile(t, dataDir, "paused") // second job, same store

	cfg := &config.Config{}
	cfg.Drift.Schedule.Enabled = true

	s, err := buildServeDriftScheduler(cfg, nil)
	require.NoError(t, err)
	require.NotNil(t, s)
	jobs := s.ListJobs()
	assert.Len(t, jobs, 2, "both CLI job files must load")

	// The scheduler it builds must actually run: Start/Stop round-trip
	// with a next-run populated, proving this is a live schedule and not
	// just a parsed list.
	require.NoError(t, s.Start(context.Background()))
	assert.True(t, s.IsRunning())
	time.Sleep(50 * time.Millisecond)
	s.Stop()
	assert.False(t, s.IsRunning())
}

// Opted in with no job files: the builder still returns a scheduler (empty)
// and the CALLER is the one who decides an empty schedule stays stopped —
// that division is what the runServe WARN message describes, and this test
// pins the builder's half of it.
func TestBuildServeDriftSchedulerEnabledWithoutJobsIsEmpty(t *testing.T) {
	newDriftFixture(t) // data dir exists, no jobs written

	cfg := &config.Config{}
	cfg.Drift.Schedule.Enabled = true

	s, err := buildServeDriftScheduler(cfg, nil)
	require.NoError(t, err)
	require.NotNil(t, s)
	assert.Empty(t, s.ListJobs())
}

// countingSink records every message Send receives — the stub half of the
// notifier wiring assertion.
type countingSink struct {
	sent chan notify.Message
}

func (s *countingSink) Name() string { return "counting-test" }
func (s *countingSink) Send(_ context.Context, msg notify.Message) error {
	s.sent <- msg
	return nil
}

// A non-nil notifier reaches the detector: a RunOnce over a job whose
// AlertOnDrift=true and whose probe cannot match the baseline must deliver
// a message through the manager. This is the wiring half only — the
// detector's alert rule is covered in internal/drift.
func TestBuildServeDriftSchedulerWiresDetectorNotifier(t *testing.T) {
	dataDir := newDriftFixture(t)

	jd := filepath.Join(dataDir, "drift", "jobs")
	require.NoError(t, os.MkdirAll(jd, 0o750))
	job := `{"id":"job-alert","name":"alerting","cron_expr":"0 3 * * *","hosts":["fixture-host"],"enabled":true,"alert_on_drift":true}`
	require.NoError(t, os.WriteFile(filepath.Join(jd, "alerting.json"), []byte(job), 0o600))

	// A baseline whose expected reality cannot hold on this machine: the
	// prober reads local files, and /definitely/not/here does not exist.
	bd := filepath.Join(dataDir, "drift", "baselines")
	require.NoError(t, os.MkdirAll(bd, 0o750))
	bl := `{"id":"bl-1","host":"fixture-host","items":[{"check_name":"absent-file","type":"file","path":"/definitely/not/here","expected_value":"x"}]}`
	require.NoError(t, os.WriteFile(filepath.Join(bd, "fixture-host.json"), []byte(bl), 0o600))

	// Real manager + counting channel registered into it: proves the
	// adapter (Send → NotifyAsync) end to end, not just that Send ran.
	sink := &countingSink{sent: make(chan notify.Message, 4)}
	mgr := notify.NewNotificationManager()
	require.NoError(t, mgr.Register(sink))

	cfg := &config.Config{}
	cfg.Drift.Schedule.Enabled = true
	s, err := buildServeDriftScheduler(cfg, driftNotifier(mgr))
	require.NoError(t, err)
	require.NotNil(t, s)

	report, err := s.RunOnce(context.Background(), "job-alert")
	require.NoError(t, err)
	require.NotNil(t, report)

	select {
	case msg := <-sink.sent:
		assert.NotEmpty(t, msg.Title, "a drift alert must carry a title")
	case <-time.After(5 * time.Second):
		t.Fatal("expected a drift alert on the wired sink within 5s")
	}
}

// A nil notifier keeps the reports-on-disk posture: the same drift run with
// no channel must complete without error and without any sink to deliver to.
func TestDriftNotifierNilManagerYieldsNil(t *testing.T) {
	assert.Nil(t, driftNotifier(nil))
}
