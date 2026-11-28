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
	"github.com/nexus/levee/internal/plan"
	"github.com/nexus/levee/internal/rollback"
)

const pgRestoreFixtureDDL = `SET statement_timeout = 0;
SELECT pg_catalog.set_config('search_path', '', false);
CREATE SEQUENCE public.users_id_seq START WITH 1 INCREMENT BY 1 NO MINVALUE NO MAXVALUE CACHE 1;
CREATE TABLE public.users (id integer DEFAULT nextval('public.users_id_seq'::regclass) NOT NULL, email text NOT NULL);
ALTER SEQUENCE public.users_id_seq OWNED BY public.users.id;
ALTER TABLE ONLY public.users ALTER COLUMN id SET DEFAULT nextval('public.users_id_seq'::regclass);
ALTER TABLE ONLY public.users ADD CONSTRAINT users_pkey PRIMARY KEY (id);
CREATE INDEX users_email_idx ON public.users USING btree (email);
`

type pgSchemaFake struct {
	mu     sync.Mutex
	exists bool
	ddl    string
	execs  []string
	tables map[string]*pgSchemaTable
}

type pgSchemaTable struct {
	ddl    string
	exists bool
}

type pgSchemaChannel struct{ fake *pgSchemaFake }

func (c *pgSchemaChannel) Connect(context.Context) error                   { return nil }
func (c *pgSchemaChannel) Close() error                                    { return nil }
func (c *pgSchemaChannel) IsConnected() bool                               { return true }
func (c *pgSchemaChannel) Upload(context.Context, string, io.Reader) error { return nil }
func (c *pgSchemaChannel) Download(context.Context, string) (io.Reader, error) {
	return nil, fmt.Errorf("not implemented")
}

func (c *pgSchemaChannel) Exec(_ context.Context, cmd string) (*channel.ExecResult, error) {
	c.fake.mu.Lock()
	defer c.fake.mu.Unlock()
	if c.fake.tables != nil {
		return c.fake.execSet(cmd)
	}
	if strings.Contains(cmd, "pg_dump ") {
		c.fake.execs = append(c.fake.execs, "PG_DUMP")
		if !c.fake.exists {
			return &channel.ExecResult{ExitCode: 1, Stderr: "relation does not exist"}, nil
		}
		return &channel.ExecResult{ExitCode: 0, Stdout: base64.StdEncoding.EncodeToString([]byte(c.fake.ddl))}, nil
	}
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
	case strings.Contains(sql, "n.nspname || '.' || c.relname"):
		if !c.fake.exists {
			return &channel.ExecResult{ExitCode: 0, Stdout: "\n"}, nil
		}
		return &channel.ExecResult{ExitCode: 0, Stdout: "public.users\n"}, nil
	case strings.HasPrefix(sql, "SELECT to_regclass") && strings.HasSuffix(sql, "IS NULL"):
		if c.fake.exists {
			return &channel.ExecResult{ExitCode: 0, Stdout: "f\n"}, nil
		}
		return &channel.ExecResult{ExitCode: 0, Stdout: "t\n"}, nil
	case strings.HasPrefix(sql, "SET statement_timeout"):
		c.fake.exists = true
		return &channel.ExecResult{ExitCode: 0}, nil
	default:
		return &channel.ExecResult{ExitCode: 127, Stderr: "unexpected SQL: " + sql}, nil
	}
}

func (f *pgSchemaFake) execSet(cmd string) (*channel.ExecResult, error) {
	if strings.Contains(cmd, "pg_dump ") {
		table := relationFromCommand(cmd)
		row := f.tables[table]
		f.execs = append(f.execs, "PG_DUMP:"+table)
		if row == nil || !row.exists {
			return &channel.ExecResult{ExitCode: 1, Stderr: "relation does not exist"}, nil
		}
		return &channel.ExecResult{ExitCode: 0, Stdout: base64.StdEncoding.EncodeToString([]byte(row.ddl))}, nil
	}
	lines := strings.Split(cmd, "\n")
	if len(lines) != 3 {
		return &channel.ExecResult{ExitCode: 127, Stderr: "unexpected command"}, nil
	}
	raw, err := base64.StdEncoding.DecodeString(lines[1])
	if err != nil {
		return nil, err
	}
	sql := string(raw)
	f.execs = append(f.execs, sql)
	switch {
	case strings.Contains(sql, "n.nspname || '.' || c.relname"):
		table := relationFromCommand(sql)
		if row := f.tables[table]; row != nil && row.exists {
			return &channel.ExecResult{ExitCode: 0, Stdout: table + "\n"}, nil
		}
		return &channel.ExecResult{ExitCode: 0, Stdout: "\n"}, nil
	case strings.HasPrefix(sql, "SELECT COALESCE(to_regclass"):
		wantPipes := strings.Count(sql, "to_regclass(") - 1
		for _, row := range f.tables {
			if row.exists {
				return &channel.ExecResult{ExitCode: 0, Stdout: "existing\n"}, nil
			}
		}
		return &channel.ExecResult{ExitCode: 0, Stdout: strings.Repeat("|", wantPipes) + "\n"}, nil
	case strings.HasPrefix(sql, "BEGIN;"):
		for _, row := range f.tables {
			row.exists = true
		}
		return &channel.ExecResult{ExitCode: 0}, nil
	default:
		return &channel.ExecResult{ExitCode: 127, Stderr: "unexpected SQL: " + sql}, nil
	}
}

func relationFromCommand(cmd string) string {
	const marker = `"public"."`
	if start := strings.Index(cmd, marker); start >= 0 {
		rest := cmd[start+len(marker):]
		if end := strings.Index(rest, `"`); end >= 0 {
			return "public." + rest[:end]
		}
	}
	const plain = "to_regclass('"
	if start := strings.Index(cmd, plain); start >= 0 {
		rest := cmd[start+len(plain):]
		if end := strings.Index(rest, "'"); end >= 0 {
			return "public." + strings.Trim(rest[:end], `"`)
		}
	}
	return ""
}

type pgSchemaFactory struct{ fake *pgSchemaFake }

func (f pgSchemaFactory) Create(channel.Target) (channel.Channel, error) {
	return &pgSchemaChannel{fake: f.fake}, nil
}

func newPGSchemaSnapshotter(t *testing.T, fake *pgSchemaFake) *remoteSnapshotter {
	t.Helper()
	reg := channel.NewChannelRegistry()
	reg.Register("local", pgSchemaFactory{fake: fake})
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
	return newRemoteSnapshotter(rx, mgr, "chg-pg-schema")
}

func pgSchemaStep(sql string) plan.PlanStep {
	step := schemaStep(sql)
	step.Module = "pg"
	return step
}

func TestPGSnapshotterSchemaRestoreAfterDrop(t *testing.T) {
	fake := &pgSchemaFake{exists: true, ddl: pgRestoreFixtureDDL}
	s := newPGSchemaSnapshotter(t, fake)
	step := pgSchemaStep("DROP TABLE users")
	require.NoError(t, s.CaptureForStep(context.Background(), "run-apply", "web1", step))

	snaps, err := s.mgr.Store().List(context.Background(), "chg-pg-schema", "web1")
	require.NoError(t, err)
	require.Len(t, snaps, 1)
	payloads, err := rollback.ReadSnapshotPayloads(snaps[0].Path)
	require.NoError(t, err)
	assert.Contains(t, payloads[pgSchemaPayloadPrefix+"public.users"], schemaSnapshotFormatForTest)

	fake.exists = false
	require.NoError(t, s.RestoreForStep(context.Background(), "run-rollback", "web1", step))
	assert.True(t, fake.exists)
	assert.Equal(t, 2, countExactSQL(&schemaFake{execs: fake.execs}, "PG_DUMP"), "capture + verification")
	assert.Equal(t, 1, countSQL(&schemaFake{execs: fake.execs}, "SET statement_timeout"), "exactly one recovery script")
}

const schemaSnapshotFormatForTest = "levee.pg.schema/v1"

func TestPGSnapshotterSchemaRestoreRefusesExistingTable(t *testing.T) {
	fake := &pgSchemaFake{exists: true, ddl: pgRestoreFixtureDDL}
	s := newPGSchemaSnapshotter(t, fake)
	step := pgSchemaStep("DROP TABLE users")
	require.NoError(t, s.CaptureForStep(context.Background(), "run-apply", "web1", step))

	err := s.RestoreForStep(context.Background(), "run-rollback", "web1", step)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "already exists")
	assert.Equal(t, 0, countSQL(&schemaFake{execs: fake.execs}, "SET statement_timeout"), "no recovery overwrite")
}

func TestPGSnapshotterCaptureRejectsMixedDMLAndLeavesNoRecord(t *testing.T) {
	fake := &pgSchemaFake{exists: true, ddl: pgRestoreFixtureDDL}
	s := newPGSchemaSnapshotter(t, fake)
	step := pgSchemaStep("DROP TABLE users; UPDATE users SET active = true WHERE id = 1")

	err := s.CaptureForStep(context.Background(), "run-apply", "web1", step)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "supports DDL only")
	snaps, listErr := s.mgr.Store().List(context.Background(), "chg-pg-schema", "web1")
	require.NoError(t, listErr)
	assert.Empty(t, snaps, "a failed capture must be deleted, not left as an audit-only baseline")
	assert.Empty(t, fake.execs, "the SQL review refuses before touching the target")
}

func TestPGSnapshotterRestoresCrossTableSetInOneTransaction(t *testing.T) {
	parentDDL := "CREATE TABLE public.parent (id integer GENERATED ALWAYS AS IDENTITY PRIMARY KEY, child_id integer);\n" +
		"ALTER TABLE ONLY public.parent ADD CONSTRAINT parent_child_fk FOREIGN KEY (child_id) REFERENCES public.child(id);\n"
	childDDL := "CREATE TABLE public.child (id integer PRIMARY KEY, parent_id integer);\n" +
		"ALTER TABLE ONLY public.child ADD CONSTRAINT child_parent_fk FOREIGN KEY (parent_id) REFERENCES public.parent(id);\n"
	fake := &pgSchemaFake{tables: map[string]*pgSchemaTable{
		"public.parent": {ddl: parentDDL, exists: true},
		"public.child":  {ddl: childDDL, exists: true},
	}}
	s := newPGSchemaSnapshotter(t, fake)
	step := pgSchemaStep("DROP TABLE parent; DROP TABLE child")
	require.NoError(t, s.CaptureForStep(context.Background(), "run-apply", "web1", step))
	for _, row := range fake.tables {
		row.exists = false
	}
	require.NoError(t, s.RestoreForStep(context.Background(), "run-rollback", "web1", step))
	for name, row := range fake.tables {
		assert.True(t, row.exists, "%s must be restored", name)
	}
	var script string
	for _, sql := range fake.execs {
		if strings.HasPrefix(sql, "BEGIN;") {
			script = sql
			break
		}
	}
	require.NotEmpty(t, script, "multiple PG payloads must use one transaction")
	assert.Less(t, strings.LastIndex(script, "CREATE TABLE"), strings.Index(script, "ADD CONSTRAINT"))
	assert.Contains(t, script, "COMMIT;")
}
