<script setup lang="ts">
// SystemView is the operator dashboard: version, daemon health, doctor report
// and the loaded config. It is read-only; the only action is "run doctor"
// which re-runs the diagnostic checks.
import { onMounted, ref } from 'vue'
import { useRoute } from 'vue-router'
import { ElMessage } from 'element-plus'
import { systemApi } from '@/api'
import type { SystemStatus, VersionInfo } from '@/types/levee'
import PageHeader from '@/components/PageHeader.vue'
import { formatTimestamp, formatUptime } from '@/utils/format'
import { healthLabel, verdictLabel } from '@/utils/diagnosis'

const route = useRoute()

const loading = ref(false)
const version = ref<VersionInfo | null>(null)
const status = ref<SystemStatus | null>(null)
const config = ref<{ format: string; content: string; sourcePath: string; loadedAt: number } | null>(null)
const doctor = ref<{
  status: string
  checks: Array<{ name: string; status: string; message: string; remediation: string }>
  checkedAt: number
} | null>(null)

async function loadVersion(): Promise<void> {
  try {
    version.value = await systemApi.version()
  } catch {
    // Tolerate failure: the dashboard still shows other panels.
  }
}

async function loadStatus(): Promise<void> {
  try {
    status.value = await systemApi.status()
  } catch {
    // Same as above.
  }
}

async function loadConfig(): Promise<void> {
  try {
    config.value = await systemApi.config({ redactSecrets: true })
  } catch {
    // Same as above.
  }
}

async function runDoctor(): Promise<void> {
  loading.value = true
  try {
    doctor.value = await systemApi.doctor()
    ElMessage.success('诊断完成')
  } catch (err) {
    ElMessage.error((err as { message?: string })?.message || '诊断失败')
  } finally {
    loading.value = false
  }
}

// Health vocabulary → console tone. The state words are the backend's
// (healthy/degraded/unhealthy) and the doctor's (pass/warn/fail); the label
// comes from utils/diagnosis, which the Go-side vocabulary guard pins.
function tone(s: string): 'ok' | 'warn' | 'bad' | 'idle' {
  if (s === 'healthy' || s === 'pass') return 'ok'
  if (s === 'degraded' || s === 'warn') return 'warn'
  if (s === 'unhealthy' || s === 'fail') return 'bad'
  return 'idle'
}

function isHealthy(s: string | undefined): boolean {
  return tone(s ?? '') === 'ok'
}

async function refresh(): Promise<void> {
  loading.value = true
  await Promise.all([loadVersion(), loadStatus(), loadConfig()])
  loading.value = false
}

onMounted(async () => {
  await refresh()
  // /system?doctor=1 — how the command palette runs the doctor without making
  // the operator click a second button. The check re-runs on every visit with
  // the flag; that is the point of a "run doctor" command.
  if (route.query.doctor) void runDoctor()
})
</script>

<template>
  <div class="levee-page">
    <PageHeader title="系统状态" description="守护进程健康、诊断报告与生效配置（配置已脱敏）">
      <template #actions>
        <el-button :icon="'Refresh'" :loading="loading" @click="refresh">刷新</el-button>
        <el-button type="primary" :icon="'FirstAidKit'" :loading="loading" @click="runDoctor">运行诊断</el-button>
      </template>
    </PageHeader>

    <div class="grid">
      <!-- Health: the headline panel -->
      <section class="lv-panel" v-loading="loading">
        <div class="lv-panel__head">
          <span class="lv-panel__title">守护进程</span>
          <span
            v-if="status"
            class="health-chip"
            :class="`health-chip--${tone(status.status)}`"
          >
            <span class="lv-dot" :class="isHealthy(status.status) ? 'lv-dot--live' : `lv-dot--${tone(status.status)}`"></span>
            {{ healthLabel(status.status) }}
          </span>
        </div>
        <div class="lv-panel__body">
          <template v-if="status">
            <div class="lv-kv">
              <div class="lv-kv__k">活跃执行</div>
              <div class="lv-kv__v lv-mono">{{ status.activeRuns }}</div>
              <div class="lv-kv__k">暂停执行</div>
              <div class="lv-kv__v lv-mono">{{ status.pausedRuns }}</div>
              <div class="lv-kv__k">运行时长</div>
              <div class="lv-kv__v lv-mono">{{ formatUptime(status.uptimeSeconds) }}</div>
              <div class="lv-kv__k">存储后端</div>
              <div class="lv-kv__v lv-mono">{{ status.storeType }}</div>
            </div>
            <div v-if="status.warnings && status.warnings.length" class="warns">
              <div v-for="(w, i) in status.warnings" :key="i" class="warn">
                <el-icon class="warn__icon"><WarningFilled /></el-icon>
                <span>{{ w }}</span>
              </div>
            </div>
          </template>
          <div v-else class="lv-empty"><span>未取到运行状态</span></div>
        </div>
      </section>

      <!-- Version -->
      <section class="lv-panel" v-loading="loading">
        <div class="lv-panel__head">
          <span class="lv-panel__title">构建信息</span>
        </div>
        <div class="lv-panel__body">
          <template v-if="version">
            <div class="lv-kv">
              <div class="lv-kv__k">版本</div>
              <div class="lv-kv__v lv-mono version-tag">{{ version.version }}</div>
              <div class="lv-kv__k">提交</div>
              <div class="lv-kv__v lv-mono">{{ version.gitCommit }}</div>
              <div class="lv-kv__k">构建时间</div>
              <div class="lv-kv__v lv-mono">{{ version.buildDate }}</div>
              <div class="lv-kv__k">工具链</div>
              <div class="lv-kv__v lv-mono">{{ version.goVersion }}</div>
            </div>
          </template>
          <div v-else class="lv-empty"><span>未取到版本信息</span></div>
        </div>
      </section>
    </div>

    <!-- Doctor -->
    <section class="lv-panel doctor">
      <div class="lv-panel__head">
        <span class="lv-panel__title">诊断报告</span>
        <span v-if="doctor" class="lv-panel__hint">
          <span class="health-chip health-chip--plain" :class="`health-chip--${tone(doctor.status)}`">{{ verdictLabel(doctor.status) }}</span>
          · 检查于 <span class="lv-mono">{{ formatTimestamp(doctor.checkedAt) }}</span>
        </span>
      </div>
      <div v-if="doctor" class="checks">
        <div
          v-for="c in doctor.checks"
          :key="c.name"
          class="check"
          :class="`check--${tone(c.status)}`"
        >
          <el-icon class="check__icon">
            <component :is="tone(c.status) === 'ok' ? 'CircleCheckFilled' : tone(c.status) === 'warn' ? 'WarningFilled' : 'CircleCloseFilled'" />
          </el-icon>
          <div class="check__main">
            <div class="check__head">
              <span class="check__name lv-mono">{{ c.name }}</span>
              <span class="check__verdict">{{ verdictLabel(c.status) }}</span>
            </div>
            <div class="check__msg">{{ c.message }}</div>
            <div v-if="c.remediation" class="check__fix">建议：{{ c.remediation }}</div>
          </div>
        </div>
      </div>
      <div v-else class="lv-empty">
        <el-icon class="lv-empty__icon"><FirstAidKit /></el-icon>
        <span class="lv-empty__title">尚未运行诊断</span>
        <span>点击右上「运行诊断」检查存储、凭据、审计链等项。</span>
      </div>
    </section>

    <!-- Config -->
    <section class="lv-panel" v-loading="loading">
      <div class="lv-panel__head">
        <span class="lv-panel__title">生效配置</span>
        <span v-if="config" class="lv-panel__hint">
          来源 <span class="lv-mono">{{ config.sourcePath }}</span> · 加载于
          <span class="lv-mono">{{ formatTimestamp(config.loadedAt) }}</span>
        </span>
      </div>
      <pre v-if="config" class="config-content">{{ config.content }}</pre>
      <div v-else class="lv-empty"><span>暂无配置信息</span></div>
    </section>
  </div>
</template>

<style scoped>
.grid {
  display: grid;
  grid-template-columns: repeat(auto-fit, minmax(340px, 1fr));
  gap: var(--lv-space-4);
  margin-bottom: var(--lv-space-4);
}

.health-chip {
  display: inline-flex;
  align-items: center;
  gap: 7px;
  height: 26px;
  padding: 0 11px 0 9px;
  border: 1px solid transparent;
  border-radius: var(--lv-radius-full);
  font-size: var(--lv-text-sm);
  font-weight: 500;
}

.health-chip--plain {
  height: auto;
  padding: 0;
  border: none;
  background: none;
}

.health-chip--ok {
  background: var(--lv-ok-soft);
  border-color: var(--lv-ok-border);
  color: var(--lv-ok);
}

.health-chip--warn {
  background: var(--lv-warn-soft);
  border-color: var(--lv-warn-border);
  color: var(--lv-warn);
}

.health-chip--bad {
  background: var(--lv-bad-soft);
  border-color: var(--lv-bad-border);
  color: var(--lv-bad);
}

.health-chip--idle {
  background: var(--lv-neutral-soft);
  border-color: var(--lv-neutral-border);
  color: var(--lv-text-2);
}

.version-tag {
  font-weight: 600;
  color: var(--lv-accent);
}

.warns {
  margin-top: var(--lv-space-3);
  display: flex;
  flex-direction: column;
  gap: 6px;
}

.warn {
  display: flex;
  align-items: flex-start;
  gap: var(--lv-space-2);
  padding: var(--lv-space-2) var(--lv-space-3);
  border: 1px solid var(--lv-warn-border);
  border-radius: var(--lv-radius);
  background: var(--lv-warn-soft);
  color: var(--lv-warn);
  font-size: var(--lv-text-xs);
  line-height: 1.6;
}

.warn__icon {
  flex: none;
  margin-top: 2px;
}

/* ------------------------------------------------------------------ doctor */

.doctor {
  margin-bottom: var(--lv-space-4);
}

.checks {
  padding: var(--lv-space-2) var(--lv-space-3) var(--lv-space-3);
  display: flex;
  flex-direction: column;
  gap: 2px;
}

.check {
  display: flex;
  gap: var(--lv-space-3);
  padding: var(--lv-space-3);
  border-radius: var(--lv-radius);
}

.check:hover {
  background: var(--lv-surface-3);
}

.check__icon {
  flex: none;
  margin-top: 1px;
  font-size: 16px;
}

.check--ok .check__icon {
  color: var(--lv-ok);
}

.check--warn .check__icon {
  color: var(--lv-warn);
}

.check--bad .check__icon {
  color: var(--lv-bad);
}

.check__main {
  min-width: 0;
}

.check__head {
  display: flex;
  align-items: baseline;
  gap: var(--lv-space-2);
}

.check__name {
  font-size: var(--lv-text-sm);
  font-weight: 600;
  color: var(--lv-text-1);
}

.check__verdict {
  font-size: var(--lv-text-xs);
  color: var(--lv-text-3);
}

.check__msg {
  margin-top: 2px;
  font-size: var(--lv-text-sm);
  color: var(--lv-text-2);
  overflow-wrap: anywhere;
}

.check__fix {
  margin-top: 3px;
  font-size: var(--lv-text-xs);
  color: var(--lv-text-3);
}

/* ------------------------------------------------------------------ config */

.config-content {
  margin: 0;
  padding: var(--lv-space-4);
  max-height: 460px;
  overflow: auto;
  background: var(--lv-surface-3);
  border-bottom-left-radius: var(--lv-radius-lg);
  border-bottom-right-radius: var(--lv-radius-lg);
  font-family: var(--lv-font-mono);
  font-size: var(--lv-text-xs);
  line-height: 1.7;
  color: var(--lv-text-2);
  tab-size: 2;
}
</style>
