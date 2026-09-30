package dsl

// run_snapshot_test.go — run 级快照基线的解析与校验（LE098–LE102）。
//
// 这些用例的价值全在 fail-closed 上：run 级基线一旦被接受，运维读到的语义
// 就是「回滚会把这些路径还原」。任何一条放过编译、实则没人恢复或恢复错对象的
// 声明，都比直接报错危险得多——所以每一行拒绝规则都要有用例。

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const runSnapshotYAML = `
name: run-baseline-demo
version: "1.0"
target:
  name: app
  type: host
  hosts: [host-a]
steps:
  - name: apply
    module: file
    action: file.copy
    args: {src: /tmp/a, dst: /etc/app.conf}
rollback:
  on_failure: auto
snapshot:
  scope: run
  paths:
    - /etc/app.conf
    - /etc/app.d/limits.conf
  type: file
`

func TestParseRunSnapshotBlock(t *testing.T) {
	p := NewParser()
	wf, err := p.ParseBytes([]byte(runSnapshotYAML))
	require.NoError(t, err)
	require.NotNil(t, wf.Snapshot, "a top-level snapshot: block must reach the AST")
	assert.Equal(t, "run", wf.Snapshot.Scope)
	assert.Equal(t, []string{"/etc/app.conf", "/etc/app.d/limits.conf"}, wf.Snapshot.Paths)
	assert.Equal(t, "file", wf.Snapshot.Type)

	// The block must survive into the validated workflow unchanged — the
	// run-level baseline is a sibling of rollback, never folded into it.
	errs := NewValidator().Validate(wf)
	assert.Empty(t, errs)
	assert.Empty(t, wf.Rollback.Strategy,
		"a run-level baseline must not leak compensation content into the rollback block")
	assert.Empty(t, wf.Rollback.SnapshotPaths)
}

func TestWorkflowWithoutSnapshotBlockIsUnaffected(t *testing.T) {
	p := NewParser()
	wf, err := p.ParseBytes([]byte(`
name: no-baseline
version: "1.0"
target:
  name: app
  type: host
  hosts: [host-a]
steps:
  - name: apply
    module: file
    action: copy
`))
	require.NoError(t, err)
	assert.Nil(t, wf.Snapshot, "no block means no baseline — not an empty one")
	assert.Empty(t, ValidateRunSnapshot(nil, wf.Rollback, "snapshot"))
}

func TestValidateRunSnapshot(t *testing.T) {
	manual := &RollbackSpec{OnFailure: RollbackOnFailureManual}
	auto := &RollbackSpec{OnFailure: RollbackOnFailureAuto}

	cases := []struct {
		name     string
		spec     *RunSnapshotSpec
		rollback *RollbackSpec
		wantCode string
	}{
		{
			name: "valid run scope",
			spec: &RunSnapshotSpec{Scope: "run", Paths: []string{"/etc/a.conf"}},
		},
		{
			name:     "valid with config type",
			spec:     &RunSnapshotSpec{Scope: "run", Paths: []string{"/etc/a.conf"}, Type: "config"},
			rollback: auto,
		},
		{
			name:     "scope omitted is rejected, not defaulted",
			spec:     &RunSnapshotSpec{Paths: []string{"/etc/a.conf"}},
			wantCode: codeRunSnapshotScope,
		},
		{
			name:     "per-step scope is rejected (that is LE097's hole, one level down)",
			spec:     &RunSnapshotSpec{Scope: "step", Paths: []string{"/etc/a.conf"}},
			wantCode: codeRunSnapshotScope,
		},
		{
			name:     "batch scope is rejected",
			spec:     &RunSnapshotSpec{Scope: "batch", Paths: []string{"/etc/a.conf"}},
			wantCode: codeRunSnapshotScope,
		},
		{
			name:     "no paths",
			spec:     &RunSnapshotSpec{Scope: "run"},
			wantCode: codeRunSnapshotNoPaths,
		},
		{
			name:     "relative path is rejected",
			spec:     &RunSnapshotSpec{Scope: "run", Paths: []string{"etc/a.conf"}},
			wantCode: codeRunSnapshotPathForm,
		},
		{
			name:     "blank path is rejected",
			spec:     &RunSnapshotSpec{Scope: "run", Paths: []string{"  "}},
			wantCode: codeRunSnapshotNoPaths,
		},
		{
			name:     "unknown type is rejected and the refusal names the accepted set",
			spec:     &RunSnapshotSpec{Scope: "run", Paths: []string{"/etc/a.conf"}, Type: "tarball"},
			wantCode: codeRunSnapshotType,
		},
		{
			name:     "declared alongside on_failure: manual is now ALLOWED",
			spec:     &RunSnapshotSpec{Scope: "run", Paths: []string{"/etc/a.conf"}},
			rollback: manual,
			// The manual rollback path restores the baseline, so there is no
			// "captured but never restored" case left to reject.
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			errs := ValidateRunSnapshot(tc.spec, tc.rollback, "snapshot")
			if tc.wantCode == "" {
				assert.Empty(t, errs, "a valid declaration must produce no errors")
				return
			}
			require.NotEmpty(t, errs)
			codes := make([]string, 0, len(errs))
			for _, e := range errs {
				codes = append(codes, e.Code)
			}
			assert.Contains(t, codes, tc.wantCode)
			if tc.wantCode == codeRunSnapshotType {
				// The refusal must list what IS accepted, or it is a dead end
				// for whoever has to fix the document.
				assert.Contains(t, errs[0].Message, "file, config")
			}
		})
	}
}

// 未声明 rollback 块时（缺省 on_failure=auto）基线本身是合法的：缺省策略就是
// 自动补偿，有人会来恢复它。
func TestRunSnapshotWithoutRollbackBlockIsValid(t *testing.T) {
	errs := ValidateRunSnapshot(&RunSnapshotSpec{Scope: "run", Paths: []string{"/etc/a.conf"}}, nil, "snapshot")
	assert.Empty(t, errs)
}
