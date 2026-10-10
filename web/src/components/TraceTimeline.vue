<script setup lang="ts">
// TraceTimeline — the lifecycle timeline, shared by the change detail page and
// the audit view.
//
// Extracted rather than copied: the two views want the same rendering (action
// token in mono, target, actor, duration, detail line) and the audit view's
// roadmap row asks for the detail page's timeline component specifically. A
// second copy would be the usual drift — one view learning to show something
// the other silently lacks.
//
// Two deliberate properties:
//
//  * The ORDER is the caller's. A timeline component that reorders its input
//    hides a decision the caller has to make anyway (the detail page reads
//    newest-first: an operator arriving at a failed change wants the verdict
//    and the last thing that happened, not the first step).
//  * The action is rendered as its RAW TOKEN. There is no label map for the
//    trace/audit action vocabulary in the UI, on purpose: the vocabulary's
//    owner is the backend, and a partial Chinese map here would drift from it
//    silently. The audit view takes the same stance.
import type { TraceEntry } from '@/types/levee'
import { formatTimestamp } from '@/utils/format'

defineProps<{
  entries: TraceEntry[]
  /** Shown when there is nothing to render. */
  emptyText?: string
  /** Hide the timestamp column for dense, already-timestamped contexts. */
  dense?: boolean
}>()
</script>

<template>
  <div class="tl-wrap">
    <el-timeline v-if="entries.length > 0" :class="{ 'tl-wrap--dense': dense }">
      <el-timeline-item
        v-for="e in entries"
        :key="e.id"
        :timestamp="dense ? '' : formatTimestamp(e.timestamp)"
        placement="top"
      >
        <div class="tl">
          <span class="tl__action lv-mono">{{ e.action }}</span>
          <span v-if="e.targetHost" class="tl__host lv-mono">{{ e.targetHost }}</span>
          <span v-if="e.actor" class="tl__host">{{ e.actor }}</span>
          <span v-if="e.durationMs" class="tl__dim">{{ e.durationMs }} ms</span>
          <span v-if="dense" class="tl__dim lv-mono">{{ formatTimestamp(e.timestamp) }}</span>
        </div>
        <div v-if="e.detail" class="tl__detail">{{ e.detail }}</div>
      </el-timeline-item>
    </el-timeline>
    <el-empty v-else :description="emptyText || '暂无记录'" :image-size="60" />
  </div>
</template>

<style scoped>
.tl {
  display: flex;
  align-items: center;
  gap: var(--lv-space-2, 8px);
  flex-wrap: wrap;
}

.tl__action {
  font-weight: 600;
}

.tl__host {
  color: var(--el-text-color-secondary);
  font-size: 12px;
}

.tl__dim {
  color: var(--el-text-color-secondary);
  font-size: 12px;
}

.tl__detail {
  color: var(--el-text-color-secondary);
  font-size: 12px;
  margin-top: 2px;
}

.tl-wrap--dense :deep(.el-timeline-item) {
  padding-bottom: 8px;
}
</style>
