package mysql

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const restoreSnapshot = "CREATE TABLE `users` (\n  `id` int NOT NULL\n)"

func TestSchemaRestoreCreatesOnlyMissingTableAndVerifies(t *testing.T) {
	ch := &mockChannel{execResponses: []execResponse{
		{result: okResult("0")},
		{result: okResult("")},
		{result: okResult(restoreSnapshot)},
	}}
	out, err := New().Execute(context.Background(), "schema_restore", newInput(ch, map[string]any{
		"table": "users", "snapshot": restoreSnapshot,
	}))
	require.NoError(t, err)
	require.NotNil(t, out)
	assert.True(t, out.Changed, "a real CREATE happened and was verified")
	assert.Equal(t, 0, out.ExitCode)
	assert.Equal(t, 3, ch.execCount(), "existence check, CREATE, verification SHOW CREATE")
	assert.Contains(t, decodedPayload(t, ch.execAt(0)), "information_schema.tables")
	assert.Contains(t, decodedPayload(t, ch.execAt(0)), "DATABASE()")
	assert.Contains(t, decodedPayload(t, ch.execAt(1)), restoreSnapshot)
	assert.Contains(t, decodedPayload(t, ch.execAt(2)), "SHOW CREATE TABLE `users`")
}

func TestSchemaRestoreRefusesAnExistingTable(t *testing.T) {
	ch := &mockChannel{execResponses: []execResponse{{result: okResult("1")}}}
	_, err := New().Execute(context.Background(), "schema_restore", newInput(ch, map[string]any{
		"table": "users", "snapshot": restoreSnapshot,
	}))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "already exists")
	assert.Equal(t, 1, ch.execCount(), "nothing is overwritten or dropped")
}

func TestSchemaRestoreRejectsMismatchedSnapshotBeforeDialing(t *testing.T) {
	ch := &mockChannel{}
	_, err := New().Execute(context.Background(), "schema_restore", newInput(ch, map[string]any{
		"table": "users", "snapshot": "CREATE TABLE `orders` (id int)",
	}))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "does not match")
	assert.Empty(t, ch.execs)
}

func TestSchemaRestoreRejectsMultipleStatementsBeforeDialing(t *testing.T) {
	ch := &mockChannel{}
	_, err := New().Execute(context.Background(), "schema_restore", newInput(ch, map[string]any{
		"table": "users", "snapshot": restoreSnapshot + "; DROP TABLE users",
	}))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "exactly one")
	assert.Empty(t, ch.execs)
}

func TestSchemaRestoreApplyFailureIsNotRecovery(t *testing.T) {
	ch := &mockChannel{execResponses: []execResponse{
		{result: okResult("0")},
		{result: failResult("table definition is invalid")},
	}}
	_, err := New().Execute(context.Background(), "schema_restore", newInput(ch, map[string]any{
		"table": "users", "snapshot": restoreSnapshot,
	}))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "apply snapshot failed")
	assert.Equal(t, 2, ch.execCount(), "no verification is claimed after a failed CREATE")
}

func TestSchemaRestoreVerificationMismatchIsNotRecovery(t *testing.T) {
	ch := &mockChannel{execResponses: []execResponse{
		{result: okResult("0")},
		{result: okResult("")},
		{result: okResult("CREATE TABLE `users` (\n  `id` int NOT NULL,\n  `note` text\n)")},
	}}
	_, err := New().Execute(context.Background(), "schema_restore", newInput(ch, map[string]any{
		"table": "users", "snapshot": restoreSnapshot,
	}))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "verification mismatch")
}

func TestSchemaRestoreQualifiedTableUsesItsSchema(t *testing.T) {
	snapshot := "CREATE TABLE `shop`.`users` (id int)"
	ch := &mockChannel{execResponses: []execResponse{
		{result: okResult("0")},
		{result: okResult("")},
		{result: okResult(snapshot)},
	}}
	_, err := New().Execute(context.Background(), "schema_restore", newInput(ch, map[string]any{
		"table": "shop.users", "snapshot": snapshot,
	}))
	require.NoError(t, err)
	assert.Contains(t, decodedPayload(t, ch.execAt(0)), "table_schema = 'shop'")
	assert.Contains(t, decodedPayload(t, ch.execAt(0)), "table_name = 'users'")
}
