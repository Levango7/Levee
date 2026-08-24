<script setup lang="ts">
// ChangesView is the change pipeline dashboard: a filterable, paginated table
// of changes plus status summary cards and batch actions. Selecting rows
// enables bulk pause / cancel / archive operations.
import { computed, onMounted, reactive, ref } from 'vue'
import { useRouter } from 'vue-router'
import { ElMessage, ElMessageBox } from 'element-plus'
import { changesApi } from '@/api'
import type { Change, ChangeStatus } from '@/types/levee'
import StatusTag from '@/components/StatusTag.vue'
import { formatTimestamp } from '@/utils/format'

const router = useRouter()

interface FilterState {
  status: ChangeStatus | ''
  labelContains: string
  pageSize: number
}

const filter = reactive<FilterState>({
  status: '',
  labelContains: '',
  pageSize: 20,
})

// Pagination state. The backend pages with opaque offset tokens, so the
// current page number is tracked separately from the request params and each
// response's nextPageToken is remembered to reach the following page.
const currentPage = ref(1)
// tokenChain[i] holds the token that fetches page i+2 (page 1 needs no token).
const tokenChain = ref<string[]>([])

const loading = ref(false)
const changes = ref<Change[]>([])
const total = ref(0)
const nextPageToken = ref('')
const selected = ref<Change[]>([])

const statusOptions: Array<{ value: ChangeStatus; label: string }> = [
  { value: 'draft', label: '草稿' },
  { value: 'planned', label: '已计划' },
  { value: 'pending', label: '待审批' },
  { value: 'pending_approval', label: '待审批' },
  { value: 'approved', label: '已审批' },
  { value: 'running', label: '执行中' },
  { value: 'paused', label: '已暂停' },
  { value: 'completed', label: '已完成' },
  { value: 'failed', label: '失败' },
  { value: 'cancelled', label: '已取消' },
  { value: 'rolled_back', label: '已回滚' },
  { value: 'archived', label: '已归档' },
]

// Status summary cards. Computed from the current page; for a true total the
// backend would need a dedicated summary endpoint, but this is enough to give
// operators a quick at-a-glance read.
const summary = computed(() => {
  const counts: Record<string, number> = {}
  for (const c of changes.value) {
    counts[c.status] = (counts[c.status] || 0) + 1
  }
  return statusOptions.map((opt) => ({ ...opt, count: counts[opt.value] || 0 }))
})

async function load(): Promise<void> {
  loading.value = true
  try {
    // Page 1 sends no token; page n reuses the token recorded when page n-1
    // was loaded. Opaque tokens cannot be computed for skipped pages, so a
    // forward jump beyond the known chain clamps back to the deepest page.
    let pageToken = ''
    if (currentPage.value > 1) {
      pageToken = tokenChain.value[currentPage.value - 2] || ''
      if (!pageToken) {
        currentPage.value = tokenChain.value.length + 1
        pageToken = currentPage.value > 1 ? tokenChain.value[currentPage.value - 2] || '' : ''
      }
    }
    const res = await changesApi.list({
      status: filter.status ? [filter.status] : undefined,
      labelContains: filter.labelContains || undefined,
      pageSize: filter.pageSize,
      pageToken: pageToken || undefined,
    })
    changes.value = res.items || []
    total.value = res.totalSize || 0
    nextPageToken.value = res.nextPageToken || ''
    // Remember the token leading to the next page; drop stale deeper tokens.
    const chain = tokenChain.value.slice(0, Math.max(currentPage.value - 1, 0))
    if (nextPageToken.value) {
      chain.push(nextPageToken.value)
    }
    tokenChain.value = chain
  } catch (err) {
    ElMessage.error((err as { message?: string })?.message || '加载变更列表失败')
  } finally {
    loading.value = false
  }
}

function applyFilter(): void {
  currentPage.value = 1
  tokenChain.value = []
  load()
}

function resetFilter(): void {
  filter.status = ''
  filter.labelContains = ''
  applyFilter()
}

// Changing the page size invalidates the token chain; restart from page 1.
function handlePageSizeChange(): void {
  applyFilter()
}

function createViaTemplate(): void {
  ElMessage.info('请在模板管理中使用模板实例化创建变更')
  router.push('/templates')
}

function handleSelectionChange(rows: Change[]): void {
  selected.value = rows
}

function viewDetail(row: Change): void {
  router.push(`/changes/${row.id}`)
}

async function bulkPause(): Promise<void> {
  if (selected.value.length === 0) return
  try {
    await ElMessageBox.confirm(`确认暂停选中的 ${selected.value.length} 个变更？`, '批量暂停', {
      type: 'warning',
    })
  } catch {
    return
  }
  for (const c of selected.value) {
    try {
      await changesApi.pause(c.id, 'batch pause from UI')
    } catch (err) {
      ElMessage.error(`暂停 ${c.label} 失败：${(err as { message?: string })?.message}`)
    }
  }
  ElMessage.success('批量暂停完成')
  load()
}

async function bulkCancel(): Promise<void> {
  if (selected.value.length === 0) return
  try {
    await ElMessageBox.confirm(`确认取消选中的 ${selected.value.length} 个变更？`, '批量取消', {
      type: 'warning',
    })
  } catch {
    return
  }
  for (const c of selected.value) {
    try {
      await changesApi.cancel(c.id, { reason: 'batch cancel from UI' })
    } catch (err) {
      ElMessage.error(`取消 ${c.label} 失败：${(err as { message?: string })?.message}`)
    }
  }
  ElMessage.success('批量取消完成')
  load()
}

async function bulkArchive(): Promise<void> {
  if (selected.value.length === 0) return
  try {
    await ElMessageBox.confirm(`确认归档选中的 ${selected.value.length} 个变更？`, '批量归档', {
      type: 'warning',
    })
  } catch {
    return
  }
  for (const c of selected.value) {
    try {
      await changesApi.archive(c.id)
    } catch (err) {
      ElMessage.error(`归档 ${c.label} 失败：${(err as { message?: string })?.message}`)
    }
  }
  ElMessage.success('批量归档完成')
  load()
}

onMounted(load)
</script>

<template>
  <div class="levee-page">
    <h2 class="levee-page__title">变更看板</h2>

    <!-- Status summary cards -->
    <el-row :gutter="12" class="summary-row">
      <el-col v-for="s in summary" :key="s.value" :span="2.4">
        <el-card shadow="hover" class="summary-card" @click="filter.status = s.value; applyFilter()">
          <div class="summary-card__count">{{ s.count }}</div>
          <div class="summary-card__label">{{ s.label }}</div>
        </el-card>
      </el-col>
    </el-row>

    <!-- Filter bar -->
    <el-card shadow="never" class="levee-card">
      <el-form :inline="true" :model="filter" @submit.prevent="applyFilter">
        <el-form-item label="状态">
          <el-select v-model="filter.status" placeholder="全部" clearable style="width: 140px">
            <el-option v-for="opt in statusOptions" :key="opt.value" :label="opt.label" :value="opt.value" />
          </el-select>
        </el-form-item>
        <el-form-item label="名称">
          <el-input v-model="filter.labelContains" placeholder="名称包含" clearable style="width: 180px" />
        </el-form-item>
        <el-form-item>
          <el-button type="primary" @click="applyFilter">查询</el-button>
          <el-button @click="resetFilter">重置</el-button>
        </el-form-item>
      </el-form>
    </el-card>

    <!-- Table -->
    <el-card shadow="never" class="levee-card">
      <div class="table-toolbar">
        <div class="table-toolbar__left">
          <el-button :disabled="selected.length === 0" @click="bulkPause">批量暂停</el-button>
          <el-button :disabled="selected.length === 0" @click="bulkCancel">批量取消</el-button>
          <el-button :disabled="selected.length === 0" @click="bulkArchive">批量归档</el-button>
        </div>
        <div class="table-toolbar__right">
          <el-button type="primary" @click="createViaTemplate">新建变更</el-button>
        </div>
      </div>

      <el-table
        v-loading="loading"
        :data="changes"
        stripe
        @selection-change="handleSelectionChange"
        @row-click="viewDetail"
      >
        <el-table-column type="selection" width="48" />
        <el-table-column prop="id" label="ID" width="180" show-overflow-tooltip />
        <el-table-column prop="label" label="名称" min-width="180" show-overflow-tooltip />
        <el-table-column label="状态" width="120">
          <template #default="{ row }">
            <StatusTag :status="row.status" />
          </template>
        </el-table-column>
        <el-table-column prop="priority" label="优先级" width="100" />
        <el-table-column prop="team" label="团队" width="120" />
        <el-table-column prop="environment" label="环境" width="120" />
        <el-table-column prop="templateName" label="模板" width="160" show-overflow-tooltip />
        <el-table-column label="创建时间" width="180">
          <template #default="{ row }">{{ formatTimestamp(row.createdAt) }}</template>
        </el-table-column>
        <el-table-column label="操作" width="160" fixed="right">
          <template #default="{ row }">
            <el-button text type="primary" @click.stop="router.push(`/monitor/${row.id}`)">监控</el-button>
            <el-button text type="primary" @click.stop="viewDetail(row)">详情</el-button>
          </template>
        </el-table-column>
      </el-table>

      <div class="pager">
        <el-pagination
          v-model:current-page="currentPage"
          v-model:page-size="filter.pageSize"
          :total="total"
          :page-sizes="[10, 20, 50, 100]"
          layout="total, sizes, prev, pager, next"
          @current-change="load"
          @size-change="handlePageSizeChange"
        />
      </div>
    </el-card>
  </div>
</template>

<style scoped>
.summary-row {
  margin-bottom: 16px;
}

.summary-card {
  cursor: pointer;
  text-align: center;
}

.summary-card__count {
  font-size: 22px;
  font-weight: 600;
  color: var(--levee-primary);
}

.summary-card__label {
  margin-top: 4px;
  font-size: 12px;
  color: #909399;
}

.table-toolbar {
  display: flex;
  justify-content: space-between;
  margin-bottom: 12px;
}

.pager {
  margin-top: 12px;
  display: flex;
  justify-content: flex-end;
}
</style>