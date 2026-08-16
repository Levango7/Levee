<script setup lang="ts">
// TargetsView manages target hosts: list, add, remove, bulk import (CSV) and
// connectivity check. The check result is shown inline so operators can spot
// unreachable hosts at a glance.
import { onMounted, reactive, ref } from 'vue'
import { ElMessage, ElMessageBox } from 'element-plus'
import { targetsApi } from '@/api'
import type { Target } from '@/types/levee'

const loading = ref(false)
const targets = ref<Target[]>([])
const total = ref(0)
const search = reactive({
  channelType: '',
  reachableOnly: false,
})

interface CheckResult {
  [id: string]: { reachable: boolean; latencyMs: number; error: string; checking: boolean }
}

const checkResults = reactive<CheckResult>({})

interface AddState {
  visible: boolean
  form: {
    hostname: string
    channelType: 'ssh' | 'winrm'
    port: number
    credentialRef: string
    labels: string
  }
}

const add = reactive<AddState>({
  visible: false,
  form: { hostname: '', channelType: 'ssh', port: 22, credentialRef: '', labels: '' },
})

const importVisible = ref(false)
const importText = ref('')

async function load(): Promise<void> {
  loading.value = true
  try {
    const res = await targetsApi.list({
      channelType: search.channelType || undefined,
      reachableOnly: search.reachableOnly || undefined,
      pageSize: 200,
    })
    targets.value = res.items || []
    total.value = res.totalSize || 0
  } catch (err) {
    ElMessage.error((err as { message?: string })?.message || '加载目标列表失败')
  } finally {
    loading.value = false
  }
}

function openAdd(): void {
  add.form = { hostname: '', channelType: 'ssh', port: 22, credentialRef: '', labels: '' }
  add.visible = true
}

async function submitAdd(): Promise<void> {
  const labels: Record<string, string> = {}
  for (const pair of add.form.labels.split(',').map((s) => s.trim()).filter(Boolean)) {
    const [k, v] = pair.split('=')
    if (k) labels[k.trim()] = (v || '').trim()
  }
  try {
    await targetsApi.add({
      hostname: add.form.hostname,
      channelType: add.form.channelType,
      port: add.form.port,
      credentialRef: add.form.credentialRef,
      labels,
    })
    ElMessage.success('目标机已添加')
    add.visible = false
    load()
  } catch (err) {
    ElMessage.error(`添加失败：${(err as { message?: string })?.message}`)
  }
}

async function remove(row: Target): Promise<void> {
  try {
    await ElMessageBox.confirm(`确认删除目标机 ${row.hostname}？`, '删除目标机', { type: 'warning' })
  } catch {
    return
  }
  try {
    await targetsApi.remove(row.id)
    ElMessage.success('已删除')
    load()
  } catch (err) {
    ElMessage.error(`删除失败：${(err as { message?: string })?.message}`)
  }
}

async function check(row: Target): Promise<void> {
  checkResults[row.id] = { reachable: false, latencyMs: 0, error: '', checking: true }
  try {
    const res = await targetsApi.check(row.id, true, 10)
    checkResults[row.id] = {
      reachable: res.reachable,
      latencyMs: res.latencyMs,
      error: res.error,
      checking: false,
    }
  } catch (err) {
    checkResults[row.id] = {
      reachable: false,
      latencyMs: 0,
      error: (err as { message?: string })?.message || '检查失败',
      checking: false,
    }
  }
}

async function bulkCheck(): Promise<void> {
  for (const t of targets.value) {
    await check(t)
  }
}

// Helper for the template: returns the check result for a target id, or null.
// We use this instead of direct indexing because noUncheckedIndexedAccess
// makes checkResults[id] possibly undefined, which Vue templates cannot
// narrow through v-if.
function resultOf(id: string): { reachable: boolean; latencyMs: number; error: string; checking: boolean } | null {
  return checkResults[id] || null
}

async function submitImport(): Promise<void> {
  // CSV format: hostname,channel_type,port,credential_ref,label1=v1,label2=v2
  const lines = importText.value.split('\n').map((l) => l.trim()).filter(Boolean)
  let success = 0
  let failed = 0
  for (const line of lines) {
    const parts = line.split(',').map((s) => s.trim())
    const [hostname, channelType, port, credentialRef, ...labelPairs] = parts
    if (!hostname || !channelType) {
      failed++
      continue
    }
    const labels: Record<string, string> = {}
    for (const pair of labelPairs) {
      const [k, v] = pair.split('=')
      if (k) labels[k.trim()] = (v || '').trim()
    }
    try {
      await targetsApi.add({
        hostname,
        channelType: channelType as 'ssh' | 'winrm',
        port: Number(port) || (channelType === 'winrm' ? 5985 : 22),
        credentialRef,
        labels,
      })
      success++
    } catch {
      failed++
    }
  }
  ElMessage.success(`导入完成：成功 ${success}，失败 ${failed}`)
  importVisible.value = false
  importText.value = ''
  load()
}

onMounted(load)
</script>

<template>
  <div class="levee-page">
    <h2 class="levee-page__title">目标机管理</h2>

    <el-card shadow="never" class="levee-card">
      <div class="toolbar">
        <el-select v-model="search.channelType" placeholder="通道类型" clearable style="width: 140px">
          <el-option label="SSH" value="ssh" />
          <el-option label="WinRM" value="winrm" />
        </el-select>
        <el-checkbox v-model="search.reachableOnly">仅可达</el-checkbox>
        <el-button @click="load">查询</el-button>
        <div class="toolbar__right">
          <el-button @click="bulkCheck">批量连通性检查</el-button>
          <el-button @click="importVisible = true">导入</el-button>
          <el-button type="primary" @click="openAdd">添加目标机</el-button>
        </div>
      </div>

      <el-table v-loading="loading" :data="targets" stripe>
        <el-table-column prop="id" label="ID" width="180" show-overflow-tooltip />
        <el-table-column prop="hostname" label="主机名" min-width="160" show-overflow-tooltip />
        <el-table-column prop="channelType" label="通道" width="100" />
        <el-table-column prop="port" label="端口" width="80" />
        <el-table-column prop="credentialRef" label="凭据" width="160" show-overflow-tooltip />
        <el-table-column label="标签" min-width="200">
          <template #default="{ row }">
            <el-tag v-for="(v, k) in row.labels" :key="k" size="small" class="label-tag">{{ k }}={{ v }}</el-tag>
          </template>
        </el-table-column>
        <el-table-column label="可达" width="100">
          <template #default="{ row }">
            <el-tag :type="row.reachable ? 'success' : 'danger'" size="small">
              {{ row.reachable ? '是' : '否' }}
            </el-tag>
          </template>
        </el-table-column>
        <el-table-column label="连通性检查" width="200">
          <template #default="{ row }">
            <template v-if="resultOf(row.id)">
              <el-tag v-if="resultOf(row.id)!.checking" type="info" size="small">检查中...</el-tag>
              <template v-else>
                <el-tag :type="resultOf(row.id)!.reachable ? 'success' : 'danger'" size="small">
                  {{ resultOf(row.id)!.reachable ? `${resultOf(row.id)!.latencyMs}ms` : '不可达' }}
                </el-tag>
                <span v-if="resultOf(row.id)!.error" class="check-error">{{ resultOf(row.id)!.error }}</span>
              </template>
            </template>
          </template>
        </el-table-column>
        <el-table-column label="操作" width="180" fixed="right">
          <template #default="{ row }">
            <el-button text type="primary" @click="check(row)">检查</el-button>
            <el-button text type="danger" @click="remove(row)">删除</el-button>
          </template>
        </el-table-column>
      </el-table>
    </el-card>

    <!-- Add dialog -->
    <el-dialog v-model="add.visible" title="添加目标机" width="480px">
      <el-form :model="add.form" label-width="100px">
        <el-form-item label="主机名" required>
          <el-input v-model="add.form.hostname" />
        </el-form-item>
        <el-form-item label="通道" required>
          <el-select v-model="add.form.channelType" style="width: 160px">
            <el-option label="SSH" value="ssh" />
            <el-option label="WinRM" value="winrm" />
          </el-select>
        </el-form-item>
        <el-form-item label="端口" required>
          <el-input-number v-model="add.form.port" :min="1" :max="65535" />
        </el-form-item>
        <el-form-item label="凭据引用">
          <el-input v-model="add.form.credentialRef" placeholder="credential://name" />
        </el-form-item>
        <el-form-item label="标签">
          <el-input v-model="add.form.labels" placeholder="key1=v1,key2=v2" />
        </el-form-item>
      </el-form>
      <template #footer>
        <el-button @click="add.visible = false">取消</el-button>
        <el-button type="primary" @click="submitAdd">添加</el-button>
      </template>
    </el-dialog>

    <!-- Import dialog -->
    <el-dialog v-model="importVisible" title="批量导入（CSV）" width="640px">
      <el-input
        v-model="importText"
        type="textarea"
        :rows="10"
        placeholder="hostname,channel_type,port,credential_ref,label1=v1,label2=v2&#10;host1,ssh,22,credential://ops,env=prod,role=web"
      />
      <template #footer>
        <el-button @click="importVisible = false">取消</el-button>
        <el-button type="primary" @click="submitImport">导入</el-button>
      </template>
    </el-dialog>
  </div>
</template>

<style scoped>
.toolbar {
  display: flex;
  gap: 8px;
  align-items: center;
  margin-bottom: 12px;
}
.toolbar__right {
  margin-left: auto;
  display: flex;
  gap: 8px;
}
.label-tag + .label-tag {
  margin-left: 4px;
}
.check-error {
  margin-left: 4px;
  color: #f56c6c;
  font-size: 12px;
}
</style>