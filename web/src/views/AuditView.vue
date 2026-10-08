<script setup lang="ts">
// AuditView queries the audit log and verifies the hash chain that links trace
// entries. Operators can filter by change/run/actor/action and trigger a
// verification that highlights any tampered entry.
//
// The chain columns are the point of this page: every row carries prev_hash and
// curr_hash, and the operator's real question is "does this row link to the one
// before it?". So the pair is rendered as a LINK (prev → curr) inside one cell,
// in the mono face, rather than as two wide grey columns whose relationship you
// have to reconstruct by eye.
import { onMounted, reactive, ref } from 'vue'
import { ElMessage } from 'element-plus'
import { auditApi } from '@/api'
import type { TraceEntry } from '@/types/levee'
import PageHeader from '@/components/PageHeader.vue'
import HashCell from '@/components/HashCell.vue'
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
    <PageHeader
      title="审计查询"
      description="变更全生命周期的审计记录；每行经哈希链关联，可整链校验"
    >
      <template #actions>
        <el-button :icon="'Refresh'" :loading="loading" @click="load">刷新</el-button>
        <el-button type="primary" :icon="'CircleCheck'" @click="runVerify">哈希链校验</el-button>
      </template>
    </PageHeader>

    <div class="lv-panel">
      <div class="lv-panel__head">
        <div class="lv-toolbar">
          <el-input v-model="filter.changeId" placeholder="变更 ID" clearable style="width: 180px" @keyup.enter="load" />
          <el-input v-model="filter.runId" placeholder="Run ID" clearable style="width: 180px" @keyup.enter="load" />
          <el-input v-model="filter.actor" placeholder="操作人" clearable style="width: 132px" @keyup.enter="load" />
          <el-input v-model="filter.action" placeholder="动作" clearable style="width: 132px" @keyup.enter="load" />
          <el-button @click="load">查询</el-button>
          <el-button text @click="resetFilter">重置</el-button>
        </div>
        <span class="lv-panel__hint lv-mono">共 {{ total }} 条</span>
      </div>

      <el-table v-loading="loading" :data="entries">
        <el-table-column label="记录 ID" width="150">
          <template #default="{ row }">
            <span class="cell-mono" :title="row.id">{{ row.id }}</span>
          </template>
        </el-table-column>
        <el-table-column label="动作" width="130">
          <template #default="{ row }">
            <span class="cell-action">{{ row.action }}</span>
          </template>
        </el-table-column>
        <el-table-column label="Run" width="128">
          <template #default="{ row }">
            <span class="cell-mono" :title="row.runId">{{ row.runId || '—' }}</span>
          </template>
        </el-table-column>
        <el-table-column label="目标" min-width="140" show-overflow-tooltip>
          <template #default="{ row }">
            <span class="cell-mono">{{ row.targetHost || '—' }}</span>
          </template>
        </el-table-column>
        <el-table-column label="耗时" width="86" align="right">
          <template #default="{ row }">
            <span class="cell-mono cell-dim">{{ row.durationMs }} ms</span>
          </template>
        </el-table-column>
        <el-table-column label="时间" width="168">
          <template #default="{ row }">
            <span class="cell-mono cell-dim">{{ formatTimestamp(row.timestamp) }}</span>
          </template>
        </el-table-column>
        <el-table-column label="哈希链" min-width="240">
          <template #default="{ row }">
            <div class="chain-cell">
              <HashCell :value="row.prevHash" :width="15" />
              <el-icon class="chain-cell__arrow"><Right /></el-icon>
              <HashCell :value="row.currHash" :width="15" />
            </div>
          </template>
        </el-table-column>
      </el-table>
    </div>

    <!-- Verify dialog: a verdict hero, then the detail. -->
    <el-dialog v-model="verify.visible" title="哈希链校验" width="720px">
      <div v-loading="verify.loading" class="verify">
        <div class="verify__hero" :class="verify.valid ? 'verify__hero--ok' : 'verify__hero--bad'">
          <el-icon class="verify__hero-icon">
            <component :is="verify.valid ? 'CircleCheckFilled' : 'WarningFilled'" />
          </el-icon>
          <div class="verify__hero-main">
            <div class="verify__verdict">{{ verify.valid ? '哈希链完整' : '哈希链损坏' }}</div>
            <div class="verify__sub">
              共校验 <b class="lv-mono">{{ verify.entriesVerified }}</b> 条记录
            </div>
          </div>
        </div>

        <div v-if="!verify.valid" class="verify__break">
          <div class="verify__break-title">首个断点</div>
          <div class="verify__break-id lv-mono">{{ verify.brokenEntryId || '—' }}</div>
          <div class="verify__break-reason">{{ verify.brokenReason || '未提供原因' }}</div>
          <p class="verify__break-note">
            断点意味着该记录的存储哈希与其内容或位置不再一致：内容被改、行被删除或插入，或链列被直接覆写。
            请先调查再决定是否重建 —— 重建会丢弃篡改证据。
          </p>
        </div>

        <div v-if="verify.runs.length > 0" class="verify__runs">
          <div class="verify__runs-title">逐 run 结果</div>
          <div
            v-for="r in verify.runs"
            :key="r.runId"
            class="verify__run"
            :class="{ 'verify__run--bad': !r.valid }"
          >
            <span class="verify__run-id lv-mono">{{ r.runId }}</span>
            <span class="verify__run-state">{{ r.valid ? '有效' : '损坏' }}</span>
            <span class="verify__run-count lv-mono">{{ r.entriesVerified }} 条</span>
            <span v-if="!r.valid" class="verify__run-reason">{{ r.brokenReason || r.brokenEntryId }}</span>
          </div>
        </div>
      </div>

      <template #footer>
        <el-button @click="verify.visible = false">关闭</el-button>
      </template>
    </el-dialog>
  </div>
</template>

<style scoped>
/* The filter panel and the table are block siblings here, and the generic
 * stacking margin was removed from the base layer (it broke grids) — so this
 * view states its own rhythm. */
.lv-panel + .lv-panel {
  margin-top: var(--lv-space-4);
}

.cell-mono {
  display: inline-block;
  max-width: 100%;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
  font-family: var(--lv-font-mono);
  font-size: var(--lv-text-xs);
  color: var(--lv-text-2);
}

.cell-action {
  font-weight: 500;
  color: var(--lv-text-1);
}

.cell-dim {
  color: var(--lv-text-3);
}

/* -------------------------------------------------------------- chain cell */

.chain-cell {
  display: flex;
  align-items: center;
  gap: 6px;
  min-width: 0;
}

.chain-cell__arrow {
  flex: none;
  font-size: 10px;
  color: var(--lv-ink-400);
}

/* ------------------------------------------------------------- verify view */

.verify {
  min-height: 180px;
}

.verify__hero {
  display: flex;
  align-items: center;
  gap: var(--lv-space-4);
  padding: var(--lv-space-4) var(--lv-space-5);
  border: 1px solid transparent;
  border-radius: var(--lv-radius-lg);
}

.verify__hero--ok {
  background: var(--lv-ok-soft);
  border-color: var(--lv-ok-border);
  color: var(--lv-ok);
}

.verify__hero--bad {
  background: var(--lv-bad-soft);
  border-color: var(--lv-bad-border);
  color: var(--lv-bad);
}

.verify__hero-icon {
  flex: none;
  font-size: 30px;
}

.verify__verdict {
  font-size: var(--lv-text-lg);
  font-weight: 600;
}

.verify__sub {
  margin-top: 2px;
  font-size: var(--lv-text-sm);
  opacity: 0.85;
}

.verify__break {
  margin-top: var(--lv-space-4);
  padding: var(--lv-space-4);
  border: 1px solid var(--lv-border);
  border-left: 3px solid var(--lv-bad);
  border-radius: var(--lv-radius);
  background: var(--lv-surface-3);
}

.verify__break-title {
  font-size: var(--lv-text-xs);
  letter-spacing: var(--lv-tracking-wide);
  text-transform: uppercase;
  color: var(--lv-text-3);
}

.verify__break-id {
  margin-top: var(--lv-space-1);
  font-size: var(--lv-text-base);
  color: var(--lv-bad);
  word-break: break-all;
}

.verify__break-reason {
  margin-top: var(--lv-space-1);
  font-size: var(--lv-text-sm);
  color: var(--lv-text-2);
}

.verify__break-note {
  margin-top: var(--lv-space-3);
  font-size: var(--lv-text-xs);
  line-height: 1.7;
  color: var(--lv-text-3);
}

.verify__runs {
  margin-top: var(--lv-space-5);
}

.verify__runs-title {
  margin-bottom: var(--lv-space-2);
  font-size: var(--lv-text-sm);
  font-weight: 600;
  color: var(--lv-text-1);
}

.verify__run {
  display: grid;
  grid-template-columns: minmax(0, 1.4fr) 56px 68px minmax(0, 1.6fr);
  align-items: center;
  gap: var(--lv-space-2);
  padding: 8px var(--lv-space-3);
  border: 1px solid var(--lv-border-soft);
  border-radius: var(--lv-radius-sm);
  font-size: var(--lv-text-xs);
  color: var(--lv-text-2);
}

.verify__run + .verify__run {
  margin-top: 4px;
}

.verify__run--bad {
  border-color: var(--lv-bad-border);
  background: var(--lv-bad-soft);
}

.verify__run-id {
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}

.verify__run-state {
  color: var(--lv-ok);
  font-weight: 500;
}

.verify__run--bad .verify__run-state {
  color: var(--lv-bad);
}

.verify__run-count {
  color: var(--lv-text-3);
}

.verify__run-reason {
  color: var(--lv-bad);
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}
</style>
