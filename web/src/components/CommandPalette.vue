<script setup lang="ts">
// CommandPalette — the Ctrl/Cmd+K action list.
//
// Why it exists: the console's two most common keyboard tasks are "take me to a
// page" and "open this run whose id I just read in a log line". Both currently
// mean navigating the rail and pasting into a field. This is the one piece of
// Cursor's design worth copying, because it is an interaction rather than a
// colour.
//
// Rendering rules it follows from the console's own design system:
//   * The panel is a CHROME surface, so it gets the glass material (blur +
//     specular edge) — the same treatment as menus and dialogs. Content surfaces
//     never do.
//   * Motion is one short scale+fade. An ops console is open during incidents;
//     a palette that springs is a palette that gets in the way.
//
// All the decision logic — ranking, id detection, which commands a query
// implies — lives in utils/commands.ts, where it is unit-tested.
import { computed, nextTick, ref, watch } from 'vue'
import { useRouter } from 'vue-router'
import { useTheme } from '@/composables/useTheme'
import {
  GROUP_ORDER,
  buildStaticCommands,
  filterCommands,
  jumpCommandsFor,
  type PaletteCommand,
  type PaletteGroup,
} from '@/utils/commands'

const visible = defineModel<boolean>({ required: true })

const router = useRouter()
const { setChoice } = useTheme()

const query = ref('')
const activeIndex = ref(0)
const inputRef = ref<HTMLInputElement | null>(null)
const listRef = ref<HTMLElement | null>(null)

// The rail's own information architecture, repeated here on purpose: the
// palette must be able to reach every page the rail does, including the two
// (/conversation, /cluster) that the rail nests under a group heading.
const ROUTES = [
  { path: '/changes', label: '变更看板', icon: 'Odometer', keywords: ['changes', 'board'] },
  { path: '/monitor', label: '实时监控', icon: 'Monitor', keywords: ['monitor', 'logs'] },
  { path: '/approval', label: '审批中心', icon: 'Stamp', keywords: ['approval', 'approve'] },
  { path: '/conversation', label: 'AI 对话', icon: 'ChatDotRound', keywords: ['ai', 'chat'] },
  { path: '/targets', label: '目标机', icon: 'Connection', keywords: ['targets', 'hosts'] },
  { path: '/templates', label: '模板管理', icon: 'Files', keywords: ['templates'] },
  { path: '/cluster', label: '集群状态', icon: 'Share', keywords: ['cluster', 'nodes'] },
  { path: '/audit', label: '审计查询', icon: 'Tickets', keywords: ['audit'] },
  { path: '/system', label: '系统状态', icon: 'Cpu', keywords: ['system', 'health'] },
]

function go(path: string): void {
  close()
  void router.push(path)
}

const staticCommands = computed<PaletteCommand[]>(() =>
  buildStaticCommands({ go, setTheme: (c) => setChoice(c), routes: ROUTES }),
)

// Jump commands first: a query that looks like an id is almost always an
// operator pasting one, and that row is the reason the palette exists.
const results = computed<PaletteCommand[]>(() => [
  ...jumpCommandsFor(query.value, go),
  ...filterCommands(staticCommands.value, query.value),
])

// Grouped for rendering, in the palette's fixed group order.
const grouped = computed<Array<{ group: PaletteGroup; items: PaletteCommand[] }>>(() =>
  GROUP_ORDER.map((group) => ({
    group,
    items: results.value.filter((c) => c.group === group),
  })).filter((g) => g.items.length > 0),
)

// Flat index ↔ command, so keyboard movement and rendering agree.
const flat = computed<PaletteCommand[]>(() => grouped.value.flatMap((g) => g.items))

function indexOfCommand(cmd: PaletteCommand): number {
  return flat.value.findIndex((c) => c.id === cmd.id)
}

watch(query, () => {
  activeIndex.value = 0
})

watch(visible, async (open) => {
  if (!open) {
    query.value = ''
    return
  }
  activeIndex.value = 0
  await nextTick()
  inputRef.value?.focus()
})

function close(): void {
  visible.value = false
}

function move(delta: number): void {
  const n = flat.value.length
  if (n === 0) return
  activeIndex.value = (activeIndex.value + delta + n) % n
  void nextTick(() => {
    const el = listRef.value?.querySelector<HTMLElement>('[data-active="true"]')
    el?.scrollIntoView({ block: 'nearest' })
  })
}

function runActive(): void {
  const cmd = flat.value[activeIndex.value]
  if (!cmd) return
  void cmd.run()
}

function onKeydown(event: KeyboardEvent): void {
  switch (event.key) {
    case 'ArrowDown':
      event.preventDefault()
      move(1)
      break
    case 'ArrowUp':
      event.preventDefault()
      move(-1)
      break
    case 'Enter':
      event.preventDefault()
      runActive()
      break
    case 'Escape':
      event.preventDefault()
      close()
      break
    default:
      break
  }
}
</script>

<template>
  <Teleport to="body">
    <Transition name="palette">
      <div
        v-if="visible"
        class="palette-root"
        role="dialog"
        aria-modal="true"
        aria-label="命令面板"
        @click.self="close"
      >
        <div class="palette lv-glass lv-glass--strong">
          <div class="palette__search">
            <el-icon class="palette__search-icon"><Search /></el-icon>
            <input
              ref="inputRef"
              v-model="query"
              class="palette__input"
              type="text"
              placeholder="搜索页面、动作，或粘贴变更 / run ID…"
              autocomplete="off"
              spellcheck="false"
              role="combobox"
              aria-expanded="true"
              aria-controls="palette-list"
              :aria-activedescendant="flat[activeIndex] ? `cmd-${flat[activeIndex]!.id}` : undefined"
              @keydown="onKeydown"
            />
            <kbd class="palette__kbd">esc</kbd>
          </div>

          <div id="palette-list" ref="listRef" class="palette__list" role="listbox">
            <div v-if="flat.length === 0" class="palette__empty">
              没有匹配的命令。试试粘贴一个变更 ID，或输入「诊断」「主题」。
            </div>

            <template v-for="g in grouped" :key="g.group">
              <div class="palette__group">{{ g.group }}</div>
              <button
                v-for="c in g.items"
                :id="`cmd-${c.id}`"
                :key="c.id"
                type="button"
                class="cmd"
                role="option"
                :aria-selected="indexOfCommand(c) === activeIndex"
                :data-active="indexOfCommand(c) === activeIndex"
                @click="c.run()"
                @mousemove="activeIndex = indexOfCommand(c)"
              >
                <el-icon class="cmd__icon"><component :is="c.icon" /></el-icon>
                <span class="cmd__title">{{ c.title }}</span>
                <span v-if="c.hint" class="cmd__hint lv-mono">{{ c.hint }}</span>
              </button>
            </template>
          </div>

          <div class="palette__foot">
            <span class="palette__hint"><kbd>↑</kbd><kbd>↓</kbd> 选择</span>
            <span class="palette__hint"><kbd>↵</kbd> 执行</span>
            <span class="palette__count lv-mono">{{ flat.length }} 项</span>
          </div>
        </div>
      </div>
    </Transition>
  </Teleport>
</template>

<style scoped>
.palette-root {
  position: fixed;
  inset: 0;
  z-index: 3000;
  display: flex;
  justify-content: center;
  align-items: flex-start;
  padding: 14vh var(--lv-space-4) var(--lv-space-4);
  background: var(--lv-scrim);
  backdrop-filter: blur(2px);
}

.palette {
  width: min(600px, 100%);
  max-height: 62vh;
  display: flex;
  flex-direction: column;
  border: 1px solid var(--lv-glass-border);
  border-radius: var(--lv-radius-xl);
  overflow: hidden;
}

/* --------------------------------------------------------------- search row */

.palette__search {
  display: flex;
  align-items: center;
  gap: var(--lv-space-3);
  padding: var(--lv-space-4) var(--lv-space-4) var(--lv-space-3);
  border-bottom: 1px solid var(--lv-border-soft);
}

.palette__search-icon {
  flex: none;
  font-size: 17px;
  color: var(--lv-text-3);
}

.palette__input {
  flex: 1;
  min-width: 0;
  border: none;
  outline: none;
  background: transparent;
  font-family: inherit;
  font-size: var(--lv-text-lg);
  color: var(--lv-text-1);
}

.palette__input::placeholder {
  color: var(--lv-text-3);
}

.palette__kbd,
.palette__hint kbd {
  display: inline-block;
  padding: 1px 5px;
  border: 1px solid var(--lv-border);
  border-radius: var(--lv-radius-sm);
  background: var(--lv-surface-3);
  font-family: var(--lv-font-mono);
  font-size: 10px;
  line-height: 1.6;
  color: var(--lv-text-3);
}

/* -------------------------------------------------------------------- list */

.palette__list {
  flex: 1;
  overflow-y: auto;
  padding: var(--lv-space-2);
}

.palette__group {
  padding: var(--lv-space-2) var(--lv-space-3) 4px;
  font-size: 11px;
  font-weight: 600;
  letter-spacing: var(--lv-tracking-caps);
  text-transform: uppercase;
  color: var(--lv-text-3);
}

.cmd {
  display: flex;
  align-items: center;
  gap: var(--lv-space-3);
  width: 100%;
  padding: 9px var(--lv-space-3);
  border: none;
  border-radius: var(--lv-radius-lg);
  background: transparent;
  font-family: inherit;
  font-size: var(--lv-text-base);
  text-align: left;
  cursor: pointer;
  /* The active row is the only thing that animates: a fast tint, no transform,
   * so the eye is never asked to track movement while typing. */
  transition: background-color 90ms var(--lv-ease);
}

.cmd[data-active='true'] {
  background: var(--lv-accent-soft);
}

.cmd[data-active='true'] .cmd__title {
  color: var(--lv-accent);
  font-weight: 600;
}

.cmd__icon {
  flex: none;
  font-size: 15px;
  color: var(--lv-text-3);
}

.cmd[data-active='true'] .cmd__icon {
  color: var(--lv-accent);
}

.cmd__title {
  flex: 1;
  min-width: 0;
  color: var(--lv-text-1);
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}

.cmd__hint {
  flex: none;
  font-size: var(--lv-text-xs);
  color: var(--lv-text-3);
}

.palette__empty {
  padding: var(--lv-space-8) var(--lv-space-4);
  text-align: center;
  font-size: var(--lv-text-sm);
  color: var(--lv-text-3);
}

/* -------------------------------------------------------------------- foot */

.palette__foot {
  display: flex;
  align-items: center;
  gap: var(--lv-space-4);
  padding: var(--lv-space-2) var(--lv-space-4);
  border-top: 1px solid var(--lv-border-soft);
}

.palette__hint {
  display: inline-flex;
  align-items: center;
  gap: 3px;
  font-size: var(--lv-text-xs);
  color: var(--lv-text-3);
}

.palette__count {
  margin-left: auto;
  font-size: var(--lv-text-xs);
  color: var(--lv-text-3);
}

/* ------------------------------------------------------------------ motion */

.palette-enter-active,
.palette-leave-active {
  transition: opacity 140ms var(--lv-ease);
}

.palette-enter-active .palette,
.palette-leave-active .palette {
  transition: transform 140ms var(--lv-ease);
}

.palette-enter-from,
.palette-leave-to {
  opacity: 0;
}

.palette-enter-from .palette,
.palette-leave-to .palette {
  transform: scale(0.985) translateY(-4px);
}

@media (prefers-reduced-motion: reduce) {
  .palette-enter-active,
  .palette-leave-active,
  .palette-enter-active .palette,
  .palette-leave-active .palette {
    transition: none;
  }
}
</style>
