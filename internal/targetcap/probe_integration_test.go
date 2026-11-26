//go:build capabilityreplay

package targetcap

import (
	"context"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nexus/levee/internal/channel"
	sshchannel "github.com/nexus/levee/internal/channel/ssh"
)

type capabilityReplayTarget struct {
	port int
	key  string
}

func (t capabilityReplayTarget) Host() string { return "127.0.0.1" }
func (t capabilityReplayTarget) Port() int    { return t.port }
func (t capabilityReplayTarget) Type() string { return "ssh" }
func (t capabilityReplayTarget) Credentials() channel.CredentialRef {
	return channel.CredentialRef{Username: "root", KeyPath: t.key}
}

func TestAlpineCapabilityProbe(t *testing.T) {
	if os.Getenv("LEVEE_PG_REPLAY") != "1" {
		t.Skip("run scripts/replay_pg_schema.py --suite capabilities-alpine for the real probe")
	}
	port, err := strconv.Atoi(os.Getenv("LEVEE_PG_REPLAY_PORT"))
	require.NoError(t, err)
	target := capabilityReplayTarget{port: port, key: os.Getenv("LEVEE_PG_REPLAY_KEY")}
	transport, err := sshchannel.NewChannel(target, &sshchannel.Config{
		StrictHostCheck: true,
		KnownHostsPath:  os.Getenv("LEVEE_PG_REPLAY_KNOWN_HOSTS"),
		ConnectTimeout:  10 * time.Second,
	})
	require.NoError(t, err)
	ctx := context.Background()
	require.NoError(t, transport.Connect(ctx))
	t.Cleanup(func() { _ = transport.Close() })

	report, err := Probe(ctx, transport, "alpine-lab")
	require.NoError(t, err)
	assert.Equal(t, SchemaVersion, report.SchemaVersion)
	assert.Equal(t, "Linux", report.Kernel)
	assert.Equal(t, "alpine", report.OS["ID"])
	assert.Equal(t, "apk", report.PackageManager)
	assert.NotEmpty(t, report.Tools["psql"])
	assert.NotEmpty(t, report.Tools["pg_dump"])
	byName := map[string]Action{}
	for _, action := range report.Actions {
		byName[action.Name] = action
	}
	assert.Equal(t, Supported, byName["pkg.install"].Status)
	assert.Equal(t, Supported, byName["file.copy"].Status)
	assert.Equal(t, Supported, byName["shell.script"].Status)
	assert.Equal(t, Supported, byName["pg.schema_snapshot_ddl"].Status)
	assert.Equal(t, Unsupported, byName["mysql.query"].Status)
	assert.Equal(t, Unknown, byName["mysql.replica_switch"].Status)
	t.Logf("CAPABILITY_REPLAY PASS: %d supported, %d unsupported, %d unknown", report.Summary.Supported, report.Summary.Unsupported, report.Summary.Unknown)
}
