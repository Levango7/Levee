<script setup lang="ts">
// HashCell — one hash as it appears in the audit table: mono, middle-elided,
// with the full value one hover away.
//
// Two decisions worth stating:
//
//  * Ellipsis in the MIDDLE, not the end. The first bytes of a chain hash are
//    what an operator compares against another screen, and the last bytes are
//    what distinguishes two otherwise-similar rows; eliding the tail would hide
//    exactly the part used to tell rows apart.
//  * The full value is in `title`, so hovering a truncated hash yields the
//    whole string without a copy step. A copy button was considered and left
//    out: the audit view shows two hash columns on every row, and two copy
//    buttons per row is noise for a case a text selection already covers.
import { computed } from 'vue'

const props = withDefaults(
  defineProps<{
    value: string
    /** Total characters shown (head + tail + the ellipsis glyph). */
    width?: number
  }>(),
  { width: 18 },
)

const HEAD = 8
const TAIL = 6

const display = computed(() => {
  const v = props.value
  if (!v) return '—'
  // Short enough to read whole, or short enough that eliding saves nothing.
  if (v.length <= props.width) return v
  return `${v.slice(0, HEAD)}…${v.slice(-TAIL)}`
})

const isGenesis = computed(() => !props.value)
</script>

<template>
  <span class="hash lv-mono" :class="{ 'hash--empty': isGenesis }" :title="value || '未封链（该行还没有链哈希）'">
    {{ display }}
  </span>
</template>

<style scoped>
.hash {
  display: inline-block;
  max-width: 100%;
  font-size: var(--lv-text-xs);
  color: var(--lv-text-2);
  letter-spacing: -0.02em;
  white-space: nowrap;
}

.hash--empty {
  color: var(--lv-text-3);
}
</style>
