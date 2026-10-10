// The partial-rollback remediation flow, shared by the change list and the
// change detail page.
//
// Extracted rather than copied: the flow has a real order to it (preview with
// dry_run, show the evidence, ask once, then commit through the same endpoint),
// and two views each carrying their own copy is how the preview/commit pairing
// drifts — the failure mode this whole area is about. The list and the detail
// page now differ only in what they do after a successful commit (refresh a
// table, or refresh the page data).
import { h } from 'vue'
import { ElMessage, ElMessageBox } from 'element-plus'

import { changesApi } from '@/api'
import type { Change } from '@/types/levee'
import { STATUS_LABEL } from '@/utils/format'

export interface RollbackPreview {
  willFix: string[]
  skipped: string[]
  message: string
}

/** previewRollback asks the endpoint what a remediation WOULD do, without
 *  doing it. The endpoint's dry_run is the only source of this evidence: it
 *  knows which hosts already compensated (skipped) and which still need it. */
export async function previewRollback(change: Change): Promise<RollbackPreview | null> {
  try {
    const preview = await changesApi.rollback(change.id, { dryRun: true })
    return {
      willFix: preview.rolledBackHosts || [],
      skipped: preview.skippedHosts || [],
      message: preview.message || '',
    }
  } catch (err) {
    ElMessage.error(`补救预览 ${change.label} 失败：${(err as { message?: string })?.message}`)
    return null
  }
}

/** remediationDialog builds the confirmation body as VNodes. Host names and
 *  the server's message are data: `dangerouslyUseHTMLString` would turn them
 *  into markup. */
export function remediationDialog(change: Change, preview: RollbackPreview) {
  const note = { style: 'margin: 8px 0 0; color: var(--el-text-color-secondary)' }
  return h('div', [
    h(
      'p',
      { style: 'margin: 0' },
      `变更「${change.label}」（${STATUS_LABEL[change.status]}）的回滚没有走完：` +
        `${preview.willFix.length} 台待补偿，${preview.skipped.length} 台将跳过。`,
    ),
    h(
      'p',
      { style: 'margin: 8px 0 0' },
      preview.willFix.length > 0
        ? `待补偿：${preview.willFix.join('、')}`
        : '没有待补偿的主机——继续执行不会改变任何主机。',
    ),
    preview.skipped.length > 0
      ? h('p', { style: 'margin: 8px 0 0' }, `将跳过：${preview.skipped.join('、')}`)
      : null,
    preview.message ? h('p', note, preview.message) : null,
    h(
      'p',
      note,
      '补救＝把没走完的回滚补完（幂等，可重复执行）；要重新执行这次变更的前向步骤请用「重试」。',
    ),
  ])
}

/**
 * remediateRollback runs the whole flow and reports whether a commit happened,
 * so the caller can refresh whatever it is showing. It never throws: a refused
 * preview, a cancelled dialog and a failed commit all end in a message.
 */
export async function remediateRollback(change: Change): Promise<boolean> {
  const preview = await previewRollback(change)
  if (!preview) return false
  try {
    await ElMessageBox.confirm(remediationDialog(change, preview), '补救未完成的回滚', {
      confirmButtonText: '现在补',
      cancelButtonText: '取消',
      type: 'warning',
    })
  } catch {
    return false // cancelled
  }
  try {
    const res = await changesApi.rollback(change.id, {})
    ElMessage.success(
      `补救已提交：补偿 ${res.rolledBackHosts?.length ?? 0} 台，回滚 run ${res.rollbackRunId || '—'}`,
    )
    return true
  } catch (err) {
    ElMessage.error(`补救 ${change.label} 失败：${(err as { message?: string })?.message}`)
    return false
  }
}
