package pg

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nexus/levee/internal/channel"
)

const parentSetDDL = `CREATE TABLE public.parent (
    id integer GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    child_id integer
);
ALTER TABLE ONLY public.parent
    ADD CONSTRAINT parent_child_fk FOREIGN KEY (child_id) REFERENCES public.child(id);
`

const childSetDDL = `CREATE TABLE public.child (
    id integer PRIMARY KEY,
    parent_id integer
);
ALTER TABLE ONLY public.child
    ADD CONSTRAINT child_parent_fk FOREIGN KEY (parent_id) REFERENCES public.parent(id);
`

func snapshotSetForTest(t *testing.T) string {
	t.Helper()
	parent, err := encodeSchemaSnapshot("public.parent", parentSetDDL)
	require.NoError(t, err)
	child, err := encodeSchemaSnapshot("public.child", childSetDDL)
	require.NoError(t, err)
	raw, err := json.Marshal([]string{parent, child})
	require.NoError(t, err)
	return string(raw)
}

func TestSchemaRestoreSetDefersForeignKeysAndVerifies(t *testing.T) {
	set := snapshotSetForTest(t)
	ch := &scriptedPGChannel{results: []channel.ExecResult{
		{ExitCode: 0, Stdout: "|\n"},
		{ExitCode: 0},
		{ExitCode: 0, Stdout: base64.StdEncoding.EncodeToString([]byte(childSetDDL))},
		{ExitCode: 0, Stdout: base64.StdEncoding.EncodeToString([]byte(parentSetDDL))},
	}}
	out, err := New().Execute(context.Background(), "schema_restore", pgInput(ch, map[string]any{
		"snapshots": set,
	}))
	require.NoError(t, err)
	assert.True(t, out.Changed)
	require.Len(t, ch.execs, 4)
	script := decodedPayload(t, ch.execs[1])
	assert.Contains(t, script, "BEGIN;")
	assert.Contains(t, script, "COMMIT;")
	firstConstraint := strings.Index(script, "ADD CONSTRAINT")
	lastCreate := strings.LastIndex(script, "CREATE TABLE")
	assert.Greater(t, firstConstraint, lastCreate, "every base table must exist before any cross-table FK is added")
	assert.NotContains(t, script[:firstConstraint], "FOREIGN KEY", "base phase must not contain cross-table FKs")
	assert.Contains(t, script[firstConstraint:], "parent_child_fk")
	assert.Contains(t, script[firstConstraint:], "child_parent_fk")
}

func TestSchemaRestoreSetRejectsUnsafeInputsBeforeDialing(t *testing.T) {
	t.Run("external reference", func(t *testing.T) {
		ddl := strings.Replace(childSetDDL, "public.parent(id)", "public.other(id)", 1)
		raw, err := json.Marshal([]string{manifestForTest(t, "public.child", ddl)})
		require.NoError(t, err)
		ch := &scriptedPGChannel{}
		_, err = New().Execute(context.Background(), "schema_restore", pgInput(ch, map[string]any{"snapshots": string(raw)}))
		require.ErrorContains(t, err, "outside the snapshot set")
		assert.Empty(t, ch.execs)
	})
	t.Run("inline cross table foreign key", func(t *testing.T) {
		ddl := "CREATE TABLE public.child (id integer PRIMARY KEY, parent_id integer REFERENCES public.parent(id));\n"
		raw, err := json.Marshal([]string{manifestForTest(t, "public.child", ddl)})
		require.NoError(t, err)
		ch := &scriptedPGChannel{}
		_, err = New().Execute(context.Background(), "schema_restore", pgInput(ch, map[string]any{"snapshots": string(raw)}))
		require.ErrorContains(t, err, "inline cross-table foreign key")
		assert.Empty(t, ch.execs)
	})
	t.Run("single payload cross table still requires set semantics", func(t *testing.T) {
		ch := &scriptedPGChannel{}
		_, err := New().Execute(context.Background(), "schema_restore", pgInput(ch, map[string]any{
			"table": "public.child", "snapshot": manifestForTest(t, "public.child", childSetDDL),
		}))
		require.ErrorContains(t, err, "snapshot set")
		assert.Empty(t, ch.execs)
	})
}

func TestSchemaRestoreSetChecksAllRelationsBeforeApplying(t *testing.T) {
	ch := &scriptedPGChannel{results: []channel.ExecResult{{ExitCode: 0, Stdout: "public.parent|\n"}}}
	_, err := New().Execute(context.Background(), "schema_restore", pgInput(ch, map[string]any{
		"snapshots": snapshotSetForTest(t),
	}))
	require.ErrorContains(t, err, "already exists")
	assert.Len(t, ch.execs, 1, "no CREATE may run when any relation in the set already exists")
}

func TestSchemaRestoreSetApplyFailureDoesNotVerify(t *testing.T) {
	ch := &scriptedPGChannel{results: []channel.ExecResult{
		{ExitCode: 0, Stdout: "|\n"},
		{ExitCode: 2, Stderr: "constraint failed"},
	}}
	out, err := New().Execute(context.Background(), "schema_restore", pgInput(ch, map[string]any{
		"snapshots": snapshotSetForTest(t),
	}))
	require.ErrorContains(t, err, "constraint failed")
	assert.False(t, out.Changed)
	assert.Len(t, ch.execs, 2, "a failed transaction must not enter the per-table verification phase")
}
