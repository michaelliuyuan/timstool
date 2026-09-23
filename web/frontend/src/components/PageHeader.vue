<template>
  <div class="tims-page-header">
    <div class="tims-page-header__text">
      <h2 class="tims-page-header__title">{{ title }}</h2>
      <p v-if="subtitle" class="tims-page-header__subtitle">{{ subtitle }}</p>
    </div>
    <div v-if="$slots.actions" class="tims-page-header__actions">
      <slot name="actions" />
    </div>
  </div>
</template>

<script setup lang="ts">
defineProps<{
  title: string
  subtitle?: string
}>()
</script>

<style scoped>
.tims-page-header {
  display: flex;
  justify-content: space-between;
  align-items: center;
  gap: 16px;
  margin-bottom: 20px;
}
.tims-page-header__title {
  margin: 0;
  font-size: 20px;
  font-weight: 600;
  color: var(--tims-text, #1f2430);
  letter-spacing: 0.2px;
}
.tims-page-header__subtitle {
  margin: 4px 0 0;
  font-size: 13px;
  color: var(--tims-text-2, #6b7280);
}
.tims-page-header__actions {
  display: flex;
  align-items: center;
  gap: 8px;
  flex: none;
}
</style>
