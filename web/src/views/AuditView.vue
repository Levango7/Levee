<script setup lang="ts">
// AuditView queries the audit log and verifies the hash chain that links
// trace entries. Operators can filter by change/run/actor/action and trigger
// a verification that highlights any tampered entry.
import { onMounted, reactive, ref } from 'vue'
import { ElMessage } from 'element-plus'
import { auditApi } from '@/api'
import type { TraceEntry } from '@/types/levee'
import { formatTimestamp } from '@/utils/format'

const loading = ref(false)
const entries = ref<TraceEntry[]>([])
const total = ref(0)

const filter = reactive({
  changeId: '',
  runId: '',
  actor: '',
  action: '',
  pageSize: 50,
})

interface VerifyResult {
  visible: boolean
  loading: boolean
  valid: boolean
  entriesVerified: number
  brokenEntryId: string
  brokenReason: string
  runs: Array<{
    runId: string
    valid: boolean
    entriesVerified: number
    brokenEntryId: string
    brokenReason: string
  }>
}

const verify = reactive<VerifyResult>({
  visible: false,
  loading: false,
  valid: false,
  entriesVerified: 0,
  brokenEntryId: '',
  brokenReason: '',
  runs: [],
})

async function load(): Promise<void> {
  loading.value = true
  try {
    const res = await auditApi.log({
      changeId: filter.changeId || undefined,
      runId: filter.runId || undefined,
      actor: filter.actor || undefined,
      action: filter.action || undefined,
      pageSize: filter.pageSize,
    })
    entries.value = res.items || []
    total.value = res.totalSize || 0
  } catch (err) {
    ElMessage.error((err as { message?: string })?.message || '加载审计日志失败')
  } finally {
    loading.value = false
  }
}

function resetFilter(): void {
  filter.changeId = ''
  filter.runId = ''
  filter.actor = ''
  filter.action = ''
  load()
}

async function runVerify(): Promise<void> {
  verify.visible = true
  verify.loading = true
  try {
    const res = await auditApi.verifyHashChain({
      runId: filter.runId || undefined,
      changeId: filter.changeId || undefined,
    })
    verify.valid = res.valid
    verify.entriesVerified = res.entriesVerified
    verify.brokenEntryId = res.brokenEntryId
    verify.brokenReason = res.brokenReason
    verify.runs = res.runs || []
  } catch (err) {
    ElMessage.error((err as { message?: string })?.message || '哈希链校验失败')
  } finally {
    verify.loading = false
  }
}

onMounted(load)
</script>

<template>
  <div class="levee-page">
    <h2 class="levee-page__title">审计查询</h2>

    <el-card shadow="never" class="levee-card">
      <el-form :inline="true" :model="filter" @submit.prevent="load">
        <el-form-item label="变更 ID">
          <el-input v-model="filter.changeId" placeholder="按变更 ID 过滤" clearable style="width: 200px" />
        </el-form-item>
        <el-form-item label="Run ID">
          <el-input v-model="filter.runId" placeholder="按 run ID 过滤" clearable style="width: 200px" />
        </el-form-item>
        <el-form-item label="操作人">
          <el-input v-model="filter.actor" placeholder="按操作人过滤" clearable style="width: 160px" />
        </el-form-item>
        <el-form-item label="动作">
          <el-input v-model="filter.action" placeholder="按动作过滤" clearable style="width: 160px" />
        </el-form-item>
        <el-form-item>
          <el-button type="primary" @click="load">查询</el-button>
          <el-button @click="resetFilter">重置</el-button>
          <el-button type="warning" @click="runVerify">哈希链校验</el-button>
        </el-form-item>
      </el-form>
    </el-card>

    <el-card shadow="never" class="levee-card">
      <el-table v-loading="loading" :data="entries" stripe>
        <el-table-column prop="id" label="记录 ID" width="220" show-overflow-tooltip />
        <el-table-column prop="runId" label="Run ID" width="180" show-overflow-tooltip />
        <el-table-column prop="action" label="动作" width="140" />
        <el-table-column prop="targetHost" label="目标" width="160" show-overflow-tooltip />
        <el-table-column label="耗时" width="100">
          <template #default="{ row }">{{ row.durationMs }} ms</template>
        </el-table-column>
        <el-table-column label="时间" width="180">
          <template #default="{ row }">{{ formatTimestamp(row.timestamp) }}</template>
        </el-table-column>
        <el-table-column prop="prevHash" label="prev_hash" width="180" show-overflow-tooltip />
        <el-table-column prop="currHash" label="curr_hash" width="180" show-overflow-tooltip />
      </el-table>
    </el-card>

    <!-- Verify dialog -->
    <el-dialog v-model="verify.visible" title="哈希链校验结果" width="640px">
      <div v-loading="verify.loading">
        <el-result
          :icon="verify.valid ? 'success' : 'error'"
          :title="verify.valid ? '哈希链完整' : '哈希链损坏'"
          :sub-title="`共校验 ${verify.entriesVerified} 条记录`"
        />
        <template v-if="!verify.valid">
          <el-alert
            type="error"
            :title="`损坏记录：${verify.brokenEntryId}`"
            :description="verify.brokenReason"
            show-icon
            :closable="false"
            style="margin-top: 12px"
          />
        </template>
        <el-table v-if="verify.runs.length > 0" :data="verify.runs" stripe style="margin-top: 12px">
          <el-table-column prop="runId" label="Run ID" width="180" show-overflow-tooltip />
          <el-table-column label="状态" width="100">
            <template #default="{ row }">
              <el-tag :type="row.valid ? 'success' : 'danger'" size="small">
                {{ row.valid ? '有效' : '损坏' }}
              </el-tag>
            </template>
          </el-table-column>
          <el-table-column prop="entriesVerified" label="记录数" width="100" />
          <el-table-column prop="brokenReason" label="原因" min-width="160" show-overflow-tooltip />
        </el-table>
      </div>
      <template #footer>
        <el-button @click="verify.visible = false">关闭</el-button>
      </template>
    </el-dialog>
  </div>
</template>