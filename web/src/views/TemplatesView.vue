<script setup lang="ts">
// TemplatesView provides CRUD over workflow templates plus a parameter form
// driven by the template's `requiredParams`. The form is reused for both
// creating a template and instantiating one into a new change.
import { computed, onMounted, reactive, ref } from 'vue'
import { useRouter } from 'vue-router'
import { ElMessage, ElMessageBox } from 'element-plus'
import { targetsApi, templatesApi } from '@/api'
import type { Template } from '@/types/levee'
import PageHeader from '@/components/PageHeader.vue'
import { formatTimestamp } from '@/utils/format'
import {
  blankForm,
  environmentOptions,
  instantiateIssues,
  instantiateWarnings,
  paramsRows,
  type InstantiateForm,
} from '@/utils/templateForm'

const router = useRouter()

const loading = ref(false)
const templates = ref<Template[]>([])
const total = ref(0)
const search = ref('')

interface EditState {
  visible: boolean
  mode: 'create' | 'edit'
  form: {
    name: string
    description: string
    workflowContent: string
    requiredParams: string
    overwrite: boolean
  }
}

const edit = reactive<EditState>({
  visible: false,
  mode: 'create',
  form: { name: '', description: '', workflowContent: '', requiredParams: '', overwrite: false },
})

interface InstantiateState {
  visible: boolean
  template: Template | null
  form: InstantiateForm
  /** 0 = parameters, 1 = review, 2 = created. */
  step: number
  /** Set on the last step: the change the wizard produced. */
  createdId: string
  createdStatus: string
}

const instantiate = reactive<InstantiateState>({
  visible: false,
  template: null,
  form: { label: '', params: {}, team: '', environment: '', priority: 'normal', dryRun: false },
  step: 0,
  createdId: '',
  createdStatus: '',
})

// Environments seen in the inventory, offered as suggestions. A hint, not a
// whitelist: the authorization layer judges the DECLARED environment, and the
// inventory does not necessarily name every environment a deployment uses.
const inventoryEnvs = ref<string[]>([])

const envOptions = computed(() => environmentOptions(inventoryEnvs.value, [instantiate.form.environment]))

const issues = computed(() =>
  instantiate.template ? instantiateIssues(instantiate.form, instantiate.template.requiredParams) : [],
)
const warnings = computed(() => instantiateWarnings(instantiate.form))
const paramRows = computed(() =>
  instantiate.template ? paramsRows(instantiate.form, instantiate.template.requiredParams) : [],
)
const issueFor = (field: string): string | undefined => issues.value.find((i) => i.field === field)?.message

async function loadInventoryEnvs(): Promise<void> {
  try {
    const res = await targetsApi.list({ pageSize: 200 })
    const seen = (res.items || []).map((t) => t.labels?.env).filter((e): e is string => !!e)
    inventoryEnvs.value = [...new Set(seen)]
  } catch {
    // A suggestion list is not worth an error banner: the field stays usable
    // and the authorization layer still judges whatever is declared.
    inventoryEnvs.value = []
  }
}

function nextStep(): void {
  // The gate: a parameter the server would refuse never leaves the browser.
  if (issues.value.length > 0) {
    ElMessage.warning(issues.value[0]?.message || '还有未填写的字段')
    return
  }
  instantiate.step = 1
}

function prevStep(): void {
  instantiate.step = 0
}

function closeInstantiate(): void {
  instantiate.visible = false
}

async function load(): Promise<void> {
  loading.value = true
  try {
    const res = await templatesApi.list({ nameContains: search.value || undefined, pageSize: 100 })
    templates.value = res.items || []
    total.value = res.totalSize || 0
  } catch (err) {
    ElMessage.error((err as { message?: string })?.message || '加载模板列表失败')
  } finally {
    loading.value = false
  }
}

function openCreate(): void {
  edit.mode = 'create'
  edit.form = { name: '', description: '', workflowContent: '', requiredParams: '', overwrite: false }
  edit.visible = true
}

function openEdit(row: Template): void {
  edit.mode = 'edit'
  edit.form = {
    name: row.name,
    description: row.description,
    workflowContent: row.workflowContent,
    requiredParams: row.requiredParams.join(', '),
    overwrite: true,
  }
  edit.visible = true
}

async function submitEdit(): Promise<void> {
  const requiredParams = edit.form.requiredParams
    .split(',')
    .map((s) => s.trim())
    .filter(Boolean)
  try {
    await templatesApi.create({
      name: edit.form.name,
      description: edit.form.description,
      workflowContent: edit.form.workflowContent,
      requiredParams,
      overwrite: edit.form.overwrite,
    })
    ElMessage.success(edit.mode === 'create' ? '模板已创建' : '模板已更新')
    edit.visible = false
    load()
  } catch (err) {
    ElMessage.error(`保存失败：${(err as { message?: string })?.message}`)
  }
}

async function remove(row: Template): Promise<void> {
  try {
    await ElMessageBox.confirm(`确认删除模板 ${row.name}？`, '删除模板', { type: 'warning' })
  } catch {
    return
  }
  try {
    await templatesApi.delete(row.name)
    ElMessage.success('已删除')
    load()
  } catch (err) {
    ElMessage.error(`删除失败：${(err as { message?: string })?.message}`)
  }
}

function openInstantiate(row: Template): void {
  instantiate.template = row
  instantiate.form = blankForm(row)
  instantiate.step = 0
  instantiate.createdId = ''
  instantiate.createdStatus = ''
  instantiate.visible = true
  if (inventoryEnvs.value.length === 0) loadInventoryEnvs()
}

async function submitInstantiate(): Promise<void> {
  if (!instantiate.template) return
  try {
    const change = await templatesApi.instantiate({
      templateName: instantiate.template.name,
      label: instantiate.form.label,
      params: instantiate.form.params,
      team: instantiate.form.team,
      environment: instantiate.form.environment,
      priority: instantiate.form.priority,
      dryRun: instantiate.form.dryRun,
    })
    instantiate.createdId = change.id
    instantiate.createdStatus = change.status
    instantiate.step = 2
    load()
  } catch (err) {
    ElMessage.error(`实例化失败：${(err as { message?: string })?.message}`)
  }
}

onMounted(load)
</script>

<template>
  <div class="levee-page">
    <PageHeader title="模板管理" description="工作流模板的版本化载体；实例化即生成一份可审批的变更">
      <template #actions>
        <el-button :icon="'Refresh'" :loading="loading" @click="load">刷新</el-button>
        <el-button type="primary" :icon="'Plus'" @click="openCreate">新建模板</el-button>
      </template>
    </PageHeader>

    <div class="lv-panel">
      <div class="lv-panel__head">
        <div class="lv-toolbar">
          <el-input
            v-model="search"
            placeholder="按名称搜索"
            clearable
            style="width: 220px"
            :prefix-icon="'Search'"
            @keyup.enter="load"
          />
          <el-button @click="load">查询</el-button>
        </div>
        <span class="lv-panel__hint lv-mono">共 {{ total }} 个模板</span>
      </div>

      <el-table v-loading="loading" :data="templates">
        <el-table-column label="模板" min-width="240">
          <template #default="{ row }">
            <div class="cell-template">
              <span class="cell-template__name lv-mono">{{ row.name }}</span>
              <span class="cell-template__desc">{{ row.description || '无描述' }}</span>
            </div>
          </template>
        </el-table-column>
        <el-table-column label="必填参数" min-width="220">
          <template #default="{ row }">
            <div class="param-list">
              <span v-for="p in row.requiredParams" :key="p" class="param-chip lv-mono">{{ p }}</span>
              <span v-if="row.requiredParams.length === 0" class="lv-muted">—</span>
            </div>
          </template>
        </el-table-column>
        <el-table-column label="创建时间" width="168">
          <template #default="{ row }">
            <span class="cell-time lv-mono">{{ formatTimestamp(row.createdAt) }}</span>
          </template>
        </el-table-column>
        <el-table-column label="更新时间" width="168">
          <template #default="{ row }">
            <span class="cell-time lv-mono">{{ formatTimestamp(row.updatedAt) }}</span>
          </template>
        </el-table-column>
        <el-table-column label="操作" width="200" fixed="right">
          <template #default="{ row }">
            <el-button text type="primary" @click="openInstantiate(row)">实例化</el-button>
            <el-button text type="primary" @click="openEdit(row)">编辑</el-button>
            <el-button text type="danger" @click="remove(row)">删除</el-button>
          </template>
        </el-table-column>
      </el-table>
    </div>

    <!-- Create / edit dialog -->
    <el-dialog v-model="edit.visible" :title="edit.mode === 'create' ? '新建模板' : '编辑模板'" width="680px">
      <el-form :model="edit.form" label-width="120px">
        <el-form-item label="名称" required>
          <el-input v-model="edit.form.name" :disabled="edit.mode === 'edit'" placeholder="如 deploy-web" />
        </el-form-item>
        <el-form-item label="描述">
          <el-input v-model="edit.form.description" type="textarea" :rows="2" />
        </el-form-item>
        <el-form-item label="必填参数">
          <el-input v-model="edit.form.requiredParams" placeholder="逗号分隔，如 host, port" />
        </el-form-item>
        <el-form-item label="Workflow" required>
          <el-input
            v-model="edit.form.workflowContent"
            type="textarea"
            :rows="12"
            class="code-input"
            placeholder="YAML workflow content"
          />
        </el-form-item>
        <el-form-item v-if="edit.mode === 'edit'" label="覆盖">
          <el-switch v-model="edit.form.overwrite" />
        </el-form-item>
      </el-form>
      <template #footer>
        <el-button @click="edit.visible = false">取消</el-button>
        <el-button type="primary" @click="submitEdit">保存</el-button>
      </template>
    </el-dialog>

    <!-- Instantiate wizard: parameters -> review -> created. The review step is
         the point of the wizard: the single dialog this replaces moved from
         "filled in" to "created" in one click, so a blank required parameter
         only surfaced as a server error and a typed environment was never read
         back. -->
    <el-dialog
      v-model="instantiate.visible"
      title="实例化模板"
      width="640px"
      :close-on-click-modal="false"
    >
      <template v-if="instantiate.template">
        <el-steps :active="instantiate.step" align-center finish-status="success" class="wizard__steps">
          <el-step title="参数" description="模板声明的必填项" />
          <el-step title="确认" description="提交前逐项核对" />
          <el-step title="完成" description="变更已创建" />
        </el-steps>

        <!-- Step 1: parameters -->
        <el-form v-if="instantiate.step === 0" :model="instantiate.form" label-width="120px">
          <el-form-item label="模板">
            <el-input :model-value="instantiate.template.name" disabled />
          </el-form-item>
          <el-form-item label="变更名称" required :error="issueFor('label')">
            <el-input v-model="instantiate.form.label" />
          </el-form-item>
          <el-form-item
            v-for="p in instantiate.template.requiredParams"
            :key="p"
            :label="p"
            required
            :error="issueFor('param:' + p)"
          >
            <el-input v-model="instantiate.form.params[p]" />
          </el-form-item>
          <el-form-item label="环境" :error="issueFor('environment')">
            <el-select
              v-model="instantiate.form.environment"
              filterable
              allow-create
              default-first-option
              clearable
              placeholder="选择或输入环境，如 prod"
              style="width: 100%"
            >
              <el-option v-for="e in envOptions" :key="e" :label="e" :value="e" />
            </el-select>
          </el-form-item>
          <el-form-item label="团队">
            <el-input v-model="instantiate.form.team" placeholder="选填" />
          </el-form-item>
          <el-form-item label="优先级">
            <el-select v-model="instantiate.form.priority" style="width: 160px">
              <el-option label="低" value="low" />
              <el-option label="中" value="normal" />
              <el-option label="高" value="high" />
              <el-option label="紧急" value="urgent" />
            </el-select>
          </el-form-item>
          <el-alert
            v-for="w in warnings"
            :key="w.field"
            type="warning"
            :closable="false"
            show-icon
            class="wizard__note"
            :title="w.message"
          />
        </el-form>

        <!-- Step 2: review -->
        <div v-else-if="instantiate.step === 1" class="wizard__review">
          <el-descriptions :column="1" border size="small">
            <el-descriptions-item label="模板">
              {{ instantiate.template.name }}
              <span v-if="instantiate.template.description" class="wizard__dim">
                — {{ instantiate.template.description }}</span>
            </el-descriptions-item>
            <el-descriptions-item label="变更名称">{{ instantiate.form.label }}</el-descriptions-item>
            <el-descriptions-item v-for="r in paramRows" :key="r.name" :label="r.name">
              <span :class="{ 'wizard__missing': r.missing }">{{ r.value || '（空）' }}</span>
            </el-descriptions-item>
            <el-descriptions-item label="环境">
              <span :class="{ 'wizard__missing': !instantiate.form.environment }">
                {{ instantiate.form.environment || '（留空，按默认环境判定）' }}
              </span>
            </el-descriptions-item>
            <el-descriptions-item label="团队">{{ instantiate.form.team || '—' }}</el-descriptions-item>
            <el-descriptions-item label="优先级">{{ instantiate.form.priority }}</el-descriptions-item>
            <el-descriptions-item label="创建方式">
              {{ instantiate.form.dryRun ? '仅计划：创建 planned 变更，不执行' : '提交审批：创建 pending 变更，等待审批后执行' }}
            </el-descriptions-item>
          </el-descriptions>
          <el-form-item label="仅计划" class="wizard__dry">
            <el-switch v-model="instantiate.form.dryRun" />
            <span class="wizard__dim">开启＝只创建一份 planned 变更供预览，不进入执行</span>
          </el-form-item>
          <el-collapse class="wizard__source">
            <el-collapse-item title="模板正文（参数由服务端代入）">
              <pre class="wizard__code">{{ instantiate.template.workflowContent }}</pre>
            </el-collapse-item>
          </el-collapse>
        </div>

        <!-- Step 3: created -->
        <div v-else-if="instantiate.step === 2" class="wizard__done">
          <p class="wizard__done-line">
            变更 <span class="lv-mono">{{ instantiate.createdId }}</span> 已创建，当前状态
            <strong>{{ instantiate.createdStatus || 'draft' }}</strong>。
          </p>
          <p class="wizard__dim">
            下一步：{{ instantiate.form.dryRun ? '查看计划与目标集' : '在审批中心等待/推进入口审批' }}。
          </p>
        </div>
      </template>

      <template #footer>
        <el-button v-if="instantiate.step === 0" @click="closeInstantiate">取消</el-button>
        <el-button v-if="instantiate.step === 1" @click="prevStep">上一步</el-button>
        <el-button v-if="instantiate.step === 0" type="primary" @click="nextStep">下一步：确认</el-button>
        <el-button v-else-if="instantiate.step === 1" type="primary" @click="submitInstantiate">
          创建变更
        </el-button>
        <template v-else>
          <el-button @click="closeInstantiate">关闭</el-button>
          <el-button type="primary" @click="router.push(`/monitor/${instantiate.createdId}`)">
            查看监控
          </el-button>
        </template>
      </template>
    </el-dialog>
  </div>
</template>

<style scoped>
.wizard__steps {
  margin-bottom: var(--lv-space-4, 16px);
}

.wizard__review {
  display: flex;
  flex-direction: column;
  gap: var(--lv-space-3, 12px);
}

.wizard__dim {
  color: var(--el-text-color-secondary);
  margin-left: 6px;
}

/* A missing value is shown, not hidden: the review step exists so the operator
   sees the gap before the server refuses or the change lands nameless. */
.wizard__missing {
  color: var(--el-color-danger);
}

.wizard__code {
  margin: 0;
  max-height: 220px;
  overflow: auto;
  font-family: var(--lv-font-mono, monospace);
  font-size: 12px;
  white-space: pre-wrap;
}

.wizard__note {
  margin-top: var(--lv-space-2, 8px);
}

.wizard__dry {
  margin-top: var(--lv-space-2, 8px);
}

.wizard__done-line {
  margin: 0 0 6px;
}

.wizard__done {
  padding: var(--lv-space-3, 12px) 0;
}

.cell-template {
  display: flex;
  flex-direction: column;
  gap: 2px;
  min-width: 0;
}

.cell-template__name {
  font-size: var(--lv-text-sm);
  font-weight: 600;
  color: var(--lv-text-1);
}

.cell-template__desc {
  font-size: var(--lv-text-xs);
  color: var(--lv-text-3);
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}

.param-list {
  display: flex;
  flex-wrap: wrap;
  gap: 4px;
}

.param-chip {
  display: inline-flex;
  align-items: center;
  height: 20px;
  padding: 0 7px;
  border-radius: var(--lv-radius-sm);
  background: var(--lv-surface-3);
  border: 1px solid var(--lv-border-soft);
  color: var(--lv-text-2);
  font-size: 11px;
}

.cell-time {
  font-size: var(--lv-text-xs);
  color: var(--lv-text-3);
}

/* The workflow body is YAML; mono is the only readable face for it. */
.code-input :deep(textarea) {
  font-family: var(--lv-font-mono);
  font-size: var(--lv-text-xs);
  line-height: 1.6;
}
</style>
