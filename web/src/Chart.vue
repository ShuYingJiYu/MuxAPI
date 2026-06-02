<script setup>
// Chart.js 薄封装：按 props 画图，数据变化时更新。配色走 pastel 风格，去网格线。
import { onMounted, onBeforeUnmount, ref, watch } from 'vue'
import {
  Chart, LineController, BarController, LineElement, BarElement,
  PointElement, LinearScale, CategoryScale, Tooltip, Filler,
} from 'chart.js'

Chart.register(LineController, BarController, LineElement, BarElement,
  PointElement, LinearScale, CategoryScale, Tooltip, Filler)

const props = defineProps({
  type: { type: String, default: 'line' }, // line | bar
  labels: { type: Array, default: () => [] },
  data: { type: Array, default: () => [] },
  color: { type: String, default: '#8b5cf6' },
  sparkline: Boolean,      // 迷你模式：隐藏坐标轴/图例
  fill: Boolean,           // 面积填充
  max: Number,             // y 轴上限（如成功率固定 1）
  fmt: { type: Function, default: v => v }, // tooltip 数值格式化
})

const el = ref(null)
let chart

function cfg() {
  const c = props.color
  const ds = {
    data: props.data,
    borderColor: c,
    backgroundColor: props.type === 'bar' ? c + '99' : (props.fill ? c + '22' : c),
    borderWidth: 2,
    borderRadius: props.type === 'bar' ? 6 : 0,
    pointRadius: 0,
    tension: 0.35,
    fill: props.fill,
  }
  return {
    type: props.type,
    data: { labels: props.labels, datasets: [ds] },
    options: {
      responsive: true, maintainAspectRatio: false,
      animation: { duration: 300 },
      plugins: {
        legend: { display: false },
        tooltip: {
          enabled: !props.sparkline,
          displayColors: false,
          callbacks: { label: ctx => props.fmt(ctx.parsed.y) },
        },
      },
      scales: {
        x: { display: !props.sparkline, grid: { display: false }, ticks: { display: false }, border: { display: false } },
        y: {
          display: !props.sparkline, beginAtZero: true, max: props.max,
          grid: { color: '#f1f1f4' }, border: { display: false },
          ticks: { font: { size: 10 }, color: '#9ca3af', maxTicksLimit: 4 },
        },
      },
    },
  }
}

onMounted(() => { chart = new Chart(el.value, cfg()) })
onBeforeUnmount(() => chart?.destroy())
watch(() => [props.labels, props.data], () => {
  if (!chart) return
  chart.data.labels = props.labels
  chart.data.datasets[0].data = props.data
  chart.update()
}, { deep: true })
</script>

<template>
  <div class="chart-box" :class="{ spark: sparkline }"><canvas ref="el" /></div>
</template>

<style scoped>
.chart-box { position: relative; height: 180px; }
.chart-box.spark { height: 40px; }
</style>
