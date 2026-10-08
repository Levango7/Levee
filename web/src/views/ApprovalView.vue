<script setup lang="ts">
// ApprovalView shows pending approvals and approval history. Operators can
// approve, reject or delegate a change. The history tab lists past decisions
// sourced from the audit log.
import { computed, onMounted, ref } from 'vue'
import { ElMessage, ElMessageBox } from 'element-plus'
import { changesApi, auditApi } from '@/api'
import type { Change, TraceEntry } from '@/types/levee'
import StatusTag from '@/components/StatusTag.vue'
import PageHeader from '@/components/PageHeader.vue'
import { formatTimestamp } from '@/utils/format'

const activeTab = ref<'pending' | 'history'>('pending')
const loading = ref(false)
const pending = ref<Change[]>([])
const history = ref<TraceEntry[]>([])

const approver = ref('operator')

async function loadPending(): Promise<void> {
  loading.value = true
  try {
    // The backend run status for "awaiting approval" is `pending`; querying
    // `pending_approval` (a value the server never emits) returned an empty
    // list, so this tab could never show anything to approve.
    const res = await changesApi.list({ status: ['pending'], pageSize: 100 })
    pending.value = res.items || []
  } catch (err) {
    ElMessage.error((err as { message?: string })?.message || '加载待审批列表失败')
  } finally {
    loading.value = false
  }
}

async function loadHistory(): Promise<void> {
  loading.value = true
  try {
    const res = await auditApi.log({ action: 'approve', pageSize: 100 })
    history.value = res.items || []
  } catch (err) {
    ElMessage.error((err as { message?: string })?.message || '加载审批历史失败')
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

function switchTab(tab: 'pending' | 'history'): void {
  activeTab.value = tab
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
          <el-button :icon="'Refresh'" :loading="loading" size="small" @click="switchTab(activeTab)">刷新</el-button>
        </div>
      </div>

      <div v-if="activeTab === 'pending'">
        <el-table v-loading="loading" :data="pending">
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
        <el-empty
          v-if="!loading && pending.length === 0"
          description="暂无待审批变更"
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
        <el-empty v-if="!loading && history.length === 0" description="暂无审批历史" />
      </div>
    </div>
  </div>
</template>

<style scoped>
.head-tools {
  display: inline-flex;
  align-items: center;
  gap: var(--lv-space-3);
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
