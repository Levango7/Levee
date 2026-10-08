<script setup lang="ts">
// LoginView collects the bearer token used by the LEVEE API. The binary does
// not ship a user database: operators pass a shared token via
// `levee serve --token <TOKEN>` and paste it here. The token is stored in
// localStorage (key `levee.token`) and attached as an Authorization header by
// the axios interceptor in @/api/client.
//
// When the server has SSO enabled (announced by the public
// /system/auth-info descriptor) additional buttons start the OIDC
// (authorization code + PKCE, see @/api/sso) or GitHub OAuth flows.
//
// Layout: a split screen. The left half is the product's own surface — what
// this console is for and what it protects — and the right half is the form.
// A bare centered card was the previous shape; carrying the product's identity
// on the sign-in screen is what makes it read as a platform rather than an
// internal tool. The left half is decorative and hidden below 900px, so the
// form is never pushed off a small screen.
import { computed, onMounted, ref } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import { ElMessage } from 'element-plus'
import { clearToken, getToken, setToken } from '@/api/client'
import { fetchAuthInfo, startSSOLogin, startGitHubLogin, type AuthInfo } from '@/api/sso'

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
    if (info.oidcEnabled || info.githubEnabled) {
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

function githubLogin(): void {
  if (!ssoInfo.value) return
  try {
    startGitHubLogin(ssoInfo.value, redirect.value)
  } catch (err) {
    ElMessage.error(err instanceof Error ? err.message : 'GitHub 跳转失败')
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

// Capabilities shown on the brand panel. These are the pipeline's own stages —
// plan, approve, batch, verify, roll back, audit — not marketing claims, so the
// sign-in screen tells an operator what they are signing in TO.
const stages = [
  { name: '计划', detail: '编译期门禁 · 不可逆动作白名单' },
  { name: '审批', detail: '分级审批 · 计划哈希绑定' },
  { name: '分批', detail: '金丝雀推进 · 门禁校验' },
  { name: '回滚', detail: '补偿执行 · 基线还原' },
  { name: '审计', detail: 'WORM 存储 · 哈希链校验' },
]
</script>

<template>
  <div class="auth">
    <aside class="auth__brand">
      <div class="auth__brand-inner">
        <div class="auth__logo">
          <span class="auth__mark" aria-hidden="true">
            <svg viewBox="0 0 24 24" width="22" height="22" fill="none">
              <path d="M3 5h18v4H3z" fill="currentColor" opacity="0.95" />
              <path d="M3 11h18v4H3z" fill="currentColor" opacity="0.6" />
              <path d="M3 17h18v3H3z" fill="currentColor" opacity="0.3" />
            </svg>
          </span>
          <span class="auth__wordmark">LEVEE</span>
        </div>

        <h1 class="auth__headline">
          高危变更的<br />
          全流程治理台
        </h1>
        <p class="auth__lede">
          计划、审批、分批执行、回滚与审计在同一条流水线上闭环，
          每一次改动都留下可校验的证据。
        </p>

        <ol class="auth__stages">
          <li v-for="(s, i) in stages" :key="s.name" class="auth__stage">
            <span class="auth__stage-index lv-mono">{{ String(i + 1).padStart(2, '0') }}</span>
            <span class="auth__stage-name">{{ s.name }}</span>
            <span class="auth__stage-detail">{{ s.detail }}</span>
          </li>
        </ol>
      </div>
    </aside>

    <main class="auth__panel">
      <div class="auth__form">
        <h2 class="auth__form-title">登录控制台</h2>
        <p class="auth__form-sub">使用服务端签发的访问令牌继续</p>

        <label class="auth__field-label" for="token-input">访问令牌</label>
        <el-input
          id="token-input"
          v-model="token"
          type="password"
          show-password
          placeholder="粘贴访问令牌"
          size="large"
          clearable
          @keyup.enter="login"
        />

        <div class="auth__actions">
          <el-button type="primary" size="large" :loading="loading" class="auth__submit" @click="login">
            登录
          </el-button>
          <el-button size="large" @click="clearStoredToken">清除本地令牌</el-button>
        </div>

        <template v-if="ssoInfo">
          <div class="auth__divider"><span>或使用企业身份</span></div>
          <div class="auth__sso">
            <el-button
              v-if="ssoInfo.githubEnabled"
              size="large"
              :disabled="loading"
              @click="githubLogin"
            >
              通过 GitHub 登录
            </el-button>
            <el-button
              v-if="ssoInfo.oidcEnabled"
              size="large"
              :disabled="loading"
              @click="ssoLogin"
            >
              通过 SSO 登录
            </el-button>
          </div>
        </template>

        <div class="auth__note">
          <el-icon class="auth__note-icon"><InfoFilled /></el-icon>
          <div>
            <p>令牌由服务端启动参数指定：<code>levee serve --token &lt;TOKEN&gt;</code></p>
            <p>仅持有令牌的成员可访问控制台；令牌保存在本机浏览器，不会上传至第三方。</p>
          </div>
        </div>
      </div>
    </main>
  </div>
</template>

<style scoped>
.auth {
  display: grid;
  grid-template-columns: minmax(0, 1.05fr) minmax(0, 1fr);
  min-height: 100vh;
  background: var(--lv-surface-2);
}

/* ------------------------------------------------------------ brand panel */

.auth__brand {
  position: relative;
  display: flex;
  align-items: center;
  justify-content: center;
  padding: var(--lv-space-10) var(--lv-space-8);
  overflow: hidden;
  color: #cfe0e0;
  background:
    radial-gradient(1100px 620px at 12% 8%, rgba(63, 173, 170, 0.22), transparent 62%),
    radial-gradient(900px 520px at 88% 96%, rgba(14, 124, 123, 0.28), transparent 60%),
    linear-gradient(165deg, #0f1e20 0%, #0a1416 58%, #081113 100%);
}

/* Course lines: a faint stacked-bands texture echoing the levee mark. Drawn
 * with repeating-linear-gradient rather than an asset so it costs nothing and
 * scales with the panel. */
.auth__brand::after {
  content: '';
  position: absolute;
  inset: 0;
  background: repeating-linear-gradient(
    180deg,
    rgba(255, 255, 255, 0.028) 0 1px,
    transparent 1px 56px
  );
  pointer-events: none;
}

.auth__brand-inner {
  position: relative;
  max-width: 460px;
}

.auth__logo {
  display: flex;
  align-items: center;
  gap: 12px;
  margin-bottom: var(--lv-space-10);
}

.auth__mark {
  display: inline-flex;
  align-items: center;
  justify-content: center;
  width: 38px;
  height: 38px;
  border-radius: var(--lv-radius-lg);
  background: rgba(63, 173, 170, 0.16);
  border: 1px solid rgba(63, 173, 170, 0.28);
  color: #7fd6d3;
}

.auth__wordmark {
  font-size: 19px;
  font-weight: 600;
  letter-spacing: 0.22em;
  color: #eaf5f4;
}

.auth__headline {
  font-size: 34px;
  line-height: 1.3;
  font-weight: 600;
  letter-spacing: -0.01em;
  color: #f2fbfa;
}

.auth__lede {
  margin-top: var(--lv-space-4);
  font-size: var(--lv-text-base);
  line-height: 1.7;
  color: #9db3b4;
}

.auth__stages {
  margin: var(--lv-space-10) 0 0;
  padding: 0;
  list-style: none;
  border-top: 1px solid rgba(255, 255, 255, 0.09);
}

.auth__stage {
  display: grid;
  grid-template-columns: 34px 66px minmax(0, 1fr);
  align-items: baseline;
  gap: var(--lv-space-3);
  padding: 11px 0;
  border-bottom: 1px solid rgba(255, 255, 255, 0.07);
}

.auth__stage-index {
  font-size: var(--lv-text-xs);
  color: #6f9fa0;
  font-variant-numeric: tabular-nums;
}

.auth__stage-name {
  font-size: var(--lv-text-base);
  font-weight: 600;
  color: #dcefee;
}

.auth__stage-detail {
  font-size: var(--lv-text-sm);
  color: #8ba3a4;
}

/* ------------------------------------------------------------- form panel */

.auth__panel {
  display: flex;
  align-items: center;
  justify-content: center;
  padding: var(--lv-space-8) var(--lv-space-6);
}

.auth__form {
  width: 100%;
  max-width: 380px;
}

.auth__form-title {
  font-size: 22px;
  font-weight: 600;
  letter-spacing: -0.01em;
}

.auth__form-sub {
  margin: var(--lv-space-1) 0 var(--lv-space-6);
  font-size: var(--lv-text-sm);
  color: var(--lv-text-3);
}

.auth__field-label {
  display: block;
  margin-bottom: var(--lv-space-2);
  font-size: var(--lv-text-sm);
  font-weight: 500;
  color: var(--lv-text-2);
}

.auth__actions {
  display: flex;
  gap: var(--lv-space-2);
  margin-top: var(--lv-space-4);
}

.auth__submit {
  flex: 1;
}

.auth__divider {
  display: flex;
  align-items: center;
  gap: var(--lv-space-3);
  margin: var(--lv-space-6) 0 var(--lv-space-4);
  font-size: var(--lv-text-xs);
  color: var(--lv-text-3);
}

.auth__divider::before,
.auth__divider::after {
  content: '';
  flex: 1;
  height: 1px;
  background: var(--lv-border);
}

.auth__sso {
  display: flex;
  flex-direction: column;
  gap: var(--lv-space-2);
}

.auth__sso .el-button + .el-button {
  margin-left: 0;
}

.auth__note {
  display: flex;
  gap: var(--lv-space-2);
  margin-top: var(--lv-space-6);
  padding: var(--lv-space-3) var(--lv-space-4);
  border: 1px solid var(--lv-border);
  border-radius: var(--lv-radius);
  background: var(--lv-surface-3);
}

.auth__note-icon {
  flex: none;
  margin-top: 2px;
  color: var(--lv-text-3);
}

.auth__note p {
  margin: 0 0 4px;
  font-size: var(--lv-text-xs);
  line-height: 1.65;
  color: var(--lv-text-3);
}

.auth__note p:last-child {
  margin-bottom: 0;
}

.auth__note code {
  font-size: 11px;
}

@media (max-width: 900px) {
  .auth {
    grid-template-columns: minmax(0, 1fr);
  }

  .auth__brand {
    display: none;
  }
}
</style>
