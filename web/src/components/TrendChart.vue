<script setup lang="ts">
// TrendChart draws the change-outcome trend as a stacked column chart.
//
// Hand-rolled SVG rather than a charting library, on purpose:
//
//   1. Every colour, font and radius already exists in styles/tokens.css, so a
//      hand-drawn chart is the same design language as the rest of the console.
//      A charting library brings its own palette and would be the one thing on
//      the page that does not match.
//   2. The bundle matters: this console is embedded in a Go binary that
//      deployments serve from a single host. A charting library is ~1MB before
//      a single bar is drawn.
//   3. What the chart actually needs is one shape (stacked columns) with a zero
//      base, and the geometry lives in utils/trend.ts where it is tested
//      without a DOM.
//
// It is a presentation component: no fetching, no state, no Element Plus. The
// parent owns the data and the three non-ok states.
import { computed } from 'vue'
import { TREND_SERIES, buildGeometry, dayLabel } from '@/utils/trend'
import type { TrendBucket } from '@/utils/trend'

const props = withDefaults(
  defineProps<{
    buckets: TrendBucket[]
    /** Cell width; the chart is fluid up to this. */
    width?: number
    height?: number
  }>(),
  { width: 640, height: 180 },
)

const geometry = computed(() => buildGeometry(props.buckets, { width: props.width, height: props.height }))

// Tone -> design token. The tone names are the ones utils/node.ts pins against
// base.css, so a missing rule is a test failure rather than an invisible bar.
const toneVar: Record<string, string> = {
  ok: 'var(--lv-ok)',
  live: 'var(--lv-accent)',
  warn: 'var(--lv-warn)',
  bad: 'var(--lv-bad)',
  idle: 'var(--lv-neutral-tone)',
}

const legend = computed(() =>
  TREND_SERIES.map((s) => ({ ...s, color: toneVar[s.tone] || toneVar.idle })),
)
</script>

<template>
  <div class="trend">
    <svg
      class="trend__svg"
      :viewBox="`0 0 ${geometry.width} ${geometry.height}`"
      :width="geometry.width"
      :height="geometry.height"
      role="img"
      aria-label="变更结局趋势"
      preserveAspectRatio="xMidYMid meet"
    >
      <!-- Y grid + labels. Drawn from the top tick down so the labels sit on
           the same line as their gridline rather than floating beside it. -->
      <g v-for="t in geometry.ticks" :key="'tick-' + t.value">
        <line
          :x1="geometry.plot.x"
          :x2="geometry.plot.x + geometry.plot.width"
          :y1="t.y"
          :y2="t.y"
          class="trend__grid"
        />
        <text :x="geometry.plot.x - 6" :y="t.y + 3" class="trend__ytick" text-anchor="end">{{ t.value }}</text>
      </g>

      <!-- Columns. A day with no runs still draws its slot's baseline tick so
           the gap reads as a gap (utils/trend.ts keeps the empty column). -->
      <g v-for="s in geometry.stacks" :key="s.day">
        <rect
          v-for="seg in s.segments"
          :key="seg.series"
          :x="s.x"
          :y="seg.y"
          :width="s.width"
          :height="Math.max(seg.height, 0.5)"
          :fill="toneVar[legend.find((l) => l.key === seg.series)?.tone || 'idle']"
          class="trend__seg"
        >
          <title>{{ seg.series }}: {{ seg.value }}</title>
        </rect>
      </g>

      <!-- X labels, thinned by the geometry until they stop colliding. -->
      <text v-for="l in geometry.labels" :key="'x-' + l.day" :x="l.x" :y="geometry.plot.y + geometry.plot.height + 14" class="trend__xtick" text-anchor="middle">
        {{ dayLabel(l.day) }}
      </text>

      <!-- Baseline: the axis the stacked columns sit on. -->
      <line
        :x1="geometry.plot.x"
        :x2="geometry.plot.x + geometry.plot.width"
        :y1="geometry.plot.y + geometry.plot.height"
        :y2="geometry.plot.y + geometry.plot.height"
        class="trend__axis"
      />
    </svg>

    <ul class="trend__legend">
      <li v-for="s in legend" :key="s.key" class="trend__legend-item">
        <span class="trend__swatch" :style="{ background: s.color }" aria-hidden="true"></span>
        {{ s.label }}
      </li>
    </ul>
  </div>
</template>

<style scoped>
.trend {
  display: flex;
  flex-direction: column;
  gap: var(--lv-space-2);
}

.trend__svg {
  display: block;
  max-width: 100%;
  height: auto;
}

/* Hairline grid: the ops console reads for hours, so the grid is the weakest
 * visible line and the axis is the strongest. */
.trend__grid {
  stroke: var(--lv-border-soft);
  stroke-width: 1;
}

.trend__axis {
  stroke: var(--lv-border-strong);
  stroke-width: 1;
}

.trend__ytick,
.trend__xtick {
  fill: var(--lv-text-3);
  font-family: var(--lv-font-mono);
  font-size: 10px;
}

.trend__seg {
  transition: opacity 0.12s ease;
}

.trend__legend {
  display: flex;
  flex-wrap: wrap;
  gap: var(--lv-space-2) var(--lv-space-4);
  margin: 0;
  padding: 0;
  list-style: none;
  font-size: var(--lv-text-xs);
  color: var(--lv-text-2);
}

.trend__legend-item {
  display: inline-flex;
  align-items: center;
  gap: 6px;
}

.trend__swatch {
  width: 9px;
  height: 9px;
  border-radius: 2px;
}

@media (prefers-reduced-motion: reduce) {
  .trend__seg {
    transition: none;
  }
}
</style>
