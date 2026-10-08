<script setup lang="ts">
// StatusTag renders a change status as a dot + label.
//
// It used to wrap el-tag, so every status inherited Element Plus's tag chrome:
// a filled rounded rectangle in one of five generic colours. On a governance
// console the status is the most-read field on the page and the SEMANTICS are
// the point — "rolled back" is not the same kind of thing as "still running",
// and neither is "the executor died". So the status-to-TONE mapping is spelled
// out here (five tones, more expressive than EP's tag types) and the visual is
// the console's own: a coloured dot beside a label, tinted only in pill form.
//
// The LABEL still comes from utils/format, which the Go-side vocabulary guards
// pin; a status the backend adds but this table has not heard of falls back to
// the neutral tone and still renders its label instead of disappearing.
import { computed } from 'vue'
import { STATUS_LABEL } from '@/utils/format'
import type { ChangeStatus } from '@/types/levee'

const props = withDefaults(
  defineProps<{
    status: ChangeStatus
    /** 'pill' for standalone use, 'plain' inside dense table cells. */
    variant?: 'pill' | 'plain'
  }>(),
  { variant: 'pill' },
)

const label = computed(() => STATUS_LABEL[props.status] || props.status)

// Tone per status. Each row's comment is the operational meaning, because that
// is what decides the colour — not a severity ordering guessed from the word.
type Tone = 'live' | 'ok' | 'warn' | 'bad' | 'idle'

const TONE_BY_STATUS: Record<ChangeStatus, Tone> = {
  draft: 'idle', // not submitted; nothing is happening yet
  planned: 'idle', // a plan exists, still no approval
  pending: 'warn', // waiting on a human — the thing that most often stalls a change
  approved: 'live', // cleared to run, not running yet
  running: 'live', // actively touching targets
  paused: 'warn', // deliberately held; resumable, not broken
  completed: 'ok',
  archived: 'idle', // sealed history
  cancelled: 'idle', // a human stopped it before any harm
  rejected: 'bad', // denied
  failed: 'bad',
  interrupted: 'bad', // the executor node died mid-flight (cluster takeover)
  rollback_incomplete: 'bad', // compensation did not finish — worst outcome
  rolled_back: 'warn', // compensated, but targets were still touched
  rolled_back_partial: 'warn', // some hosts compensated, some did not
}

const tone = computed<Tone>(() => TONE_BY_STATUS[props.status] ?? 'idle')
const isLive = computed(() => props.status === 'running')
</script>

<template>
  <span class="status" :class="[`status--${tone}`, `status--${variant}`]">
    <span class="status__dot lv-dot" :class="isLive ? 'lv-dot--live' : `lv-dot--${tone}`" aria-hidden="true"></span>
    <span class="status__label">{{ label }}</span>
  </span>
</template>

<style scoped>
.status {
  display: inline-flex;
  align-items: center;
  gap: 7px;
  white-space: nowrap;
  font-size: var(--lv-text-sm);
  color: var(--lv-text-2);
}

.status--pill {
  height: 24px;
  padding: 0 10px 0 8px;
  border: 1px solid transparent;
  border-radius: var(--lv-radius-full);
}

.status--plain {
  padding: 0;
  border: none;
  background: none;
}

.status__dot {
  width: 7px;
  height: 7px;
}

/* Pill variant: tint + border per tone. */
.status--pill.status--ok {
  background: var(--lv-ok-soft);
  border-color: var(--lv-ok-border);
  color: var(--lv-ok);
}

.status--pill.status--warn {
  background: var(--lv-warn-soft);
  border-color: var(--lv-warn-border);
  color: var(--lv-warn);
}

.status--pill.status--bad {
  background: var(--lv-bad-soft);
  border-color: var(--lv-bad-border);
  color: var(--lv-bad);
}

.status--pill.status--live {
  background: var(--lv-accent-soft);
  border-color: var(--lv-accent-border);
  color: var(--lv-accent);
}

.status--pill.status--idle {
  background: var(--lv-neutral-soft);
  border-color: var(--lv-neutral-border);
  color: var(--lv-text-2);
}

.status--pill .status__label {
  color: inherit;
  font-weight: 500;
}

/* Plain variant: the dot carries the tone and the label stays body-coloured,
 * so a dense table does not become a field of coloured text. */
.status--plain.status--ok .status__dot {
  background: var(--lv-ok);
}

.status--plain.status--warn .status__dot {
  background: var(--lv-warn);
}

.status--plain.status--bad .status__dot {
  background: var(--lv-bad);
}

.status--plain.status--live .status__dot {
  background: var(--lv-accent);
}

@media (prefers-reduced-motion: reduce) {
  .status__dot {
    animation: none;
  }
}
</style>
