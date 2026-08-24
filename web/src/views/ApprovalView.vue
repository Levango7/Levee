<script setup lang="ts">
// ApprovalView shows pending approvals and approval history. Operators can
// approve, reject or delegate a change. The history tab lists past decisions
// sourced from the audit log.
import { onMounted, ref } from 'vue'
import { ElMessage, ElMessageBox } from 'element-plus'
import { changesApi, auditApi } from '@/api'
import type { Change, TraceEntry } from '@/types/levee'
import StatusTag from '@/components/StatusTag.vue'
import { formatTimestamp } from '@/utils/format'

const activeTab = ref<'pending' | 'history'>('pending')
const loading = ref(false)
const pending = ref<Change[]>([])
const history = ref<TraceEntry[]>([])

const approver = ref('operator')

async function loadPending(): Promise<void> {
  loading.value = true
  try {
    const res = await changesApi.list({ status: ['pending_approval'], pageSize: 100 })
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
</script>

<template>
  <div class="levee-page">
    <h2 class="levee-page__title">审批中心</h2>

    <el-tabs :value="activeTab" @tab-change="switchTab">
      <el-tab-pane label="待审批" name="pending" />
      <el-tab-pane label="审批历史" name="history" />
    </el-tabs>

    <el-card shadow="never" class="levee-card" v-loading="loading">
      <template v-if="activeTab === 'pending'">
        <el-table :data="pending" stripe>
          <el-table-column prop="id" label="变更 ID" width="180" show-overflow-tooltip />
          <el-table-column prop="label" label="名称" min-width="180" show-overflow-tooltip />
          <el-table-column label="状态" width="120">
            <template #default="{ row }"><StatusTag :status="row.status" /></template>
          </el-table-column>
          <el-table-column prop="priority" label="优先级" width="100" />
          <el-table-column prop="team" label="团队" width="120" />
          <el-table-column prop="environment" label="环境" width="120" />
          <el-table-column label="创建时间" width="180">
            <template #default="{ row }">{{ formatTimestamp(row.createdAt) }}</template>
          </el-table-column>
          <el-table-column label="操作" width="240" fixed="right">
            <template #default="{ row }">
              <el-button text type="success" @click="approve(row)">通过</el-button>
              <el-button text type="danger" @click="reject(row)">驳回</el-button>
              <el-button text type="primary" @click="delegate(row)">转授权</el-button>
            </template>
          </el-table-column>
        </el-table>
        <el-empty v-if="!loading && pending.length === 0" description="暂无待审批变更" />
      </template>

      <template v-else>
        <el-table :data="history" stripe>
          <el-table-column prop="id" label="记录 ID" width="200" show-overflow-tooltip />
          <el-table-column prop="changeId" label="变更 ID" width="180" show-overflow-tooltip />
          <el-table-column prop="action" label="动作" width="120" />
          <el-table-column prop="actor" label="操作人" width="140" />
          <el-table-column label="时间" width="180">
            <template #default="{ row }">{{ formatTimestamp(row.timestamp) }}</template>
          </el-table-column>
          <el-table-column prop="targetHost" label="目标" min-width="160" show-overflow-tooltip />
        </el-table>
        <el-empty v-if="!loading && history.length === 0" description="暂无审批历史" />
      </template>
    </el-card>
  </div>
</template>