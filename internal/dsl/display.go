package dsl

import "strings"

// DisplayName answers "what is this workflow called" for a value whose contract
// is the workflow SOURCE (state.Run.WorkflowName): an inline document, or a
// path, or whatever the writer stored.
//
// It is the display counterpart of parsing — nothing here validates a
// workflow, and a source that does not parse is returned trimmed rather than
// turned into an error, because a label that refuses to render is worse than a
// label that shows the raw string:
//
//   - single-line input (a path, a plain name) is already a name: as-is
//   - multi-line input is parsed and its declared `name:` is used
//   - multi-line input that does not parse, or declares no name: the trimmed
//     source is returned, which is at least honest about what it is
//
// Both readers of a run's WorkflowName need this — the CLI (list/show/audit
// output) and the gRPC change board — so it lives here rather than in either
// of them: two implementations would eventually produce two names for one run,
// the same way two status vocabularies produced two answers for one batch.
func DisplayName(src string) string {
	src = strings.TrimSpace(src)
	if src == "" || !strings.ContainsAny(src, "\n\r") {
		return src
	}
	wf, err := NewParser().ParseBytes([]byte(src))
	if err != nil || wf == nil || wf.Meta.Name == "" {
		return src
	}
	return wf.Meta.Name
}
