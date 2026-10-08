<script setup lang="ts">
// LoginCallbackView completes both SSO flows that redirect here with
// ?code=...&state=...:
//   - OIDC: the browser exchanges the code at the IdP's token endpoint
//     directly (PKCE verifier from sessionStorage, see @/api/sso),
//   - GitHub: the browser POSTs the code to the gateway (POST /auth/github),
//     which exchanges it server-side and returns a LEVEE session token.
// The provider is decided by what /system/auth-info says is enabled.
// IdP-side errors (?error=...) are surfaced with a way back to /login.
import { onMounted, ref } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import { ElMessage } from 'element-plus'
import { clearToken } from '@/api/client'
import {
  completeSSOLogin,
  completeGitHubLogin,
  consumeSSORedirect,
  fetchAuthInfo,
} from '@/api/sso'

const route = useRoute()
const router = useRouter()

const status = ref<'working' | 'error'>('working')
const errorMessage = ref('')

onMounted(async () => {
  const code = typeof route.query.code === 'string' ? route.query.code : ''
  const state = typeof route.query.state === 'string' ? route.query.state : ''
  const idpError = typeof route.query.error === 'string' ? route.query.error : ''
  const idpErrorDesc =
    typeof route.query.error_description === 'string' ? route.query.error_description : ''

  if (idpError) {
    status.value = 'error'
    errorMessage.value = idpErrorDesc || idpError
    return
  }
  if (!code || !state) {
    status.value = 'error'
    errorMessage.value = '回调缺少 code/state 参数，请从登录页重新发起'
    return
  }
  try {
    // Ask the gateway which provider this callback belongs to. GitHub
    // requires the server-side exchange; OIDC completes browser-direct.
    const info = await fetchAuthInfo()
    if (info.githubEnabled) {
      await completeGitHubLogin(code, state)
    } else if (info.oidcEnabled) {
      await completeSSOLogin(code, state)
    } else {
      throw new Error('SSO 已被服务端停用，请使用访问令牌登录')
    }
    ElMessage.success('登录成功')
    await router.push(consumeSSORedirect())
  } catch (err) {
    status.value = 'error'
    errorMessage.value = err instanceof Error ? err.message : 'SSO 登录失败'
  }
})

function backToLogin(): void {
  clearToken()
  void router.push('/login')
}
</script>

<template>
  <div class="callback">
    <div class="callback__inner">
      <div class="callback__brand">
        <span class="callback__mark" aria-hidden="true">
          <svg viewBox="0 0 24 24" width="20" height="20" fill="none">
            <path d="M3 5h18v4H3z" fill="currentColor" opacity="0.95" />
            <path d="M3 11h18v4H3z" fill="currentColor" opacity="0.6" />
            <path d="M3 17h18v3H3z" fill="currentColor" opacity="0.3" />
          </svg>
        </span>
        <span class="callback__wordmark">LEVEE</span>
      </div>

      <template v-if="status === 'working'">
        <div class="callback__spinner" aria-hidden="true"></div>
        <h1 class="callback__title">正在完成 SSO 登录</h1>
        <p class="callback__desc">正在与身份提供方交换凭据，请稍候…</p>
      </template>

      <template v-else>
        <h1 class="callback__title">SSO 登录失败</h1>
        <div class="callback__error">
          <el-icon class="callback__error-icon"><WarningFilled /></el-icon>
          <span>{{ errorMessage }}</span>
        </div>
        <el-button type="primary" size="large" class="callback__btn" @click="backToLogin">
          返回登录页
        </el-button>
      </template>
    </div>
  </div>
</template>

<style scoped>
.callback {
  display: flex;
  align-items: center;
  justify-content: center;
  min-height: 100vh;
  padding: var(--lv-space-6);
  background: var(--lv-surface-2);
}

.callback__inner {
  width: 100%;
  max-width: 360px;
  text-align: center;
}

.callback__brand {
  display: flex;
  align-items: center;
  justify-content: center;
  gap: 10px;
  margin-bottom: var(--lv-space-8);
}

.callback__mark {
  display: inline-flex;
  align-items: center;
  justify-content: center;
  width: 32px;
  height: 32px;
  border-radius: var(--lv-radius);
  background: var(--lv-accent-soft);
  border: 1px solid var(--lv-accent-border);
  color: var(--lv-accent);
}

.callback__wordmark {
  font-size: 16px;
  font-weight: 600;
  letter-spacing: 0.2em;
  color: var(--lv-text-1);
}

/* A ring rather than a skeleton: the wait is a network exchange, not content
 * loading, and a spinner says that without implying a page shape. */
.callback__spinner {
  width: 26px;
  height: 26px;
  margin: 0 auto var(--lv-space-4);
  border: 2px solid var(--lv-border);
  border-top-color: var(--lv-accent);
  border-radius: var(--lv-radius-full);
  animation: callback-spin 700ms linear infinite;
}

@keyframes callback-spin {
  to {
    transform: rotate(360deg);
  }
}

@media (prefers-reduced-motion: reduce) {
  .callback__spinner {
    animation-duration: 2s;
  }
}

.callback__title {
  font-size: var(--lv-text-lg);
  font-weight: 600;
}

.callback__desc {
  margin-top: var(--lv-space-2);
  font-size: var(--lv-text-sm);
  color: var(--lv-text-3);
}

.callback__error {
  display: flex;
  align-items: flex-start;
  gap: var(--lv-space-2);
  margin-top: var(--lv-space-4);
  padding: var(--lv-space-3) var(--lv-space-4);
  border: 1px solid var(--lv-bad-border);
  border-radius: var(--lv-radius);
  background: var(--lv-bad-soft);
  color: var(--lv-bad);
  font-size: var(--lv-text-sm);
  line-height: 1.6;
  text-align: left;
}

.callback__error-icon {
  flex: none;
  margin-top: 2px;
}

.callback__btn {
  margin-top: var(--lv-space-5);
  width: 100%;
}
</style>
