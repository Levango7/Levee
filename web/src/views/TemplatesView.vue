<script setup lang="ts">
// TemplatesView provides CRUD over workflow templates plus a parameter form
// driven by the template's `requiredParams`. The form is reused for both
// creating a template and instantiating one into a new change.
import { onMounted, reactive, ref } from 'vue'
import { ElMessage, ElMessageBox } from 'element-plus'
import { templatesApi } from '@/api'
import type { Template } from '@/types/levee'
import { formatTimestamp } from '@/utils/format'

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
  form: {
    label: string
    params: Record<string, string>
    team: string
    environment: string
    priority: string
    dryRun: boolean
  }
}

const instantiate = reactive<InstantiateState>({
  visible: false,
  template: null,
  form: { label: '', params: {}, team: '', environment: '', priority: 'normal', dryRun: false },
})

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
  instantiate.form = {
    label: `${row.name}-${Date.now()}`,
    params: {},
    team: '',
    environment: '',
    priority: 'normal',
    dryRun: false,
  }
  for (const p of row.requiredParams) {
    instantiate.form.params[p] = ''
  }
  instantiate.visible = true
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
    ElMessage.success(`已创建变更 ${change.id}`)
    instantiate.visible = false
  } catch (err) {
    ElMessage.error(`实例化失败：${(err as { message?: string })?.message}`)
  }
}

onMounted(load)
</script>

<template>
  <div class="levee-page">
    <h2 class="levee-page__title">模板管理</h2>

    <el-card shadow="never" class="levee-card">
      <div class="toolbar">
        <el-input v-model="search" placeholder="按名称搜索" clearable style="width: 220px" @keyup.enter="load" />
        <el-button @click="load">查询</el-button>
        <div class="toolbar__right">
          <el-button type="primary" @click="openCreate">新建模板</el-button>
        </div>
      </div>

      <el-table v-loading="loading" :data="templates" stripe>
        <el-table-column prop="name" label="名称" min-width="160" show-overflow-tooltip />
        <el-table-column prop="description" label="描述" min-width="220" show-overflow-tooltip />
        <el-table-column label="必填参数" width="220">
          <template #default="{ row }">
            <el-tag v-for="p in row.requiredParams" :key="p" size="small" class="param-tag">{{ p }}</el-tag>
          </template>
        </el-table-column>
        <el-table-column label="创建时间" width="180">
          <template #default="{ row }">{{ formatTimestamp(row.createdAt) }}</template>
        </el-table-column>
        <el-table-column label="更新时间" width="180">
          <template #default="{ row }">{{ formatTimestamp(row.updatedAt) }}</template>
        </el-table-column>
        <el-table-column label="操作" width="240" fixed="right">
          <template #default="{ row }">
            <el-button text type="primary" @click="openInstantiate(row)">实例化</el-button>
            <el-button text type="primary" @click="openEdit(row)">编辑</el-button>
            <el-button text type="danger" @click="remove(row)">删除</el-button>
          </template>
        </el-table-column>
      </el-table>
    </el-card>

    <!-- Create / edit dialog -->
    <el-dialog v-model="edit.visible" :title="edit.mode === 'create' ? '新建模板' : '编辑模板'" width="640px">
      <el-form :model="edit.form" label-width="120px">
        <el-form-item label="名称" required>
          <el-input v-model="edit.form.name" :disabled="edit.mode === 'edit'" />
        </el-form-item>
        <el-form-item label="描述">
          <el-input v-model="edit.form.description" type="textarea" :rows="2" />
        </el-form-item>
        <el-form-item label="必填参数">
          <el-input v-model="edit.form.requiredParams" placeholder="逗号分隔，如 host, port" />
        </el-form-item>
        <el-form-item label="Workflow" required>
          <el-input v-model="edit.form.workflowContent" type="textarea" :rows="10" placeholder="YAML workflow content" />
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

    <!-- Instantiate dialog -->
    <el-dialog v-model="instantiate.visible" title="实例化模板" width="560px">
      <template v-if="instantiate.template">
        <el-form :model="instantiate.form" label-width="120px">
          <el-form-item label="模板">
            <el-input :model-value="instantiate.template.name" disabled />
          </el-form-item>
          <el-form-item label="变更名称" required>
            <el-input v-model="instantiate.form.label" />
          </el-form-item>
          <el-form-item v-for="p in instantiate.template.requiredParams" :key="p" :label="p">
            <el-input v-model="instantiate.form.params[p]" />
          </el-form-item>
          <el-form-item label="团队">
            <el-input v-model="instantiate.form.team" />
          </el-form-item>
          <el-form-item label="环境">
            <el-input v-model="instantiate.form.environment" />
          </el-form-item>
          <el-form-item label="优先级">
            <el-select v-model="instantiate.form.priority" style="width: 160px">
              <el-option label="低" value="low" />
              <el-option label="中" value="normal" />
              <el-option label="高" value="high" />
              <el-option label="紧急" value="urgent" />
            </el-select>
          </el-form-item>
          <el-form-item label="仅计划">
            <el-switch v-model="instantiate.form.dryRun" />
          </el-form-item>
        </el-form>
      </template>
      <template #footer>
        <el-button @click="instantiate.visible = false">取消</el-button>
        <el-button type="primary" @click="submitInstantiate">创建</el-button>
      </template>
    </el-dialog>
  </div>
</template>

<style scoped>
.toolbar {
  display: flex;
  gap: 8px;
  margin-bottom: 12px;
}
.toolbar__right {
  margin-left: auto;
}
.param-tag + .param-tag {
  margin-left: 4px;
}
</style>