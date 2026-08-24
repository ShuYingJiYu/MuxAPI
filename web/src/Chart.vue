<script setup>
// Chart.js 薄封装：按 props 画图，数据变化时更新。配色走 pastel 风格，去网格线。
import { computed, onMounted, onBeforeUnmount, ref, watch } from 'vue'
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
  datasets: { type: Array, default: null },
  color: { type: String, default: '#8b5cf6' },
  sparkline: Boolean,      // 迷你模式：隐藏坐标轴/图例
  fill: Boolean,           // 面积填充
  max: Number,             // y 轴上限（如成功率固定 1）
  axisLabels: Boolean,     // 显示横轴标签；迷你图默认关闭
  showLegend: Boolean,     // 多线图显示可滚动图例
  fmt: { type: Function, default: v => v }, // tooltip 数值格式化
})

const el = ref(null)
let chart

const legendItems = computed(() => props.datasets?.length ? props.datasets : [])

function yBounds(source) {
  if (props.max != null) return { min: 0, max: props.max }
  const values = source.flatMap(item => item.data || [])
    .filter(value => value != null)
    .map(Number)
    .filter(value => Number.isFinite(value))
  if (!values.length) return { min: undefined, max: undefined }

  const dataMin = Math.min(...values)
  const dataMax = Math.max(...values)
  const span = Math.max(dataMax - dataMin, Math.abs(dataMin), Math.abs(dataMax), 1)
  const padding = span * 0.08
  return {
    min: dataMin < 0 ? dataMin - padding : 0,
    max: dataMax > 0 ? dataMax + padding : 0,
  }
}

function cfg() {
  const c = props.color
  const source = props.datasets?.length ? props.datasets : [{ data: props.data, color: c }]
  const bounds = yBounds(source)
  const datasets = source.map((item, index) => {
    const color = item.color || c
    const fill = item.fill ?? props.fill
    return {
      label: item.label || '',
      data: item.data || [],
      borderColor: color,
      backgroundColor: props.type === 'bar' ? color + '99' : (fill ? color + '22' : color),
      borderWidth: item.borderWidth ?? (source.length > 1 ? 1.6 : 2),
      borderRadius: props.type === 'bar' ? 6 : 0,
      pointRadius: item.pointRadius ?? 0,
      pointHoverRadius: item.pointHoverRadius ?? (source.length > 1 ? 4 : 3),
      tension: item.tension ?? 0.35,
      fill: item.fill ?? props.fill,
      spanGaps: true,
      order: index,
    }
  })
  return {
    type: props.type,
    data: { labels: props.labels, datasets },
    options: {
      responsive: true, maintainAspectRatio: false,
      animation: { duration: 300 },
      interaction: { mode: source.length > 1 ? 'index' : 'nearest', intersect: false },
      plugins: {
        legend: { display: false },
        tooltip: {
          enabled: !props.sparkline,
          displayColors: source.length > 1,
          callbacks: {
            title: items => items.length ? items[0].label : '',
            label: ctx => {
              const value = props.fmt(ctx.parsed.y)
              return source.length > 1 && ctx.dataset.label ? `${ctx.dataset.label}: ${value}` : value
            },
          },
        },
      },
      scales: {
        x: { display: !props.sparkline, grid: { display: false }, ticks: { display: props.axisLabels && !props.sparkline, color: '#9ca3af', font: { size: 10 }, maxTicksLimit: 6, maxRotation: 0 }, border: { display: false } },
        y: {
          display: !props.sparkline, beginAtZero: false, min: bounds.min, max: bounds.max,
          grid: { color: '#f1f1f4' }, border: { display: false },
          ticks: { font: { size: 10 }, color: '#9ca3af', maxTicksLimit: 4 },
        },
      },
    },
  }
}

onMounted(() => { chart = new Chart(el.value, cfg()) })
onBeforeUnmount(() => chart?.destroy())
// 数据变化时复用 Chart 实例，避免轮询刷新导致 canvas 和监听器重复创建。
watch(() => [props.labels, props.data, props.datasets, props.max], () => {
  if (!chart) return
  chart.data.labels = props.labels
  const source = props.datasets?.length ? props.datasets : [{ data: props.data }]
  if (chart.data.datasets.length !== source.length) {
    chart.destroy()
    chart = new Chart(el.value, cfg())
    return
  }
  source.forEach((item, index) => {
    if (chart.data.datasets[index]) {
      chart.data.datasets[index].data = item.data || []
      if (item.label != null) chart.data.datasets[index].label = item.label
    }
  })
  const bounds = yBounds(source)
  chart.options.scales.y.min = bounds.min
  chart.options.scales.y.max = bounds.max
  chart.update()
}, { deep: true })
</script>

<template>
  <div class="chart-box" :class="{ spark: sparkline }"><canvas ref="el" /></div>
  <div v-if="showLegend && legendItems.length" class="chart-legend" role="list">
    <span v-for="item in legendItems" :key="item.label" class="chart-legend-item" role="listitem" :title="item.label">
      <i class="chart-legend-dot" :style="{ backgroundColor: item.color || color }"></i>
      <span>{{ item.label }}</span>
    </span>
  </div>
</template>

<style scoped>
.chart-box { position: relative; height: 180px; }
.chart-box.spark { height: 40px; }
.chart-legend {
  display: flex;
  flex-wrap: wrap;
  gap: 5px 12px;
  max-height: 58px;
  overflow-y: auto;
  padding: 7px 2px 0;
  scrollbar-width: thin;
}
.chart-legend-item {
  display: inline-flex;
  align-items: center;
  gap: 5px;
  min-width: 0;
  max-width: 100%;
  color: #756d7c;
  font-size: 11px;
  line-height: 16px;
  white-space: nowrap;
}
.chart-legend-item span { overflow: hidden; text-overflow: ellipsis; }
.chart-legend-dot { width: 9px; height: 9px; flex: 0 0 9px; border-radius: 2px; }
</style>
