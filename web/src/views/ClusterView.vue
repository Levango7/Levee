<script setup lang="ts">
import { computed, onMounted, onUnmounted, ref } from 'vue'
import { batchApi, systemApi, type BatchSummaryDTO, type ClusterStatus } from '@/api'

const data = ref<ClusterStatus | null>(null)
const batchData = ref<BatchSummaryDTO | null>(null)
const selectedRunID = ref('')
const error = ref('')
const batchError = ref('')
const loading = ref(false)
let timer: ReturnType<typeof setInterval> | undefined

async function fetchStatus() {
	loading.value = true
	try {
		data.value = await systemApi.clusterStatus()
		error.value = ''
	} catch (e) {
		error.value = e instanceof Error ? e.message : String(e)
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
		batchError.value = ''
	} catch (e) {
		batchError.value = e instanceof Error ? e.message : String(e)
	}
}

onMounted(() => {
	fetchStatus()
	timer = setInterval(fetchStatus, 30_000)
})
onUnmounted(() => { if (timer) clearInterval(timer) })

const nodes = computed(() => data.value?.nodes ?? [])
const summary = computed(() => data.value?.summary)
const backend = computed(() => data.value?.backend ?? 'unknown')

const activeNodeLoad = computed(() => {
	const load = summary.value?.nodeLoad ?? {}
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

const statusClass = (s: string) => `status-dot status-${s}`

function batchProgressPct(b: { total_hosts: number; succeeded: number; failed: number }): number {
	if (!b.total_hosts) return 0
	return Math.round(((b.succeeded + b.failed) / b.total_hosts) * 100)
}
</script>

<template>
	<div class="levee-page">
		<h2 class="levee-page__title">集群状态</h2>

		<p v-if="backend === 'sqlite'" class="hint">
			单节点模式（SQLite）：集群协调未启用。使用 <code>levee serve --cluster --pg-dsn …</code> 启用集群模式。
		</p>

		<p v-else-if="backend === 'postgres' && nodes.length === 0" class="hint">
			集群模式已启用，但尚无节点注册。启动 worker 节点以加入集群。
		</p>

		<template v-else>
			<!-- Summary cards -->
			<el-row :gutter="12" class="summary-row">
				<el-col :span="6">
					<el-card shadow="hover" class="summary-card">
						<div class="summary-card__count">{{ nodes.length }}</div>
						<div class="summary-card__label">节点总数</div>
					</el-card>
				</el-col>
				<el-col :span="6">
					<el-card shadow="hover" class="summary-card">
						<div class="summary-card__count">{{ summary?.totalActive ?? 0 }}</div>
						<div class="summary-card__label">活跃分配</div>
					</el-card>
				</el-col>
				<el-col :span="6">
					<el-card shadow="hover" class="summary-card">
						<div class="summary-card__count">{{ summary?.counts?.done ?? 0 }}</div>
						<div class="summary-card__label">已完成</div>
					</el-card>
				</el-col>
				<el-col :span="6">
					<el-card shadow="hover" class="summary-card">
						<div class="summary-card__count">{{ summary?.counts?.interrupted ?? 0 }}</div>
						<div class="summary-card__label">已中断</div>
					</el-card>
				</el-col>
			</el-row>

			<!-- Node grid -->
			<h3 class="section-title">节点</h3>
			<el-row :gutter="12">
				<el-col v-for="n in nodes" :key="n.id" :span="8">
					<el-card shadow="hover" class="node-card">
						<div class="node-card__header">
							<span class="node-card__id">{{ n.id }}</span>
							<span :class="statusClass(n.status)"></span>
						</div>
						<div class="node-card__addr">{{ n.address }}</div>
						<div class="node-card__meta">
							<el-tag size="small">{{ n.role }}</el-tag>
							<span class="node-card__heartbeat">最后心跳 {{ lastHeartbeatRelative(n.lastHeartbeat) }}</span>
						</div>
					</el-card>
				</el-col>
			</el-row>

			<!-- Assignment distribution -->
			<h3 class="section-title">分配状态分布</h3>
			<el-card shadow="never" class="dist-card">
				<div class="dist-row" v-for="(count, state) in (summary?.counts ?? {})" :key="state">
					<span class="dist-label">{{ state }}</span>
					<el-progress
						:percentage="summary?.totalActive ? Math.round((count / Math.max(summary.totalActive, 1)) * 100) : 0"
						:stroke-width="14"
						class="dist-bar"
					></el-progress>
					<span class="dist-count">{{ count }}</span>
				</div>
			</el-card>

			<!-- Worker load -->
			<h3 class="section-title">Worker 负载</h3>
			<el-card shadow="never" class="dist-card">
				<div v-if="activeNodeLoad.length === 0" class="muted">无活跃分配</div>
				<div class="dist-row" v-for="n in activeNodeLoad" :key="n.id">
					<span class="dist-label">{{ n.id }}</span>
					<el-progress :percentage="100" :stroke-width="14" class="dist-bar"></el-progress>
					<span class="dist-count">{{ n.count }} 活跃</span>
				</div>
			</el-card>

			<!-- Batch progress (cluster v2 observability) -->
			<h3 class="section-title">Run 批次进度</h3>
			<el-card shadow="never" class="dist-card">
				<div class="batch-run-input">
					<el-input
						v-model="selectedRunID"
						placeholder="输入 run_id 查看批次进度"
						clearable
						@change="fetchBatchStatus"
						class="batch-run-input__field"
					/>
					<el-button type="primary" @click="fetchBatchStatus">查询</el-button>
				</div>
				<p v-if="!selectedRunID.trim()" class="muted">输入 run_id 查看该 run 的各批次执行进度</p>
				<p v-else-if="batchError" class="error">{{ batchError }}</p>
				<template v-else-if="batchData">
					<div class="batch-summary">
						<el-tag size="small" :type="batchData.done_batches === batchData.total_batches ? 'success' : 'info'">
							{{ batchData.done_batches }} / {{ batchData.total_batches }} 批次完成
						</el-tag>
						<span v-if="batchData.current_batch_no > 0" class="batch-current">
							当前批次 #{{ batchData.current_batch_no }}
						</span>
					</div>
					<div class="batch-row" v-for="b in batchData.batches" :key="b.batch_no">
						<span class="batch-label">#{{ b.batch_no }}</span>
						<el-tag size="small" :type="b.status === 'done' ? 'success' : b.status === 'failed' || b.status === 'interrupted' ? 'danger' : b.status === 'running' ? 'warning' : 'info'">
							{{ b.status }}
						</el-tag>
						<el-progress
							:percentage="batchProgressPct(b)"
							:stroke-width="12"
							class="batch-bar"
						></el-progress>
						<span class="batch-count">{{ b.succeeded }}/{{ b.total_hosts }} 成功<span v-if="b.failed"> · {{ b.failed }} 失败</span></span>
					</div>
				</template>
			</el-card>
		</template>

		<p v-if="error" class="error">{{ error }}</p>
	</div>
</template>

<style scoped>
.section-title {
	margin: 1.5rem 0 0.75rem;
	font-size: 1rem;
}
.summary-row { margin-bottom: 1rem; }
.summary-card { text-align: center; }
.summary-card__count { font-size: 1.6rem; font-weight: 600; }
.summary-card__label { color: var(--el-text-color-secondary); font-size: 0.85rem; }

.node-card__header { display: flex; justify-content: space-between; align-items: center; }
.node-card__id { font-weight: 600; }
.node-card__addr { color: var(--el-text-color-secondary); font-size: 0.85rem; margin: 0.25rem 0; }
.node-card__meta { display: flex; justify-content: space-between; align-items: center; }
.node-card__heartbeat { font-size: 0.75rem; color: var(--el-text-color-secondary); }

.status-dot { width: 10px; height: 10px; border-radius: 50%; display: inline-block; }
.status-active { background: var(--el-color-success); }
.status-offline { background: var(--el-color-danger); }

.dist-row { display: flex; align-items: center; margin-bottom: 0.5rem; }
.dist-label { width: 8rem; text-transform: capitalize; }
.dist-bar { flex: 1; margin: 0 1rem; }
.dist-count { width: 5rem; text-align: right; color: var(--el-text-color-secondary); }

.hint, .muted { color: var(--el-text-color-secondary); }
.error { color: var(--el-color-danger); margin-top: 1rem; }

.batch-run-input { display: flex; gap: 0.5rem; margin-bottom: 1rem; }
.batch-run-input__field { flex: 1; }
.batch-summary { display: flex; align-items: center; gap: 1rem; margin-bottom: 0.75rem; }
.batch-current { font-size: 0.85rem; color: var(--el-text-color-secondary); }
.batch-row { display: flex; align-items: center; margin-bottom: 0.5rem; gap: 0.5rem; }
.batch-label { width: 2.5rem; font-weight: 600; }
.batch-bar { flex: 1; }
.batch-count { width: 9rem; text-align: right; color: var(--el-text-color-secondary); font-size: 0.8rem; }
</style>
