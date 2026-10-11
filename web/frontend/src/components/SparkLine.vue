<script setup lang="ts">
import { ref, watch, onUnmounted } from 'vue'
import * as echarts from 'echarts/core'
import { LineChart } from 'echarts/charts'
import { CanvasRenderer } from 'echarts/renderers'
import { GridComponent } from 'echarts/components'

echarts.use([LineChart, CanvasRenderer, GridComponent])

const props = withDefaults(
  defineProps<{
    data: number[]
    color?: string
    height?: number
  }>(),
  { color: '#0FA3A3', height: 46 },
)

const el = ref<HTMLElement | null>(null)
let chart: echarts.ECharts | null = null

function render() {
  if (!el.value) return
  if (!chart) chart = echarts.init(el.value)
  chart.setOption({
    grid: { left: 0, right: 0, top: 4, bottom: 0 },
    xAxis: { type: 'category', show: false, data: props.data.map((_, i) => i) },
    yAxis: { type: 'value', show: false, min: 'dataMin' },
    animation: false,
    series: [
      {
        type: 'line',
        data: props.data,
        showSymbol: false,
        lineStyle: { width: 1.6, color: props.color },
        areaStyle: {
          color: new echarts.graphic.LinearGradient(0, 0, 0, 1, [
            { offset: 0, color: props.color + '33' },
            { offset: 1, color: props.color + '00' },
          ]),
        },
      },
    ],
  })
}

watch(() => props.data, render, { deep: true })
watch(el, (v) => { if (v) render() })
onUnmounted(() => chart?.dispose())
</script>

<template>
  <div ref="el" class="tims-sparkline" :style="{ height: height + 'px' }"></div>
</template>

<style scoped>
.tims-sparkline { width: 100%; }
</style>
