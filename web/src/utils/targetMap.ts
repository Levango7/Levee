// Step-status vocabulary and the host × batch matrix for the monitor page.
//
// Why the vocabulary lives here rather than in a switch in MonitorView: these
// strings say what a HOST did ("this host executed the step", "this host was
// skipped"), which is a claim about engine state and has to be testable without
// a browser. The five spellings are the ones internal/writing writes; the Go
// side pins this list against state.StepStatus* (see
// internal/state/step_status_vocabulary_test.go), so the backend cannot add a
// spelling that renders here as a grey blank.
//
// The matrix itself answers a question neither of the two views on this page
// can: the batch bar says "this batch is 60% accounted for", the log says what
// happened last — neither tells you WHICH HOST is in which state. That is the
// view an operator opens an incident with.
/** The five spellings a step row can carry. Source of truth below. */
export const STEP_STATES = ['pending', 'running', 'success', 'failed', 'skipped'] as const
export type StepState = (typeof STEP_STATES)[number]

export type CellTone = 'ok' | 'bad' | 'run' | 'wait' | 'skip'

export const STEP_TONE: Record<StepState, CellTone> = {
  pending: 'wait',
  running: 'run',
  success: 'ok',
  failed: 'bad',
  skipped: 'skip',
}

export const STEP_LABEL: Record<StepState, string> = {
  pending: '未开始',
  running: '执行中',
  success: '成功',
  failed: '失败',
  skipped: '跳过',
}

/** One host's cell in one batch. */
export interface TargetCell {
  state: StepState
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
  worst: StepState
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
const SEVERITY: Record<StepState, number> = {
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
    const state = STEP_STATES.includes(row.status as StepState) ? (row.status as StepState) : 'pending'
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
    let worst: StepState = 'pending'
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
