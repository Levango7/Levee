<script setup lang="ts">
// LoginCallbackView completes the OIDC authorization-code + PKCE flow that
// LoginView started: the IdP redirects here with ?code=...&state=..., we
// exchange the code at the token endpoint browser-direct (see @/api/sso),
// store the returned JWT and continue to the originally requested page.
// IdP-side errors (?error=...) are surfaced with a way back to /login.
import { onMounted, ref } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import { ElMessage } from 'element-plus'
import { clearToken } from '@/api/client'
import { completeSSOLogin, consumeSSORedirect } from '@/api/sso'

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
    await completeSSOLogin(code, state)
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
