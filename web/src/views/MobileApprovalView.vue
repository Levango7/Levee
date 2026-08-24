<script setup lang="ts">
// MobileApprovalView is the mobile-first approval page. It is designed to be
// opened from a push-notification deep link (levee://approve/run-123?token=xxx)
// and supports one-tap approve / reject without leaving the page.
//
// Layout adapts to screen size via the useResponsive composable: on phone-width
// screens the action buttons are sticky at the bottom; on tablet / desktop they
// appear inline next to the change summary.
import { computed, onMounted, ref } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import { ElMessage, ElMessageBox } from 'element-plus'
import { useResponsive } from '@/composables/useResponsive'
import { changesApi, auditApi } from '@/api'
import type { Change, TraceEntry } from '@/types/levee'
import StatusTag from '@/components/StatusTag.vue'
import { formatTimestamp } from '@/utils/format'

const route = useRoute()
const router = useRouter()
const { isMobile, isTablet, isDesktop, breakpoint, gridCols } = useResponsive()

// --- state -----------------------------------------------------------------

const loading = ref(false)
const change = ref<Change | null>(null)
const history = ref<TraceEntry[]>([])
const error = ref<string | null>(null)
const approver = ref('operator')

// One-tap token extracted from the deep-link query string. When present the
// page attempts the action immediately on mount.
const deeplinkToken = computed<string>(() => (route.query.token as string) || '')
const deeplinkAction = computed<'approve' | 'reject' | ''>(() => {
  // The action is encoded in the path: /m/approve/<id> or /m/reject/<id>.
  if (route.path.includes('/approve')) return 'approve'
  if (route.path.includes('/reject')) return 'reject'
  return ''
})

// --- data loading ----------------------------------------------------------

async function loadChange(runID: string): Promise<void> {
  loading.value = true
  error.value = null
  try {
    const res = await changesApi.get(runID)
    change.value = res
  } catch (err) {
    error.value = (err as { message?: string })?.message || '加载变更失败'
  } finally {
    loading.value = false
  }
}

async function loadHistory(runID: string): Promise<void> {
  try {
    const res = await auditApi.log({ runId: runID, pageSize: 50 })
    history.value = res.items || []
  } catch {
    // History is best-effort; do not block the page on it.
    history.value = []
  }
}

// --- actions ---------------------------------------------------------------

async function approve(): Promise<void> {
  if (!change.value) return
  let comment = ''
  if (!isMobile.value) {
    // On desktop / tablet we prompt for an optional comment.
    try {
      const result = await ElMessageBox.prompt('请输入审批意见（可选）', `通过审批：${change.value.label}`, {
        confirmButtonText: '通过',
        cancelButtonText: '取消',
        inputPlaceholder: '审批意见',
        inputType: 'textarea',
      })
      comment = result.value || ''
    } catch {
      return
    }
  }
  try {
    await changesApi.approve(change.value.id, { approver: approver.value, comment })
    ElMessage.success('审批通过')
    await loadChange(change.value.id)
    await loadHistory(change.value.id)
  } catch (err) {
    ElMessage.error(`审批失败：${(err as { message?: string })?.message}`)
  }
}

async function reject(): Promise<void> {
  if (!change.value) return
  let reason = 'rejected via mobile'
  if (!isMobile.value) {
    try {
      const result = await ElMessageBox.prompt('请输入驳回原因', `驳回：${change.value.label}`, {
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
  }
  try {
    await changesApi.reject(change.value.id, { rejecter: approver.value, reason })
    ElMessage.success('已驳回')
    await loadChange(change.value.id)
    await loadHistory(change.value.id)
  } catch (err) {
    ElMessage.error(`驳回失败：${(err as { message?: string })?.message}`)
  }
}

async function oneTap(action: 'approve' | 'reject', token: string): Promise<void> {
  try {
    if (action === 'approve') {
      await changesApi.approveViaDeepLink(token)
    } else {
      await changesApi.rejectViaDeepLink(token)
    }
    ElMessage.success(action === 'approve' ? '一键审批通过' : '一键驳回成功')
    if (change.value) {
      await loadChange(change.value.id)
      await loadHistory(change.value.id)
    }
  } catch (err) {
    ElMessage.error(`一键${action === 'approve' ? '审批' : '驳回'}失败：${(err as { message?: string })?.message}`)
  }
}

// --- lifecycle -------------------------------------------------------------

onMounted(async () => {
  // The change id comes from the route param: /m/approve/:id or /m/reject/:id.
  const runID = String(route.params.id || '')
  if (!runID) {
    error.value = '缺少变更 ID'
    return
  }
  await loadChange(runID)
  await loadHistory(runID)

  // When a deep-link token is present, perform the one-tap action immediately.
  if (deeplinkToken.value && deeplinkAction.value) {
    await oneTap(deeplinkAction.value, deeplinkToken.value)
    // Clear the token so a page refresh does not replay the action.
    router.replace({ path: route.path, query: {} })
  }
})

// --- layout helpers --------------------------------------------------------

const containerClass = computed(() => ({
  'mobile-approval': true,
  'mobile-approval--mobile': isMobile.value,
  'mobile-approval--tablet': isTablet.value,
  'mobile-approval--desktop': isDesktop.value,
  [`mobile-approval--${breakpoint.value}`]: true,
}))

const summaryCols = computed(() => (isMobile.value ? 1 : Math.min(gridCols.value, 2)))
</script>

<template>
  <div :class="containerClass">
    <!-- Header: change title + status. Stays single-line per the layout rule. -->
    <header class="mobile-approval__header">
      <h1 class="mobile-approval__title">
        {{ change?.label || '加载中…' }}
      </h1>
      <StatusTag v-if="change" :status="change.status" />
    </header>

    <!-- Error banner. -->
    <el-alert
      v-if="error"
      :title="error"
      type="error"
      show-icon
      :closable="false"
      class="mobile-approval__error"
    />

    <!-- Loading spinner. -->
    <div v-if="loading" class="mobile-approval__loading">
      <el-icon class="is-loading"><Loading /></el-icon>
      <span>加载中…</span>
    </div>

    <!-- Change summary. -->
    <section
      v-if="change && !loading"
      class="mobile-approval__summary"
      :style="{ gridTemplateColumns: `repeat(${summaryCols}, 1fr)` }"
    >
      <div class="mobile-approval__field">
        <span class="mobile-approval__label">变更 ID</span>
        <span class="mobile-approval__value">{{ change.id }}</span>
      </div>
      <div class="mobile-approval__field">
        <span class="mobile-approval__label">状态</span>
        <span class="mobile-approval__value">{{ change.status }}</span>
      </div>
      <div class="mobile-approval__field">
        <span class="mobile-approval__label">创建时间</span>
        <span class="mobile-approval__value">{{ formatTimestamp(change.createdAt) }}</span>
      </div>
      <div class="mobile-approval__field">
        <span class="mobile-approval__label">申请人</span>
        <span class="mobile-approval__value">{{ change.createdBy || '—' }}</span>
      </div>
    </section>

    <!-- Action buttons. On mobile they are sticky at the bottom; on larger
         screens they appear inline. -->
    <section
      v-if="change && change.status === 'pending_approval'"
      class="mobile-approval__actions"
    >
      <el-button
        type="primary"
        size="large"
        :class="{ 'mobile-approval__btn--sticky': isMobile }"
        @click="approve"
      >
        通过
      </el-button>
      <el-button
        type="danger"
        size="large"
        plain
        :class="{ 'mobile-approval__btn--sticky': isMobile }"
        @click="reject"
      >
        驳回
      </el-button>
    </section>

    <!-- Approval history. -->
    <section v-if="history.length > 0" class="mobile-approval__history">
      <h2 class="mobile-approval__subtitle">审批历史</h2>
      <el-timeline>
        <el-timeline-item
          v-for="entry in history"
          :key="entry.id"
          :timestamp="formatTimestamp(entry.timestamp)"
          :type="entry.action === 'approve' ? 'success' : 'danger'"
        >
          <div class="mobile-approval__history-entry">
            <span class="mobile-approval__history-actor">{{ entry.actor || '—' }}</span>
            <span class="mobile-approval__history-action">{{ entry.action }}</span>
            <span v-if="entry.detail" class="mobile-approval__history-detail">{{ entry.detail }}</span>
          </div>
        </el-timeline-item>
      </el-timeline>
    </section>
  </div>
</template>

<style scoped>
.mobile-approval {
  --ma-padding: 24px;
  --ma-gap: 16px;
  display: flex;
  flex-direction: column;
  gap: var(--ma-gap);
  padding: var(--ma-padding);
  max-width: 960px;
  margin: 0 auto;
  min-height: 100vh;
  box-sizing: border-box;
}

.mobile-approval--mobile {
  --ma-padding: 16px;
  --ma-gap: 12px;
  max-width: 100%;
}

.mobile-approval__header {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 12px;
}

.mobile-approval__title {
  font-size: 20px;
  font-weight: 600;
  margin: 0;
  white-space: nowrap;
  overflow: hidden;
  text-overflow: ellipsis;
}

.mobile-approval--mobile .mobile-approval__title {
  font-size: 18px;
}

.mobile-approval__error {
  margin-bottom: 8px;
}

.mobile-approval__loading {
  display: flex;
  align-items: center;
  justify-content: center;
  gap: 8px;
  padding: 48px 0;
  color: var(--el-text-color-secondary);
}

.mobile-approval__summary {
  display: grid;
  gap: 12px 24px;
  padding: 16px;
  background: var(--el-fill-color-light);
  border-radius: 8px;
}

.mobile-approval__field {
  display: flex;
  flex-direction: column;
  gap: 4px;
}

.mobile-approval__label {
  font-size: 12px;
  color: var(--el-text-color-secondary);
}

.mobile-approval__value {
  font-size: 14px;
  font-weight: 500;
}

.mobile-approval__actions {
  display: flex;
  gap: 12px;
  justify-content: flex-end;
}

.mobile-approval--mobile .mobile-approval__actions {
  position: fixed;
  left: 0;
  right: 0;
  bottom: 0;
  padding: 12px 16px;
  background: var(--el-bg-color);
  border-top: 1px solid var(--el-border-color-light);
  justify-content: stretch;
  z-index: 10;
}

.mobile-approval__btn--sticky {
  flex: 1;
}

.mobile-approval__history {
  margin-top: 8px;
}

.mobile-approval__subtitle {
  font-size: 16px;
  font-weight: 600;
  margin: 0 0 12px 0;
}

.mobile-approval__history-entry {
  display: flex;
  flex-wrap: wrap;
  gap: 8px;
  align-items: baseline;
}

.mobile-approval__history-actor {
  font-weight: 600;
}

.mobile-approval__history-action {
  color: var(--el-text-color-secondary);
}

.mobile-approval__history-detail {
  color: var(--el-text-color-secondary);
  font-size: 12px;
}
</style>