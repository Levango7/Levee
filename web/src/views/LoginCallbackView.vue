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
  <div class="login">
    <el-card shadow="hover" class="login__card">
      <h2 class="login__title">SSO 登录</h2>
      <template v-if="status === 'working'">
        <p class="login__subtitle">正在完成登录，请稍候…</p>
        <el-skeleton :rows="2" animated />
      </template>
      <template v-else>
        <el-alert type="error" :closable="false" class="login__error">
          {{ errorMessage }}
        </el-alert>
        <el-button type="primary" size="large" class="login__btn" @click="backToLogin">
          返回登录页
        </el-button>
      </template>
    </el-card>
  </div>
</template>

<style scoped>
.login {
  min-height: 100vh;
  display: flex;
  align-items: center;
  justify-content: center;
  background: var(--levee-bg, #f5f7fa);
}

.login__card {
  width: 400px;
  max-width: calc(100vw - 32px);
}

.login__title {
  margin: 0;
  font-size: 20px;
  text-align: center;
}

.login__subtitle {
  margin: 8px 0 20px;
  text-align: center;
  color: #909399;
  font-size: 13px;
}

.login__error {
  margin-bottom: 16px;
}

.login__btn {
  width: 100%;
}
</style>
