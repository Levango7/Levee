<script setup lang="ts">
// App.vue is the layout shell: a dark navigation rail, a sticky glass top bar,
// and the scrolling work area.
//
// Two structural decisions worth stating:
//
//  * The top bar is STICKY INSIDE the scroll container, not a fixed row above
//    it. That is what makes the glass material mean anything: content slides
//    under the bar and shows through it. A blurred bar over a static page
//    background renders identically to an opaque one, at a compositing cost.
//  * The rail is opaque in both themes — it is the console's frame, and it is
//    the only place the brand teal appears at full strength. Glass is for the
//    chrome that floats over content; the frame does not float.
//
// Shell responsibilities stay narrow: navigation, the theme switch, the session
// menu, and the command palette. Per-page state lives in the views.
import { computed, onMounted, onUnmounted, ref, watch } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import { ElMessage } from 'element-plus'
import { clearToken } from '@/api/client'
import { useTheme, type ThemeChoice } from '@/composables/useTheme'
import CommandPalette from '@/components/CommandPalette.vue'

const route = useRoute()
const router = useRouter()
const { choice: themeChoice, resolved: themeResolved, setChoice: setTheme } = useTheme()

const collapsed = ref(false)
const paletteOpen = ref(false)

// Navigation groups: the rail's information architecture. Ordered by how often
// an operator touches them — work first, inventory second, platform last.
interface NavItem {
  path: string
  label: string
  icon: string
}
interface NavGroup {
  title: string
  items: NavItem[]
}

const navGroups: NavGroup[] = [
  {
    title: '变更运营',
    items: [
      { path: '/changes', label: '变更看板', icon: 'Odometer' },
      { path: '/monitor', label: '实时监控', icon: 'Monitor' },
      { path: '/approval', label: '审批中心', icon: 'Stamp' },
      { path: '/conversation', label: 'AI 对话', icon: 'ChatDotRound' },
    ],
  },
  {
    title: '资产与模板',
    items: [
      { path: '/targets', label: '目标机', icon: 'Connection' },
      { path: '/templates', label: '模板管理', icon: 'Files' },
    ],
  },
  {
    title: '平台',
    items: [
      { path: '/cluster', label: '集群状态', icon: 'Share' },
      { path: '/audit', label: '审计查询', icon: 'Tickets' },
      { path: '/system', label: '系统状态', icon: 'Cpu' },
    ],
  },
]

// isActive matches by path prefix so nested routes (/changes/:id, /monitor/:x)
// keep their parent entry lit; root-anchored so /cluster cannot match /c.
function isActive(path: string): boolean {
  return route.path === path || route.path.startsWith(path + '/')
}

const pageTitle = computed(() => (route.meta.title as string) || 'LEVEE')

const currentGroup = computed(
  () => navGroups.find((g) => g.items.some((i) => isActive(i.path)))?.title ?? '',
)

// Standalone pages (login, SSO callback, mobile approval deeplinks) render
// without the shell chrome so they work on phones and pre-auth screens.
const showShell = computed(
  () => !route.path.startsWith('/m/') && route.path !== '/login' && route.path !== '/login/callback',
)

const THEME_OPTIONS: Array<{ value: ThemeChoice; label: string; icon: string }> = [
  { value: 'light', label: '浅色', icon: 'Sunny' },
  { value: 'dark', label: '深色', icon: 'Moon' },
  { value: 'system', label: '跟随系统', icon: 'Laptop' },
]

const themeIcon = computed(() => (themeResolved.value === 'dark' ? 'Moon' : 'Sunny'))

function handleCommand(command: string): void {
  if (command === 'logout') {
    clearToken()
    ElMessage.success('已退出登录')
    router.push('/login')
  } else if (command === 'docs') {
    window.open('https://github.com/nexus/levee', '_blank')
  } else {
    setTheme(command as ThemeChoice)
  }
}

// ---------------------------------------------------------------------------
// Command palette. Ctrl/Cmd+K anywhere in the shell; the same shortcut closes
// it. Not bound on standalone pages (login has nothing to navigate to).
function onGlobalKeydown(event: KeyboardEvent): void {
  if (!showShell.value) return
  const k = event.key.toLowerCase()
  if ((event.ctrlKey || event.metaKey) && k === 'k') {
    event.preventDefault()
    paletteOpen.value = !paletteOpen.value
  }
}

onMounted(() => window.addEventListener('keydown', onGlobalKeydown))
onUnmounted(() => window.removeEventListener('keydown', onGlobalKeydown))

// ---------------------------------------------------------------------------
// Clock: an operator correlates what they are reading with the timestamps in
// logs and audit rows, so the header carries the current time. Fixed width so
// it never jitters.
const now = ref(new Date())
let clockTimer: ReturnType<typeof setInterval> | undefined

onMounted(() => {
  clockTimer = setInterval(() => {
    now.value = new Date()
  }, 1000)
})
onUnmounted(() => {
  if (clockTimer) clearInterval(clockTimer)
})

const clockText = computed(() => {
  const d = now.value
  const p = (n: number): string => String(n).padStart(2, '0')
  return `${p(d.getHours())}:${p(d.getMinutes())}:${p(d.getSeconds())}`
})

// Keep the window title in step with the route: an operator running several
// consoles needs to tell them apart from the task bar.
watch(
  pageTitle,
  (t) => {
    document.title = t === 'LEVEE' ? 'LEVEE Console' : `${t} · LEVEE`
  },
  { immediate: true },
)
</script>

<template>
  <div class="layout">
    <aside v-if="showShell" class="rail" :class="{ 'rail--collapsed': collapsed }">
      <div class="rail__brand">
        <span class="rail__mark" aria-hidden="true">
          <svg viewBox="0 0 24 24" width="18" height="18" fill="none">
            <!-- Levee mark: three stacked courses of a flood wall. -->
            <path d="M3 5h18v4H3z" fill="currentColor" opacity="0.95" />
            <path d="M3 11h18v4H3z" fill="currentColor" opacity="0.6" />
            <path d="M3 17h18v3H3z" fill="currentColor" opacity="0.3" />
          </svg>
        </span>
        <span v-if="!collapsed" class="rail__wordmark">
          <span class="rail__name">LEVEE</span>
          <span class="rail__tagline">变更治理控制台</span>
        </span>
      </div>

      <nav class="rail__nav" aria-label="主导航">
        <div v-for="group in navGroups" :key="group.title" class="rail__group">
          <div v-if="!collapsed" class="rail__group-title">{{ group.title }}</div>
          <div v-else class="rail__group-rule" aria-hidden="true"></div>

          <router-link
            v-for="item in group.items"
            :key="item.path"
            :to="item.path"
            class="rail__link"
            :class="{ 'rail__link--active': isActive(item.path) }"
            :title="collapsed ? item.label : undefined"
            :aria-current="isActive(item.path) ? 'page' : undefined"
          >
            <el-icon class="rail__icon"><component :is="item.icon" /></el-icon>
            <span v-if="!collapsed" class="rail__label">{{ item.label }}</span>
          </router-link>
        </div>
      </nav>

      <div class="rail__foot">
        <button class="rail__collapse" type="button" @click="collapsed = !collapsed">
          <el-icon><component :is="collapsed ? 'Expand' : 'Fold'" /></el-icon>
          <span v-if="!collapsed">收起导航</span>
        </button>
      </div>
    </aside>

    <!-- The scroll container. The top bar is sticky INSIDE it, so page content
         passes underneath the glass material. -->
    <main class="sheet" :class="{ 'sheet--standalone': !showShell }">
      <header v-if="showShell" class="topbar lv-glass">
        <div class="topbar__left">
          <span v-if="currentGroup" class="topbar__crumb">{{ currentGroup }}</span>
          <el-icon v-if="currentGroup" class="topbar__crumb-sep"><ArrowRight /></el-icon>
          <h1 class="topbar__title">{{ pageTitle }}</h1>
        </div>

        <div class="topbar__right">
          <button class="topbar__search" type="button" @click="paletteOpen = true">
            <el-icon class="topbar__search-icon"><Search /></el-icon>
            <span class="topbar__search-label">搜索或跳转…</span>
            <kbd class="topbar__search-kbd">Ctrl K</kbd>
          </button>

          <span class="topbar__clock lv-mono" :title="now.toLocaleString()">{{ clockText }}</span>

          <el-dropdown trigger="click" @command="handleCommand">
            <button
              class="topbar__icon-btn"
              type="button"
              :title="`主题：${THEME_OPTIONS.find((o) => o.value === themeChoice)?.label}`"
            >
              <el-icon><component :is="themeIcon" /></el-icon>
            </button>
            <template #dropdown>
              <el-dropdown-menu>
                <el-dropdown-item
                  v-for="opt in THEME_OPTIONS"
                  :key="opt.value"
                  :command="opt.value"
                  :class="{ 'is-active': themeChoice === opt.value }"
                >
                  <el-icon><component :is="opt.icon" /></el-icon>
                  {{ opt.label }}
                </el-dropdown-item>
              </el-dropdown-menu>
            </template>
          </el-dropdown>

          <el-dropdown trigger="click" @command="handleCommand">
            <button class="topbar__user" type="button">
              <span class="topbar__avatar" aria-hidden="true">OP</span>
              <span class="topbar__user-name">operator</span>
              <el-icon class="topbar__caret"><ArrowDown /></el-icon>
            </button>
            <template #dropdown>
              <el-dropdown-menu>
                <el-dropdown-item command="docs">
                  <el-icon><Document /></el-icon>
                  文档
                </el-dropdown-item>
                <el-dropdown-item command="logout" divided>
                  <el-icon><SwitchButton /></el-icon>
                  退出登录
                </el-dropdown-item>
              </el-dropdown-menu>
            </template>
          </el-dropdown>
        </div>
      </header>

      <div class="page-host">
        <router-view v-slot="{ Component }">
          <transition name="page" mode="out-in">
            <component :is="Component" />
          </transition>
        </router-view>
      </div>
    </main>

    <CommandPalette v-model="paletteOpen" />
  </div>
</template>

<style scoped>
.layout {
  display: flex;
  height: 100%;
  overflow: hidden;
}

/* ------------------------------------------------------------------- rail */

.rail {
  display: flex;
  flex-direction: column;
  flex: none;
  width: var(--lv-rail-width);
  background: linear-gradient(180deg, var(--lv-rail-bg-top), var(--lv-rail-bg));
  border-right: 1px solid var(--lv-rail-border);
  transition: width var(--lv-dur) var(--lv-ease);
  overflow: hidden;
}

.rail--collapsed {
  width: var(--lv-rail-width-collapsed);
}

.rail__brand {
  display: flex;
  align-items: center;
  gap: 10px;
  height: var(--lv-topbar-height);
  padding: 0 16px;
  border-bottom: 1px solid var(--lv-rail-border);
  flex: none;
}

.rail__mark {
  display: inline-flex;
  align-items: center;
  justify-content: center;
  width: 28px;
  height: 28px;
  flex: none;
  border-radius: var(--lv-radius);
  background: rgba(63, 173, 170, 0.16);
  color: var(--lv-rail-active-text);
}

.rail__wordmark {
  display: flex;
  flex-direction: column;
  min-width: 0;
}

.rail__name {
  font-size: 15px;
  font-weight: 600;
  letter-spacing: 0.14em;
  color: var(--lv-rail-text-strong);
  line-height: 1.2;
}

.rail__tagline {
  font-size: 11px;
  color: var(--lv-rail-section);
  white-space: nowrap;
}

.rail__nav {
  flex: 1;
  overflow-y: auto;
  overflow-x: hidden;
  padding: var(--lv-space-3) var(--lv-space-2);
}

.rail__nav::-webkit-scrollbar-thumb {
  background: rgba(255, 255, 255, 0.14);
  background-clip: content-box;
}

.rail__group + .rail__group {
  margin-top: var(--lv-space-4);
}

.rail__group-title {
  padding: 0 10px var(--lv-space-2);
  font-size: 11px;
  font-weight: 600;
  letter-spacing: var(--lv-tracking-caps);
  text-transform: uppercase;
  color: var(--lv-rail-section);
}

.rail__group-rule {
  height: 1px;
  margin: 0 10px var(--lv-space-2);
  background: var(--lv-rail-border);
}

.rail__link {
  position: relative;
  display: flex;
  align-items: center;
  gap: 10px;
  height: 36px;
  padding: 0 10px;
  margin-bottom: 2px;
  border-radius: var(--lv-radius);
  color: var(--lv-rail-text);
  font-size: var(--lv-text-sm);
  font-weight: 500;
  text-decoration: none;
  transition:
    background-color var(--lv-dur-fast) var(--lv-ease),
    color var(--lv-dur-fast) var(--lv-ease);
}

.rail__link:hover {
  background: var(--lv-rail-hover-bg);
  color: var(--lv-rail-text-strong);
}

.rail__link--active {
  background: var(--lv-rail-active-bg);
  color: var(--lv-rail-active-text);
}

/* Active marker: a 2px bar bled to the rail's edge — readable in peripheral
 * vision, which a background tint alone is not. */
.rail__link--active::before {
  content: '';
  position: absolute;
  left: -8px;
  top: 8px;
  bottom: 8px;
  width: 2px;
  border-radius: 0 2px 2px 0;
  background: var(--lv-rail-accent);
}

.rail__icon {
  font-size: 16px;
  flex: none;
}

.rail__label {
  white-space: nowrap;
  overflow: hidden;
  text-overflow: ellipsis;
}

.rail__foot {
  flex: none;
  padding: var(--lv-space-2);
  border-top: 1px solid var(--lv-rail-border);
}

.rail__collapse {
  display: flex;
  align-items: center;
  gap: 10px;
  width: 100%;
  height: 32px;
  padding: 0 10px;
  border: none;
  border-radius: var(--lv-radius);
  background: transparent;
  color: var(--lv-rail-section);
  font-family: inherit;
  font-size: var(--lv-text-xs);
  cursor: pointer;
  transition: background-color var(--lv-dur-fast) var(--lv-ease);
}

.rail__collapse:hover {
  background: var(--lv-rail-hover-bg);
  color: var(--lv-rail-text-strong);
}

/* ------------------------------------------------------------------ sheet */

/* The scroll container. The ambient tint lives here rather than on the panels:
 * glass needs something to pick up, and a completely flat page background makes
 * a blurred bar indistinguishable from an opaque one. Kept at a few percent so
 * tables stay clean. Fixed attachment so the tint reads as environment rather
 * than as a band that scrolls away. */
.sheet {
  position: relative;
  flex: 1;
  min-width: 0;
  overflow-y: auto;
  background-color: var(--lv-surface-2);
  background-image:
    radial-gradient(1100px 560px at 12% -8%, var(--lv-ambient-1), transparent 62%),
    radial-gradient(900px 480px at 88% 108%, var(--lv-ambient-2), transparent 60%);
  background-attachment: fixed;
}

.sheet--standalone {
  background-image: none;
}

/* ----------------------------------------------------------------- topbar */

.topbar {
  position: sticky;
  top: 0;
  z-index: 20;
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: var(--lv-space-4);
  height: var(--lv-topbar-height);
  padding: 0 var(--lv-space-5);
  border-bottom: 1px solid var(--lv-border);
}

.topbar__left {
  display: flex;
  align-items: baseline;
  gap: var(--lv-space-2);
  min-width: 0;
}

.topbar__crumb {
  font-size: var(--lv-text-sm);
  color: var(--lv-text-3);
  white-space: nowrap;
}

.topbar__crumb-sep {
  font-size: 11px;
  color: var(--lv-ink-400);
  align-self: center;
}

.topbar__title {
  font-size: var(--lv-text-lg);
  font-weight: 600;
  letter-spacing: -0.01em;
  white-space: nowrap;
  overflow: hidden;
  text-overflow: ellipsis;
}

.topbar__right {
  display: flex;
  align-items: center;
  gap: var(--lv-space-3);
  flex: none;
}

/* Palette trigger: shaped like a search field, because that is what it is —
 * and it advertises the shortcut, which is the only way a palette gets used. */
.topbar__search {
  display: inline-flex;
  align-items: center;
  gap: var(--lv-space-2);
  height: 30px;
  min-width: 200px;
  padding: 0 var(--lv-space-2) 0 10px;
  border: 1px solid var(--lv-border);
  border-radius: var(--lv-radius-lg);
  background: var(--lv-surface);
  color: var(--lv-text-3);
  font-family: inherit;
  font-size: var(--lv-text-sm);
  cursor: pointer;
  transition: border-color var(--lv-dur-fast) var(--lv-ease);
}

.topbar__search:hover {
  border-color: var(--lv-border-strong);
}

.topbar__search-icon {
  flex: none;
  font-size: 14px;
}

.topbar__search-label {
  flex: 1;
  text-align: left;
}

.topbar__search-kbd {
  flex: none;
  padding: 1px 5px;
  border: 1px solid var(--lv-border);
  border-radius: var(--lv-radius-sm);
  background: var(--lv-surface-3);
  font-family: var(--lv-font-mono);
  font-size: 10px;
  color: var(--lv-text-3);
}

.topbar__clock {
  font-size: var(--lv-text-sm);
  color: var(--lv-text-3);
  font-variant-numeric: tabular-nums;
}

.topbar__icon-btn {
  display: inline-flex;
  align-items: center;
  justify-content: center;
  width: 32px;
  height: 32px;
  border: 1px solid transparent;
  border-radius: var(--lv-radius);
  background: transparent;
  color: var(--lv-text-2);
  cursor: pointer;
  transition:
    background-color var(--lv-dur-fast) var(--lv-ease),
    color var(--lv-dur-fast) var(--lv-ease);
}

.topbar__icon-btn:hover {
  background: var(--lv-surface-hover);
  color: var(--lv-text-1);
}

.topbar__user {
  display: inline-flex;
  align-items: center;
  gap: var(--lv-space-2);
  height: 32px;
  padding: 0 var(--lv-space-2) 0 4px;
  border: 1px solid var(--lv-border);
  border-radius: var(--lv-radius-full);
  background: var(--lv-surface);
  color: var(--lv-text-2);
  font-family: inherit;
  font-size: var(--lv-text-sm);
  cursor: pointer;
  transition: border-color var(--lv-dur-fast) var(--lv-ease);
}

.topbar__user:hover {
  border-color: var(--lv-border-strong);
}

.topbar__avatar {
  display: inline-flex;
  align-items: center;
  justify-content: center;
  width: 24px;
  height: 24px;
  border-radius: var(--lv-radius-full);
  background: var(--lv-accent-soft);
  color: var(--lv-accent);
  font-family: var(--lv-font-mono);
  font-size: 10px;
  font-weight: 700;
}

.topbar__caret {
  font-size: 12px;
  color: var(--lv-text-3);
}

/* Hide the palette trigger's label on narrow screens; the shortcut still works
 * and the icon still opens it. */
@media (max-width: 900px) {
  .topbar__search-label,
  .topbar__search-kbd,
  .topbar__clock {
    display: none;
  }

  .topbar__search {
    min-width: 0;
    width: 34px;
    justify-content: center;
    padding: 0;
  }
}

.page-host {
  min-height: calc(100% - var(--lv-topbar-height));
}

/* ----------------------------------------------------------------- motion */

.page-enter-active,
.page-leave-active {
  transition:
    opacity var(--lv-dur) var(--lv-ease),
    transform var(--lv-dur) var(--lv-ease);
}

.page-enter-from {
  opacity: 0;
  transform: translateY(4px);
}

.page-leave-to {
  opacity: 0;
}

@media (prefers-reduced-motion: reduce) {
  .page-enter-active,
  .page-leave-active {
    transition: none;
  }
}

/* Dropdown items carry their icon inline; align it with the label. */
.el-dropdown-menu__item {
  display: flex;
  align-items: center;
  gap: var(--lv-space-2);
}

.el-dropdown-menu__item.is-active {
  color: var(--lv-accent);
  font-weight: 600;
}
</style>
