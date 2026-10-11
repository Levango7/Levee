<script setup lang="ts">
// StateBlock — the single surface for the three non-ok outcomes of a region's
// load. Before this, each view answered them differently: some raised a toast
// and rendered an empty table, some wrote a bare <p class="error">, one had an
// el-empty with phrasing it invented. An operator reading "nothing happened"
// could not tell "there is nothing" from "the fetch failed" from "you may not
// look" — and only one of those is fixed by retrying.
//
// The three tiers map to three actions (see utils/apiState.ts):
//   empty     — offer the next step (clear the filter, create the first item)
//   error     — offer a retry
//   forbidden — quote the server's reason and point at the role that is missing
//
// Loading stays where it was (the view's v-loading / skeleton), so this
// component only renders terminal states and a region never shows two of them
// at once.
import { computed } from 'vue'
import type { StateTier } from '@/utils/apiState'

const props = withDefaults(
  defineProps<{
    tier: StateTier
    /** Server message for the error tier; the shown reason for forbidden. */
    message?: string
    /** One line under the title explaining what to do next. */
    hint?: string
    /** Label for the leading action (slot `default`), e.g. "清除筛选条件". */
    actionLabel?: string
    /** Retry button label for the error tier. Defaults to "重试". */
    retryLabel?: string
    /** Sit inside a panel with less vertical air than on the page. */
    dense?: boolean
  }>(),
  {
    message: '',
    hint: '',
    actionLabel: '',
    retryLabel: '重试',
    dense: false,
  },
)

const emit = defineEmits<{
  (e: 'retry'): void
  (e: 'action'): void
}>()

// Titles per tier. They name the situation, not the button: "没有数据" is what
// the operator needs to know, and the hint below says what to do about it.
const TITLES: Record<StateTier, string> = {
  empty: '没有数据',
  error: '加载失败',
  forbidden: '没有权限查看',
}

const title = computed(() => TITLES[props.tier])
</script>

<template>
  <div class="state" :class="[`state--${tier}`, { 'state--dense': dense }]" role="status">
    <!-- Inline stroke icons, one per tier and never reused for another
         meaning, so the shape alone answers "which of the three is this". -->
    <svg v-if="tier === 'empty'" class="state__icon" viewBox="0 0 48 48" fill="none" aria-hidden="true">
      <path d="M8 16.5 24 8l16 8.5v19L24 44 8 35.5v-19Z" stroke="currentColor" stroke-width="1.5" stroke-linejoin="round" />
      <path d="M8 16.5 24 25l16-8.5M24 25v19" stroke="currentColor" stroke-width="1.5" stroke-linejoin="round" />
    </svg>
    <svg v-else-if="tier === 'error'" class="state__icon" viewBox="0 0 48 48" fill="none" aria-hidden="true">
      <path d="M24 7 44 40H4L24 7Z" stroke="currentColor" stroke-width="1.5" stroke-linejoin="round" />
      <path d="M24 19v10" stroke="currentColor" stroke-width="1.5" stroke-linecap="round" />
      <circle cx="24" cy="35" r="1.25" fill="currentColor" />
    </svg>
    <svg v-else class="state__icon" viewBox="0 0 48 48" fill="none" aria-hidden="true">
      <rect x="11" y="21" width="26" height="18" rx="3" stroke="currentColor" stroke-width="1.5" />
      <path d="M16 21v-5a8 8 0 0 1 16 0v5" stroke="currentColor" stroke-width="1.5" stroke-linecap="round" />
      <circle cx="24" cy="30" r="2" fill="currentColor" />
    </svg>

    <p class="state__title">{{ title }}</p>
    <!-- The server's own words: a 403 handler names the rule it refused
         ("session belongs to another user", "unauthorized approver"), which is
         more actionable than any generic copy we could write here. -->
    <p v-if="message" class="state__message">{{ message }}</p>
    <p v-if="hint" class="state__hint">{{ hint }}</p>

    <div v-if="tier === 'error'" class="state__actions">
      <button type="button" class="state__btn state__btn--primary" @click="emit('retry')">
        {{ retryLabel }}
      </button>
    </div>
    <div v-else-if="tier === 'empty' && (actionLabel || $slots.default)" class="state__actions">
      <button v-if="actionLabel" type="button" class="state__btn" @click="emit('action')">
        {{ actionLabel }}
      </button>
      <slot />
    </div>
  </div>
</template>

<style scoped>
.state {
  display: flex;
  flex-direction: column;
  align-items: center;
  justify-content: center;
  gap: 4px;
  padding: 48px 24px;
  text-align: center;
}

.state--dense {
  padding: 28px 16px;
}

/* Tone per tier, from the same token families StatusTag uses, so an error
 * state reads the same red everywhere. */
.state--empty {
  color: var(--lv-text-3);
}

.state--error .state__icon {
  color: var(--lv-bad);
}

.state--forbidden .state__icon {
  color: var(--lv-warn);
}

.state__icon {
  width: 40px;
  height: 40px;
  margin-bottom: 10px;
}

.state__title {
  margin: 0;
  font-size: var(--lv-text-md);
  font-weight: 600;
  color: var(--lv-text-1);
}

.state__message {
  margin: 2px 0 0;
  max-width: 52ch;
  font-size: var(--lv-text-sm);
  color: var(--lv-text-2);
  /* Server messages are prose a handler assembled; let them wrap rather than
   * truncate, the way a refusal reason already wraps in the log pane. */
  white-space: pre-line;
}

.state__hint {
  margin: 2px 0 0;
  max-width: 52ch;
  font-size: var(--lv-text-sm);
  color: var(--lv-text-3);
}

.state__actions {
  display: flex;
  align-items: center;
  gap: 8px;
  margin-top: 14px;
}

.state__btn {
  padding: 7px 14px;
  border: 1px solid var(--lv-border);
  border-radius: var(--lv-radius);
  background: var(--lv-surface);
  color: var(--lv-text-1);
  font-size: var(--lv-text-sm);
  font-family: inherit;
  cursor: pointer;
  transition: border-color 0.15s ease, color 0.15s ease;
}

.state__btn:hover {
  border-color: var(--lv-accent-border);
  color: var(--lv-accent);
}

.state__btn:focus-visible {
  outline: 2px solid var(--lv-accent);
  outline-offset: 1px;
}

.state__btn--primary {
  border-color: var(--lv-accent);
  background: var(--lv-accent);
  color: var(--lv-accent-contrast);
}

.state__btn--primary:hover {
  background: var(--lv-accent-hover);
  border-color: var(--lv-accent-hover);
  color: var(--lv-accent-contrast);
}
</style>
