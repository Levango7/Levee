package pg

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"io"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nexus/levee/internal/channel"
	"github.com/nexus/levee/internal/executor"
)

const restoreTestDDL = `SET statement_timeout = 0;
SELECT pg_catalog.set_config('search_path', '', false);
CREATE SEQUENCE public.users_id_seq START WITH 1 INCREMENT BY 1 NO MINVALUE NO MAXVALUE CACHE 1;
CREATE TABLE public.users (id integer DEFAULT nextval('public.users_id_seq'::regclass) NOT NULL, email text NOT NULL);
ALTER SEQUENCE public.users_id_seq OWNED BY public.users.id;
ALTER TABLE ONLY public.users ALTER COLUMN id SET DEFAULT nextval('public.users_id_seq'::regclass);
ALTER TABLE ONLY public.users ADD CONSTRAINT users_pkey PRIMARY KEY (id);
CREATE INDEX users_email_idx ON public.users USING btree (email);
`

type scriptedPGChannel struct {
	results []channel.ExecResult
	execs   []string
}

func (c *scriptedPGChannel) Connect(context.Context) error                   { return nil }
func (c *scriptedPGChannel) IsConnected() bool                               { return true }
func (c *scriptedPGChannel) Close() error                                    { return nil }
func (c *scriptedPGChannel) Upload(context.Context, string, io.Reader) error { return nil }
func (c *scriptedPGChannel) Download(context.Context, string) (io.Reader, error) {
	return nil, io.EOF
}
func (c *scriptedPGChannel) Exec(_ context.Context, cmd string) (*channel.ExecResult, error) {
	c.execs = append(c.execs, cmd)
	if len(c.results) == 0 {
		return &channel.ExecResult{ExitCode: 127, Stderr: "unexpected command"}, nil
	}
	res := c.results[0]
	c.results = c.results[1:]
	return &res, nil
}

func pgInput(ch channel.Channel, args map[string]any) executor.ModuleInput {
	return executor.ModuleInput{Args: args, Target: mockTarget{host: "pg01.prod"}, Channel: ch}
}

func manifestForTest(t *testing.T, table, ddl string) string {
	t.Helper()
	sum := sha256.Sum256([]byte(ddl))
	data, err := json.Marshal(schemaSnapshot{
		Format: schemaSnapshotFormat, Table: table, DDL: ddl, SHA256: hex.EncodeToString(sum[:]),
	})
	require.NoError(t, err)
	return string(data)
}

func validManifestForTest(t *testing.T) string {
	t.Helper()
	manifest, err := encodeSchemaSnapshot("public.users", restoreTestDDL)
	require.NoError(t, err)
	return manifest
}

func TestSchemaSnapshotDDLCapturesVersionedPayload(t *testing.T) {
	ch := &scriptedPGChannel{results: []channel.ExecResult{
		{ExitCode: 0, Stdout: "public.users\n"},
		{ExitCode: 0, Stdout: base64.StdEncoding.EncodeToString([]byte(restoreTestDDL))},
	}}
	out, err := New().Execute(context.Background(), "schema_snapshot_ddl", pgInput(ch, map[string]any{
		"tables": []any{"users"}, "schema": "public",
	}))
	require.NoError(t, err)
	assert.False(t, out.Changed)
	manifest, err := decodeSchemaSnapshot(out.Stdout, "public.users")
	require.NoError(t, err)
	assert.Equal(t, schemaSnapshotFormat, manifest.Format)
	assert.Equal(t, restoreTestDDL, manifest.DDL)
	require.Len(t, ch.execs, 2)
	assert.Contains(t, decodedPayload(t, ch.execs[0]), "to_regclass")
	assert.Contains(t, ch.execs[1], "set -o pipefail; pg_dump --schema-only")
	assert.Contains(t, ch.execs[1], "-t '\"public\".\"users\"'")
	assert.Contains(t, ch.execs[1], "--no-owner --no-privileges --no-tablespaces")
	assert.Contains(t, ch.execs[1], "--restrict-key=leveepgschema1")
}

func TestSchemaSnapshotDDLRejectsMissingRelationBeforeDump(t *testing.T) {
	ch := &scriptedPGChannel{results: []channel.ExecResult{{ExitCode: 0, Stdout: "\n"}}}
	_, err := New().Execute(context.Background(), "schema_snapshot_ddl", pgInput(ch, map[string]any{
		"tables": []any{"users"},
	}))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "does not exist")
	assert.Len(t, ch.execs, 1, "a missing relation must not produce an empty dump")
}

func TestSchemaSnapshotDDLRejectsEmptyDump(t *testing.T) {
	ch := &scriptedPGChannel{results: []channel.ExecResult{
		{ExitCode: 0, Stdout: "public.users\n"},
		{ExitCode: 0, Stdout: "", Stderr: "pg_dump: unsupported option"},
	}}
	_, err := New().Execute(context.Background(), "schema_snapshot_ddl", pgInput(ch, map[string]any{
		"tables": []any{"users"},
	}))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "returned no schema output")
	assert.Contains(t, err.Error(), "unsupported option")
}

func TestSchemaSnapshotDDLRejectsUnsupportedDump(t *testing.T) {
	ch := &scriptedPGChannel{results: []channel.ExecResult{
		{ExitCode: 0, Stdout: "public.users\n"},
		{ExitCode: 0, Stdout: base64.StdEncoding.EncodeToString([]byte("DROP TABLE public.users;\n"))},
	}}
	_, err := New().Execute(context.Background(), "schema_snapshot_ddl", pgInput(ch, map[string]any{
		"tables": []any{"users"},
	}))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unsupported statement type")
}

func TestSchemaRestoreRecreatesAndVerifies(t *testing.T) {
	ch := &scriptedPGChannel{results: []channel.ExecResult{
		{ExitCode: 0, Stdout: "t\n"},
		{ExitCode: 0},
		{ExitCode: 0, Stdout: base64.StdEncoding.EncodeToString([]byte(restoreTestDDL))},
	}}
	out, err := New().Execute(context.Background(), "schema_restore", pgInput(ch, map[string]any{
		"table": "public.users", "snapshot": validManifestForTest(t),
	}))
	require.NoError(t, err)
	assert.True(t, out.Changed)
	assert.Equal(t, restoreTestDDL, out.Stdout)
	require.Len(t, ch.execs, 3)
	assert.Contains(t, decodedPayload(t, ch.execs[0]), "to_regclass")
	assert.Contains(t, ch.execs[1], "psql")
	assert.Contains(t, ch.execs[2], "pg_dump", "success requires a fresh definition readback")
}

func TestSchemaRestoreRefusesExistingRelation(t *testing.T) {
	ch := &scriptedPGChannel{results: []channel.ExecResult{{ExitCode: 0, Stdout: "f\n"}}}
	_, err := New().Execute(context.Background(), "schema_restore", pgInput(ch, map[string]any{
		"table": "public.users", "snapshot": validManifestForTest(t),
	}))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "already exists")
	assert.Len(t, ch.execs, 1, "no CREATE may run when the relation still exists")
}

func TestSchemaRestorePropagatesApplyFailureWithoutVerification(t *testing.T) {
	ch := &scriptedPGChannel{results: []channel.ExecResult{
		{ExitCode: 0, Stdout: "t\n"},
		{ExitCode: 2, Stderr: "permission denied"},
	}}
	out, err := New().Execute(context.Background(), "schema_restore", pgInput(ch, map[string]any{
		"table": "public.users", "snapshot": validManifestForTest(t),
	}))
	require.Error(t, err)
	assert.Equal(t, 2, out.ExitCode)
	assert.False(t, out.Changed)
	assert.Len(t, ch.execs, 2)
}

func TestSchemaRestoreRejectsVerificationMismatch(t *testing.T) {
	changed := strings.Replace(restoreTestDDL, "email text NOT NULL", "email text", 1)
	ch := &scriptedPGChannel{results: []channel.ExecResult{
		{ExitCode: 0, Stdout: "t\n"},
		{ExitCode: 0},
		{ExitCode: 0, Stdout: base64.StdEncoding.EncodeToString([]byte(changed))},
	}}
	_, err := New().Execute(context.Background(), "schema_restore", pgInput(ch, map[string]any{
		"table": "public.users", "snapshot": validManifestForTest(t),
	}))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "verification mismatch")
}

func TestSchemaRestoreRejectsUnsafePayloadBeforeDialing(t *testing.T) {
	valid := validManifestForTest(t)
	var fields map[string]any
	require.NoError(t, json.Unmarshal([]byte(valid), &fields))
	fields["unexpected"] = true
	unknownField, err := json.Marshal(fields)
	require.NoError(t, err)

	cases := []struct {
		name     string
		table    string
		snapshot string
		want     string
	}{
		{name: "checksum", table: "public.users", snapshot: strings.Replace(valid, fields["sha256"].(string), strings.Repeat("0", 64), 1), want: "mismatch"},
		{name: "unknown field", table: "public.users", snapshot: string(unknownField), want: "unknown field"},
		{name: "wrong table", table: "public.other", snapshot: valid, want: "does not match"},
		{name: "drop smuggling", table: "public.users", snapshot: manifestForTest(t, "public.users", restoreTestDDL+"DROP TABLE public.users;\n"), want: "unsupported statement"},
		{name: "dml smuggling", table: "public.users", snapshot: manifestForTest(t, "public.users", restoreTestDDL+"DELETE FROM public.users;\n"), want: "unsupported statement"},
		{name: "cross table index", table: "public.users", snapshot: manifestForTest(t, "public.users", restoreTestDDL+"CREATE INDEX other_idx ON public.other(id);\n"), want: "plain index"},
		{name: "second table", table: "public.users", snapshot: manifestForTest(t, "public.users", restoreTestDDL+"CREATE TABLE public.other(id integer);\n"), want: "does not match"},
		{name: "unowned sequence", table: "public.users", snapshot: manifestForTest(t, "public.users", strings.Replace(restoreTestDDL, "ALTER SEQUENCE public.users_id_seq OWNED BY public.users.id;\n", "", 1)), want: "not proven to be owned"},
		{name: "cross table foreign key", table: "public.users", snapshot: manifestForTest(t, "public.users", strings.Replace(restoreTestDDL, "email text NOT NULL", "email integer REFERENCES public.other(id)", 1)), want: "cross-table foreign keys"},
		{name: "identity", table: "public.users", snapshot: manifestForTest(t, "public.users", "CREATE TABLE public.users (id integer GENERATED ALWAYS AS IDENTITY, email text);\n"), want: "IDENTITY"},
		{name: "trailing json", table: "public.users", snapshot: valid + "{}", want: "second JSON value"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ch := &scriptedPGChannel{}
			_, err := New().Execute(context.Background(), "schema_restore", pgInput(ch, map[string]any{
				"table": tc.table, "snapshot": tc.snapshot,
			}))
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.want)
			assert.Empty(t, ch.execs, "unsafe input must be rejected before any target command")
		})
	}
}

func TestSchemaRestoreRefusesPasswordBeforeDialing(t *testing.T) {
	ch := &scriptedPGChannel{}
	_, err := New().Execute(context.Background(), "schema_restore", pgInput(ch, map[string]any{
		"table": "public.users", "snapshot": validManifestForTest(t), "password": "not-allowed",
	}))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "~/.pgpass")
	assert.Empty(t, ch.execs)
}

func TestSchemaRestoreScriptAcceptsSelfForeignKey(t *testing.T) {
	ddl := `CREATE TABLE public.users (id integer PRIMARY KEY, parent_id integer REFERENCES public.users(id), email text);`
	require.NoError(t, validateSchemaRestoreScript(ddl, "public.users"))
}

func TestSchemaRestoreScriptHandlesPgDumpRestrictPair(t *testing.T) {
	valid := "-- PostgreSQL dump\n" + `\restrict leveepgschema1` + "\n" + restoreTestDDL + `\unrestrict leveepgschema1` + "\n-- complete\n"
	require.NoError(t, validateSchemaRestoreScript(valid, "public.users"))
	for _, invalid := range []string{
		`\restrict leveepgschema1` + "\n" + restoreTestDDL,
		`\restrict other` + "\n" + restoreTestDDL,
		`\quit` + "\n" + restoreTestDDL,
		restoreTestDDL + `\unrestrict leveepgschema1` + "\nSELECT 1;\n",
	} {
		require.Error(t, validateSchemaRestoreScript(invalid, "public.users"), invalid)
	}
}
