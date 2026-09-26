<script setup lang="ts">
// S1-UI-06: shared "which one should I use?" comparison card shown on both
// sync pages (CDC real-time vs timestamp-watermark backfill), so each page
// states its own nature AND points at the other module. Collapsible to keep
// the first screen clean; module-specific boundary notes go in the slot.
defineProps<{ current: 'cdc' | 'watermark' }>()

import { ref } from 'vue'
const active = ref<string[]>([])
</script>

<template>
  <el-collapse v-model="active" class="sync-compare" style="margin-bottom: 16px;">
    <el-collapse-item name="cmp">
      <template #title>
        <span style="font-weight: 600;">该用哪个？CDC 实时同步 vs 时间戳水位补齐</span>
        <el-tag size="small" type="info" style="margin-left: 8px;">点开对照</el-tag>
      </template>
      <table class="cmp-table">
        <thead>
          <tr>
            <th style="width: 110px;"></th>
            <th :class="{ 'is-here': current === 'cdc' }">CDC 实时同步<span v-if="current === 'cdc'" class="here-mark"> ◀ 当前页</span></th>
            <th :class="{ 'is-here': current === 'watermark' }">时间戳水位补齐<span v-if="current === 'watermark'" class="here-mark"> ◀ 当前页</span></th>
          </tr>
        </thead>
        <tbody>
          <tr>
            <td class="cmp-key">实时性</td>
            <td>秒级持续推送，日志级跟随源端写入</td>
            <td>手动触发按轮执行（可由外部定时调度拉起）</td>
          </tr>
          <tr>
            <td class="cmp-key">捕获删除</td>
            <td>是——自动捕获 INSERT / UPDATE / DELETE 全量 DML</td>
            <td>否——只追水位列前进后的新行（详见下方边界说明）</td>
          </tr>
          <tr>
            <td class="cmp-key">源端要求</td>
            <td>逻辑复制 slot（wal_level=logical、复制权限）</td>
            <td>时间戳/整数水位列（建议建索引，否则可能全表扫描）</td>
          </tr>
          <tr>
            <td class="cmp-key">运维形态</td>
            <td>常驻进程，需持续运行与监控</td>
            <td>手动 Job，跑完即止、可重复执行（幂等）</td>
          </tr>
        </tbody>
      </table>
      <p class="cmp-foot">
        两者在左侧导航「数据同步」组下各占一页，可按表组合使用（如 CDC 负责实时链路、水位补齐兜底核对/补数）。
      </p>
      <slot />
    </el-collapse-item>
  </el-collapse>
</template>

<style scoped>
.cmp-table {
  width: 100%;
  border-collapse: collapse;
  font-size: var(--tims-font-sm);
  margin-bottom: 10px;
}
.cmp-table th,
.cmp-table td {
  border: 1px solid var(--tims-border);
  padding: 7px 10px;
  text-align: left;
  vertical-align: top;
  line-height: 1.55;
}
.cmp-table th {
  background: var(--tims-work);
  font-weight: 600;
}
.cmp-table th.is-here {
  color: var(--tims-brand);
}
.here-mark {
  font-weight: 400;
  font-size: 12px;
}
.cmp-key {
  color: var(--tims-text-2);
  white-space: nowrap;
}
.cmp-foot {
  font-size: var(--tims-font-sm);
  color: var(--tims-text-2);
  margin: 0 0 4px;
}
</style>
