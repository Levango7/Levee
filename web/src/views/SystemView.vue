<script setup lang="ts">
// SystemView is the operator dashboard: version, daemon health, doctor report
// and the loaded config. It is read-only; the only action is "run doctor"
// which re-runs the diagnostic checks.
import { onMounted, ref } from 'vue'
import { ElMessage } from 'element-plus'
import { systemApi } from '@/api'
import type { SystemStatus, VersionInfo } from '@/types/levee'
import { formatTimestamp, formatUptime } from '@/utils/format'

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

function statusType(s: string): 'success' | 'warning' | 'danger' | 'info' {
  if (s === 'healthy' || s === 'pass') return 'success'
  if (s === 'degraded' || s === 'warn') return 'warning'
  if (s === 'unhealthy' || s === 'fail') return 'danger'
  return 'info'
}

onMounted(async () => {
  loading.value = true
  await Promise.all([loadVersion(), loadStatus(), loadConfig()])
  loading.value = false
})
</script>

<template>
  <div class="levee-page">
    <h2 class="levee-page__title">系统状态</h2>

    <el-row :gutter="16">
      <!-- Version -->
      <el-col :span="8">
        <el-card shadow="never" class="levee-card" v-loading="loading">
          <template #header>版本信息</template>
          <template v-if="version">
            <el-descriptions :column="1" border>
              <el-descriptions-item label="版本">{{ version.version }}</el-descriptions-item>
              <el-descriptions-item label="Commit">{{ version.gitCommit }}</el-descriptions-item>
              <el-descriptions-item label="构建时间">{{ version.buildDate }}</el-descriptions-item>
              <el-descriptions-item label="Go 版本">{{ version.goVersion }}</el-descriptions-item>
            </el-descriptions>
          </template>
        </el-card>
      </el-col>

      <!-- Health -->
      <el-col :span="8">
        <el-card shadow="never" class="levee-card" v-loading="loading">
          <template #header>健康状态</template>
          <template v-if="status">
            <el-descriptions :column="1" border>
              <el-descriptions-item label="状态">
                <el-tag :type="statusType(status.status)" size="small">{{ status.status }}</el-tag>
              </el-descriptions-item>
              <el-descriptions-item label="活跃执行">{{ status.activeRuns }}</el-descriptions-item>
              <el-descriptions-item label="暂停执行">{{ status.pausedRuns }}</el-descriptions-item>
              <el-descriptions-item label="运行时长">{{ formatUptime(status.uptimeSeconds) }}</el-descriptions-item>
              <el-descriptions-item label="存储后端">{{ status.storeType }}</el-descriptions-item>
            </el-descriptions>
            <el-alert
              v-for="(w, i) in status.warnings"
              :key="i"
              type="warning"
              :title="w"
              show-icon
              :closable="false"
              style="margin-top: 8px"
            />
          </template>
        </el-card>
      </el-col>

      <!-- Doctor -->
      <el-col :span="8">
        <el-card shadow="never" class="levee-card">
          <template #header>
            <div class="doctor-head">
              <span>诊断报告</span>
              <el-button text type="primary" :loading="loading" @click="runDoctor">运行诊断</el-button>
            </div>
          </template>
          <template v-if="doctor">
            <el-tag :type="statusType(doctor.status)" size="small">{{ doctor.status }}</el-tag>
            <span class="doctor-time">检查时间：{{ formatTimestamp(doctor.checkedAt) }}</span>
            <el-table :data="doctor.checks" stripe style="margin-top: 8px">
              <el-table-column prop="name" label="检查项" width="160" show-overflow-tooltip />
              <el-table-column label="状态" width="80">
                <template #default="{ row }">
                  <el-tag :type="statusType(row.status)" size="small">{{ row.status }}</el-tag>
                </template>
              </el-table-column>
              <el-table-column prop="message" label="信息" min-width="180" show-overflow-tooltip />
            </el-table>
          </template>
          <el-empty v-else description="点击「运行诊断」开始检查" />
        </el-card>
      </el-col>
    </el-row>

    <!-- Config -->
    <el-card shadow="never" class="levee-card" v-loading="loading">
      <template #header>
        <div class="config-head">
          <span>当前配置</span>
          <span v-if="config" class="config-source">来源：{{ config.sourcePath }} · 加载于 {{ formatTimestamp(config.loadedAt) }}</span>
        </div>
      </template>
      <pre v-if="config" class="config-content">{{ config.content }}</pre>
      <el-empty v-else description="暂无配置信息" />
    </el-card>
  </div>
</template>

<style scoped>
.doctor-head {
  display: flex;
  justify-content: space-between;
  align-items: center;
}
.doctor-time {
  margin-left: 8px;
  font-size: 12px;
  color: #909399;
}
.config-head {
  display: flex;
  justify-content: space-between;
  align-items: center;
}
.config-source {
  font-size: 12px;
  color: #909399;
}
.config-content {
  background: #fafafa;
  padding: 12px;
  border-radius: 4px;
  font-family: 'Consolas', 'Menlo', monospace;
  font-size: 12px;
  overflow-x: auto;
  margin: 0;
}
</style>