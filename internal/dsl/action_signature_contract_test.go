package dsl_test

// action_signature_contract_test.go — the compile-time signature table and the
// runtime modules must name the same args.
//
// The table (internal/dsl/typechecker.go, actionSignatures) is what a workflow
// is checked against; the module is what actually runs. When the two disagree,
// the disagreement is invisible until run time: the compiler green-lights a
// workflow whose arg the module never reads, and the failure surfaces as a
// run-time error on a real target.
//
// file.copy was exactly that. The table said "dst"; the module reads "dest"
// (internal/executor/modules/file/file.go). Every shipped workflow writes
// "dest", so nothing in the assets caught it — and a workflow written with
// "dst" would have passed strict type checking (the key IS declared) and then
// died on the target with "missing argument dest".
//
// This file lives in package dsl_test (external) because it needs the executor
// modules, and internal/executor imports internal/dsl (executor/irreversible.go)
// — from inside package dsl that would close an import cycle.

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nexus/levee/internal/executor"
	"github.com/nexus/levee/internal/executor/modules/file"
)

// TestFileCopyRuntimeArgNameMatchesSignature pins the runtime half of the
// file.copy contract: the module reads "dest" and nothing else.
//
// The three cases below are read as a pair with the registry side (the
// internal test TestFileCopySignatureUsesRuntimeArgName): the name the module
// requires is exactly the name the signature table declares. Cases 1 and 2 say
// "dest" is required; case 3 says "dest" is accepted (the call gets past the
// argument gate and fails later, on the source file that does not exist).
func TestFileCopyRuntimeArgNameMatchesSignature(t *testing.T) {
	cases := []struct {
		name    string
		args    map[string]any
		wantErr string
	}{
		{
			name:    "dest 缺了就拒",
			args:    map[string]any{"src": "lab-site.conf"},
			wantErr: `missing argument "dest"`,
		},
		{
			// dst is the name the signature table used to advertise. The
			// module never reads it, so a workflow written that way passes
			// strict type checking and then dies here. This is the concrete
			// reason the table had to be corrected.
			name:    "dst 不顶用",
			args:    map[string]any{"src": "lab-site.conf", "dst": "/etc/nginx/http.d/lab-site.conf"},
			wantErr: `missing argument "dest"`,
		},
		{
			// dest is accepted: the call moves past the argument gate and
			// fails on the local source file instead.
			name:    "dest 收下了",
			args:    map[string]any{"src": "no-such-file-in-this-dir.conf", "dest": "/etc/nginx/http.d/lab-site.conf"},
			wantErr: "read src",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := file.New().Execute(context.Background(), "copy", executor.ModuleInput{Args: tc.args})
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.wantErr)
		})
	}
}
