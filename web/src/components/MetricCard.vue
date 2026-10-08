<script setup lang="ts">
// MetricCard — one KPI: label, value, optional footnote and accent tone.
//
// The value is rendered in the mono face with tabular figures so a row of
// metrics does not jitter as numbers change, and so the digits line up with the
// ids and timestamps elsewhere on the page.
//
// `interactive` turns it into a button (the change board uses metrics as status
// filters). It is a real <button> rather than a div with role=button, because
// the latter owes the user a keydown handler, a tabindex and a disabled state,
// and this way the browser provides all three.
const props = withDefaults(
  defineProps<{
    label: string
    value: string | number
    footnote?: string
    tone?: 'neutral' | 'ok' | 'warn' | 'bad' | 'accent'
    interactive?: boolean
    selected?: boolean
  }>(),
  { tone: 'neutral', interactive: false, selected: false },
)

const emit = defineEmits<{ (e: 'select'): void }>()

function onClick(): void {
  if (props.interactive) emit('select')
}
</script>

<template>
  <component
    :is="interactive ? 'button' : 'div'"
    class="lv-metric"
    :class="[
      interactive ? 'lv-metric--interactive' : '',
      selected ? 'lv-metric--selected' : '',
    ]"
    :type="interactive ? 'button' : undefined"
    @click="onClick"
  >
    <span class="lv-metric__accent" :class="`metric-accent--${tone}`" aria-hidden="true"></span>
    <div class="lv-metric__label">{{ label }}</div>
    <div class="lv-metric__value">{{ value }}</div>
    <!-- Rendered even when empty: the card is a fixed three-row grid, and
         omitting this row made a footnote-less card's 1fr value row taller, so
         its number sat ~10px below its neighbours' in the same strip. -->
    <div class="lv-metric__foot">{{ footnote }}</div>
  </component>
</template>

<style scoped>
/* Reset for the button rendering; a <button> carries UA chrome that a div does
 * not, and both variants must look identical. */
button.lv-metric {
  display: block;
  width: 100%;
  font-family: inherit;
  font-size: inherit;
  text-align: left;
  color: inherit;
}

.metric-accent--ok {
  background: var(--lv-ok);
}

.metric-accent--warn {
  background: var(--lv-warn);
}

.metric-accent--bad {
  background: var(--lv-bad);
}

.metric-accent--accent {
  background: var(--lv-accent);
}

.metric-accent--neutral {
  background: var(--lv-neutral-tone);
}
</style>
