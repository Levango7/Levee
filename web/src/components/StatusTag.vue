<script setup lang="ts">
// StatusTag renders a coloured el-tag for a change status. Centralised so the
// label / colour mapping never drifts between views.
import { computed } from 'vue'
import { STATUS_COLOR, STATUS_LABEL } from '@/utils/format'
import type { ChangeStatus } from '@/types/levee'

const props = defineProps<{ status: ChangeStatus }>()

const label = computed(() => STATUS_LABEL[props.status] || props.status)
const type = computed(() => STATUS_COLOR[props.status] || 'info')
</script>

<template>
  <el-tag :type="type" size="small" effect="light">{{ label }}</el-tag>
</template>