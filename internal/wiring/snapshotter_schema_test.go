package wiring

import (
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nexus/levee/internal/channel"
	"github.com/nexus/levee/internal/dsl"
	"github.com/nexus/levee/internal/plan"
	"github.com/nexus/levee/internal/rollback"
)

// schemaFake is a tiny MySQL target: it knows whether users exists, returns
// canonical SHOW CREATE output, and records the CREATE that restores it.
type schemaFake struct {
	mu     sync.Mutex
	exists bool
	ddl    string
	execs  []string
}

type schemaChannel struct{ fake *schemaFake }

func (c *schemaChannel) Connect(context.Context) error                   { return nil }
func (c *schemaChannel) Close() error                                    { return nil }
func (c *schemaChannel) IsConnected() bool                               { return true }
func (c *schemaChannel) Upload(context.Context, string, io.Reader) error { return nil }
func (c *schemaChannel) Download(context.Context, string) (io.Reader, error) {
	return nil, fmt.Errorf("not implemented")
}

func (c *schemaChannel) Exec(_ context.Context, cmd string) (*channel.ExecResult, error) {
	c.fake.mu.Lock()
	defer c.fake.mu.Unlock()
	lines := strings.Split(cmd, "\n")
	if len(lines) != 3 {
		return &channel.ExecResult{ExitCode: 127, Stderr: "unexpected command"}, nil
	}
	raw, err := base64.StdEncoding.DecodeString(lines[1])
	if err != nil {
		return nil, err
	}
	sql := string(raw)
	c.fake.execs = append(c.fake.execs, sql)
	switch {
	case strings.Contains(sql, "SHOW CREATE TABLE"):
		if !c.fake.exists {
			return &channel.ExecResult{ExitCode: 1, Stderr: "relation does not exist"}, nil
		}
		return &channel.ExecResult{ExitCode: 0, Stdout: "users\t" + c.fake.ddl}, nil
	case strings.HasPrefix(sql, "SELECT COUNT(*) FROM information_schema.tables"):
		if c.fake.exists {
			return &channel.ExecResult{ExitCode: 0, Stdout: "1"}, nil
		}
		return &channel.ExecResult{ExitCode: 0, Stdout: "0"}, nil
	case strings.HasPrefix(sql, "CREATE TABLE"):
		c.fake.exists = true
		return &channel.ExecResult{ExitCode: 0}, nil
	default:
		return &channel.ExecResult{ExitCode: 127, Stderr: "unexpected SQL: " + sql}, nil
	}
}

type schemaFactory struct{ fake *schemaFake }

func (f schemaFactory) Create(channel.Target) (channel.Channel, error) {
	return &schemaChannel{fake: f.fake}, nil
}

func newSchemaSnapshotter(t *testing.T, fake *schemaFake) *remoteSnapshotter {
	t.Helper()
	reg := channel.NewChannelRegistry()
	reg.Register("local", schemaFactory{fake: fake})
	dir := t.TempDir()
	e, store := newLoopEngine(t, &loopRecorder{}, WithChannelRegistry(reg), WithSnapshotDir(dir))
	seedLocalTargets(t, store, "web1")
	rx, err := newRunExec(context.Background(), e, nil)
	require.NoError(t, err)
	t.Cleanup(rx.close)
	snapshotStore, err := rollback.NewFileSnapshotStore(dir)
	require.NoError(t, err)
	mgr, err := rollback.NewSnapshotManager(snapshotStore)
	require.NoError(t, err)
	return newRemoteSnapshotter(rx, mgr, "chg-schema")
}

func schemaStep(sql string) plan.PlanStep {
	return plan.PlanStep{
		Name: "drop-users", Module: "mysql", Action: "query",
		Args:     map[string]any{"sql": sql},
		Rollback: &dsl.RollbackSpec{Strategy: "snapshot"},
	}
}

func TestSnapshotterSchemaRestoreAfterDrop(t *testing.T) {
	fake := &schemaFake{exists: true, ddl: "CREATE TABLE `users` (\n  `id` int NOT NULL\n)"}
	s := newSchemaSnapshotter(t, fake)
	step := schemaStep("DROP TABLE users")
	require.NoError(t, s.CaptureForStep(context.Background(), "run-apply", "web1", step))

	snaps, err := s.mgr.Store().List(context.Background(), "chg-schema", "web1")
	require.NoError(t, err)
	require.Len(t, snaps, 1)
	payloads, err := rollback.ReadSnapshotPayloads(snaps[0].Path)
	require.NoError(t, err)
	assert.Equal(t, "users\t"+fake.ddl, payloads[mysqlSchemaPayloadPrefix+"users"])

	// The failed apply dropped the table. Restore must recreate only because it
	// is missing, then verify the exact definition came back.
	fake.exists = false
	require.NoError(t, s.RestoreForStep(context.Background(), "run-rollback", "web1", step))
	assert.True(t, fake.exists)
	assert.Equal(t, 2, countSQL(fake, "SHOW CREATE TABLE"), "capture + verification")
	assert.Equal(t, 1, countExactSQL(fake, "CREATE TABLE `users`"), "exactly one recovery CREATE")
}

func TestSnapshotterSchemaRestoreRefusesExistingTable(t *testing.T) {
	fake := &schemaFake{exists: true, ddl: "CREATE TABLE `users` (id int)"}
	s := newSchemaSnapshotter(t, fake)
	step := schemaStep("DROP TABLE users")
	require.NoError(t, s.CaptureForStep(context.Background(), "run-apply", "web1", step))

	err := s.RestoreForStep(context.Background(), "run-rollback", "web1", step)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "already exists")
	assert.Equal(t, 0, countExactSQL(fake, "CREATE TABLE `users`"), "no recovery overwrite is attempted")
}

func TestSnapshotterSchemaCaptureRejectsMixedDML(t *testing.T) {
	fake := &schemaFake{exists: true, ddl: "CREATE TABLE `users` (id int)"}
	s := newSchemaSnapshotter(t, fake)
	step := schemaStep("DROP TABLE users; UPDATE users SET active = true WHERE id = 1")

	err := s.CaptureForStep(context.Background(), "run-apply", "web1", step)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "supports DDL only")
}

func countExactSQL(fake *schemaFake, prefix string) int {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	n := 0
	for _, sql := range fake.execs {
		if strings.HasPrefix(sql, prefix) {
			n++
		}
	}
	return n
}

func countSQL(fake *schemaFake, prefix string) int {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	n := 0
	for _, sql := range fake.execs {
		if strings.Contains(sql, prefix) {
			n++
		}
	}
	return n
}
