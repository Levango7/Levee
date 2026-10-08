<script setup lang="ts">
// NotFoundView is the SPA 404 page. Kept minimal so it loads instantly.
import { useRouter } from 'vue-router'

const router = useRouter()
</script>

<template>
  <div class="not-found">
    <div class="not-found__inner">
      <div class="not-found__code lv-mono">404</div>
      <h1 class="not-found__title">页面不存在</h1>
      <p class="not-found__desc">
        该地址没有对应的页面。它可能已被重命名，或链接本身就是错的。
      </p>
      <div class="not-found__actions">
        <el-button type="primary" @click="router.push('/')">返回变更看板</el-button>
        <el-button @click="router.back()">返回上一页</el-button>
      </div>
    </div>
  </div>
</template>

<style scoped>
.not-found {
  display: flex;
  align-items: center;
  justify-content: center;
  min-height: calc(100vh - var(--lv-topbar-height));
  padding: var(--lv-space-8);
}

.not-found__inner {
  max-width: 420px;
  text-align: center;
}

/* The code is the largest thing on the page: this is an instrument console, and
 * a wrong address is a state, not an emotion. */
.not-found__code {
  font-size: 64px;
  font-weight: 600;
  line-height: 1;
  color: var(--lv-accent);
  letter-spacing: -0.03em;
  opacity: 0.85;
}

.not-found__title {
  margin-top: var(--lv-space-4);
  font-size: var(--lv-text-xl);
  font-weight: 600;
}

.not-found__desc {
  margin-top: var(--lv-space-2);
  font-size: var(--lv-text-sm);
  line-height: 1.7;
  color: var(--lv-text-3);
}

.not-found__actions {
  display: flex;
  justify-content: center;
  gap: var(--lv-space-2);
  margin-top: var(--lv-space-6);
}
</style>
