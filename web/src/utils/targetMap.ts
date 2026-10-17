// The host × batch matrix for the monitor page, over the generated step vocabulary.
//
// Why the matrix lives here rather than in a switch in MonitorView: these strings
// say what a HOST did ("this host executed the step", "this host was skipped"),
// which is a claim about engine state and has to be testable without a browser.
//
// The five spellings used to be a hand copy in this file, with a comment pointing at
// `internal/state/step_status_vocabulary_test.go` as the thing pinning them. Two
// things were wrong with that: the file it names does not exist (the real one is
// step_approval_status_vocabulary_test.go, and it pins the WRITERS, not this copy),
// and a copy of a list cannot be pinned by parsing the copy — that is #153's shape,
// where four artefacts agreed with each other and disagreed with the server while
// every guard stayed green.
//
// So the copy is gone. STEP_STATES is generated into ./types/vocabulary.ts from
// internal/state.StepStatusNames() by internal/docgen, and the completeness claim
// sits where it belongs: internal/state/vocabulary_ownership_test.go fails if
// store.go declares a StepStatus constant that its order list does not name.
import { STEP_STATES } from '@/types/vocabulary'
import type { StepStatus } from '@/types/vocabulary'

// The five spellings a step row can carry, GENERATED from
// internal/state.StepStatusNames() by internal/docgen and re-exported here — the
// import + export pair (not `export … from`) is what keeps the name usable in the
// keyed tables below, and matches ./batch.ts and ./assignment.ts.
//
// Those keyed `Record`s are the reason a list and not a switch lives here: "no
// label / no colour for the new state" is a compile error rather than a grey cell
// with the English word on it.
export { STEP_STATES }
export type { StepStatus }

export type CellTone = 'ok' | 'bad' | 'run' | 'wait' | 'skip'

export const STEP_TONE: Record<StepStatus, CellTone> = {
  pending: 'wait',
  running: 'run',
  success: 'ok',
  failed: 'bad',
  skipped: 'skip',
}

export const STEP_LABEL: Record<StepStatus, string> = {
  pending: '未开始',
  running: '执行中',
  success: '成功',
  failed: '失败',
  skipped: '跳过',
}

/** One host's cell in one batch. */
export interface TargetCell {
  state: StepStatus
  /** The step that produced this state, when the row carries one. */
  stepName: string
  durationMs: number
  exitCode?: number
}

/** One host's row across the whole matrix. */
export interface TargetRow {
  host: string
  /** Cells by batch number; a host with no row in a batch is not in it. */
  cells: Record<number, TargetCell>
  batches: number[]
  /** Worst state this host carries, for the row summary. */
  worst: StepStatus
}

export interface TargetMap {
  /** Batch numbers in plan order, including batches with no step rows. */
  batchNos: number[]
  rows: TargetRow[]
  /** Hosts that appear in the plan but have no step row yet. */
  untouched: string[]
}

// The severity order a row's summary uses: a host is as bad as its worst cell,
// which is the number an operator reads first when scanning for red.
const SEVERITY: Record<StepStatus, number> = {
  failed: 4,
  running: 3,
  pending: 2,
  skipped: 1,
  success: 0,
}

/**
 * buildTargetMap joins the plan's batch→host assignment with the step rows the
 * engine wrote. Both sides are needed: the plan says who SHOULD run in which
 * batch (so a host that has not started still has a place in the matrix), the
 * steps say what actually happened.
 */
export function buildTargetMap(
  planBatches: Array<{ index: number; hosts: string[] }>,
  stepRows: Array<{
    host: string
    batchNo: number
    status: string
    stepName: string
    durationMs: number
    exitCode?: number
  }>,
  batchProgress: Array<{ batch_no: number }>,
): TargetMap {
  const cells = new Map<string, Map<number, TargetCell>>()
  const batchNos = new Set<number>()

  // The plan fixes the row order and which batches exist at all — including
  // batches with no steps (a plan that has not run yet must still render).
  for (const b of planBatches) {
    batchNos.add(b.index)
    for (const host of b.hosts) {
      if (!cells.has(host)) cells.set(host, new Map())
    }
  }
  // A batch that exists only as progress rows (pre-plan or plan-lost) still
  // deserves its column, otherwise the map hides work that happened.
  for (const b of batchProgress) {
    batchNos.add(b.batch_no)
  }

  for (const row of stepRows) {
    if (!cells.has(row.host)) cells.set(row.host, new Map())
    const perBatch = cells.get(row.host)!
    // The row arrives as text straight off the wire, so an unrecognised spelling
    // is possible in principle even though the writers only emit the five. It is
    // rendered as 未开始 — a rendering choice for "nothing here yet", NOT a claim
    // that the engine said pending; the difference is only visible in the source.
    const state = STEP_STATES.includes(row.status as StepStatus) ? (row.status as StepStatus) : 'pending'
    perBatch.set(row.batchNo, {
      state,
      stepName: row.stepName,
      durationMs: row.durationMs,
      exitCode: row.exitCode,
    })
    batchNos.add(row.batchNo)
  }

  const nos = [...batchNos].sort((a, b) => a - b)
  const rows: TargetRow[] = []
  const untouched: string[] = []
  for (const [host, perBatch] of cells) {
    const hostBatches = [...perBatch.keys()].sort((a, b) => a - b)
    if (hostBatches.length === 0) untouched.push(host)
    // A host with no row has not started, which reads as pending rather than as
    // success: "nothing happened yet" is not "everything went well".
    let worst: StepStatus = 'pending'
    if (perBatch.size > 0) {
      // The floor is success once a host has rows: a succeeded host must not
      // be summarised as pending just because pending outranks success.
      worst = 'success'
      for (const cell of perBatch.values()) {
        if (SEVERITY[cell.state] > SEVERITY[worst]) worst = cell.state
      }
    }
    const cellRecord: Record<number, TargetCell> = {}
    for (const b of hostBatches) cellRecord[b] = perBatch.get(b)!
    rows.push({ host, cells: cellRecord, batches: hostBatches, worst })
  }
  // Hosts with work first, then the rest alphabetically: a map that puts the
  // twelve unstarted hosts above the one that is failing is a map nobody reads.
  rows.sort((a, b) => SEVERITY[b.worst] - SEVERITY[a.worst] || a.host.localeCompare(b.host))
  return { batchNos: nos, rows, untouched }
}

/** cellSummary renders one cell's line for the tooltip / detail table. */
export function cellSummary(cell: TargetCell): string {
  const parts = [STEP_LABEL[cell.state]]
  if (cell.stepName) parts.push(cell.stepName)
  if (cell.durationMs > 0) parts.push(`${cell.durationMs} ms`)
  if (cell.exitCode !== undefined) parts.push(`exit=${cell.exitCode}`)
  return parts.join(' · ')
}

// ---------------------------------------------------------------------------
// Row windowing for the target map
// ---------------------------------------------------------------------------

/**
 * The target map's cost is O(hosts × batches) DOM nodes, and it is the one
 * surface in the console with no natural ceiling: a fleet-wide change over 2000
 * hosts × 12 batches measured 28k cells / 56k nodes / a 61ms worst frame
// (measured on this repo's own dev server — see the scaling table below). The
 * list pages do not have this problem — they are paginated at ≤200 rows.
 *
 * So the map renders only the visible slice plus an overscan, and two spacer
 * rows hold the scrollbar's place. The maths is here rather than in the
 * component because "which rows are visible" is exactly the kind of thing that
 * is wrong in a screenshot and right in a unit test.
 *
 * Measured on this repo's own dev server (1600px viewport, dev build, 2026-10-12):
 * the pitch is 27px, not the 21px a quick look at the CSS suggests — a 6px
 * miss over 300 rows is a 1800px scrollbar drift. The value is pinned in
 * targetMap.spec.ts with the same provenance, so a CSS change that moves it
 * fails a test rather than quietly mis-aligning the window.
 *
 * The OLD pitch was 64px, and that was a layout bug rather than a row height:
 * the map used to live in the 400px side column, where twelve 72px batch
 * columns squeezed the "主机结论" verdict column to 34px and every chip wrapped
 * to four lines. Widening the verdict column (and the host column) took the
 * pitch to 27px before any windowing was written.
 */
export const MAP_ROW_PITCH = 27

/** Rows rendered above and below the viewport, so a quick flick never shows a gap. */
export const MAP_OVERSCAN = 8

export interface MapWindow {
  /** First row index to render. */
  start: number
  /** Number of rows to render. */
  count: number
  /** Spacer heights in pixels, so the scrollbar matches the full row count. */
  padTop: number
  padBottom: number
  /** True when windowing is actually active (i.e. the list exceeds the window). */
  windowed: boolean
}

/**
 * windowRange answers "which rows are visible" from raw scroll geometry.
 *
 * total is the full row count, scrollTop the container's scroll offset,
 * viewportHeight the container's visible height. All three are plain numbers so
 * the function is testable without a DOM — and callers that have not laid out
 * yet (viewportHeight 0) get the first page rather than nothing, because a map
 * that renders zero rows on first paint reads as "no targets".
 */
export function windowRange(
  total: number,
  scrollTop: number,
  viewportHeight: number,
  rowPitch: number = MAP_ROW_PITCH,
  overscan: number = MAP_OVERSCAN,
): MapWindow {
  if (total <= 0 || rowPitch <= 0) {
    return { start: 0, count: 0, padTop: 0, padBottom: 0, windowed: false }
  }
  // Not laid out yet: show the first window instead of an empty map.
  if (viewportHeight <= 0) {
    viewportHeight = rowPitch * 20
  }
  // first is clamped to the last row as well as the first: a caller that hands
  // us a scrollTop beyond the content (a stale handler firing after the list
  // shrank, a test) must not walk the window off the end. Without this clamp
  // the arithmetic produced start+count past the end and a negative padBottom.
  const first = Math.min(
    Math.max(0, Math.floor(scrollTop / rowPitch) - overscan),
    Math.max(0, total - 1),
  )
  const visible = Math.ceil(viewportHeight / rowPitch) + overscan * 2
  const count = Math.max(1, Math.min(visible, total - first))
  const windowed = count < total
  return {
    start: first,
    count,
    padTop: first * rowPitch,
    padBottom: Math.max(0, (total - first - count) * rowPitch),
    windowed,
  }
}
