<script setup lang="ts">
// App.vue is the layout shell: a fixed sidebar + top bar + scrollable content
// area. The content area is filled by <router-view>, so each route renders
// inside this frame. We keep the shell free of business logic; per-page state
// lives in the views.
import { computed, ref } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import { ElMessage } from 'element-plus'
import { clearToken } from '@/api/client'

const route = useRoute()
const router = useRouter()

const collapsed = ref(false)

const pageTitle = computed(() => (route.meta.title as string) || 'LEVEE')

// Standalone pages (login, SSO callback, mobile approval deeplinks) render
// without the sidebar / header chrome so they work on phones and pre-auth
// screens.
const showShell = computed(
  () => !route.path.startsWith('/m/') && route.path !== '/login' && route.path !== '/login/callback',
)

function toggleCollapse(): void {
  collapsed.value = !collapsed.value
}

function handleCommand(command: string): void {
  if (command === 'logout') {
    clearToken()
    ElMessage.success('已退出登录')
    router.push('/login')
  } else if (command === 'docs') {
    window.open('https://github.com/nexus/levee', '_blank')
  }
}
</script>

<template>
  <el-container class="layout">
    <el-aside v-if="showShell" :width="collapsed ? '64px' : '220px'" class="layout__aside">
      <div class="layout__brand">
        <span class="layout__brand-mark">L</span>
        <span v-if="!collapsed" class="layout__brand-text">LEVEE</span>
      </div>
      <el-menu
        :default-active="route.path"
        :collapse="collapsed"
        router
        class="layout__menu"
      >
        <el-menu-item index="/changes">
          <el-icon><List /></el-icon>
          <template #title>变更看板</template>
        </el-menu-item>
        <el-menu-item index="/approval">
          <el-icon><Select /></el-icon>
          <template #title>审批中心</template>
        </el-menu-item>
        <el-menu-item index="/monitor">
          <el-icon><Monitor /></el-icon>
          <template #title>实时监控</template>
        </el-menu-item>
        <el-menu-item index="/templates">
          <el-icon><Document /></el-icon>
          <template #title>模板管理</template>
        </el-menu-item>
        <el-menu-item index="/targets">
          <el-icon><Connection /></el-icon>
          <template #title>目标机</template>
        </el-menu-item>
        <el-menu-item index="/audit">
          <el-icon><Tickets /></el-icon>
          <template #title>审计查询</template>
        </el-menu-item>
        <el-menu-item index="/system">
          <el-icon><DataLine /></el-icon>
          <template #title>系统状态</template>
        </el-menu-item>
      </el-menu>
    </el-aside>

    <el-container>
      <el-header v-if="showShell" class="layout__header">
        <div class="layout__header-left">
          <el-button text @click="toggleCollapse">
            <el-icon><Fold v-if="!collapsed" /><Expand v-else /></el-icon>
          </el-button>
          <span class="layout__header-title">{{ pageTitle }}</span>
        </div>
        <div class="layout__header-right">
          <el-dropdown trigger="click" @command="handleCommand">
            <span class="layout__user">
              <el-icon><User /></el-icon>
              <span class="layout__user-name">operator</span>
              <el-icon><ArrowDown /></el-icon>
            </span>
            <template #dropdown>
              <el-dropdown-menu>
                <el-dropdown-item command="docs">文档</el-dropdown-item>
                <el-dropdown-item command="logout" divided>退出登录</el-dropdown-item>
              </el-dropdown-menu>
            </template>
          </el-dropdown>
        </div>
      </el-header>

      <el-main class="layout__main">
        <router-view v-slot="{ Component }">
          <component :is="Component" />
        </router-view>
      </el-main>
    </el-container>
  </el-container>
</template>

<style scoped>
.layout {
  height: 100%;
}

.layout__aside {
  background: #ffffff;
  border-right: 1px solid var(--levee-border);
  transition: width 0.2s ease;
  overflow: hidden;
}

.layout__brand {
  height: var(--levee-header-height);
  display: flex;
  align-items: center;
  justify-content: center;
  gap: 8px;
  border-bottom: 1px solid var(--levee-border);
}

.layout__brand-mark {
  display: inline-flex;
  width: 28px;
  height: 28px;
  align-items: center;
  justify-content: center;
  background: var(--levee-primary);
  color: #fff;
  font-weight: 700;
  border-radius: 6px;
}

.layout__brand-text {
  font-size: 16px;
  font-weight: 600;
  color: var(--levee-text-regular);
  letter-spacing: 1px;
}

.layout__menu {
  border-right: none;
}

.layout__header {
  height: var(--levee-header-height);
  background: #ffffff;
  border-bottom: 1px solid var(--levee-border);
  display: flex;
  align-items: center;
  justify-content: space-between;
  padding: 0 16px;
}

.layout__header-left {
  display: flex;
  align-items: center;
  gap: 8px;
}

.layout__header-title {
  font-size: 16px;
  font-weight: 600;
}

.layout__header-right {
  display: flex;
  align-items: center;
}

.layout__user {
  display: inline-flex;
  align-items: center;
  gap: 4px;
  cursor: pointer;
  color: var(--levee-text-regular);
}

.layout__user-name {
  margin: 0 4px 0 2px;
  font-size: 14px;
}

.layout__main {
  background: var(--levee-bg);
  padding: 0;
  overflow-y: auto;
}
</style>