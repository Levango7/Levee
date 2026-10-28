<script setup lang="ts">
// MonitorView is the real-time execution monitor: an SSE log stream (the server
// publishes /changes/<id>/events and this view subscribes with EventSource —
// the old 2s poll is gone; see MonitorStream.spec.ts), batch progress read from
// the same endpoint the cluster page uses, and a gate panel that is
// deliberately EMPTY until gate verdicts get a read route.
import { computed, nextTick, onMounted, onUnmounted, ref, watch } from 'vue'
import { useRoute } from 'vue-router'
import {
  batchApi,
  changesApi,
  gatesApi,
  type BatchSummaryDTO,
  type GateVerifyRequest,
  type GateVerifyResult,
} from '@/api'
import type { Change, LogEntry } from '@/types/levee'
import StatusTag from '@/components/StatusTag.vue'
import StateBlock from '@/components/StateBlock.vue'
import PageHeader from '@/components/PageHeader.vue'
import { classifyError, errorMessage } from '@/utils/apiState'
import { formatTimestamp, priorityLabel } from '@/utils/format'
import { batchBarStatus, batchLabel, batchProgress } from '@/utils/batch'
import { buildTargetMap, cellSummary, STEP_LABEL, windowRange, mapRowPitch } from '@/utils/targetMap'
import { useDensity } from '@/composables/useDensity'
import type { MapWindow } from '@/utils/targetMap'

const route = useRoute()

const changeId = computed(() => (route.params.changeId as string) || '')
const change = ref<Change | null>(null)
const logs = ref<LogEntry[]>([])
const loading = ref(false)
const autoScroll = ref(true)
const logStreamRef = ref<HTMLElement | null>(null)

// Region states (utils/apiState.ts). The monitor's three panels answer
// separately: a change the operator may not read must not blank the batch
// panel, and a log tail that failed to load must not claim "no logs".
const loadError = ref<unknown>(null)
const batchError = ref<unknown>(null)
const logsError = ref<unknown>(null)
// Kept last-refresh failure for the stream path: the panels stay on their last
// real data, so the fact that the newest refresh failed is worth surfacing
// somewhere without overwriting what is on screen.
const lastError = ref<unknown>(null)

const loadTier = computed(() => (loadError.value === null ? 'ok' : classifyError(loadError.value)))
const loadText = computed(() => errorMessage(loadError.value, '加载变更失败'))
const batchTier = computed(() => (batchError.value === null ? 'ok' : classifyError(batchError.value)))
const batchText = computed(() => errorMessage(batchError.value, '加载批次进度失败'))
const logsTier = computed(() => (logsError.value === null ? 'ok' : classifyError(logsError.value)))
const logsText = computed(() => errorMessage(logsError.value, '加载执行日志失败'))

// Polling interval. 2s is a reasonable trade-off between freshness and load.

// Batch progress is `GET /system/batch-status?run_id=…`. A change id and a run
// id are the same key in this system (ApplyChange and GetRunReport both resolve
// against the run row), so the route param goes straight through.
//
// The status vocabulary is the store's own — pending | running | done | failed |
// interrupted. This file used to declare a LOCAL copy of the shape whose states
// were `pending|running|success|failed`: nothing ever writes `success`, and
// `interrupted` (the real "the run stopped mid-batch" state) had no rendering
// at all. Binding to the server DTO is what keeps the two vocabularies from
// drifting.
const summary = ref<BatchSummaryDTO | null>(null)
// The plan's batch → host assignment, so a host that has not started still has
// a row (and a host that ran before the plan is known is not dropped).
const planBatches = ref<Array<{ index: number; hosts: string[] }>>([])
// The host × batch matrix. It is composed here rather than inside the endpoint
// because it needs both of the page's data sets: the plan says which host
// belongs to which batch (including hosts that have not started), the step rows
// say what each host actually did.

const targetMap = computed(() =>
  buildTargetMap(
    planBatches.value,
    (summary.value?.steps || []).map((s) => ({
      host: s.host, batchNo: s.batch_no, status: s.status,
      stepName: s.step_name, durationMs: s.duration_ms, exitCode: s.exit_code,
    })),
    summary.value?.batches || [],
  ),
)

// Derived rather than piped through a watch: the rows and the batch numbers are
// the same computation, and a second copy of them in refs is one more place the
// two can disagree.
const targetRows = computed(() => targetMap.value.rows)
const targetBatchNos = computed(() => targetMap.value.batchNos)

// Row windowing for the map. The container's geometry is read on scroll rather
// than from a ResizeObserver: the panel's height is driven by the layout, and a
// scroll-driven recompute is one pure call (windowRange) per event, which is
// cheaper than observing and re-measuring.
const mapScrollTop = ref(0)
const mapViewport = ref(0)

function onMapScroll(e: Event): void {
  const el = e.target as HTMLElement | null
  if (!el) return
  mapScrollTop.value = el.scrollTop
}

// eslint-disable-next-line @typescript-eslint/no-explicit-any -- :ref's callback
// signature is (ref, refs); the second arg is unused here, and this file's
// sibling (ConversationView.onContainer) uses the same shape.
function measureMapViewport(el: any): void {
  mapViewport.value = el ? el.clientHeight : 0
}

// The map's row pitch follows the active density, and the windowing math must
// use the pitch the CSS is actually applying — otherwise the scrollbar describes
// a layout that does not exist (the bug this constant was measured to fix).
// Reading the shared density choice keeps one source for it.
const { choice: densityChoice } = useDensity()
const rowPitch = computed(() => mapRowPitch(densityChoice.value === 'compact'))

const mapWindow = computed<MapWindow>(() =>
  windowRange(targetRows.value.length, mapScrollTop.value, mapViewport.value, rowPitch.value),
)

const windowed = computed(() => mapWindow.value.windowed)
const visibleRows = computed(() => {
  const w = mapWindow.value
  return targetRows.value.slice(w.start, w.start + w.count)
})
const padTop = computed(() => mapWindow.value.padTop)
const padBottom = computed(() => mapWindow.value.padBottom)

// Gate verdicts are recorded per run inside the engine's closure, but there is
// no read route for them: `POST /gates/verify` evaluates on demand and returns
// nothing historical. The card used to render three hardcoded rows all saying
// `pending`, so a run with no gates at all looked like a run whose gates were
// still running. That is removed rather than dressed up — see the panel below.

async function loadChange(quiet = false): Promise<void> {
  if (!changeId.value) return
  try {
    change.value = await changesApi.get(changeId.value)
  } catch (err) {
    // A FIRST load that fails is a page with no content: it says so in place.
    // A stream-driven refresh that fails is the opposite case by policy — the
    // panels keep the last real numbers rather than blanking to an error — so
    // the failure is carried, not raised, when quiet.
    if (quiet) {
      lastError.value = err
    } else {
      loadError.value = err
    }
    return
  }
  try {
    const plan = await changesApi.plan(changeId.value)
    planBatches.value = plan.batches || []
  } catch {
    // An unplanned change legitimately has no plan: the map then renders from
    // the step rows alone, and the untouched column is simply empty.
    planBatches.value = []
  }
}

async function loadLogs(quiet = false): Promise<void> {
  if (!changeId.value) return
  try {
    const res = await changesApi.logs(changeId.value, { limit: 200 })
    logs.value = res.entries || []
    if (autoScroll.value) {
      await nextTick()
      scrollToBottom()
    }
  } catch (err) {
    // Same quiet policy: a dropped tail during a stream refresh is the next
    // event's problem, not a toast's.
    if (!quiet) logsError.value = err
  }
}

// The event stream, not a poller.
//
// The monitor used to poll /changes/<id>/logs every 2 seconds, so progress
// arrived up to 2s late and every poll re-sent the whole tail. The server now
// streams events at /changes/<id>/events (internal/grpc/sse.go), and the
// browser subscribes with EventSource:
//
//   - a status change re-reads the change and the aggregates, because those
//     two panels are snapshots the event does not carry;
//   - the log tail is appended by the log request, which now fires on an
//     event instead of on a timer, so no idle ticking and a gap after an
//     event closes with one request;
//   - a disconnect backs off and retries: EventSource does not retry by
//     itself once closed, and a closed stream stays closed until re-opened.

const retryDelay = ref(1_000)
const eventSource = ref<EventSource | null>(null)

function streamUrl(): string {
	// The same base the axios client uses, so a stream and a request cannot
	// drift into two prefixes (one of them being whatever a proxy strips).
	return `/api/v1/changes/${changeId.value}/events`
}

function closeStream(): void {
  if (eventSource.value) {
    eventSource.value.close()
    eventSource.value = null
  }
}

function openStream(): void {
  if (!changeId.value || typeof EventSource === 'undefined') return
  closeStream()
  const es = new EventSource(streamUrl())
  es.onopen = () => {
    retryDelay.value = 1_000
  }
  es.onerror = () => {
    closeStream()
    setTimeout(() => {
      retryDelay.value = Math.min(retryDelay.value * 2, 30_000)
      openStream()
    }, retryDelay.value)
  }
  es.onmessage = () => {
    void loadChange(true)
    void loadSummary(true)
    void loadLogs(true)
  }
  eventSource.value = es
}

function scrollToBottom(): void {
  const el = logStreamRef.value
  if (el) {
    el.scrollTop = el.scrollHeight
  }
}

async function loadSummary(quiet = false): Promise<void> {
  if (!changeId.value) return
  try {
    summary.value = await batchApi.batchStatus(changeId.value)
    batchError.value = null
  } catch (err) {
    // A stream refresh that fails is silent by policy: a toast per event on a
    // flaky network is noise, and the panel keeps the last real numbers rather
    // than replacing them with anything invented. refreshAll (mount + manual)
    // passes quiet=false, so an operator pressing 刷新 does get an answer.
    if (!quiet) batchError.value = err
  }
}

async function refreshAll(): Promise<void> {
  loading.value = true
  loadError.value = null
  batchError.value = null
  logsError.value = null
  await Promise.all([loadChange(), loadLogs(), loadSummary()])
  loading.value = false
}

// Log level → tone. Kept as a class name rather than an el-tag type because the
// terminal panel paints its own colours (a tag's chrome would fight the pane).
function levelClass(level: LogEntry['level']): string {
  return `log-line--${(level || 'INFO').toLowerCase()}`
}

// The timestamp column is fixed-width in the pane; logs carry an ISO string and
// only the time-of-day part is useful at a glance, so trim it here rather than
// widening the column for the date everyone already knows.
function logTime(ts: string): string {
  if (!ts) return ''
  const t = ts.includes('T') ? ts.split('T')[1] || ts : ts
  return t.replace('Z', '').slice(0, 12)
}

// When autoScroll is toggled on, immediately jump to the bottom.
watch(autoScroll, (v) => {
  if (v) scrollToBottom()
})

// Another run is opened in place (a link from the change list): re-subscribe
// rather than leaving the previous change's events flowing into this view.
watch(changeId, () => {
  openStream()
})

onMounted(async () => {
  await refreshAll()
  openStream()
})

onUnmounted(closeStream)

const batchTotals = computed(() => {
  const s = summary.value
  if (!s) return null
  return {
    total: s.total_batches,
    done: s.done_batches,
    current: s.current_batch_no,
  }
})

// --- Ad-hoc single-step verification (POST /gates/verify) -------------------
//
// The gate panel has no historical read route, but the endpoint CAN evaluate
// one check on demand. This is that action: the operator picks a type and a
// host from the same matrix the page already shows, runs the check, and reads
// the verdict in place. Nothing is persisted server-side (the response carries
// ran_at, not a history), so the result is deliberately ephemeral — it is a
// live answer, not a row.

const verifyOpen = ref(false)
const verifyType = ref<GateVerifyRequest['type']>('cmd')
const verifyTarget = ref('')
const verifyCmd = ref('')
const verifyExpectExit = ref(0)
const verifyURL = ref('')
const verifyQuery = ref('')
const verifyThreshold = ref(0)
const verifyRunning = ref(false)
const verifyResult = ref<GateVerifyResult | null>(null)
const verifyError = ref<unknown>(null)

// Hosts come from the matrix the page already composed, so the picker cannot
// offer a host this change does not target.
const verifyHosts = computed(() => targetRows.value.map((r) => r.host))

function openVerify(): void {
  verifyResult.value = null
  verifyError.value = null
  if (!verifyTarget.value && verifyHosts.value.length > 0) {
    verifyTarget.value = verifyHosts.value[0] || ''
  }
  verifyOpen.value = true
}

function verifyParams(): Record<string, unknown> {
  switch (verifyType.value) {
    case 'cmd':
      return { cmd: verifyCmd.value, expect_exit: verifyExpectExit.value }
    case 'probe':
      return { kind: 'http', url: verifyURL.value }
    case 'slo':
      return { query: verifyQuery.value, threshold: verifyThreshold.value, comparison: 'lte' }
  }
}

async function runVerify(): Promise<void> {
  verifyRunning.value = true
  verifyError.value = null
  verifyResult.value = null
  try {
    verifyResult.value = await gatesApi.verify({
      type: verifyType.value,
      target: verifyTarget.value || undefined,
      run_id: changeId.value || undefined,
      params: verifyParams(),
    })
  } catch (err) {
    verifyError.value = err
  } finally {
    verifyRunning.value = false
  }
}
</script>

<template>
  <div class="levee-page">
    <PageHeader
      :title="change ? `执行监控 · ${change.label}` : '执行监控'"
      description="实时日志与分批推进；事件走 SSE 流，不再轮询"
    >
      <template #actions>
        <el-button :icon="'Refresh'" :loading="loading" @click="refreshAll">刷新</el-button>
      </template>
    </PageHeader>

    <div class="monitor">
      <!-- Left column: what is running -->
      <div class="monitor__side">
        <section class="lv-panel" v-loading="loading">
          <div class="lv-panel__head">
            <span class="lv-panel__title">变更概览</span>
            <StatusTag v-if="change" :status="change.status" />
          </div>
          <div class="lv-panel__body">
            <template v-if="change">
              <div class="lv-kv">
                <div class="lv-kv__k">变更 ID</div>
                <div class="lv-kv__v lv-mono">{{ change.id }}</div>
                <div class="lv-kv__k">优先级</div>
                <div class="lv-kv__v">{{ priorityLabel(change.priority) }}</div>
                <div class="lv-kv__k">环境</div>
                <div class="lv-kv__v">{{ change.environment }}</div>
                <div class="lv-kv__k">模板</div>
                <div class="lv-kv__v lv-mono">{{ change.templateName || '—' }}</div>
                <div class="lv-kv__k">创建时间</div>
                <div class="lv-kv__v lv-mono">{{ formatTimestamp(change.createdAt) }}</div>
                <div class="lv-kv__k">更新时间</div>
                <div class="lv-kv__v lv-mono">{{ formatTimestamp(change.updatedAt) }}</div>
              </div>
            </template>
            <!-- A failed read must not fall through to "未指定变更": the id was
                 specified, it just could not be fetched. -->
            <StateBlock
              v-else-if="loadTier === 'error' || loadTier === 'forbidden'"
              :tier="loadTier"
              :message="loadText"
              :hint="loadTier === 'forbidden' ? '该账号的角色看不到这个变更，请联系管理员授权' : ''"
              dense
              @retry="refreshAll"
            />
            <div v-else class="lv-empty">
              <el-icon class="lv-empty__icon"><Aim /></el-icon>
              <span class="lv-empty__title">未指定变更</span>
              <span>从变更看板点进某个变更，或直接用 /monitor/&lt;change-id&gt; 打开。</span>
            </div>
          </div>
        </section>

        <section class="lv-panel">
          <div class="lv-panel__head">
            <span class="lv-panel__title">批次推进</span>
            <span v-if="batchTotals" class="lv-panel__hint lv-mono">
              {{ batchTotals.done }} / {{ batchTotals.total }} 批完成
            </span>
          </div>
          <div class="lv-panel__body">
            <!-- "No batches" and "batches could not be read" are different
                 answers; the second one used to render the first one. -->
            <StateBlock
              v-if="batchTier === 'error' || batchTier === 'forbidden'"
              :tier="batchTier"
              :message="batchText"
              :hint="batchTier === 'forbidden' ? '批次进度受角色管控，请联系管理员授权' : ''"
              dense
              @retry="loadSummary()"
            />
            <StateBlock
              v-else-if="!summary || summary.batches.length === 0"
              tier="empty"
              hint="该变更还没有进入分批执行阶段"
              dense
            />
            <ol v-else class="batches">
              <li
                v-for="b in summary.batches"
                :key="b.batch_no"
                class="batch"
                :class="{ 'batch--current': b.batch_no === batchTotals?.current }"
              >
                <div class="batch__rail" aria-hidden="true">
                  <span class="batch__node" :class="`batch__node--${b.status}`"></span>
                </div>
                <div class="batch__body">
                  <div class="batch__head">
                    <span class="batch__no lv-mono">批次 #{{ b.batch_no }}</span>
                    <span class="batch__state" :class="`batch__state--${b.status}`">{{ batchLabel(b.status) }}</span>
                  </div>
                  <el-progress
                    :percentage="batchProgress(b)"
                    :status="batchBarStatus(b.status)"
                    :stroke-width="6"
                    :show-text="false"
                  />
                  <div class="batch__meta">
                    <span>{{ b.total_hosts }} 台</span>
                    <span class="batch__ok">成功 {{ b.succeeded }}</span>
                    <span v-if="b.failed" class="batch__bad">失败 {{ b.failed }}</span>
                  </div>
                </div>
              </li>
            </ol>
          </div>
        </section>



        <section class="lv-panel">
          <div class="lv-panel__head">
            <span class="lv-panel__title">门禁状态</span>
            <el-button size="small" @click="openVerify">单步验证…</el-button>
          </div>
          <div class="lv-panel__body">
            <div class="lv-empty">
              <el-icon class="lv-empty__icon"><Lock /></el-icon>
              <span class="lv-empty__title">暂无历史门禁结论可读</span>
              <span>
                门禁结论按 run 记录在引擎内部，但没有对外读路由 ——
                <code>POST /gates/verify</code> 只做即时求值，不返回历史。
                这里不放占位数据。
              </span>
            </div>
          </div>

          <!-- The one thing this panel CAN do: evaluate a single check now.
               Kept inside the panel that explains the endpoint, and additive
               to the empty state above (which stays put — it is the accurate
               answer for "what did the gates decide", a different question). -->
          <el-dialog v-model="verifyOpen" title="单步验证" width="560px" append-to-body>
            <el-form label-width="96px" @submit.prevent>
              <el-form-item label="检查类型">
                <el-select v-model="verifyType" style="width: 220px">
                  <el-option label="cmd（远端命令）" value="cmd" />
                  <el-option label="probe（http 探针）" value="probe" />
                  <el-option label="slo（Prometheus 指标）" value="slo" />
                </el-select>
              </el-form-item>
              <el-form-item v-if="verifyType === 'cmd'" label="目标机">
                <el-select
                  v-model="verifyTarget"
                  filterable
                  allow-create
                  style="width: 100%"
                  placeholder="inventory 主机名"
                >
                  <el-option v-for="h in verifyHosts" :key="h" :label="h" :value="h" />
                </el-select>
              </el-form-item>
              <el-form-item v-if="verifyType === 'cmd'" label="命令">
                <el-input v-model="verifyCmd" placeholder="systemctl is-active nginx" />
              </el-form-item>
              <el-form-item v-if="verifyType === 'cmd'" label="期望退出码">
                <el-input-number v-model="verifyExpectExit" :min="0" />
              </el-form-item>
              <el-form-item v-if="verifyType === 'probe'" label="URL">
                <el-input v-model="verifyURL" placeholder="http://web-1:8080/health" />
              </el-form-item>
              <el-form-item v-if="verifyType === 'slo'" label="PromQL">
                <el-input v-model="verifyQuery" placeholder="rate(errors[5m])" />
              </el-form-item>
              <el-form-item v-if="verifyType === 'slo'" label="阈值 (≤)">
                <el-input-number v-model="verifyThreshold" :step="0.01" />
              </el-form-item>
            </el-form>

            <el-alert
              v-if="verifyError"
              type="error"
              :closable="false"
              show-icon
              :title="errorMessage(verifyError, '验证请求失败')"
            />
            <el-alert
              v-else-if="verifyResult"
              :type="verifyResult.passed ? 'success' : 'error'"
              :closable="false"
              show-icon
              :title="verifyResult.passed ? '通过' : '未通过'"
              :description="verifyResult.message + '（' + verifyResult.latency_ms + ' ms，' + verifyResult.gate + '）'"
            />

            <template #footer>
              <el-button @click="verifyOpen = false">关闭</el-button>
              <el-button type="primary" :loading="verifyRunning" @click="runVerify">立即验证</el-button>
            </template>
          </el-dialog>
        </section>
      </div>

      <!-- Right column: the target map first, then the live log. The map used
           to sit in the 400px side column, where 12 batch columns could not fit
           and the squeezed "主机结论" chip wrapped every row to 65px — measured,
           not guessed. A host × batch matrix needs the wide column and its own
           horizontal scroll; a fleet of many batches scrolls instead of
           squeezing. -->
      <section class="lv-panel map-panel" v-loading="loading">
        <div class="lv-panel__head">
          <span class="lv-panel__title">目标地图</span>
          <span v-if="targetRows.length" class="lv-panel__hint lv-mono">
            {{ targetRows.length }} 台 · {{ targetBatchNos.length }} 批
            <template v-if="windowed"> · 显示 {{ visibleRows.length }} 行</template>
          </span>
        </div>
        <div class="lv-panel__body">
          <!-- An unread run (403) must not render as "no targets yet": the
               hosts exist, this principal just may not see them. -->
          <StateBlock
            v-if="batchTier === 'error' || batchTier === 'forbidden'"
            :tier="batchTier"
            :message="batchText"
            :hint="batchTier === 'forbidden' ? '目标进度受角色管控，请联系管理员授权' : ''"
            dense
            @retry="loadSummary()"
          />
          <div v-else-if="targetRows.length === 0" class="lv-empty">
            <span class="lv-empty__title">暂无目标进度</span>
            <span>计划还没有分配到主机，或执行还没有写出第一步。</span>
          </div>
          <div v-else class="map-scroll" @scroll.passive="onMapScroll" :ref="measureMapViewport">
            <table class="map">
              <thead>
                <tr>
                  <th class="map__host">主机</th>
                  <th v-for="n in targetBatchNos" :key="n" class="map__batch">#{{ n }}</th>
                  <th class="map__worst">主机结论</th>
                </tr>
              </thead>
              <tbody>
                <!-- Row windowing: only the visible slice (plus overscan) is in
                     the DOM. The two spacer rows keep the scrollbar honest about
                     how many hosts there are, so scrolling a 2000-host fleet
                     costs the same as scrolling a 30-host one. -->
                <tr class="map__spacer" :style="{ height: padTop + 'px' }" aria-hidden="true"></tr>
                <tr v-for="r in visibleRows" :key="r.host">
                  <td class="map__host lv-mono">{{ r.host }}</td>
                  <td v-for="n in targetBatchNos" :key="n" class="map__cell">
                    <span
                      v-if="r.cells[n]"
                      class="map__dot"
                      :class="`map__dot--${r.cells[n]!.state}`"
                      :title="cellSummary(r.cells[n]!)"
                    ></span>
                    <span v-else class="map__off" title="该批次未涉及此主机">—</span>
                  </td>
                  <td class="map__worst">
                    <span class="map__chip" :class="`map__chip--${r.worst}`">{{ STEP_LABEL[r.worst] }}</span>
                  </td>
                </tr>
                <tr class="map__spacer" :style="{ height: padBottom + 'px' }" aria-hidden="true"></tr>
              </tbody>
            </table>
          </div>
          <div class="map__legend">
            <span v-for="s in ['failed', 'running', 'pending', 'skipped', 'success']" :key="s" class="map__legend-item">
              <span class="map__dot map__dot--inline" :class="`map__dot--${s}`"></span>
              {{ STEP_LABEL[s as 'failed'] }}
            </span>
          </div>
        </div>
      </section>

      <!-- Right column: the live log terminal -->
      <section class="lv-panel terminal-card">
        <div class="lv-panel__head">
          <span class="lv-panel__title">实时日志</span>
          <div class="terminal-tools">
            <span class="terminal-count lv-mono">{{ logs.length }} 行</span>
            <el-switch v-model="autoScroll" inline-prompt active-text="自动滚动" />
            <el-button text :icon="'Refresh'" @click="loadLogs">刷新</el-button>
          </div>
        </div>
        <div ref="logStreamRef" class="terminal">
          <!-- A tail that failed to load says so; "no logs yet" stays the
               prompt, which is what an idle run actually looks like. -->
          <StateBlock
            v-if="logsTier === 'error' || logsTier === 'forbidden'"
            :tier="logsTier"
            :message="logsText"
            :hint="logsTier === 'forbidden' ? '执行日志受角色管控，请联系管理员授权' : ''"
            dense
            @retry="loadLogs()"
          />
          <div v-else-if="logs.length === 0" class="terminal__empty">
            <span class="terminal__prompt lv-mono">$</span>
            <span>等待日志输出…（该 run 尚未产生日志，或日志已被轮转）</span>
          </div>
          <div v-for="(log, idx) in logs" :key="idx" class="log-line" :class="levelClass(log.level)">
            <span class="log-line__ts lv-mono">{{ logTime(log.timestamp) }}</span>
            <span class="log-line__level lv-mono">{{ log.level }}</span>
            <span class="log-line__source lv-mono">{{ log.source }}</span>
            <span class="log-line__msg">{{ log.message }}</span>
          </div>
        </div>
      </section>
    </div>
  </div>
</template>

<style scoped>
/* Target map: a dense matrix, so cells are colour + title rather than words.
   A column of Chinese labels would be unreadable at 12 hosts × 4 batches. */

/* The map scrolls in BOTH axes inside its own box: horizontally so a fleet
   with many batches scrolls instead of squeezing the host/conclusion columns
   (which used to wrap every row to 65px), and vertically with the row windowing
   above — the vertical extent is capped so a 2000-host fleet does not produce a
   42,000px page. */
.map-scroll {
  max-height: 460px;
  overflow: auto;
  /* The header must stay readable while the rows scroll under it. */
  position: relative;
}

.map-scroll thead th {
  position: sticky;
  top: 0;
  z-index: 1;
  background: var(--lv-surface);
}

.map__spacer {
  border: none;
}

.map {
  width: 100%;
  border-collapse: collapse;
  font-size: 12px;
}

.map__host,
.map__batch,
.map__worst {
  text-align: left;
  /* The map's row pitch is this padding plus the line box plus the 1px border.
   * It is the ONLY thing density changes here, and the JS windowing constant
   * (MAP_ROW_PITCH / MAP_ROW_PITCH_COMPACT) is pinned against a real
   * measurement at both densities — see utils/targetMap.ts. */
  padding: var(--lv-density-map-cell-pad-y) 8px;
  color: var(--lv-text-3);
  font-weight: 500;
  border-bottom: 1px solid var(--lv-border-soft);
}

/* Column widths. These are the shape of the data, not aesthetics: the HOST
   column holds an address, the WORST column holds a 2-4 character verdict chip
   ("部分回滚"), and a BATCH column holds one dot. When the batch columns were
   72px each they squeezed the verdict column to 34px and every chip wrapped to
   four lines — a 64px row pitch, measured. The verdict column keeps its width
   and never wraps. */
.map__host {
  width: 132px;
  white-space: nowrap;
}

.map__batch {
  width: 46px;
  text-align: center;
}

.map__worst {
  width: 92px;
  white-space: nowrap;
}

.map__cell {
  padding: var(--lv-density-map-cell-pad-y) 8px;
  border-bottom: 1px solid var(--lv-border-soft);
  text-align: center;
}

.map__dot {
  display: inline-block;
  width: 12px;
  height: 12px;
  border-radius: 3px;
  vertical-align: middle;
}

.map__dot--inline {
  width: 8px;
  height: 8px;
  margin-right: 4px;
}

.map__dot--success { background: var(--lv-ok); }
.map__dot--failed { background: var(--lv-bad); }
.map__dot--running { background: var(--lv-accent); }
.map__dot--pending { background: var(--lv-ink-400); }
.map__dot--skipped { background: var(--lv-warn); }

.map__off {
  color: var(--lv-text-3);
}

.map__chip {
  padding: 0 6px;
  border-radius: 4px;
  border: 1px solid var(--lv-border);
  font-size: 11px;
}

.map__chip--success { color: var(--lv-ok); border-color: var(--lv-ok-border); }
.map__chip--failed { color: var(--lv-bad); border-color: var(--lv-bad-border); }
.map__chip--running { color: var(--lv-accent); }
.map__chip--pending { color: var(--lv-text-3); }
.map__chip--skipped { color: var(--lv-warn); border-color: var(--lv-warn-border); }

.map__legend {
  display: flex;
  flex-wrap: wrap;
  gap: var(--lv-space-2, 8px);
  margin-top: var(--lv-space-2, 8px);
  font-size: 11px;
  color: var(--lv-text-3);
}

.monitor {
  display: grid;
  grid-template-columns: minmax(320px, 400px) minmax(0, 1fr);
  gap: var(--lv-space-4);
  align-items: stretch;
}

@media (max-width: 1180px) {
  .monitor {
    grid-template-columns: minmax(0, 1fr);
  }
}

.monitor__side {
  display: flex;
  flex-direction: column;
  gap: var(--lv-space-4);
}

/* ---------------------------------------------------------------- batches */

.batches {
  margin: 0;
  padding: 0;
  list-style: none;
}

.batch {
  display: grid;
  grid-template-columns: 20px minmax(0, 1fr);
  gap: var(--lv-space-3);
}

.batch__rail {
  position: relative;
  display: flex;
  justify-content: center;
}

/* The connecting rail between nodes: a timeline, so a run's advance reads as a
 * sequence instead of as N unrelated progress bars. */
.batch__rail::before {
  content: '';
  position: absolute;
  top: 0;
  bottom: 0;
  width: 1px;
  background: var(--lv-border);
}

.batch:first-child .batch__rail::before {
  top: 10px;
}

.batch:last-child .batch__rail::before {
  bottom: calc(100% - 10px);
}

.batch__node {
  position: relative;
  z-index: 1;
  width: 9px;
  height: 9px;
  margin-top: 5px;
  border-radius: var(--lv-radius-full);
  background: var(--lv-ink-400);
  box-shadow: 0 0 0 3px var(--lv-surface);
}

html.dark .batch__node {
  background: var(--lv-ink-500);
}

.batch__node--completed,
.batch__node--done {
  background: var(--lv-ok);
}

.batch__node--failed {
  background: var(--lv-bad);
}

.batch__node--rolled_back,
.batch__node--interrupted {
  background: var(--lv-warn);
}

.batch__node--running {
  background: var(--lv-accent);
  animation: lv-pulse 1.8s var(--lv-ease) infinite;
  box-shadow: 0 0 0 3px var(--lv-accent-soft);
}

.batch__body {
  padding-bottom: var(--lv-space-4);
}

.batch:last-child .batch__body {
  padding-bottom: 0;
}

.batch__head {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: var(--lv-space-2);
  margin-bottom: 6px;
}

.batch__no {
  font-size: var(--lv-text-sm);
  font-weight: 500;
  color: var(--lv-text-1);
}

.batch--current .batch__no {
  color: var(--lv-accent);
}

.batch__state {
  font-size: var(--lv-text-xs);
  color: var(--lv-text-3);
}

.batch__state--completed,
.batch__state--done {
  color: var(--lv-ok);
}

.batch__state--failed {
  color: var(--lv-bad);
}

.batch__state--rolled_back,
.batch__state--interrupted,
.batch__state--running {
  color: var(--lv-warn);
}

.batch__meta {
  display: flex;
  gap: var(--lv-space-3);
  margin-top: 5px;
  font-size: var(--lv-text-xs);
  color: var(--lv-text-3);
}

.batch__ok {
  color: var(--lv-ok);
}

.batch__bad {
  color: var(--lv-bad);
}

/* --------------------------------------------------------------- terminal */

.terminal-card {
  display: flex;
  flex-direction: column;
  /* Fills the grid row so the terminal's frame lines up with the left column's
   * bottom edge instead of stopping where its own content ended. */
  height: 100%;
  min-height: 520px;
  overflow: hidden;
}

.terminal-tools {
  display: flex;
  align-items: center;
  gap: var(--lv-space-3);
}

.terminal-count {
  font-size: var(--lv-text-xs);
  color: var(--lv-text-3);
}

/* The log pane is dark in BOTH themes: it is a terminal, and inverting it with
 * the page would both break the operator's expectation and destroy the level
 * colours' contrast. */
.terminal {
  flex: 1;
  min-height: 320px;
  overflow-y: auto;
  padding: var(--lv-space-3) 0;
  background: #0b1016;
  font-family: var(--lv-font-mono);
  font-size: 12px;
  line-height: 1.65;
  border-bottom-left-radius: var(--lv-radius-lg);
  border-bottom-right-radius: var(--lv-radius-lg);
}

.terminal::-webkit-scrollbar-thumb {
  background: rgba(255, 255, 255, 0.16);
  background-clip: content-box;
}

.terminal__empty {
  display: flex;
  align-items: center;
  gap: var(--lv-space-2);
  padding: var(--lv-space-6) var(--lv-space-4);
  color: #6b7c85;
}

.terminal__prompt {
  color: #3fadaa;
}

.log-line {
  display: grid;
  grid-template-columns: 84px 52px 110px minmax(0, 1fr);
  gap: var(--lv-space-2);
  padding: 1px var(--lv-space-4);
  color: #c3ced6;
  border-left: 2px solid transparent;
}

.log-line:hover {
  background: rgba(255, 255, 255, 0.03);
}

.log-line__ts {
  color: #5c6b75;
  font-variant-numeric: tabular-nums;
}

.log-line__level {
  font-weight: 600;
  font-size: 11px;
}

.log-line__source {
  color: #7d8f9a;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}

.log-line__msg {
  overflow-wrap: anywhere;
}

.log-line--info .log-line__level {
  color: #6fbfa8;
}

.log-line--debug .log-line__level {
  color: #7d8f9a;
}

.log-line--debug .log-line__msg {
  color: #8d9ba4;
}

.log-line--warn {
  border-left-color: rgba(232, 163, 61, 0.55);
}

.log-line--warn .log-line__level {
  color: #e8a33d;
}

.log-line--warn .log-line__msg {
  color: #f0d9b0;
}

.log-line--error {
  border-left-color: rgba(249, 112, 102, 0.6);
  background: rgba(249, 112, 102, 0.06);
}

.log-line--error .log-line__level {
  color: #f97066;
}

.log-line--error .log-line__msg {
  color: #f6cfcb;
}

@media (prefers-reduced-motion: reduce) {
  .batch__node--running {
    animation: none;
  }
}
</style>
