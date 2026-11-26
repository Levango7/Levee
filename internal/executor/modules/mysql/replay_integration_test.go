//go:build mysqlreplay

package mysql

// Opt-in real-server proof, driven by scripts/replay_mysql_schema.py.

import (
	"context"
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

type mysqlReplayTarget struct {
	port int
	key  string
}

func (t mysqlReplayTarget) Host() string { return "127.0.0.1" }
func (t mysqlReplayTarget) Port() int    { return t.port }
func (t mysqlReplayTarget) Type() string { return "ssh" }
func (t mysqlReplayTarget) Credentials() channel.CredentialRef {
	return channel.CredentialRef{Username: "root", KeyPath: t.key}
}

type countingMySQLChannel struct {
	channel.Channel
	execs int
}

func (c *countingMySQLChannel) Exec(ctx context.Context, command string) (*channel.ExecResult, error) {
	c.execs++
	return c.Channel.Exec(ctx, command)
}

func TestRealMySQLSchemaReplay(t *testing.T) {
	if os.Getenv("LEVEE_MYSQL_REPLAY") != "1" {
		t.Skip("run scripts/replay_mysql_schema.py for the real MySQL proof")
	}
	port, err := strconv.Atoi(os.Getenv("LEVEE_MYSQL_REPLAY_PORT"))
	require.NoError(t, err)
	target := mysqlReplayTarget{port: port, key: os.Getenv("LEVEE_MYSQL_REPLAY_KEY")}
	transport, err := sshchannel.NewChannel(target, &sshchannel.Config{
		StrictHostCheck: true,
		KnownHostsPath:  os.Getenv("LEVEE_MYSQL_REPLAY_KNOWN_HOSTS"),
		ConnectTimeout:  10 * time.Second,
	})
	require.NoError(t, err)
	ctx := context.Background()
	require.NoError(t, transport.Connect(ctx))
	t.Cleanup(func() { _ = transport.Close() })
	ch := &countingMySQLChannel{Channel: transport}
	conn := mysqlConn{Host: "127.0.0.1", Port: "3306", User: "root"}

	seed, err := runMysql(ctx, ch, conn, `CREATE DATABASE levee_test;
		USE levee_test;
		CREATE TABLE users (
			id INT AUTO_INCREMENT PRIMARY KEY,
			email VARCHAR(255) NOT NULL UNIQUE,
			parent_id INT NULL,
			CONSTRAINT users_parent_fk FOREIGN KEY (parent_id) REFERENCES users(id)
		) ENGINE=InnoDB;`)
	require.NoError(t, err)
	require.Zero(t, seed.ExitCode, seed.Stderr)
	conn.Database = "levee_test"
	input := func(args map[string]any) executor.ModuleInput {
		args["database"] = conn.Database
		return executor.ModuleInput{Args: args, Target: target, Channel: ch}
	}

	capture, err := New().Execute(ctx, "schema_snapshot", input(map[string]any{"tables": []any{"users"}}))
	require.NoError(t, err)
	require.Zero(t, capture.ExitCode, capture.Stderr)
	assert.False(t, capture.Changed)
	rawSnapshot := strings.TrimSpace(capture.Stdout)
	t.Logf("raw snapshot=%q", rawSnapshot)
	snapshot := normalizeSnapshotDDL(rawSnapshot)
	assert.Contains(t, snapshot, "CREATE TABLE `users`")
	t.Logf("captured %d bytes of canonical MySQL DDL", len(snapshot))

	drop, err := runMysql(ctx, ch, conn, "DROP TABLE users")
	require.NoError(t, err)
	require.Zero(t, drop.ExitCode, drop.Stderr)
	restored, err := New().Execute(ctx, "schema_restore", input(map[string]any{
		"table": "users", "snapshot": snapshot,
	}))
	require.NoError(t, err)
	require.Zero(t, restored.ExitCode, restored.Stderr)
	assert.True(t, restored.Changed)

	verify, err := runMysqlRead(ctx, ch, conn, "SHOW CREATE TABLE users")
	require.NoError(t, err)
	require.Zero(t, verify.ExitCode, verify.Stderr)
	assert.Equal(t, snapshot, normalizeSnapshotDDL(verify.Stdout))

	_, err = New().Execute(ctx, "schema_restore", input(map[string]any{
		"table": "users", "snapshot": snapshot,
	}))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "already exists")

	before := ch.execs
	_, err = New().Execute(ctx, "schema_restore", input(map[string]any{
		"table": "users", "snapshot": snapshot + "; DROP TABLE users",
	}))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "exactly one CREATE TABLE")
	assert.Equal(t, before, ch.execs, "tampered SQL must not reach the server")
	t.Log("MYSQL_SCHEMA_REPLAY PASS: capture, drop, restore, definition equality, existing-table and tamper refusal")
}
