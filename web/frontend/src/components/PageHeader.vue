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
  flex-wrap: wrap; /* #16 375px 溢出补刀：仅动作行 wrap 不够——__actions flex:none 不收缩，
    标题+动作合计超视口时整行仍溢出（任务监控 375px 溢 3px）；header 根行可 wrap，
    窄视口动作落次行，桌面有富余不换行零扰 */
}
.tims-page-header__title {
  margin: 0;
  font-size: var(--tims-font-xl); /* #13 页面标题归一 22px xl 档 */
  font-weight: 600;
  color: var(--tims-text, #1f2430);
  letter-spacing: 0.2px;
}
.tims-page-header__subtitle {
  margin: 4px 0 0;
  font-size: var(--tims-font-sm);
  color: var(--tims-text-2, #6b7280);
}
.tims-page-header__actions {
  display: flex;
  align-items: center;
  gap: 8px;
  flex: 0 1 auto; /* #16 原 flex:none 不可收缩，窄视口动作行超出页头 17px；可收缩后才触发换行 */
  min-width: 0;
  flex-wrap: wrap; /* #16 窄视口动作行可换行（任务监控页双按钮 375px 溢出 3px），桌面单行零扰 */
  justify-content: flex-end;
}
</style>
