import { existsSync, readFileSync, readdirSync } from 'node:fs'
import { dirname, join, relative } from 'node:path'

import ts from 'typescript'
import { describe, expect, it } from 'vitest'

import { COLD_IMPORT_TIMEOUT } from './coldImport'

// Structural guard for the flake class this batch fixes: a spec that re-imports
// the module graph (vi.resetModules()) runs its first test against vitest's
// default 5000ms timeout, which the cold transform can exceed under load — the
// test then fails at random. The fix is one owned constant; this file is what
// keeps a fifth spec from silently reintroducing the flake, keeps the constant
// from being copied back into the specs, and keeps its value from being shrunk
// into flake range.
//
// The checks parse the sources (typescript's own parser, so a mention in a
// comment or a string is not a call site — this header names the call), they
// read the files rather than a fixture so they cannot pass on a mock, and they
// hard-fail when they cannot locate web/src or find no specs: a scan that
// silently sees nothing would be worse than no scan.

/** locateSrcDir finds web/src from either web/ or the repository root. */
function locateSrcDir(): string {
  let dir = process.cwd()
  for (let hop = 0; hop < 4; hop++) {
    for (const rel of [join('src'), join('web', 'src')]) {
      const candidate = join(dir, rel)
      // The marker keeps the walk from latching onto an unrelated `src`
      // further up the tree: this must be the web UI's source directory.
      if (existsSync(candidate) && existsSync(join(candidate, 'api', 'client.ts'))) {
        return candidate
      }
    }
    const parent = dirname(dir)
    if (parent === dir) break
    dir = parent
  }
  throw new Error(`cannot locate web/src from ${process.cwd()}`)
}

const srcDir = locateSrcDir()

function specFiles(dir: string): string[] {
  const out: string[] = []
  for (const entry of readdirSync(dir, { withFileTypes: true })) {
    const full = join(dir, entry.name)
    if (entry.isDirectory()) out.push(...specFiles(full))
    else if (entry.name.endsWith('.spec.ts')) out.push(full)
  }
  return out
}

type Spec = {
  /** posix-style, so assertions read the same on Windows and CI */
  path: string
  source: string
  /** calls vi.resetModules() (a real call, not a mention) */
  reimports: boolean
  /** imports COLD_IMPORT_TIMEOUT from the owner module */
  importsTimeout: boolean
  /** applies it to a suite, i.e. `{ timeout: COLD_IMPORT_TIMEOUT }` */
  appliesTimeout: boolean
  /** declares a COLD_IMPORT_TIMEOUT of its own */
  declaresLocalCopy: boolean
}

function inspect(path: string, source: string): Spec {
  const sf = ts.createSourceFile(path, source, ts.ScriptTarget.ES2020, true)
  const spec: Spec = {
    path: relative(srcDir, path).split('\\').join('/'),
    source,
    reimports: false,
    importsTimeout: false,
    appliesTimeout: false,
    declaresLocalCopy: false,
  }
  const visit = (node: ts.Node): void => {
    if (ts.isCallExpression(node)) {
      const callee = node.expression
      if (ts.isPropertyAccessExpression(callee) && callee.name.text === 'resetModules') spec.reimports = true
    }
    if (ts.isImportDeclaration(node) && ts.isStringLiteral(node.moduleSpecifier) && node.moduleSpecifier.text.endsWith('coldImport')) {
      const bindings = node.importClause?.namedBindings
      if (bindings && ts.isNamedImports(bindings) && bindings.elements.some((e) => e.name.text === 'COLD_IMPORT_TIMEOUT')) {
        spec.importsTimeout = true
      }
    }
    if (ts.isPropertyAssignment(node) && ts.isIdentifier(node.name) && node.name.text === 'timeout' && ts.isIdentifier(node.initializer) && node.initializer.text === 'COLD_IMPORT_TIMEOUT') {
      spec.appliesTimeout = true
    }
    if (ts.isVariableStatement(node)) {
      for (const decl of node.declarationList.declarations) {
        if (ts.isIdentifier(decl.name) && decl.name.text === 'COLD_IMPORT_TIMEOUT') spec.declaresLocalCopy = true
      }
    }
    ts.forEachChild(node, visit)
  }
  visit(sf)
  return spec
}

const specs = specFiles(srcDir).map((p) => inspect(p, readFileSync(p, 'utf8')))

const KNOWN_REIMPORTING = [
  'api/conversation.spec.ts',
  'api/cluster.spec.ts',
  'api/client.spec.ts',
  'composables/useTheme.spec.ts',
]

describe('COLD_IMPORT_TIMEOUT ownership', () => {
  it('finds the specs (an empty scan must fail, not pass)', () => {
    expect(specs.length).toBeGreaterThanOrEqual(4)
    const paths = specs.map((s) => s.path)
    for (const known of KNOWN_REIMPORTING) {
      expect(paths, `scan missed ${known}`).toContain(known)
    }
  })

  it('every spec that re-imports the module graph applies the owned timeout', () => {
    // Both directions: the known four are still detected as re-importing (so a
    // parser change cannot make this vacuous), and nothing else is missed.
    for (const known of KNOWN_REIMPORTING) {
      expect(specs.find((s) => s.path === known)?.reimports, `${known} no longer detected as re-importing`).toBe(true)
    }
    const reimporting = specs.filter((s) => s.reimports)
    expect(reimporting.map((s) => s.path).sort()).toEqual([...KNOWN_REIMPORTING].sort())
    for (const spec of reimporting) {
      expect(spec.importsTimeout, `${spec.path} re-imports the module graph without the owned timeout`).toBe(true)
      expect(spec.appliesTimeout, `${spec.path} imports the timeout but never applies it to a suite`).toBe(true)
    }
  })

  it('is declared only in its owner (no hand-copies back into specs)', () => {
    for (const spec of specs) {
      expect(spec.declaresLocalCopy, `${spec.path} declares its own COLD_IMPORT_TIMEOUT`).toBe(false)
    }
    const owner = readFileSync(join(srcDir, 'test', 'coldImport.ts'), 'utf8')
    expect(owner, 'the owner module test/coldImport.ts does not declare/export the constant').toMatch(
      /export\s+const\s+COLD_IMPORT_TIMEOUT\s*=/,
    )
  })

  it('has not been shrunk back into flake range', () => {
    // Parsed from the owner source, not from the import: a shrink must be
    // judged by what ships. 30s is the measured-under-load floor (18090ms).
    const owner = readFileSync(join(srcDir, 'test', 'coldImport.ts'), 'utf8')
    const declared = /export\s+const\s+COLD_IMPORT_TIMEOUT\s*=\s*([\d_]+)/.exec(owner)
    expect(declared, 'cannot parse the declared value out of the owner').not.toBeNull()
    const value = Number((declared?.[1] ?? '').replace(/_/g, ''))
    expect(value).toBeGreaterThanOrEqual(30_000)
    // And the import the specs use must be that same value.
    expect(COLD_IMPORT_TIMEOUT).toBe(value)
  })
})
