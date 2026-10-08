<script setup lang="ts">
// PageHeader — the standard title block for every page: an optional description
// under the title and an action cluster on the right.
//
// Exists so the console's pages share one entry rhythm. Before it each view
// carried its own `<h2 class="levee-page__title">` and the vertical spacing
// drifted between them.
defineProps<{
  title: string
  description?: string
}>()
</script>

<template>
  <header class="lv-page-head">
    <div class="lv-page-head__main">
      <h2 class="lv-page-head__title">{{ title }}</h2>
      <p v-if="description" class="lv-page-head__desc">{{ description }}</p>
    </div>
    <div v-if="$slots.actions" class="lv-page-head__actions">
      <slot name="actions" />
    </div>
  </header>
</template>
