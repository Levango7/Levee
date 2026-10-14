<script setup lang="ts">
// ClusterView shows cluster coordination state: registered nodes, assignment
// distribution, and per-run batch progress.
//
// Indentation note: this file is tab-indented, and its template keeps exactly
// one `\n\t\t</template>` (the night-watch guard in ClusterView.spec.ts keys on
// that string to assert the batch panel sits OUTSIDE the backend/nodes branch).
// Reformatting this file with spaces silently disarms that guard — change the
// spec first if the shape ever needs to move.
import { computed, onMounted, onUnmounted, ref } from 'vue'
import { useRoute } from 'vue-router'
import { batchApi, systemApi, type BatchSummaryDTO, type ClusterStatus, type TakeoverEventDTO } from '@/api'
import type { ChangeStatus } from '@/types/levee'
import { batchBarStatus, batchLabel, batchProgress } from '@/utils/batch'
import { assignmentLabel } from '@/utils/assignment'
import { nodeRoleLabel, nodeStatusLabel, nodeStatusTone } from '@/utils/node'
import PageHeader from '@/components/PageHeader.vue'
import StateBlock from '@/components/StateBlock.vue'
import StatusTag from '@/components/StatusTag.vue'
import { classifyError, errorMessage } from '@/utils/apiState'
import type { LoadTier } from '@/utils/apiState'
import { formatTimestamp } from '@/utils/format'

const data = ref<ClusterStatus | null>(null)
const batchData = ref<BatchSummaryDTO | null>(null)
const selectedRunID = ref('')
// The capability the operator is asking about, and the one the CURRENT response
// was actually narrowed by. Both are kept because they differ while a request is
// in flight, and a page that describes its data with the pending wording is
// claiming to have filtered before the server agreed to.
const capabilityFilter = ref('')
const appliedCapability = ref('')
const route = useRoute()
// The caught failure itself, not a string: the tier (utils/apiState.ts) reads
// the HTTP status off it, and the server's own wording travels with it.
const error = ref<unknown>(null)
const batchError = ref<unknown>(null)
const loading = ref(false)
let timer: ReturnType<typeof setInterval> | undefined

const errorTier = computed<LoadTier>(() => (error.value === null ? 'ok' : classifyError(error.value)))
const errorText = computed(() => errorMessage(error.value, '集群状态加载失败'))

const batchErrorTier = computed<LoadTier>(() =>
	batchError.value === null ? 'ok' : classifyError(batchError.value),
)
const batchErrorText = computed(() => errorMessage(batchError.value, '批次进度加载失败'))

// Takeover settlements: the cluster page's second observability surface. Until
// this the loop in internal/takeover wrote a trace row per settlement and
// NOTHING read it — so an operator could not tell "the cluster interrupted this
// run because its executor died" from "a human cancelled it".
const takeovers = ref<TakeoverEventDTO[] | null>(null)
const takeoverError = ref<unknown>(null)
const takeoverLoading = ref(false)

const takeoverTier = computed<LoadTier>(() => {
	if (takeoverError.value !== null) return classifyError(takeoverError.value)
	if (!takeoverLoading.value && takeovers.value !== null && takeovers.value.length === 0) return 'empty'
	return 'ok'
})
const takeoverText = computed(() => errorMessage(takeoverError.value, '接管事件加载失败'))

// Newest first, and a run's CURRENT status travels with the event: an
// interrupted run that has since been resumed must not read as still
// interrupted.
const takeoverRows = computed(() =>
	[...(takeovers.value ?? [])].sort((a, b) => b.timestamp - a.timestamp),
)

async function fetchTakeovers() {
	takeoverLoading.value = true
	takeoverError.value = null
	try {
		const res = await systemApi.takeoverEvents({ days: 7, limit: 100 })
		takeovers.value = res.events || []
	} catch (e) {
		takeoverError.value = e
	} finally {
		takeoverLoading.value = false
	}
}

async function fetchStatus() {
	loading.value = true
	const want = capabilityFilter.value.trim()
	try {
		data.value = await systemApi.clusterStatus(want === '' ? undefined : { capability: want })
		// Only what the server ANSWERED for. A page that treated the typed value
		// as applied would explain an empty list as "no node declares pkg" while
		// the request carrying pkg had not returned yet — or had failed.
		appliedCapability.value = want
		error.value = null
	} catch (e) {
		error.value = e
	} finally {
		loading.value = false
	}
}

async function fetchBatchStatus() {
	if (!selectedRunID.value.trim()) {
		batchData.value = null
		return
	}
	try {
		batchData.value = await batchApi.batchStatus(selectedRunID.value.trim())
		batchError.value = null
	} catch (e) {
		batchError.value = e
	}
}

onMounted(() => {
	// A deep-linkable filter, in the same shape the changes board's `?q=` takes:
	// "which nodes declare pkg" is worth being able to send as a link rather than
	// a screenshot. Seeded before the first fetch so the first request is already
	// the narrowed one, instead of a full list followed by a second round trip.
	const fromLink = route.query.capability
	if (typeof fromLink === 'string' && fromLink.trim() !== '') {
		capabilityFilter.value = fromLink.trim()
	}
	fetchStatus()
	fetchTakeovers()
	timer = setInterval(fetchStatus, 30_000)
})
onUnmounted(() => { if (timer) clearInterval(timer) })

const nodes = computed(() => data.value?.nodes ?? [])
const summary = computed(() => data.value?.summary)
const backend = computed(() => data.value?.backend ?? 'unknown')

// `answered` exists so the page can claim nothing before the server speaks.
// The hint below states a fact about the deployment, and while the first
// request is in flight (or has failed) there is no fact to state — the old
// shape papered over it by defaulting `backend` to 'unknown', which fell
// through to the dashboard and showed zeros that read as a live registry.
const answered = computed(() => data.value !== null)

// Whether THIS process runs cluster coordination. It is not `backend ===
// 'postgres'`: a single-node PostgreSQL deployment is a supported shape, and
// telling its operator to start workers was the bug. See ClusterStatus in
// @/api for the two axes.
const clusterEnabled = computed(() => data.value?.cluster_enabled === true)

// nodeCapabilities parses the node's declared-capability JSON.
//
// The column's default is '[]', so an absent declaration is a normal empty
// list rather than an error to raise. An unparseable value (a column written by
// an older shape, or a hand-edited row) yields an empty list too, and the card
// then says "未声明能力" — which is honest about what we know, where throwing
// would take down the whole page for one bad row.
function nodeCapabilities(raw: string | undefined): string[] {
	if (!raw) return []
	try {
		const parsed = JSON.parse(raw)
		return Array.isArray(parsed) ? parsed.filter((c): c is string => typeof c === 'string') : []
	} catch {
		return []
	}
}

const activeNodeLoad = computed(() => {
	const load = summary.value?.node_load ?? {}
	return Object.entries(load)
		.map(([id, count]) => ({ id, count }))
		.sort((a, b) => b.count - a.count)
})

function lastHeartbeatRelative(iso: string): string {
	if (!iso) return '—'
	const t = new Date(iso).getTime()
	if (Number.isNaN(t)) return '—'
	const diff = Date.now() - t
	if (diff < 60_000) return `${Math.floor(diff / 1000)}s ago`
	if (diff < 3_600_000) return `${Math.floor(diff / 60_000)}m ago`
	return `${Math.floor(diff / 3_600_000)}h ago`
}

// Node liveness and role wording live in @/utils/node so the vocabulary is declared in
// one place and pinned against internal/cluster by
// internal/cluster/node_vocabulary_test.go. The expression this replaces
// (`status === 'active' ? 'ok' : 'bad'`) folded three registry states onto two colours,
// so a node that asked to leave gracefully was painted as a failure.

const assignmentDistribution = computed(() => {
	const counts = summary.value?.counts ?? {}
	const entries = Object.entries(counts).map(([state, count]) => ({
		state,
		label: assignmentLabel(state),
		count: Number(count) || 0,
	}))
	const max = Math.max(1, ...entries.map((e) => e.count))
	return entries.map((e) => ({ ...e, pct: Math.round((e.count / max) * 100) }))
})
</script>

<template>
	<div class="levee-page">
		<PageHeader title="集群状态" description="节点注册、分配负载与分批推进（每 30 秒自动刷新）">
			<template #actions>
				<el-input
					v-model="capabilityFilter"
					placeholder="按能力筛选节点（如 pkg）"
					clearable
					style="width: 200px"
					@change="fetchStatus"
					@keyup.enter="fetchStatus"
				/>
				<el-button :icon="'Refresh'" :loading="loading" @click="fetchStatus">刷新</el-button>
			</template>
		</PageHeader>

		<p v-if="answered && !clusterEnabled" class="hint">
			<el-icon class="hint__icon"><InfoFilled /></el-icon>
			<span>
				当前进程未启用集群协调（存储后端：<code>{{ backend }}</code>），节点注册表与分配统计在此形态下不会有数据。
				启用集群：<code>levee serve --cluster --node-id … --node-addr … --pg-dsn …</code>。
				单节点 PostgreSQL 是受支持的部署形态，不必为了让这张页面出数据而改成集群。
			</span>
		</p>

		<p v-else-if="answered && nodes.length === 0 && !appliedCapability" class="hint">
			<el-icon class="hint__icon"><InfoFilled /></el-icon>
			<span>
				集群协调已启用，但节点注册表为空。master 节点在启动时就会自行登记，
				所以正常形态下这里至少有本节点：请查看 <code>levee serve</code> 日志里的 join 记录，
				并确认 <code>cluster_nodes</code> 表没有被外部清理。
			</span>
		</p>

		<!-- A filtered empty list is a DIFFERENT fact from an empty registry, and
		     the page used to have only one sentence for both. Saying "no worker is
		     registered" while the operator is looking for nodes that declare pkg
		     sends them at the cluster instead of at the declaration. -->
		<p v-else-if="nodes.length === 0 && appliedCapability" class="hint">
			<el-icon class="hint__icon"><InfoFilled /></el-icon>
			<span>
				没有节点声明能力 <code class="lv-mono">{{ appliedCapability }}</code>。
				这不是注册表为空——清空筛选即可查看全部节点。
				节点要声明能力，需以 <code>levee serve --cluster --node-capabilities …</code> 启动。
			</span>
		</p>

		<template v-else>
			<div class="stat-row">
				<div class="lv-metric">
					<span class="lv-metric__accent metric-accent--accent"></span>
					<div class="lv-metric__label">节点总数</div>
					<div class="lv-metric__value">{{ nodes.length }}</div>
				</div>
				<div class="lv-metric">
					<span class="lv-metric__accent metric-accent--ok"></span>
					<div class="lv-metric__label">活跃分配</div>
					<div class="lv-metric__value">{{ summary?.total_active ?? 0 }}</div>
				</div>
				<div class="lv-metric">
					<span class="lv-metric__accent metric-accent--ok"></span>
					<div class="lv-metric__label">已完成</div>
					<div class="lv-metric__value">{{ summary?.counts?.done ?? 0 }}</div>
				</div>
				<div class="lv-metric">
					<span class="lv-metric__accent metric-accent--bad"></span>
					<div class="lv-metric__label">已中断</div>
					<div class="lv-metric__value">{{ summary?.counts?.interrupted ?? 0 }}</div>
				</div>
			</div>

			<h3 class="lv-section-title">
				节点
				<!-- Say what the counts next to them are NOT narrowed by. Nodes come
				     back filtered; the assignment summary is deployment-wide on
				     purpose, because dispatch places work by load and never by
				     capability. Leaving that unstated would let "2 nodes" sit above
				     "17 active assignments" and read as though the scheduler had
				     honoured the declaration. -->
				<span v-if="appliedCapability" class="lv-panel__hint">
					已按能力 <code class="lv-mono">{{ appliedCapability }}</code> 筛选；
					分配统计仍为全部署口径（派发按负载，不按能力）
				</span>
			</h3>
			<div class="node-grid">
			<div v-for="n in nodes" :key="n.id" class="lv-panel node">
				<div class="node__head">
					<span class="node__id lv-mono">{{ n.id }}</span>
					<span class="lv-dot" :class="`lv-dot--${nodeStatusTone(n.status)}`" :title="nodeStatusLabel(n.status)"></span>
				</div>
				<div class="node__addr lv-mono">{{ n.address }}</div>
				<div class="node__meta">
					<span class="node__role">{{ nodeRoleLabel(n.role) }}</span>
					<span class="node__beat">心跳 {{ lastHeartbeatRelative(n.last_heartbeat) }}</span>
				</div>
				<!-- Declared capabilities, read-only. The cluster_nodes column has
				     existed since the PG v7 migration and was read by nothing until
				     now — a node's declared capabilities were written to a column
				     no code ever looked at. This surfaces them; routing by
				     capability is a separate (dispatcher-semantics) decision. -->
				<div v-if="nodeCapabilities(n.capabilities).length > 0" class="node__caps">
					<span v-for="c in nodeCapabilities(n.capabilities)" :key="c" class="node__cap lv-mono">{{ c }}</span>
				</div>
				<div v-else class="node__caps node__caps--none">未声明能力</div>
			</div>
			</div>

			<div class="split">
				<div>
					<h3 class="lv-section-title">分配状态分布</h3>
					<div class="lv-panel">
						<div class="lv-panel__body">
							<div v-if="assignmentDistribution.length === 0" class="lv-empty">
								<span>暂无分配记录</span>
							</div>
							<div v-for="d in assignmentDistribution" :key="d.state" class="bar-row">
								<span class="bar-row__label">{{ d.label }}</span>
								<div class="bar-row__track">
									<div class="bar-row__fill" :style="{ width: d.pct + '%' }"></div>
								</div>
								<span class="bar-row__count lv-mono">{{ d.count }}</span>
							</div>
						</div>
					</div>
				</div>

				<div>
					<h3 class="lv-section-title">Worker 负载</h3>
					<div class="lv-panel">
						<div class="lv-panel__body">
							<div v-if="activeNodeLoad.length === 0" class="lv-empty">
								<span>无活跃分配</span>
							</div>
							<div v-for="n in activeNodeLoad" :key="n.id" class="bar-row">
								<span class="bar-row__label lv-mono">{{ n.id }}</span>
								<div class="bar-row__track">
									<div class="bar-row__fill bar-row__fill--load" :style="{ width: '100%' }"></div>
								</div>
								<span class="bar-row__count lv-mono">{{ n.count }} 活跃</span>
							</div>
						</div>
					</div>
				</div>
			</div>

		</template>

		<!-- Batch progress (cluster v2 observability).
		     Deliberately OUTSIDE the backend/nodes branch above: this is per-run
		     data from GET /system/batch-status and has nothing to do with cluster
		     coordination. Inside that branch it was unreachable in the default
		     SQLite deployment -- `v-if="backend === 'sqlite'"` swallowed the whole
		     block -- so the same rows were visible on /monitor but not here. -->
		<h3 class="lv-section-title">Run 批次进度</h3>
		<div class="lv-panel">
			<div class="lv-panel__body">
				<div class="batch-query">
					<el-input
						v-model="selectedRunID"
						placeholder="输入 run_id 查看批次进度"
						clearable
						@change="fetchBatchStatus"
					/>
					<el-button type="primary" @click="fetchBatchStatus">查询</el-button>
				</div>
				<p v-if="!selectedRunID.trim()" class="muted">
					按 run 查询逐批推进：每批的主机数、成功/失败分布与当前批次。
				</p>
				<StateBlock
					v-else-if="batchErrorTier === 'error' || batchErrorTier === 'forbidden'"
					:tier="batchErrorTier"
					:message="batchErrorText"
					retry-label="重新查询"
					@retry="fetchBatchStatus"
				/>
				<template v-else-if="batchData">
					<div class="batch-head">
						<span class="batch-head__stamp lv-mono">
							{{ batchData.done_batches }} / {{ batchData.total_batches }}
						</span>
						<span class="muted">批次完成</span>
						<span v-if="batchData.current_batch_no > 0" class="muted">
							· 当前第 <b class="lv-mono">{{ batchData.current_batch_no }}</b> 批
						</span>
					</div>
					<div v-for="b in batchData.batches" :key="b.batch_no" class="batch-row">
						<span class="batch-row__no lv-mono">#{{ b.batch_no }}</span>
						<span class="batch-row__state">{{ batchLabel(b.status) }}</span>
						<el-progress
							:percentage="batchProgress(b)"
							:status="batchBarStatus(b.status)"
							:stroke-width="6"
							:show-text="false"
							class="batch-row__bar"
						/>
						<span class="batch-row__count lv-mono">
							{{ b.succeeded }}/{{ b.total_hosts }}<template v-if="b.failed"> · <b class="bad">{{ b.failed }} 失败</b></template>
						</span>
					</div>
				</template>
			</div>
		</div>

		<!-- Cluster-status failure answers in place. It used to be a bare red
		     paragraph under the panels, i.e. below everything it invalidated:
		     the node cards above stayed on screen showing stale registry data
		     that read as current. -->
		<StateBlock
			v-if="errorTier === 'error' || errorTier === 'forbidden'"
			:tier="errorTier"
			:message="errorText"
			:hint="errorTier === 'forbidden' ? '集群状态受角色管控，请联系管理员授权' : ''"
			@retry="fetchStatus"
		/>

		<!-- Takeover timeline. Rows are settlements the cluster performed on
		     runs whose executor died, so the row's `status` column is the run's
		     CURRENT state: "已中断" here means nobody has resumed it yet, and a
		     resumed run reads as whatever it is now. -->
		<section class="lv-panel" v-loading="takeoverLoading">
			<div class="lv-panel__head">
				<span class="lv-panel__title">接管事件</span>
				<span v-if="takeoverRows.length" class="lv-panel__hint lv-mono">近 7 天 {{ takeoverRows.length }} 次</span>
			</div>
			<div class="lv-panel__body">
				<StateBlock
					v-if="takeoverTier === 'error' || takeoverTier === 'forbidden'"
					:tier="takeoverTier"
					:message="takeoverText"
					:hint="takeoverTier === 'forbidden' ? '接管事件受角色管控，请联系管理员授权' : ''"
					dense
					@retry="fetchTakeovers"
				/>
				<StateBlock
					v-else-if="takeoverTier === 'empty'"
					tier="empty"
					hint="近 7 天没有执行器掉线被接管"
					dense
				/>
				<ul v-else class="takeovers">
					<li v-for="t in takeoverRows" :key="t.id" class="takeover">
						<span class="takeover__time lv-mono">{{ formatTimestamp(t.timestamp) }}</span>
						<span class="takeover__run lv-mono" :title="t.run_id">{{ t.run_id }}</span>
						<span class="takeover__status">
							<StatusTag :status="t.status as ChangeStatus" variant="plain" />
						</span>
						<span class="takeover__actor lv-mono">{{ t.actor }}</span>
					</li>
				</ul>
			</div>
		</section>
	</div>
</template>

<style scoped>
/* Takeover timeline rows: time, run, current status, actor — the four facts
   that separate "the cluster noticed an executor died" from "someone
   cancelled it". */
.takeovers {
	display: flex;
	flex-direction: column;
	gap: 0;
	margin: 0;
	padding: 0;
	list-style: none;
}

.takeover {
	display: grid;
	grid-template-columns: 150px minmax(0, 1fr) 110px 130px;
	align-items: center;
	gap: var(--lv-space-3);
	padding: var(--lv-space-2) 0;
	border-bottom: 1px solid var(--lv-border-soft);
	font-size: var(--lv-text-sm);
}

.takeover:last-child {
	border-bottom: none;
}

.takeover__time {
	color: var(--lv-text-3);
}

.takeover__run {
	color: var(--lv-text-1);
	overflow: hidden;
	text-overflow: ellipsis;
	white-space: nowrap;
}

.takeover__actor {
	color: var(--lv-text-3);
	font-size: var(--lv-text-xs);
}

/* Capability chips on a node card. Monospace because they are machine strings
   (module names), and muted: they describe the node, they are not a status. */
.node__caps {
	display: flex;
	flex-wrap: wrap;
	gap: 4px;
	margin-top: var(--lv-space-2);
}

.node__caps--none {
	font-size: var(--lv-text-xs);
	color: var(--lv-text-3);
}

.node__cap {
	padding: 1px 6px;
	border: 1px solid var(--lv-border);
	border-radius: var(--lv-radius-sm);
	background: var(--lv-surface-3);
	font-size: 11px;
	color: var(--lv-text-2);
}

.hint {
	display: flex;
	align-items: flex-start;
	gap: var(--lv-space-2);
	padding: var(--lv-space-3) var(--lv-space-4);
	margin-bottom: var(--lv-space-4);
	background: var(--lv-accent-soft);
	border: 1px solid var(--lv-accent-border);
	border-radius: var(--lv-radius);
	color: var(--lv-text-2);
	font-size: var(--lv-text-sm);
}

.hint__icon {
	flex: none;
	margin-top: 2px;
	color: var(--lv-accent);
}

.stat-row {
	display: grid;
	grid-template-columns: repeat(auto-fit, minmax(160px, 1fr));
	gap: var(--lv-space-3);
}

.metric-accent--accent { background: var(--lv-accent); }
.metric-accent--ok { background: var(--lv-ok); }
.metric-accent--bad { background: var(--lv-bad); }

.node-grid {
	display: grid;
	grid-template-columns: repeat(auto-fill, minmax(220px, 1fr));
	gap: var(--lv-space-3);
}

.node {
	padding: var(--lv-space-3) var(--lv-space-4);
}

.node__head {
	display: flex;
	align-items: center;
	justify-content: space-between;
	gap: var(--lv-space-2);
}

.node__id {
	font-size: var(--lv-text-sm);
	font-weight: 600;
	color: var(--lv-text-1);
	overflow: hidden;
	text-overflow: ellipsis;
	white-space: nowrap;
}

.node__addr {
	margin: 3px 0 6px;
	font-size: var(--lv-text-xs);
	color: var(--lv-text-3);
}

.node__meta {
	display: flex;
	align-items: center;
	justify-content: space-between;
	gap: var(--lv-space-2);
	font-size: var(--lv-text-xs);
	color: var(--lv-text-3);
}

.node__role {
	padding: 1px 7px;
	border-radius: var(--lv-radius-sm);
	background: var(--lv-neutral-soft);
	border: 1px solid var(--lv-neutral-border);
}

.split {
	display: grid;
	grid-template-columns: repeat(auto-fit, minmax(320px, 1fr));
	gap: var(--lv-space-4);
}

.bar-row {
	display: grid;
	grid-template-columns: minmax(0, 88px) minmax(0, 1fr) 76px;
	align-items: center;
	gap: var(--lv-space-3);
	padding: 5px 0;
}

.bar-row__label {
	font-size: var(--lv-text-sm);
	color: var(--lv-text-2);
	overflow: hidden;
	text-overflow: ellipsis;
	white-space: nowrap;
}

.bar-row__track {
	height: 6px;
	border-radius: var(--lv-radius-full);
	background: var(--lv-surface-3);
	overflow: hidden;
}

.bar-row__fill {
	height: 100%;
	background: var(--lv-accent);
	border-radius: var(--lv-radius-full);
	transition: width var(--lv-dur) var(--lv-ease);
}

.bar-row__fill--load {
	background: linear-gradient(90deg, var(--lv-accent), var(--lv-teal-300));
}

.bar-row__count {
	font-size: var(--lv-text-xs);
	color: var(--lv-text-3);
	text-align: right;
}

.batch-query {
	display: flex;
	gap: var(--lv-space-2);
	margin-bottom: var(--lv-space-3);
}

.batch-query .el-input {
	flex: 1;
}

.batch-head {
	display: flex;
	align-items: baseline;
	gap: var(--lv-space-2);
	margin-bottom: var(--lv-space-3);
	font-size: var(--lv-text-sm);
}

.batch-head__stamp {
	font-size: var(--lv-text-lg);
	font-weight: 600;
	color: var(--lv-text-1);
}

.batch-row {
	display: grid;
	grid-template-columns: 44px 64px minmax(0, 1fr) 132px;
	align-items: center;
	gap: var(--lv-space-3);
	padding: 5px 0;
}

.batch-row__no {
	font-size: var(--lv-text-sm);
	font-weight: 600;
	color: var(--lv-text-1);
}

.batch-row__state {
	font-size: var(--lv-text-xs);
	color: var(--lv-text-3);
}

.batch-row__count {
	font-size: var(--lv-text-xs);
	color: var(--lv-text-3);
	text-align: right;
}

.batch-row__count b {
	font-weight: 600;
}

.muted {
	color: var(--lv-text-3);
	font-size: var(--lv-text-sm);
}

.bad {
	color: var(--lv-bad);
}
</style>
