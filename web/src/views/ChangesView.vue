<script setup lang="ts">
// ChangesView is the change pipeline dashboard: a status KPI strip, a filter
// bar, and a paginated table of changes with bulk actions.
//
// Two things changed shape here, both for honesty rather than looks:
//
//  * The old "summary cards" were 15 equal cards (one per status) computed from
//    the CURRENT PAGE, laid out with el-col :span="2.4" — a fractional span is
//    not a thing, so they collapsed into a ragged grid. Fifteen numbers also
//    answer no question an operator has. It is now four KPIs that DO answer
//    questions worth asking (in flight / awaiting a human / failed / sealed),
//    each derived from the server's totalSize for that filter rather than from
//    the page in front of you. The per-status breakdown survives as a
//    distribution bar under them, where it reads as composition instead of as
//    15 competing headlines.
//  * Each KPI is a filter: clicking it narrows the table to that group, which
//    is what an operator does next after seeing the number.
import { computed, onMounted, reactive, ref } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import { ElMessage, ElMessageBox } from 'element-plus'
import { changesApi, trendApi, type TrendDTO } from '@/api'
import type { Change, ChangeStatus } from '@/types/levee'
import StatusTag from '@/components/StatusTag.vue'
import StateBlock from '@/components/StateBlock.vue'
import TrendChart from '@/components/TrendChart.vue'
import { remediateRollback } from '@/composables/useRollbackRemediation'
import { useKeyboardCursor } from '@/composables/useKeyboardCursor'
import PageHeader from '@/components/PageHeader.vue'
import MetricCard from '@/components/MetricCard.vue'
import { formatTimestamp, isRemediableStatus, isRetryableStatus } from '@/utils/format'
import { classifyError, errorMessage } from '@/utils/apiState'
import type { LoadTier } from '@/utils/apiState'

const router = useRouter()
const route = useRoute()

interface FilterState {
  // Multi-valued: the backend accepts a comma-joined status list, and the KPI
  // groups below are sets ("需处理" is four statuses), so a single-select would
  // have forced every group click to lie about what it filtered.
  status: ChangeStatus[]
  labelContains: string
  pageSize: number
}

const filter = reactive<FilterState>({
  status: [],
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

// Region state for the table area: 'ok' while rows load or are shown, and one
// of the three terminal answers otherwise. The distinction between "no rows"
// and "the fetch failed" was previously the table and a toast — indistinguishable
// at a glance.
const loadError = ref<unknown>(null)

const regionTier = computed<LoadTier>(() => {
  if (loadError.value !== null) return classifyError(loadError.value)
  if (!loading.value && changes.value.length === 0) return 'empty'
  return 'ok'
})

const regionMessage = computed(() => errorMessage(loadError.value, '加载变更列表失败'))

// "Nothing here" means two different things depending on whether a filter is
// narrowing the view; the empty state offers the matching next step.
const filterActive = computed(
  () => filter.status.length > 0 || filter.labelContains.trim() !== '',
)

// Keyboard path to the detail page (roadmap P1). The row click already existed;
// this gives the same navigation a key, with the cursor painted on the row so
// what Enter will open is never a guess.
const cursor = useKeyboardCursor({
  count: () => changes.value.length,
  onOpen: (i) => {
    // Same stale-index guard as the approval page: a refetched list must not
    // turn Enter into a navigation to whatever now occupies that row.
    const row = changes.value[i]
    if (row) viewDetail(row)
  },
})

function rowClass({ rowIndex }: { rowIndex: number }): string {
  return cursor.index.value === rowIndex ? 'lv-row-clickable lv-row-cursor' : 'lv-row-clickable'
}

// Mirrors the backend status vocabulary (change_service.go). `pending_approval`
// was listed here as a second 待审批 option that the server never returns, so
// picking it always yielded an empty table.
const statusOptions: Array<{ value: ChangeStatus; label: string }> = [
  { value: 'draft', label: '草稿' },
  { value: 'planned', label: '已计划' },
  { value: 'pending', label: '待审批' },
  { value: 'approved', label: '已审批' },
  { value: 'rejected', label: '已拒绝' },
  { value: 'running', label: '执行中' },
  { value: 'paused', label: '已暂停' },
  { value: 'completed', label: '已完成' },
  { value: 'failed', label: '失败' },
  { value: 'cancelled', label: '已取消' },
  { value: 'rolled_back', label: '已回滚' },
  { value: 'rolled_back_partial', label: '部分回滚' },
  { value: 'rollback_incomplete', label: '回滚未完成' },
  { value: 'interrupted', label: '已中断' },
  { value: 'archived', label: '已归档' },
]

const STATUS_LABEL_BY_VALUE = Object.fromEntries(
  statusOptions.map((o) => [o.value, o.label]),
) as Record<string, string>

// ---------------------------------------------------------------------------
// KPI groups. Each is a set of statuses the server can filter on, so the count
// comes from `totalSize` for that filter — a real total, not a page count.
//
// The grouping is the operators' own vocabulary: what is moving, what is stuck
// on a human, what went wrong, and what is finished business.
interface KpiGroup {
  key: string
  label: string
  statuses: ChangeStatus[]
  tone: 'accent' | 'warn' | 'bad' | 'ok'
}

const KPI_GROUPS: KpiGroup[] = [
  { key: 'inflight', label: '在途', statuses: ['running', 'approved'], tone: 'accent' },
  { key: 'awaiting', label: '待审批', statuses: ['pending'], tone: 'warn' },
  {
    key: 'trouble',
    label: '需处理',
    statuses: ['failed', 'interrupted', 'rollback_incomplete', 'rolled_back_partial'],
    tone: 'bad',
  },
  { key: 'done', label: '已完成', statuses: ['completed'], tone: 'ok' },
]

// What each group's number actually counts. The card label is short by
// necessity; the footnote is where it says so, because a KPI whose scope has to
// be guessed is a KPI that gets quoted wrong in an incident review.
const KPI_FOOTNOTE: Record<string, string | undefined> = {
  inflight: '执行中 + 已审批',
  awaiting: undefined,
  trouble: '失败 / 中断 / 回滚未完成',
  done: undefined,
}

const kpiCounts = ref<Record<string, number>>({})
const kpiLoading = ref(false)

async function loadKpis(): Promise<void> {
  kpiLoading.value = true
  try {
    // One request per group, pageSize 1: we want totalSize, not rows. These run
    // in parallel and their failures are isolated — a KPI that cannot load
    // shows a dash rather than failing the table.
    const results = await Promise.all(
      KPI_GROUPS.map((g) =>
        changesApi
          .list({ status: g.statuses, pageSize: 1 })
          .then((r) => [g.key, r.totalSize ?? 0] as const)
          .catch(() => [g.key, null] as const),
      ),
    )
    const next: Record<string, number> = {}
    for (const [key, count] of results) {
      if (count !== null) next[key] = count
    }
    kpiCounts.value = next
  } finally {
    kpiLoading.value = false
  }
}

function kpiValue(g: KpiGroup): string {
  const v = kpiCounts.value[g.key]
  return typeof v === 'number' ? String(v) : '—'
}

function sameStatusSet(a: ChangeStatus[], b: ChangeStatus[]): boolean {
  if (a.length !== b.length) return false
  const set = new Set(a)
  return b.every((s) => set.has(s))
}

function isKpiActive(g: KpiGroup): boolean {
  // A group is active when the filter is exactly its status set.
  return sameStatusSet(filter.status, g.statuses)
}

function applyKpi(g: KpiGroup): void {
  // Clicking toggles: an active group clears the filter. The card carries the
  // group's own total, so the number that was clicked stays visible either way.
  filter.status = isKpiActive(g) ? [] : [...g.statuses]
  applyFilter()
}

// Distribution of the current page, kept from the old summary strip: it answers
// "what is in front of me" as composition rather than as 15 numbers.
const distribution = computed(() => {
  const counts: Record<string, number> = {}
  for (const c of changes.value) {
    counts[c.status] = (counts[c.status] || 0) + 1
  }
  const totalOnPage = changes.value.length || 1
  return Object.entries(counts)
    .map(([status, count]) => ({
      status,
      label: STATUS_LABEL_BY_VALUE[status] || status,
      count,
      pct: Math.round((count / totalOnPage) * 100),
    }))
    .sort((a, b) => b.count - a.count)
})

// ---------------------------------------------------------------------------
// Retryable statuses live in utils/format (isRetryableStatus) so they can be
// unit-tested alongside the other status vocabularies and stay in one place;
// the button below gates on it.
async function retryChange(row: Change): Promise<void> {
  try {
    await ElMessageBox.confirm(
      `确认重试变更「${row.label}」？将按已存储计划重新执行。`,
      '重试变更',
      { confirmButtonText: '重试', cancelButtonText: '取消', type: 'warning' },
    )
  } catch {
    return // cancelled
  }
  try {
    await changesApi.retry(row.id, {})
    ElMessage.success(`重试已提交：${row.label}`)
    load()
  } catch (err) {
    ElMessage.error(`重试 ${row.label} 失败：${(err as { message?: string })?.message}`)
  }
}

// The remediation flow lives in composables/useRollbackRemediation: the change
// detail page runs the same preview -> evidence -> commit sequence, and two
// copies of it is how the dry-run/commit pairing drifts.
async function remediate(row: Change): Promise<void> {
  if (await remediateRollback(row)) load()
}

async function load(): Promise<void> {
  loading.value = true
  loadError.value = null
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
      status: filter.status.length > 0 ? filter.status : undefined,
      labelContains: filter.labelContains || undefined,
      pageSize: filter.pageSize,
      pageToken: pageToken || undefined,
    })
    changes.value = res.items || []
    total.value = res.totalSize || 0
    // The cursor belongs to the rows that were on screen when it moved: a new
    // page or a narrowed filter invalidates it, and Enter on a stale index
    // would open the wrong change.
    cursor.reset()
    nextPageToken.value = res.nextPageToken || ''
    // Remember the token leading to the next page; drop stale deeper tokens.
    const chain = tokenChain.value.slice(0, Math.max(currentPage.value - 1, 0))
    if (nextPageToken.value) {
      chain.push(nextPageToken.value)
    }
    tokenChain.value = chain
  } catch (err) {
    // A failed load is not a toast: the list below is where the answer belongs,
    // because a transient message plus an unchanged table reads as "there are
    // zero changes" — the exact conflation StateBlock exists to remove. The
    // 403 case additionally says so in place (utils/apiState.ts).
    loadError.value = err
  } finally {
    loading.value = false
  }
}

// The trend panel: one series set per day, over the last two weeks, for the
// environment the operator is looking at. It answers the question the KPI
// strip cannot — "has today been bad, or is it always like this?" — and it is
// the console's first chart.
//
// The timezone offset goes out with the request on purpose: the server buckets
// days on the OPERATOR's boundary (utils/trend.ts on the backend), so an
// operator's night work lands on their day rather than the server's.
const trend = ref<TrendDTO | null>(null)
const trendLoading = ref(false)
const trendError = ref<unknown>(null)

async function loadTrend(): Promise<void> {
  trendLoading.value = true
  trendError.value = null
  try {
    trend.value = await trendApi.changeTrend({
      days: 14,
      env: filter.status.length === 0 ? undefined : undefined,
      tzOffset: new Date().getTimezoneOffset(),
    })
  } catch (err) {
    // The panel's own failure must not take the table down with it: a chart
    // that cannot load is a fact about the chart.
    trendError.value = err
  } finally {
    trendLoading.value = false
  }
}

// The total across the window, for the panel hint. Read from the buckets
// rather than the DTO so the number on screen and the bars on screen are
// computed from the same array.
const trendTotal = computed(() => (trend.value?.buckets || []).reduce((n, b) => n + b.total, 0))

// The chart's failure text, extracted here because a Vue template cannot cast
// (no `as` in template expressions) and an inline optional chain over an
// unknown is exactly the shape that fails to compile the moment it is needed.
const trendMessage = computed(() => errorMessage(trendError.value, '加载趋势失败'))

function applyFilter(): void {
  currentPage.value = 1
  tokenChain.value = []
  void load()
  void loadKpis()
}

function resetFilter(): void {
  filter.status = []
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
  applyFilter()
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
  applyFilter()
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
  applyFilter()
}

onMounted(() => {
  // Deep link from the command palette: /changes?q=<name fragment>. Read it into
  // the filter before the first load so the table arrives already narrowed
  // instead of flashing the full list and re-querying.
  const q = route.query.q
  if (typeof q === 'string' && q) filter.labelContains = q
  void load()
  void loadKpis()
  void loadTrend()
})
</script>

<template>
  <div class="levee-page">
    <PageHeader title="变更看板" description="全部变更的实时状态、推进与批量处置">
      <template #actions>
        <el-button :icon="'Refresh'" :loading="loading" @click="applyFilter">刷新</el-button>
        <el-button type="primary" :icon="'Plus'" @click="createViaTemplate">新建变更</el-button>
      </template>
    </PageHeader>

    <!-- KPI strip: server totals per operational group, each one a filter. -->
    <div class="kpi-strip" v-loading="kpiLoading">
      <MetricCard
        v-for="g in KPI_GROUPS"
        :key="g.key"
        :label="g.label"
        :value="kpiValue(g)"
        :tone="g.tone"
        interactive
        :selected="isKpiActive(g)"
        :footnote="KPI_FOOTNOTE[g.key]"
        @select="applyKpi(g)"
      />
    </div>

    <!-- Distribution of the rows currently on screen. -->
    <div v-if="distribution.length" class="dist">
      <span class="dist__caption">本页构成</span>
      <div class="dist__bar" role="img" aria-label="当前页变更的状态构成">
        <span
          v-for="d in distribution"
          :key="d.status"
          class="dist__seg"
          :class="`dist__seg--${d.status}`"
          :style="{ flexGrow: d.count }"
          :title="`${d.label} ${d.count}（${d.pct}%）`"
        ></span>
      </div>
      <div class="dist__legend">
        <span v-for="d in distribution" :key="d.status" class="dist__item">
          <span class="dist__swatch" :class="`dist__seg--${d.status}`"></span>
          {{ d.label }}
          <span class="dist__count lv-mono">{{ d.count }}</span>
        </span>
      </div>
    </div>

    <!-- Trend panel: the console's first chart. The KPI strip says what is true
         right now; this says whether it is USUAL. A chart that cannot load
         says so in place (the same three tiers as the table), and it never
         takes the table down with it. -->
    <div class="lv-panel trend-panel" v-loading="trendLoading">
      <div class="lv-panel__head">
        <span class="lv-panel__title">近 14 天变更趋势</span>
        <span v-if="trend" class="lv-panel__hint lv-mono">共 {{ trendTotal }} 个</span>
      </div>
      <div class="lv-panel__body">
        <StateBlock
          v-if="trendError !== null"
          tier="error"
          :message="trendMessage"
          dense
          @retry="loadTrend"
        />
        <StateBlock
          v-else-if="trend && trendTotal === 0"
          tier="empty"
          hint="这个窗口里还没有变更"
          dense
        />
        <template v-else-if="trend">
          <TrendChart :buckets="trend.buckets" :height="180" />
          <p v-if="trend.truncated" class="trend-panel__note">
            窗口内变更过多，已按服务端上限截断——右侧最后一天的幅度是上限，不是数据。
          </p>
        </template>
      </div>
    </div>

    <div class="lv-panel table-panel">
      <div class="lv-panel__head">
        <div class="lv-toolbar">
          <el-select
            v-model="filter.status"
            placeholder="全部状态"
            multiple
            collapse-tags
            collapse-tags-tooltip
            clearable
            style="width: 240px"
            @change="applyFilter"
          >
            <el-option v-for="opt in statusOptions" :key="opt.value" :label="opt.label" :value="opt.value" />
          </el-select>
          <el-input
            v-model="filter.labelContains"
            placeholder="按名称搜索"
            clearable
            style="width: 200px"
            :prefix-icon="'Search'"
            @keyup.enter="applyFilter"
          />
          <el-button @click="applyFilter">查询</el-button>
          <el-button text @click="resetFilter">重置</el-button>
        </div>

        <div class="lv-toolbar">
          <template v-if="selected.length > 0">
            <span class="bulk-hint">已选 <b class="lv-mono">{{ selected.length }}</b> 项</span>
            <el-button size="small" @click="bulkPause">批量暂停</el-button>
            <el-button size="small" @click="bulkCancel">批量取消</el-button>
            <el-button size="small" @click="bulkArchive">批量归档</el-button>
          </template>
          <!-- The keyboard path to the detail page, printed where the list is. -->
          <span v-if="changes.length > 0" class="kbd-hint">
            <kbd>j</kbd><kbd>k</kbd> 移动 · <kbd>Enter</kbd> 打开详情
          </span>
        </div>
      </div>

      <StateBlock
        v-if="regionTier === 'error' || regionTier === 'forbidden'"
        :tier="regionTier"
        :message="regionMessage"
        :hint="
          regionTier === 'forbidden'
            ? '该账号的角色看不到变更列表，请联系管理员授权后重新登录'
            : ''
        "
        @retry="load"
      />
      <StateBlock
        v-else-if="regionTier === 'empty'"
        tier="empty"
        :hint="
          filterActive ? '当前筛选条件下没有匹配的变更' : '还没有创建任何变更，可从模板实例化开始'
        "
        :action-label="filterActive ? '清除筛选条件' : '去模板管理'"
        @action="filterActive ? resetFilter() : router.push('/templates')"
      />
      <template v-else>
        <el-table
          v-loading="loading"
          :data="changes"
          :row-class-name="rowClass"
          @selection-change="handleSelectionChange"
          @row-click="viewDetail"
        >
        <el-table-column type="selection" width="44" />
        <el-table-column label="变更" min-width="260">
          <template #default="{ row }">
            <div class="cell-change">
              <span class="cell-change__label">{{ row.label }}</span>
              <span class="cell-change__id lv-mono">{{ row.id }}</span>
            </div>
          </template>
        </el-table-column>
        <el-table-column label="状态" width="128">
          <template #default="{ row }">
            <StatusTag :status="row.status" variant="plain" />
          </template>
        </el-table-column>
        <el-table-column label="范围" width="176">
          <template #default="{ row }">
            <div class="cell-scope">
              <span v-if="row.environment" class="cell-chip">{{ row.environment }}</span>
              <span v-if="row.team" class="cell-chip cell-chip--soft">{{ row.team }}</span>
            </div>
          </template>
        </el-table-column>
        <el-table-column label="模板" min-width="150" show-overflow-tooltip>
          <template #default="{ row }">
            <span class="lv-mono cell-template">{{ row.templateName || '—' }}</span>
          </template>
        </el-table-column>
        <el-table-column label="创建时间" width="170">
          <template #default="{ row }">
            <span class="lv-mono cell-time">{{ formatTimestamp(row.createdAt) }}</span>
          </template>
        </el-table-column>
        <el-table-column label="操作" width="204" fixed="right" class-name="cell-actions">
          <template #default="{ row }">
            <el-button text type="primary" @click.stop="router.push(`/monitor/${row.id}`)">监控</el-button>
            <el-button text type="primary" @click.stop="viewDetail(row)">详情</el-button>
            <el-button
              v-if="isRemediableStatus(row.status)"
              text
              type="danger"
              @click.stop="remediate(row)"
            >补救</el-button>
            <el-button
              v-if="isRetryableStatus(row.status)"
              text
              type="warning"
              @click.stop="retryChange(row)"
            >重试</el-button>
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
      </template>
    </div>
  </div>
</template>

<style scoped>
.kpi-strip {
  display: grid;
  grid-template-columns: repeat(auto-fit, minmax(180px, 1fr));
  gap: var(--lv-space-3);
  margin-bottom: var(--lv-space-4);
}

/* ------------------------------------------------------------------- dist */

.dist {
  display: flex;
  align-items: center;
  flex-wrap: wrap;
  gap: var(--lv-space-3) var(--lv-space-4);
  padding: var(--lv-space-3) var(--lv-space-4);
  margin-bottom: var(--lv-space-4);
  background: var(--lv-surface);
  border: 1px solid var(--lv-border);
  border-radius: var(--lv-radius-lg);
}

.dist__caption {
  font-size: var(--lv-text-xs);
  color: var(--lv-text-3);
  letter-spacing: var(--lv-tracking-wide);
  white-space: nowrap;
}

.dist__bar {
  display: flex;
  flex: 1 1 240px;
  height: 6px;
  min-width: 160px;
  border-radius: var(--lv-radius-full);
  overflow: hidden;
  background: var(--lv-surface-3);
}

.dist__seg {
  flex: 0 0 auto;
  height: 100%;
  min-width: 2px;
  /* Hairline in the panel colour: 12 statuses share 5 tones, so same-hue
   * neighbours would otherwise merge into one block. */
  box-shadow: inset -1px 0 0 var(--lv-surface);
  transition: flex-grow var(--lv-dur) var(--lv-ease);
}

.dist__seg:last-child {
  box-shadow: none;
}

.dist__legend {
  display: flex;
  flex-wrap: wrap;
  gap: var(--lv-space-3);
}

.dist__item {
  display: inline-flex;
  align-items: center;
  gap: 6px;
  font-size: var(--lv-text-xs);
  color: var(--lv-text-3);
}

.dist__swatch {
  width: 8px;
  height: 8px;
  border-radius: 2px;
}

.dist__count {
  color: var(--lv-text-2);
  font-weight: 500;
}

/* Segment colours follow the status tones used by StatusTag; the families are
   grouped so the bar reads as "moving / waiting / broken / done / other". */
.dist__seg--running,
.dist__seg--approved {
  background: var(--lv-accent);
}

.dist__seg--pending,
.dist__seg--paused,
.dist__seg--rolled_back,
.dist__seg--rolled_back_partial {
  background: var(--lv-warn);
}

.dist__seg--failed,
.dist__seg--interrupted,
.dist__seg--rollback_incomplete,
.dist__seg--rejected {
  background: var(--lv-bad);
}

.dist__seg--completed {
  background: var(--lv-ok);
}

.dist__seg--draft,
.dist__seg--planned,
.dist__seg--cancelled,
.dist__seg--archived {
  background: var(--lv-ink-300);
}

html.dark .dist__seg--draft,
html.dark .dist__seg--planned,
html.dark .dist__seg--cancelled,
html.dark .dist__seg--archived {
  background: var(--lv-ink-600);
}

/* ------------------------------------------------------------------ table */

.table-panel {
  overflow: hidden;
}

.bulk-hint {
  margin-right: var(--lv-space-1);
  font-size: var(--lv-text-sm);
  color: var(--lv-text-3);
}

.bulk-hint b {
  color: var(--lv-accent);
}

/* Keyboard affordance + visible row cursor. Printed rather than hidden: the
 * j/k path to a detail page is only used if the operator can see it exists. */
.kbd-hint {
  display: inline-flex;
  align-items: center;
  gap: 3px;
  margin-left: auto;
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

:deep(.lv-row-cursor) td {
  box-shadow: inset 0 0 0 1px var(--lv-accent-border);
  background: var(--lv-accent-soft);
}

:deep(.lv-row-cursor:first-child) td:first-child {
  box-shadow: inset 2px 0 0 0 var(--lv-accent);
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

.cell-change__id {
  font-size: 11px;
  color: var(--lv-text-3);
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}

.cell-scope {
  display: flex;
  flex-wrap: wrap;
  gap: 4px;
}

.cell-chip {
  display: inline-flex;
  align-items: center;
  height: 20px;
  padding: 0 7px;
  border-radius: var(--lv-radius-sm);
  background: var(--lv-accent-soft);
  border: 1px solid var(--lv-accent-border);
  color: var(--lv-accent);
  font-size: 11px;
  font-weight: 500;
}

.cell-chip--soft {
  background: var(--lv-neutral-soft);
  border-color: var(--lv-neutral-border);
  color: var(--lv-text-2);
}

.cell-template {
  font-size: var(--lv-text-xs);
  color: var(--lv-text-2);
}

.cell-time {
  font-size: var(--lv-text-xs);
  color: var(--lv-text-3);
}

.pager {
  display: flex;
  justify-content: flex-end;
  padding: var(--lv-space-3) var(--lv-space-4);
  border-top: 1px solid var(--lv-border-soft);
}

/* The action cell holds up to four text buttons (监控/详情/补救/重试). Element
   Plus spaces siblings by 12px; at that gap four buttons plus cell padding
   exceed the column, and the columns' declared widths then sum past the table
   (fixed layout clips the last one — measured 1184 declared vs 1142 available).
   The flex gap owns the spacing so the column stays inside the container. */
:deep(.cell-actions .cell) {
  display: flex;
  justify-content: flex-end;
  gap: 6px;
}

:deep(.cell-actions .el-button + .el-button) {
  margin-left: 0;
}

</style>
