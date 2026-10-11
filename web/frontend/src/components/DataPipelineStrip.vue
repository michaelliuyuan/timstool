<script setup lang="ts">
// Signature component: 源库 ──▶ TiDB flowing pipeline strip.
// status: running (teal) | warn (amber) | stopped (gray)
export interface PipelineBadge {
  label: string
  value: string
}

withDefaults(
  defineProps<{
    source?: string
    target?: string
    status?: 'running' | 'warn' | 'stopped'
    badges?: PipelineBadge[]
    mini?: boolean
  }>(),
  { source: 'PostgreSQL', target: 'TiDB', status: 'stopped', badges: () => [], mini: false },
)
</script>

<template>
  <div class="dps" :class="[`is-${status}`, { 'is-mini': mini }]">
    <div class="dps-node dps-src">
      <span class="dps-node-name">{{ source }}</span>
    </div>

    <div class="dps-flow">
      <div class="dps-line"></div>
      <span class="dps-arrow" aria-hidden="true">▶</span>
      <div v-if="!mini && badges.length" class="dps-badges">
        <span v-for="b in badges" :key="b.label" class="dps-badge">
          <span class="dps-badge-label">{{ b.label }}</span>
          <span class="dps-badge-value tims-mono">{{ b.value }}</span>
        </span>
      </div>
    </div>

    <div class="dps-node dps-dst">
      <span class="dps-node-name">{{ target }}</span>
    </div>
  </div>
</template>

<style scoped>
.dps {
  display: flex;
  align-items: center;
  gap: 10px;
  width: 100%;
  box-sizing: border-box; /* width:100% + padding/border must not overflow the page card grid */
  padding: 14px 16px;
  border-radius: var(--tims-radius);
  border: 1px solid var(--tims-border);
  background: var(--tims-card);
  box-shadow: var(--tims-shadow);
}

.dps-node {
  flex: none;
  padding: 8px 14px;
  border-radius: var(--tims-radius-s);
  font-weight: 600;
  font-size: var(--tims-font-sm);
  letter-spacing: 0.3px;
  white-space: nowrap;
}
.dps-src { background: var(--tims-ink); color: var(--tims-text-inv); }
.dps-dst { background: var(--tims-teal-soft); color: var(--tims-tag-success-text); border: 1px solid rgba(15, 163, 163, 0.35); } /* #t2 对比度二轮：仅改文字色 */

.dps-flow {
  position: relative;
  flex: 1;
  min-width: 60px;
  display: flex;
  flex-direction: column;
  align-items: center;
  gap: 6px;
}

.dps-line {
  width: 100%;
  height: 0;
  border-top: 2px dashed #c3c8d6;
}

/* The signature: flowing dashes + traveling arrow while running */
.dps.is-running .dps-line {
  border-color: var(--tims-teal);
  animation: dps-flow 1.1s linear infinite;
}
.dps.is-running .dps-arrow { color: var(--tims-teal); }
.dps.is-warn .dps-line { border-color: var(--tims-amber); animation: dps-flow 2.2s linear infinite; }
.dps.is-warn .dps-arrow { color: var(--tims-amber); }
.dps.is-stopped .dps-arrow { color: #b6bccb; }

@keyframes dps-flow {
  to { border-top-style: dashed; transform: translateX(8px); }
  from { transform: translateX(0); }
}

.dps-arrow {
  position: absolute;
  top: -9px;
  right: -2px;
  font-size: var(--tims-font-xs);
  line-height: 1;
  display: inline-flex;
  min-width: 1em; /* #t2 P2：▶ 字形占位不足致 375 瞬态溢出 */
}
.dps.is-running .dps-arrow { animation: dps-travel 2.4s linear infinite; }
@keyframes dps-travel {
  from { left: 4%; opacity: 0.2; }
  12% { opacity: 1; }
  88% { opacity: 1; }
  to { left: 96%; opacity: 0.2; }
}

.dps-badges {
  display: flex;
  flex-wrap: wrap;
  gap: 8px;
  justify-content: center;
}
.dps-badge {
  display: inline-flex;
  align-items: baseline;
  gap: 6px;
  padding: 2px 10px;
  border-radius: 999px;
  background: var(--tims-work);
  border: 1px solid var(--tims-border);
  font-size: var(--tims-font-xs);
}
.dps.is-running .dps-badge { border-color: rgba(15, 163, 163, 0.35); }
.dps-badge-label { color: var(--tims-text-2); }
.dps-badge-value { color: var(--tims-text); font-size: var(--tims-font-xs); }

/* Mini variant: topbar / compact cards */
.dps.is-mini { padding: 6px 8px; box-shadow: none; border: none; background: transparent; }
.dps.is-mini .dps-node { padding: 3px 8px; font-size: var(--tims-font-xs); }
.dps.is-mini .dps-line { border-top-width: 1.5px; }

@media (prefers-reduced-motion: reduce) {
  .dps-line, .dps-arrow { animation: none !important; }
}
</style>
