//go:build pgreplay

package pg

// Opt-in real-server proof, driven by scripts/replay_pg_schema.py. The Python
// launcher owns the disposable SSH+PostgreSQL container; this test uses the
// production SSH channel and strict known_hosts verification.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nexus/levee/internal/channel"
	sshchannel "github.com/nexus/levee/internal/channel/ssh"
	"github.com/nexus/levee/internal/executor"
)

type replayTarget struct {
	port int
	key  string
}

func (t replayTarget) Host() string { return "127.0.0.1" }
func (t replayTarget) Port() int    { return t.port }
func (t replayTarget) Type() string { return "ssh" }
func (t replayTarget) Credentials() channel.CredentialRef {
	return channel.CredentialRef{Username: "postgres", KeyPath: t.key}
}

type countingReplayChannel struct {
	channel.Channel
	execs int
}

func (c *countingReplayChannel) Exec(ctx context.Context, command string) (*channel.ExecResult, error) {
	c.execs++
	return c.Channel.Exec(ctx, command)
}

func TestRealPostgresSchemaReplay(t *testing.T) {
	if os.Getenv("LEVEE_PG_REPLAY") != "1" {
		t.Skip("run scripts/replay_pg_schema.py for the isolated real-server proof")
	}
	port, err := strconv.Atoi(os.Getenv("LEVEE_PG_REPLAY_PORT"))
	require.NoError(t, err)
	key := os.Getenv("LEVEE_PG_REPLAY_KEY")
	knownHosts := os.Getenv("LEVEE_PG_REPLAY_KNOWN_HOSTS")
	require.NotEmpty(t, key)
	require.NotEmpty(t, knownHosts)

	ctx := context.Background()
	target := replayTarget{port: port, key: key}
	transport, err := sshchannel.NewChannel(target, &sshchannel.Config{
		StrictHostCheck: true,
		KnownHostsPath:  knownHosts,
		ConnectTimeout:  10 * time.Second,
	})
	require.NoError(t, err)
	require.NoError(t, transport.Connect(ctx))
	t.Cleanup(func() { _ = transport.Close() })
	ch := &countingReplayChannel{Channel: transport}
	conn := pgConn{Host: "127.0.0.1", Port: "5432", User: "postgres", Database: "postgres"}
	input := func(args map[string]any) executor.ModuleInput {
		args["database"] = "postgres"
		return executor.ModuleInput{Args: args, Target: target, Channel: ch}
	}

	seed, err := runPsql(ctx, ch, conn, `CREATE TABLE public.users (
		id serial PRIMARY KEY,
		email text NOT NULL UNIQUE,
		parent_id integer REFERENCES public.users(id)
	);
	CREATE INDEX users_email_idx ON public.users USING btree (email);`)
	require.NoError(t, err)
	require.Zero(t, seed.ExitCode, seed.Stderr)

	capture, err := New().Execute(ctx, "schema_snapshot_ddl", input(map[string]any{
		"tables": []any{"users"}, "schema": "public",
	}))
	require.NoError(t, err)
	assert.False(t, capture.Changed)
	manifest := capture.Stdout
	assert.Contains(t, manifest, schemaSnapshotFormat)
	t.Logf("captured %d bytes of versioned schema evidence", len(manifest))

	// Simulate the failed change's only recoverable end state: the original
	// relation is gone. Restore must recreate it and verify a fresh dump.
	drop, err := runPsql(ctx, ch, conn, "DROP TABLE public.users")
	require.NoError(t, err)
	require.Zero(t, drop.ExitCode, drop.Stderr)
	restored, err := New().Execute(ctx, "schema_restore", input(map[string]any{
		"table": "public.users", "snapshot": manifest,
	}))
	require.NoError(t, err)
	assert.True(t, restored.Changed)

	check, err := runPsqlRead(ctx, ch, conn, `SELECT
		(SELECT count(*) FROM pg_constraint WHERE conrelid = 'public.users'::regclass AND contype IN ('p', 'f', 'u')),
		(SELECT count(*) FROM pg_indexes WHERE schemaname = 'public' AND tablename = 'users')`)
	require.NoError(t, err)
	require.Zero(t, check.ExitCode, check.Stderr)
	assert.Equal(t, "3|3", strings.TrimSpace(check.Stdout), "PK + self FK + unique; their indexes + the secondary index")

	_, err = New().Execute(ctx, "schema_restore", input(map[string]any{
		"table": "public.users", "snapshot": manifest,
	}))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "already exists")

	// Even a correctly re-checksummed malicious payload must fail offline,
	// before another command reaches PostgreSQL.
	var tampered map[string]any
	require.NoError(t, json.Unmarshal([]byte(manifest), &tampered))
	tampered["ddl"] = tampered["ddl"].(string) + "\nDROP TABLE public.users;\n"
	sum := sha256.Sum256([]byte(tampered["ddl"].(string)))
	tampered["sha256"] = hex.EncodeToString(sum[:])
	rawTampered, err := json.Marshal(tampered)
	require.NoError(t, err)
	before := ch.execs
	_, err = New().Execute(ctx, "schema_restore", input(map[string]any{
		"table": "public.users", "snapshot": string(rawTampered),
	}))
	require.Error(t, err)
	require.Contains(t, err.Error(), "outside the pg_dump restrict pair")
	require.Equal(t, before, ch.execs, "tampered input must not reach the server")
	t.Log("REAL_PG_REPLAY PASS: capture, drop, restore, catalog checks, existing-table refusal, tamper refusal")

	// IDENTITY + a cyclic cross-table FK: grouped recovery must create both base
	// tables before adding either constraint, inside one transaction.
	seedSet, err := runPsql(ctx, ch, conn, `CREATE TABLE public.parent (
		id integer GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
		child_id integer
	);
	CREATE TABLE public.child (
		id integer PRIMARY KEY,
		parent_id integer
	);
	ALTER TABLE public.parent ADD CONSTRAINT parent_child_fk FOREIGN KEY (child_id) REFERENCES public.child(id);
	ALTER TABLE public.child ADD CONSTRAINT child_parent_fk FOREIGN KEY (parent_id) REFERENCES public.parent(id);`)
	require.NoError(t, err)
	require.Zero(t, seedSet.ExitCode, seedSet.Stderr)
	parentCapture, err := New().Execute(ctx, "schema_snapshot_ddl", input(map[string]any{"tables": []any{"parent"}}))
	require.NoError(t, err)
	childCapture, err := New().Execute(ctx, "schema_snapshot_ddl", input(map[string]any{"tables": []any{"child"}}))
	require.NoError(t, err)
	rawSet, err := json.Marshal([]string{childCapture.Stdout, parentCapture.Stdout})
	require.NoError(t, err)
	_, err = runPsql(ctx, ch, conn, "DROP TABLE public.parent, public.child")
	require.NoError(t, err)
	restoredSet, err := New().Execute(ctx, "schema_restore", input(map[string]any{"snapshots": string(rawSet)}))
	require.NoError(t, err)
	assert.True(t, restoredSet.Changed)
	setCheck, err := runPsqlRead(ctx, ch, conn, `SELECT
		(SELECT count(*) FROM pg_constraint WHERE conrelid IN ('public.parent'::regclass, 'public.child'::regclass) AND contype IN ('p', 'f')),
		(SELECT attidentity FROM pg_attribute WHERE attrelid = 'public.parent'::regclass AND attname = 'id')`)
	require.NoError(t, err)
	assert.Equal(t, "4|a", strings.TrimSpace(setCheck.Stdout), "two PKs + two FKs; parent ID is ALWAYS IDENTITY")
	_, err = New().Execute(ctx, "schema_restore", input(map[string]any{"snapshots": string(rawSet)}))
	require.ErrorContains(t, err, "already exists")

	var childTampered map[string]any
	require.NoError(t, json.Unmarshal([]byte(childCapture.Stdout), &childTampered))
	originalChildDDL := childTampered["ddl"].(string)
	mutatedChildDDL := strings.ReplaceAll(originalChildDDL, `"public"."parent"("id")`, `"public"."other"("id")`)
	mutatedChildDDL = strings.ReplaceAll(mutatedChildDDL, `"public"."parent"(id)`, `"public"."other"(id)`)
	mutatedChildDDL = strings.ReplaceAll(mutatedChildDDL, "public.parent(id)", "public.other(id)")
	require.NotEqual(t, originalChildDDL, mutatedChildDDL, "the tamper test must change real pg_dump bytes")
	childTampered["ddl"] = mutatedChildDDL
	tamperedSum := sha256.Sum256([]byte(childTampered["ddl"].(string)))
	childTampered["sha256"] = hex.EncodeToString(tamperedSum[:])
	rawTamperedChild, err := json.Marshal(childTampered)
	require.NoError(t, err)
	rawTamperedSet, err := json.Marshal([]string{string(rawTamperedChild), parentCapture.Stdout})
	require.NoError(t, err)
	beforeSet := ch.execs
	_, err = New().Execute(ctx, "schema_restore", input(map[string]any{"snapshots": string(rawTamperedSet)}))
	require.ErrorContains(t, err, "outside the snapshot set")
	assert.Equal(t, beforeSet, ch.execs, "an out-of-set FK must be rejected before any server command")

	// The deferred constraint fails only after every base table was created.
	// ON_ERROR_STOP must roll the entire transaction back rather than leaving a
	// half-restored parent/child pair behind.
	seedRollback, err := runPsql(ctx, ch, conn, `CREATE TABLE public.parent_fail (id integer PRIMARY KEY);
	CREATE TABLE public.child_fail (id integer PRIMARY KEY, parent_id text);`)
	require.NoError(t, err)
	require.Zero(t, seedRollback.ExitCode, seedRollback.Stderr)
	parentFail, err := New().Execute(ctx, "schema_snapshot_ddl", input(map[string]any{"tables": []any{"parent_fail"}}))
	require.NoError(t, err)
	childFail, err := New().Execute(ctx, "schema_snapshot_ddl", input(map[string]any{"tables": []any{"child_fail"}}))
	require.NoError(t, err)
	_, err = runPsql(ctx, ch, conn, "DROP TABLE public.parent_fail, public.child_fail")
	require.NoError(t, err)
	var failFields map[string]any
	require.NoError(t, json.Unmarshal([]byte(childFail.Stdout), &failFields))
	failFields["ddl"] = failFields["ddl"].(string) + "\nALTER TABLE ONLY public.child_fail ADD CONSTRAINT bad_fk FOREIGN KEY (parent_id) REFERENCES public.parent_fail(id);\n"
	failSum := sha256.Sum256([]byte(failFields["ddl"].(string)))
	failFields["sha256"] = hex.EncodeToString(failSum[:])
	rawFailChild, err := json.Marshal(failFields)
	require.NoError(t, err)
	rawFailSet, err := json.Marshal([]string{string(rawFailChild), parentFail.Stdout})
	require.NoError(t, err)
	_, err = New().Execute(ctx, "schema_restore", input(map[string]any{"snapshots": string(rawFailSet)}))
	require.Error(t, err, "an incompatible FK must fail the transaction")
	rollbackCheck, err := runPsqlRead(ctx, ch, conn, `SELECT
		COALESCE(to_regclass('public.parent_fail')::text, '') || '|' || COALESCE(to_regclass('public.child_fail')::text, '')`)
	require.NoError(t, err)
	assert.Equal(t, "|", strings.TrimSpace(rollbackCheck.Stdout), "a deferred FK failure must roll back both base tables")
	t.Log("REAL_PG_SET_REPLAY PASS: plain IDENTITY, cyclic FKs, one transaction, existing-set and out-of-set tamper refusal, deferred-failure rollback")
}
