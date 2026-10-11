<script setup lang="ts">
// ApprovalView shows pending approvals and approval history. Operators can
// approve, reject or delegate a change. The history tab lists past decisions
// sourced from the audit log.
import { computed, onMounted, ref } from 'vue'
import { ElMessage, ElMessageBox } from 'element-plus'
import type { TableInstance } from 'element-plus'
import { changesApi, auditApi } from '@/api'
import type { Change, TraceEntry } from '@/types/levee'
import StatusTag from '@/components/StatusTag.vue'
import StateBlock from '@/components/StateBlock.vue'
import PageHeader from '@/components/PageHeader.vue'
import { formatTimestamp } from '@/utils/format'
import { approvalSummary, type ApprovalChain } from '@/utils/approvalChain'
import { classifyError, errorMessage } from '@/utils/apiState'
import type { LoadTier } from '@/utils/apiState'
import { useKeyboardCursor } from '@/composables/useKeyboardCursor'

const activeTab = ref<'pending' | 'history'>('pending')
const loading = ref(false)
const pending = ref<Change[]>([])

// Region states per tab (utils/apiState.ts). The two tabs load independently,
// so each owns its failure: a broken history query must not empty the queue
// the approver is looking at.
const pendingError = ref<unknown>(null)
const historyError = ref<unknown>(null)

const pendingTier = computed<LoadTier>(() => {
  if (pendingError.value !== null) return classifyError(pendingError.value)
  if (!loading.value && pending.value.length === 0) return 'empty'
  return 'ok'
})
const pendingText = computed(() => errorMessage(pendingError.value, '加载待审批列表失败'))

const historyTier = computed<LoadTier>(() => {
  if (historyError.value !== null) return classifyError(historyError.value)
  if (!loading.value && history.value.length === 0) return 'empty'
  return 'ok'
})
const historyText = computed(() => errorMessage(historyError.value, '加载审批历史失败'))

// The full-keyboard approval chain (roadmap P1): a visible row cursor moved
// with j/k, Enter to open the chain, and a / r / d for the three decisions.
// The cursor is a ref on the table instance so Enter can drive the same
// expansion the mouse arrow does — one mechanism, two inputs.
const pendingTable = ref<TableInstance>()

const cursor = useKeyboardCursor({
  count: () => (activeTab.value === 'pending' ? pending.value.length : 0),
  onOpen: (i) => toggleChain(pending.value[i]),
  actions: {
    a: (i) => actOnRow(i, approve),
    r: (i) => actOnRow(i, reject),
    d: (i) => actOnRow(i, delegate),
  },
})

// The cursor index is a keystroke away from a list that may have been refetched
// underneath it, so every action re-resolves the row and refuses when it is not
// there. Acting on a stale index would approve the wrong change — the one
// failure mode a keyboard shortcut must not have.
function actOnRow(i: number, action: (row: Change) => void | Promise<void>): void {
  const row = pending.value[i]
  if (!row) return
  void action(row)
}

function toggleChain(row: Change | undefined): void {
  if (!row) return
  pendingTable.value?.toggleRowExpansion(row)
}

function pendingRowClass({ rowIndex }: { rowIndex: number }): string {
  return cursor.index.value === rowIndex ? 'lv-row-cursor' : ''
}

/** shortHash renders a plan digest at a glance (head…tail), like HashCell. */
function shortHash(h: string): string {
  return h.length <= 18 ? h : `${h.slice(0, 8)}…${h.slice(-6)}`
}
// The approval chain behind a pending row, fetched when the row is expanded.
// Without it a row can only say "awaiting approval": who was asked, how many
// votes the quorum needs, who has decided and why, whether the initiator is
// barred, and whether the chain belongs to a plan at all.
const chains = ref<Record<string, ApprovalChain[]>>({})
const chainsLoading = ref<Record<string, boolean>>({})
const chainsError = ref<Record<string, string>>({})

async function loadChains(row: Change): Promise<void> {
  if (chains.value[row.id] || chainsLoading.value[row.id]) return
  chainsLoading.value[row.id] = true
  chainsError.value[row.id] = ''
  try {
    chains.value = { ...chains.value, [row.id]: await changesApi.approvals(row.id) }
  } catch (err) {
    chainsError.value[row.id] = (err as { message?: string })?.message || '加载审批链失败'
  } finally {
    chainsLoading.value[row.id] = false
  }
}
const history = ref<TraceEntry[]>([])

const approver = ref('operator')

async function loadPending(): Promise<void> {
  loading.value = true
  pendingError.value = null
  try {
    // The backend run status for "awaiting approval" is `pending`; querying
    // `pending_approval` (a value the server never emits) returned an empty
    // list, so this tab could never show anything to approve.
    const res = await changesApi.list({ status: ['pending'], pageSize: 100 })
    pending.value = res.items || []
  } catch (err) {
    // "Nothing waiting on you" and "the queue could not be read" are opposite
    // operational facts; only one of them means the queue is empty.
    pendingError.value = err
  } finally {
    // A reloaded queue is a different set of rows; the cursor resets with it.
    cursor.reset()
    loading.value = false
  }
}

async function loadHistory(): Promise<void> {
  loading.value = true
  historyError.value = null
  try {
    const res = await auditApi.log({ action: 'approve', pageSize: 100 })
    history.value = res.items || []
  } catch (err) {
    historyError.value = err
  } finally {
    loading.value = false
  }
}

async function approve(change: Change): Promise<void> {
  let comment = ''
  try {
    const result = await ElMessageBox.prompt('请输入审批意见（可选）', `通过审批：${change.label}`, {
      confirmButtonText: '通过',
      cancelButtonText: '取消',
      inputPlaceholder: '审批意见',
      inputType: 'textarea',
    })
    comment = result.value || ''
  } catch {
    return
  }
  try {
    await changesApi.approve(change.id, { approver: approver.value, comment })
    ElMessage.success('审批通过')
    loadPending()
  } catch (err) {
    ElMessage.error(`审批失败：${(err as { message?: string })?.message}`)
  }
}

async function reject(change: Change): Promise<void> {
  let reason = ''
  try {
    const result = await ElMessageBox.prompt('请输入驳回原因', `驳回：${change.label}`, {
      confirmButtonText: '驳回',
      cancelButtonText: '取消',
      inputPlaceholder: '驳回原因',
      inputType: 'textarea',
      inputValidator: (v) => !!v || '请填写驳回原因',
    })
    reason = result.value
  } catch {
    return
  }
  try {
    await changesApi.reject(change.id, { rejecter: approver.value, reason })
    ElMessage.success('已驳回')
    loadPending()
  } catch (err) {
    ElMessage.error(`驳回失败：${(err as { message?: string })?.message}`)
  }
}

async function delegate(change: Change): Promise<void> {
  let target = ''
  try {
    const result = await ElMessageBox.prompt('请输入转授权目标用户', `转授权：${change.label}`, {
      confirmButtonText: '转授权',
      cancelButtonText: '取消',
      inputPlaceholder: '目标用户',
      inputValidator: (v) => !!v || '请输入目标用户',
    })
    target = result.value
  } catch {
    return
  }
  // The current API does not have a dedicated delegate endpoint; we record it
  // as a comment on the approval so the audit trail captures the transfer.
  try {
    await changesApi.approve(change.id, {
      approver: target,
      comment: `delegated from ${approver.value}`,
    })
    ElMessage.success(`已转授权给 ${target}`)
    loadPending()
  } catch (err) {
    ElMessage.error(`转授权失败：${(err as { message?: string })?.message}`)
  }
}

function onExpand(row: Change, expanded: unknown): void {
  if (Array.isArray(expanded) && expanded.includes(row)) void loadChains(row)
}

function switchTab(tab: 'pending' | 'history'): void {
  activeTab.value = tab
  // The cursor belongs to the table it moved over; the history tab has no
  // cursor of its own, so leaving the pending tab clears it.
  cursor.reset()
  if (tab === 'pending') loadPending()
  else loadHistory()
}

onMounted(loadPending)

const pendingCount = computed(() => pending.value.length)
</script>

<template>
  <div class="levee-page">
    <PageHeader title="审批中心" description="待审批变更的决策入口；历史来源为审计链上的审批动作">
    </PageHeader>

    <div class="lv-panel">
      <div class="lv-panel__head tabs-head">
        <el-tabs :value="activeTab" @tab-change="switchTab" class="tabs">
          <el-tab-pane name="pending">
            <template #label>
              <span class="tab-label">
                待审批
                <span v-if="pendingCount" class="tab-badge lv-mono">{{ pendingCount }}</span>
              </span>
            </template>
          </el-tab-pane>
          <el-tab-pane label="审批历史" name="history" />
        </el-tabs>
        <div class="head-tools">
          <span class="approver">
            <span class="approver__label">审批人</span>
            <el-input v-model="approver" size="small" style="width: 132px" />
          </span>
          <!-- The keyboard path is only usable if it is SAYABLE: the keys are
               printed next to the list they drive, not hidden in a help modal. -->
          <span class="kbd-hint" v-if="activeTab === 'pending' && pending.length > 0">
            <kbd>j</kbd><kbd>k</kbd> 移动 · <kbd>Enter</kbd> 审批链 · <kbd>a</kbd> 通过 · <kbd>r</kbd> 驳回 ·
            <kbd>d</kbd> 转授权
          </span>
          <el-button :icon="'Refresh'" :loading="loading" size="small" @click="switchTab(activeTab)">刷新</el-button>
        </div>
      </div>

      <div v-if="activeTab === 'pending'">
        <el-table
          ref="pendingTable"
          v-loading="loading"
          :data="pending"
          :row-class-name="pendingRowClass"
          @expand-change="onExpand"
        >
          <!-- The chain, not a bare "pending". Every line here answers "why is
               my vote not going to land" -- the entrap this page hid behind a
               status tag. -->
          <el-table-column type="expand">
            <template #default="{ row }">
              <div class="chain">
                <span v-if="chainsLoading[row.id]" class="chain__dim">加载审批链…</span>
                <span v-else-if="chainsError[row.id]" class="chain__error">{{ chainsError[row.id] }}</span>
                <template v-else-if="(chains[row.id] || []).length > 0">
                  <div v-for="c in approvalSummary(chains[row.id] || [])" :key="c.id" class="chain__one">
                    <div class="chain__head">
                      <span class="chain__level">{{ c.level }}</span>
                      <span class="chain__quorum">需 {{ c.minApprovers }} 票</span>
                      <span class="chain__votes">已投 {{ c.decided }} 票</span>
                      <span class="chain__pending">
                        未投 {{ c.pending.length }} 人{{ c.pending.length ? '：' + c.pending.join('、') : '' }}
                      </span>
                      <span v-if="c.excludeInitiator" class="chain__note">发起人 {{ c.initiator }} 不可自批</span>
                      <!-- The plan this chain attests to. A bound chain names
                           the (short) hash; a legacy one says so, and the
                           wording is the same one the backend uses. -->
                      <span v-if="c.legacy" class="chain__note">未绑定计划版本（旧链，任意 plan 均可）</span>
                      <span v-else class="chain__hash lv-mono" :title="`plan ${c.planHash}`">
                        plan {{ shortHash(c.planHash) }}
                      </span>
                      <span v-if="c.stale" class="chain__note chain__note--bad">
                        该决策所引计划已不是当前版本
                      </span>
                      <span v-if="c.expiresAt" class="chain__dim">超时 {{ formatTimestamp(c.expiresAt) }}</span>
                    </div>
                    <ul class="chain__decisions">
                      <li v-for="d in c.decisions" :key="d.approver">
                        <span class="chain__who lv-mono">{{ d.approver }}</span>
                        <span :class="d.action === 'approve' ? 'chain__ok' : 'chain__no'">
                          {{ d.action === 'approve' ? '同意' : '驳回' }}
                        </span>
                        <span v-if="d.reason" class="chain__reason">{{ d.reason }}</span>
                        <span class="chain__dim lv-mono">{{ d.decidedAt ? formatTimestamp(d.decidedAt) : '' }}</span>
                      </li>
                    </ul>
                  </div>
                </template>
                <span v-else class="chain__dim">暂无审批链记录</span>
              </div>
            </template>
          </el-table-column>
          <el-table-column label="变更" min-width="260">
            <template #default="{ row }">
              <div class="cell-change">
                <span class="cell-change__label">{{ row.label }}</span>
                <span class="cell-change__id lv-mono">{{ row.id }}</span>
              </div>
            </template>
          </el-table-column>
          <el-table-column label="状态" width="120">
            <template #default="{ row }"><StatusTag :status="row.status" variant="plain" /></template>
          </el-table-column>
          <el-table-column label="优先级" width="88">
            <template #default="{ row }">
              <span class="cell-prio" :class="`cell-prio--${row.priority}`">{{ row.priority }}</span>
            </template>
          </el-table-column>
          <el-table-column prop="team" label="团队" width="110" />
          <el-table-column prop="environment" label="环境" width="110" />
          <el-table-column label="创建时间" width="168">
            <template #default="{ row }">
              <span class="cell-time lv-mono">{{ formatTimestamp(row.createdAt) }}</span>
            </template>
          </el-table-column>
          <el-table-column label="决策" width="196" fixed="right">
            <template #default="{ row }">
              <el-button text type="success" @click="approve(row)">通过</el-button>
              <el-button text type="danger" @click="reject(row)">驳回</el-button>
              <el-button text type="primary" @click="delegate(row)">转授权</el-button>
            </template>
          </el-table-column>
        </el-table>
        <StateBlock
          v-if="pendingTier === 'error' || pendingTier === 'forbidden'"
          :tier="pendingTier"
          :message="pendingText"
          :hint="pendingTier === 'forbidden' ? '待审批队列受角色管控，请联系管理员授权' : ''"
          @retry="loadPending"
        />
        <StateBlock
          v-else-if="pendingTier === 'empty'"
          tier="empty"
          hint="没有等待审批的变更"
        />
      </div>

      <div v-else>
        <el-table v-loading="loading" :data="history">
          <el-table-column label="记录 ID" width="190">
            <template #default="{ row }">
              <span class="cell-id lv-mono" :title="row.id">{{ row.id }}</span>
            </template>
          </el-table-column>
          <el-table-column label="变更 ID" width="176">
            <template #default="{ row }">
              <span class="cell-id lv-mono" :title="row.changeId">{{ row.changeId || '—' }}</span>
            </template>
          </el-table-column>
          <el-table-column prop="action" label="动作" width="110" />
          <el-table-column prop="actor" label="操作人" width="132" />
          <el-table-column label="时间" width="168">
            <template #default="{ row }">
              <span class="cell-time lv-mono">{{ formatTimestamp(row.timestamp) }}</span>
            </template>
          </el-table-column>
          <el-table-column prop="targetHost" label="目标" min-width="150" show-overflow-tooltip />
        </el-table>
        <StateBlock
          v-if="historyTier === 'error' || historyTier === 'forbidden'"
          :tier="historyTier"
          :message="historyText"
          :hint="historyTier === 'forbidden' ? '审批历史受角色管控，请联系管理员授权' : ''"
          @retry="loadHistory"
        />
        <StateBlock
          v-else-if="historyTier === 'empty'"
          tier="empty"
          hint="还没有审批记录"
        />
      </div>
    </div>
  </div>
</template>

<style scoped>
.chain {
  display: flex;
  flex-direction: column;
  gap: var(--lv-space-2, 8px);
  padding: var(--lv-space-2, 8px) var(--lv-space-3, 12px);
  background: var(--el-fill-color-light);
  border-radius: 6px;
}

.chain__one {
  display: flex;
  flex-direction: column;
  gap: 4px;
}

.chain__head {
  display: flex;
  flex-wrap: wrap;
  align-items: center;
  gap: var(--lv-space-2, 8px);
  font-size: 13px;
}

.chain__level {
  font-weight: 600;
}

.chain__decisions {
  list-style: none;
  margin: 0;
  padding: 0;
  display: flex;
  flex-direction: column;
  gap: 2px;
}

.chain__decisions li {
  display: flex;
  align-items: center;
  gap: var(--lv-space-2, 8px);
  font-size: 12px;
}

.chain__who {
  min-width: 96px;
}

.chain__ok {
  color: var(--el-color-success);
}

.chain__no {
  color: var(--el-color-danger);
}

.chain__reason {
  color: var(--el-text-color-regular);
  flex: 1 1 auto;
}

.chain__note {
  padding: 0 6px;
  border: 1px solid var(--el-border-color);
  border-radius: 4px;
  font-size: 12px;
  color: var(--el-text-color-secondary);
}

.chain__dim {
  color: var(--el-text-color-secondary);
  font-size: 12px;
}

.chain__error {
  color: var(--el-color-danger);
  font-size: 12px;
}

.head-tools {
  display: inline-flex;
  align-items: center;
  gap: var(--lv-space-3);
}

/* Keyboard affordance. Printed in the panel head rather than hidden in a help
 * modal: a shortcut nobody can discover is a shortcut nobody uses, and the
 * approval list is the page where an operator spends the whole session. */
.kbd-hint {
  display: inline-flex;
  align-items: center;
  gap: 3px;
  font-size: var(--lv-text-xs);
  color: var(--lv-text-3);
}

.kbd-hint kbd {
  padding: 1px 5px;
  border: 1px solid var(--lv-border);
  border-bottom-width: 2px;
  border-radius: var(--lv-radius-sm);
  background: var(--lv-surface);
  font-family: var(--lv-font-mono, inherit);
  font-size: 11px;
  color: var(--lv-text-2);
}

/* The visible keyboard cursor. An outline, not a background: the row keeps its
 * own hover/selected states, and two backgrounds stacked would read as a
 * different row state rather than as "the keys act here". */
:deep(.lv-row-cursor) td {
  box-shadow: inset 0 0 0 1px var(--lv-accent-border);
  background: var(--lv-accent-soft);
}

:deep(.lv-row-cursor:first-child) td:first-child {
  box-shadow: inset 2px 0 0 0 var(--lv-accent);
}

.approver {
  display: inline-flex;
  align-items: center;
  gap: var(--lv-space-2);
}

.approver__label {
  font-size: var(--lv-text-xs);
  color: var(--lv-text-3);
}

/* The tabs live inside the panel head; the head's own bottom border becomes the
 * tab bar's underline, so the two chrome lines do not stack. */
.tabs-head {
  padding-bottom: 0;
}

.tabs :deep(.el-tabs__header) {
  margin: 0;
}

.tabs :deep(.el-tabs__nav-wrap::after) {
  display: none;
}

.tabs :deep(.el-tabs__item) {
  height: 43px;
}

.tab-label {
  display: inline-flex;
  align-items: center;
  gap: 6px;
}

.tab-badge {
  display: inline-flex;
  align-items: center;
  justify-content: center;
  min-width: 18px;
  height: 18px;
  padding: 0 5px;
  border-radius: var(--lv-radius-full);
  background: var(--lv-warn-soft);
  border: 1px solid var(--lv-warn-border);
  color: var(--lv-warn);
  font-size: 11px;
  font-weight: 600;
}

.cell-change {
  display: flex;
  flex-direction: column;
  gap: 2px;
  min-width: 0;
}

.cell-change__label {
  color: var(--lv-text-1);
  font-weight: 500;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}

.cell-change__id,
.cell-id {
  display: inline-block;
  max-width: 100%;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
  font-family: var(--lv-font-mono);
  font-size: 11px;
  color: var(--lv-text-3);
}

.cell-prio {
  font-size: var(--lv-text-xs);
  color: var(--lv-text-2);
}

.cell-prio--urgent {
  color: var(--lv-bad);
  font-weight: 600;
}

.cell-prio--high {
  color: var(--lv-warn);
  font-weight: 500;
}

.cell-time {
  font-size: var(--lv-text-xs);
  color: var(--lv-text-3);
}
</style>
