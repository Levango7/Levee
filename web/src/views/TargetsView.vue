<script setup lang="ts">
// TargetsView manages target hosts: list, add, remove, bulk import (CSV) and
// connectivity check. The check result is shown inline so operators can spot
// unreachable hosts at a glance.
import { computed, onMounted, reactive, ref } from 'vue'
import { ElMessage, ElMessageBox } from 'element-plus'
import { targetsApi, groupsApi, type HostGroup, type ImportResult, type TargetHistoryEntry } from '@/api'
import type { Target } from '@/types/levee'
import PageHeader from '@/components/PageHeader.vue'
import StateBlock from '@/components/StateBlock.vue'
import { classifyError, errorMessage } from '@/utils/apiState'
import type { LoadTier } from '@/utils/apiState'

const loading = ref(false)
const targets = ref<Target[]>([])
const total = ref(0)
const search = reactive({
  channelType: '',
  reachableOnly: false,
})

// Region state (utils/apiState.ts). The "no hosts at all" and "the fetch
// failed" answers used to be identical: an empty table plus a toast that had
// already timed out.
const loadError = ref<unknown>(null)

const regionTier = computed<LoadTier>(() => {
  if (loadError.value !== null) return classifyError(loadError.value)
  if (!loading.value && targets.value.length === 0) return 'empty'
  return 'ok'
})

const regionMessage = computed(() => errorMessage(loadError.value, '加载目标列表失败'))

const filterActive = computed(
  () => search.channelType !== '' || search.reachableOnly || groupFilter.value !== '',
)

// Group tree (groupsApi → /groups). Targets carry their group_id from the
// stored row (proto Target.group_id), so the tree filters the LOADED page
// locally — the list endpoint's group filter is the server-side twin for
// paged deployments.
const groups = ref<HostGroup[]>([])
const groupFilter = ref('')
const groupNameOf = computed(() => {
  const m: Record<string, string> = {}
  for (const g of groups.value) m[g.id] = g.name
  return m
})

const visibleTargets = computed(() => {
  if (groupFilter.value === '') return targets.value
  return targets.value.filter((tg) => tg.groupId === groupFilter.value)
})

async function loadGroups(): Promise<void> {
  try {
    groups.value = await groupsApi.list()
  } catch {
    // The tree is an aid, not a gate: a failed group load keeps the flat
    // list working and the filter hidden, which is what an operator with a
    // broken auxiliary file needs.
    groups.value = []
  }
}



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
// YAML import (atomic, grouped, label-move-guarded) — the CSV loop below is
// the legacy path kept for one-host paste jobs.
const yamlImportVisible = ref(false)
const yamlText = ref('')
const yamlImporting = ref(false)
const importResult = ref<ImportResult | null>(null)
const historyVisible = ref(false)
const historyFor = ref<Target | null>(null)
const historyLoading = ref(false)
const historyEntries = ref<TargetHistoryEntry[]>([])

async function load(): Promise<void> {
  loading.value = true
  loadError.value = null
  try {
    const res = await targetsApi.list({
      channelType: search.channelType || undefined,
      reachableOnly: search.reachableOnly || undefined,
      pageSize: 200,
    })
    targets.value = res.items || []
    total.value = res.totalSize || 0
  } catch (err) {
    loadError.value = err
  } finally {
    loading.value = false
  }
}

function openAdd(): void {
  add.form = { hostname: '', channelType: 'ssh', port: 22, credentialRef: '', labels: '' }
  add.visible = true
}

// Named rather than inline in the template so the empty state's action and the
// toolbar's 查询 button cannot drift into two different definitions of
// "no filter".
function resetSearch(): void {
  search.channelType = ''
  search.reachableOnly = false
  load()
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

// YAML import through /targets/import: one atomic request. The reply's
// created/updated/failed counts and per-row errors are shown in place instead
// of a bare toast, because a partial import with a label-move refusal names
// the row that was refused — hiding that behind "导入完成" would be the lie
// the atomic endpoint exists to prevent.
async function submitYamlImport(): Promise<void> {
  if (!yamlText.value.trim()) return
  yamlImporting.value = true
  try {
    importResult.value = await targetsApi.import(yamlText.value)
    if (importResult.value.failed === 0 && importResult.value.errors.length === 0) {
      ElMessage.success(`导入完成：新增 ${importResult.value.created}，更新 ${importResult.value.updated}`)
      yamlImportVisible.value = false
      yamlText.value = ''
      importResult.value = null
    }
    load()
  } catch (err) {
    // Label-move refusals name the moved host and the environment it would
    // leave — surface the server's words, not a generic failure.
    ElMessage.error(errorMessage(err, '清单导入失败'))
  } finally {
    yamlImporting.value = false
  }
}

async function openHistory(row: Target): Promise<void> {
  historyFor.value = row
  historyVisible.value = true
  historyLoading.value = true
  historyEntries.value = []
  try {
    historyEntries.value = await targetsApi.history(row.id, 50)
  } catch (err) {
    ElMessage.error(errorMessage(err, '加载主机历史失败'))
  } finally {
    historyLoading.value = false
  }
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

onMounted(() => {
  load()
  loadGroups()
})
</script>

<template>
  <div class="levee-page">
    <PageHeader title="目标机管理" description="受管主机清单与连通性；执行通道与凭据在此绑定">
      <template #actions>
        <el-button :icon="'Refresh'" :loading="loading" @click="load">刷新</el-button>
        <el-button type="primary" :icon="'Plus'" @click="openAdd">添加目标机</el-button>
      </template>
    </PageHeader>

    <div class="lv-panel">
      <div class="lv-panel__head">
        <div class="lv-toolbar">
          <el-select v-model="search.channelType" placeholder="通道类型" clearable style="width: 140px">
            <el-option label="SSH" value="ssh" />
            <el-option label="WinRM" value="winrm" />
          </el-select>
          <el-checkbox v-model="search.reachableOnly">仅可达</el-checkbox>
          <el-select
            v-if="groups.length > 0"
            v-model="groupFilter"
            placeholder="全部分组"
            clearable
            style="width: 160px"
          >
            <el-option v-for="g in groups" :key="g.id" :label="g.name" :value="g.id" />
          </el-select>
          <el-button @click="load">查询</el-button>
        </div>
        <div class="lv-toolbar">
          <span class="lv-panel__hint lv-mono">共 {{ total }} 台</span>
          <el-button @click="bulkCheck">批量连通性检查</el-button>
          <el-button @click="yamlImportVisible = true">导入清单（YAML）</el-button>
          <el-button @click="importVisible = true">CSV 导入</el-button>
        </div>
      </div>

      <StateBlock
        v-if="regionTier === 'error' || regionTier === 'forbidden'"
        :tier="regionTier"
        :message="regionMessage"
        :hint="regionTier === 'forbidden' ? '目标清单受角色管控，请联系管理员授权' : ''"
        @retry="load"
      />
      <StateBlock
        v-else-if="regionTier === 'empty'"
        tier="empty"
        :hint="
          filterActive
            ? '当前筛选条件下没有目标机'
            : '还没有登记任何目标机，先用「添加」或「导入」录入'
        "
        :action-label="filterActive ? '清除筛选条件' : '添加目标机'"
        @action="filterActive ? resetSearch() : openAdd()"
      />
      <el-table v-else v-loading="loading" :data="visibleTargets" stripe>
        <el-table-column prop="id" label="ID" width="180" show-overflow-tooltip />
        <el-table-column prop="hostname" label="主机名" min-width="160" show-overflow-tooltip />
        <el-table-column prop="channelType" label="通道" width="100" />
        <el-table-column prop="port" label="端口" width="80" />
        <el-table-column prop="credentialRef" label="凭据" width="160" show-overflow-tooltip />
        <el-table-column label="分组" width="140" show-overflow-tooltip>
          <template #default="{ row }">
            <span v-if="row.groupId">{{ groupNameOf[row.groupId] || row.groupId }}</span>
            <span v-else style="color: var(--lv-text-3)">—</span>
          </template>
        </el-table-column>
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
            <el-button text type="primary" @click="openHistory(row)">历史</el-button>
            <el-button text type="danger" @click="remove(row)">删除</el-button>
          </template>
        </el-table-column>
      </el-table>
    </div>

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

    <!-- YAML inventory import: ONE atomic request through
         /targets/import. The backend previews label moves before writing
         anything — a file that would move a registered host out of its
         environment is refused whole, and the refusal names the row and the
         environment. The result panel shows created/updated/failed and the
         per-row errors, because "导入完成" over a partial import is the
         lie the atomic endpoint exists to prevent. -->
    <el-dialog v-model="yamlImportVisible" title="导入清单（YAML，原子）" width="720px">
      <el-input
        v-model="yamlText"
        type="textarea"
        :rows="12"
        placeholder="targets:&#10;  - address: 10.0.0.1&#10;    channel_type: ssh&#10;    labels:&#10;      env: prod&#10;    group: prod/db"
      />
      <!-- Import result is a FORM-level outcome inside a dialog (same reason
           TemplatesView's el-alert stays), rendered as plain text rather than
           el-alert: regionStates.spec keeps el-alert out of views. -->
      <div
        v-if="importResult && (importResult.failed > 0 || importResult.errors.length > 0)"
        class="import-result"
      >
        <p>新增 {{ importResult.created }}，更新 {{ importResult.updated }}，失败 {{ importResult.failed }}</p>
        <p v-for="(e, i) in importResult.errors" :key="i" class="import-error">{{ e }}</p>
      </div>
      <template #footer>
        <el-button @click="yamlImportVisible = false">关闭</el-button>
        <el-button type="primary" :loading="yamlImporting" @click="submitYamlImport">导入</el-button>
      </template>
    </el-dialog>

    <!-- Host history: the runs that executed on this host. -->
    <el-dialog
      v-model="historyVisible"
      :title="historyFor ? `主机历史：${historyFor.hostname}` : '主机历史'"
      width="760px"
    >
      <div v-loading="historyLoading" style="min-height: 120px">
        <el-table v-if="historyEntries.length > 0" :data="historyEntries" size="small">
          <el-table-column prop="run_id" label="变更" min-width="160" show-overflow-tooltip />
          <el-table-column prop="workflow_name" label="工作流" min-width="180" show-overflow-tooltip />
          <el-table-column label="状态" width="140">
            <template #default="{ row }">
              <el-tag size="small">{{ row.status }}</el-tag>
            </template>
          </el-table-column>
          <el-table-column prop="creator" label="发起人" width="120" />
        </el-table>
        <p v-else-if="!historyLoading" class="history-empty">该主机还没有执行过任何变更</p>
      </div>
      <template #footer>
        <el-button @click="historyVisible = false">关闭</el-button>
      </template>
    </el-dialog>
  </div>
</template>

<style scoped>
/* Only the failure text of an inline check needs a rule: reachability and check
 * results use the themed tag colours from the base layer. */
.check-error {
  margin-left: 6px;
  color: var(--lv-bad);
  font-size: var(--lv-text-xs);
}

.import-result {
  margin-top: 8px;
  padding: var(--lv-space-2) var(--lv-space-3);
  background: var(--lv-surface-3);
  border-radius: var(--lv-radius);
}

.import-result p {
  margin: 2px 0;
  font-size: var(--lv-text-sm);
}

.import-error {
  color: var(--lv-bad);
}

.history-empty {
  padding: var(--lv-space-6) 0;
  text-align: center;
  color: var(--lv-text-3);
  font-size: var(--lv-text-sm);
}
</style>