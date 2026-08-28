<script setup lang="ts">
// LoginView collects the bearer token used by the LEVEE API. The binary does
// not ship a user database: operators pass a shared token via
// `levee serve --token <TOKEN>` and paste it here. The token is stored in
// localStorage (key `levee.token`) and attached as an Authorization header by
// the axios interceptor in @/api/client.
//
// When the server has OIDC enabled (announced by the public
// /system/auth-info descriptor) an additional SSO button starts the
// authorization-code + PKCE flow (see @/api/sso).
import { computed, onMounted, ref } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import { ElMessage } from 'element-plus'
import { clearToken, getToken, setToken } from '@/api/client'
import { fetchAuthInfo, startSSOLogin, type AuthInfo } from '@/api/sso'

const route = useRoute()
const router = useRouter()

const token = ref(getToken())
const loading = ref(false)
const ssoInfo = ref<AuthInfo | null>(null)

// After being bounced to /login by a 401 or logout, return to the originally
// requested page. Only allow in-app redirects.
const redirect = computed(() => {
  const target = route.query.redirect
  return typeof target === 'string' && target.startsWith('/') ? target : '/'
})

// The auth-info endpoint is public; a failure only means SSO stays hidden.
onMounted(async () => {
  try {
    const info = await fetchAuthInfo()
    if (info.oidcEnabled) {
      ssoInfo.value = info
    }
  } catch {
    // Static-token login remains available; ignore descriptor errors.
  }
})

async function ssoLogin(): Promise<void> {
  if (!ssoInfo.value) return
  loading.value = true
  try {
    await startSSOLogin(ssoInfo.value, redirect.value)
  } catch (err) {
    ElMessage.error(err instanceof Error ? err.message : 'SSO 跳转失败')
  } finally {
    loading.value = false
  }
}

function login(): void {
  const value = token.value.trim()
  if (!value) {
    ElMessage.warning('请输入访问令牌')
    return
  }
  loading.value = true
  try {
    setToken(value)
    ElMessage.success('登录成功')
    void router.push(redirect.value)
  } finally {
    loading.value = false
  }
}

function clearStoredToken(): void {
  clearToken()
  token.value = ''
  ElMessage.success('已清除本地令牌')
}
</script>

<template>
  <div class="login">
    <el-card shadow="hover" class="login__card">
      <h2 class="login__title">LEVEE 控制台</h2>
      <p class="login__subtitle">请输入访问令牌登录</p>

      <el-input
        v-model="token"
        type="password"
        show-password
        placeholder="访问令牌"
        size="large"
        clearable
        @keyup.enter="login"
      />

      <div class="login__actions">
        <el-button type="primary" size="large" :loading="loading" class="login__btn" @click="login">
          登录
        </el-button>
        <el-button size="large" class="login__btn" @click="clearStoredToken">清除</el-button>
      </div>

      <template v-if="ssoInfo">
        <el-divider class="login__divider">或</el-divider>
        <el-button
          size="large"
          class="login__btn login__sso"
          :disabled="loading"
          @click="ssoLogin"
        >
          通过 SSO 登录
        </el-button>
      </template>

      <el-alert type="info" :closable="false" class="login__hint">
        <p>令牌由服务端启动参数指定：`levee serve --token &lt;TOKEN&gt;`。</p>
        <p>仅持有令牌的成员可访问控制台，令牌不会上传至第三方。</p>
      </el-alert>
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

.login__actions {
  display: flex;
  gap: 12px;
  margin-top: 16px;
}

.login__divider {
  margin: 20px 0 12px;
}

.login__sso {
  width: 100%;
}

.login__btn {
  flex: 1;
}

.login__hint {
  margin-top: 20px;
}

.login__hint p {
  margin: 2px 0;
  font-size: 12px;
  line-height: 1.6;
}
</style>
