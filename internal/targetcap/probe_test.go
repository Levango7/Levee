package targetcap

import (
	"context"
	"io"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nexus/levee/internal/channel"
)

type probeChannel struct {
	responses map[string]channel.ExecResult
	execs     []string
}

func (c *probeChannel) Connect(context.Context) error                   { return nil }
func (c *probeChannel) IsConnected() bool                               { return true }
func (c *probeChannel) Close() error                                    { return nil }
func (c *probeChannel) Upload(context.Context, string, io.Reader) error { return nil }
func (c *probeChannel) Download(context.Context, string) (io.Reader, error) {
	return nil, io.EOF
}
func (c *probeChannel) Exec(_ context.Context, command string) (*channel.ExecResult, error) {
	c.execs = append(c.execs, command)
	if response, ok := c.responses[command]; ok {
		result := response
		return &result, nil
	}
	return &channel.ExecResult{ExitCode: 1}, nil
}

func TestProbeReportsObservedCapabilitiesWithoutMutating(t *testing.T) {
	ch := &probeChannel{responses: map[string]channel.ExecResult{
		"uname -s":                     {ExitCode: 0, Stdout: "Linux\n"},
		"cat /etc/os-release":          {ExitCode: 0, Stdout: "ID=alpine\nVERSION_ID=3.23.0\n"},
		"which apt":                    {ExitCode: 1},
		"which dnf":                    {ExitCode: 1},
		"which yum":                    {ExitCode: 1},
		"which apk":                    {ExitCode: 0},
		"which systemctl":              {ExitCode: 1},
		"which service":                {ExitCode: 0},
		"which psql":                   {ExitCode: 0},
		"psql --version":               {ExitCode: 0, Stdout: "psql (PostgreSQL) 16.15\n"},
		"which pg_dump":                {ExitCode: 0},
		"pg_dump --version":            {ExitCode: 0, Stdout: "pg_dump (PostgreSQL) 16.15\n"},
		"pg_dump --help":               {ExitCode: 0, Stdout: "--restrict-key=KEY\n"},
		"which sh":                     {ExitCode: 0},
		"test -d /tmp && test -w /tmp": {ExitCode: 0},
		"which sha256sum":              {ExitCode: 0},
		"which chmod":                  {ExitCode: 0},
		"which chown":                  {ExitCode: 0},
		"which rm":                     {ExitCode: 0},
	}}
	report, err := Probe(context.Background(), ch, "alpine-1")
	require.NoError(t, err)
	assert.Equal(t, SchemaVersion, report.SchemaVersion)
	assert.Equal(t, "apk", report.PackageManager)
	assert.Equal(t, "sysvinit", report.InitSystem)
	assert.Equal(t, "alpine", report.OS["ID"])
	assert.Contains(t, report.Evidence, "not an acceptance verdict")

	byName := map[string]Action{}
	for _, action := range report.Actions {
		byName[action.Name] = action
	}
	assert.Equal(t, Supported, byName["pkg.install"].Status)
	assert.Equal(t, Supported, byName["svc.start"].Status)
	assert.Equal(t, Supported, byName["pg.schema_restore"].Status)
	assert.Equal(t, Unsupported, byName["mysql.query"].Status)
	assert.Equal(t, Unknown, byName["mysql.replica_switch"].Status)
	assert.Equal(t, Unsupported, byName["user.add"].Status)
	assert.Equal(t, 20, report.Summary.Supported)
	assert.Equal(t, 8, report.Summary.Unsupported)
	assert.Equal(t, 1, report.Summary.Unknown)
	assert.Len(t, report.Actions, 29)

	for _, command := range ch.execs {
		for _, forbidden := range []string{"apk add", "apk del", "apk upgrade", "DROP ", "systemctl start", "service "} {
			assert.NotContains(t, command, forbidden, "probe must not mutate the target")
		}
	}
}

func TestProbeRejectsNilChannel(t *testing.T) {
	_, err := Probe(context.Background(), nil, "x")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "nil channel")
}

func TestCommandOutputFailsClosedToEmpty(t *testing.T) {
	ch := &probeChannel{responses: map[string]channel.ExecResult{
		"uname -s": {ExitCode: 1, Stdout: "not-a-kernel"},
	}}
	assert.Empty(t, commandOutput(context.Background(), ch, "uname -s"))
	assert.False(t, strings.Contains(commandOutput(context.Background(), ch, "uname -s"), "not-a-kernel"))
}
