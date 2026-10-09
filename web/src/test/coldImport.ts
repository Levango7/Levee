// Owner of COLD_IMPORT_TIMEOUT, the per-suite timeout for specs that re-import
// the module graph (vi.resetModules() + await import(...)).
//
// That cold transform costs ~1s when the machine is idle but was measured at
// 18090ms under load, and vitest's default testTimeout is 5000ms — so the FIRST
// test of such a file fails at random whenever the box is busy. 30s absorbs the
// transform without hiding a real hang (an unresolvable promise still fails,
// just later). Source of the numbers: the runs recorded in the PR description.
//
// One owner, because the first fix was the same constant hand-copied into three
// specs while a fourth spec that re-imports the graph never got one at all.
// coldImport.spec.ts is the guard: it fails when a spec re-imports without
// importing this, when the constant is declared anywhere else, or when its
// value is shrunk back into flake range.
export const COLD_IMPORT_TIMEOUT = 30_000
