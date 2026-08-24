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
let externalTooltipEl
let externalTooltipPanel
let tooltipFrame
let tooltipHideTimer
let tooltipHovered = false
let tooltipScrollFrame
let tooltipScrollTarget = 0
let tooltipContentKey = ''
let canvasPointerX
let canvasPointerY
let tooltipCaretX
let tooltipCaretY
let tooltipPositionX = 0
let tooltipPositionY = 0
let tooltipPositionReady = false

const legendItems = computed(() => props.datasets?.length ? props.datasets : [])
const legendGroups = computed(() => {
  const groups = new Map()
  for (const item of legendItems.value) {
    const name = item.group || '未设置主标签'
    if (!groups.has(name)) groups.set(name, { name, color: item.groupColor || 'gray', items: [] })
    groups.get(name).items.push(item)
  }
  return [...groups.values()]
})

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

function hideExternalTooltip() {
  if (externalTooltipEl) externalTooltipEl.hidden = true
  tooltipHovered = false
  tooltipContentKey = ''
  tooltipScrollTarget = 0
  tooltipPositionReady = false
  cancelAnimationFrame(tooltipScrollFrame)
  tooltipScrollFrame = undefined
}

function scheduleHideExternalTooltip() {
  clearTimeout(tooltipHideTimer)
  tooltipHideTimer = setTimeout(() => {
    if (!tooltipHovered) hideExternalTooltip()
  }, 260)
}

function ensureExternalTooltip(enabled) {
  if (enabled && !externalTooltipEl) {
    externalTooltipEl = document.createElement('div')
    externalTooltipEl.className = 'chart-tooltip'
    externalTooltipEl.hidden = true
    externalTooltipPanel = document.createElement('div')
    externalTooltipPanel.className = 'chart-tooltip-panel'
    externalTooltipEl.append(externalTooltipPanel)
    document.addEventListener('pointermove', handleDocumentPointerMove, { passive: true })
    window.addEventListener('wheel', handleDocumentWheel, { passive: false })
    document.body.append(externalTooltipEl)
  } else if (!enabled && externalTooltipEl) {
    cancelAnimationFrame(tooltipScrollFrame)
    tooltipScrollFrame = undefined
    tooltipScrollTarget = 0
    tooltipContentKey = ''
    tooltipHovered = false
    tooltipPositionReady = false
    document.removeEventListener('pointermove', handleDocumentPointerMove)
    window.removeEventListener('wheel', handleDocumentWheel)
    externalTooltipEl.remove()
    externalTooltipEl = null
    externalTooltipPanel = null
  }
}

function animateTooltipScroll() {
  if (!externalTooltipPanel) return
  const current = externalTooltipPanel.scrollTop
  const distance = tooltipScrollTarget - current
  if (Math.abs(distance) < 0.35) {
    externalTooltipPanel.scrollTop = tooltipScrollTarget
    tooltipScrollFrame = undefined
    return
  }
  externalTooltipPanel.scrollTop = current + distance * 0.24
  tooltipScrollFrame = requestAnimationFrame(animateTooltipScroll)
}

function handleTooltipWheel(event) {
  if (!externalTooltipPanel || !event.deltaY) return
  const maxScroll = externalTooltipPanel.scrollHeight - externalTooltipPanel.clientHeight
  if (maxScroll <= 0) return

  const unit = event.deltaMode === 1 ? 16 : event.deltaMode === 2 ? externalTooltipPanel.clientHeight : 1
  const current = externalTooltipPanel.scrollTop
  const base = tooltipScrollFrame === undefined ? current : tooltipScrollTarget
  const target = Math.max(0, Math.min(maxScroll, base + event.deltaY * unit))
  if (target === base) return

  event.preventDefault()
  event.stopPropagation()
  tooltipScrollTarget = target
  if (tooltipScrollFrame === undefined) tooltipScrollFrame = requestAnimationFrame(animateTooltipScroll)
}

function handleCanvasPointerMove(event) {
  canvasPointerX = event.clientX
  canvasPointerY = event.clientY
  scheduleTooltipPosition()
}

function handleDocumentPointerMove(event) {
  if (!externalTooltipEl || externalTooltipEl.hidden) return
  canvasPointerX = event.clientX
  canvasPointerY = event.clientY
  const canvasRect = el.value?.getBoundingClientRect()
  const tooltipRect = externalTooltipEl.getBoundingClientRect()
  const inCanvas = canvasRect
    && event.clientX >= canvasRect.left && event.clientX <= canvasRect.right
    && event.clientY >= canvasRect.top && event.clientY <= canvasRect.bottom
  const inTooltip = event.clientX >= tooltipRect.left && event.clientX <= tooltipRect.right
    && event.clientY >= tooltipRect.top && event.clientY <= tooltipRect.bottom

  if (inCanvas) {
    tooltipHovered = false
    clearTimeout(tooltipHideTimer)
    scheduleTooltipPosition()
  } else if (inTooltip) {
    tooltipHovered = true
    clearTimeout(tooltipHideTimer)
  } else if (tooltipHovered) {
    tooltipHovered = false
    scheduleHideExternalTooltip()
  }
}

function handleDocumentWheel(event) {
  if (!externalTooltipEl || externalTooltipEl.hidden || !externalTooltipPanel) return
  const rect = externalTooltipEl.getBoundingClientRect()
  const inTooltip = event.clientX >= rect.left && event.clientX <= rect.right
    && event.clientY >= rect.top && event.clientY <= rect.bottom
  if (inTooltip) handleTooltipWheel(event)
}

function getTooltipPositionTarget() {
  if (!externalTooltipEl || externalTooltipEl.hidden) return null
  const tooltipRect = externalTooltipEl.getBoundingClientRect()
  const pointerX = Number.isFinite(canvasPointerX) ? canvasPointerX : tooltipCaretX
  const pointerY = Number.isFinite(canvasPointerY) ? canvasPointerY : tooltipCaretY
  if (!Number.isFinite(pointerX) || !Number.isFinite(pointerY)) return null

  const gap = 2
  const leftFloor = gap
  const leftCeil = Math.max(leftFloor, window.innerWidth - tooltipRect.width - gap)
  let left = pointerX + gap
  if (left > leftCeil) left = pointerX - tooltipRect.width - gap
  left = Math.max(leftFloor, Math.min(left, leftCeil))
  const topFloor = gap
  const topCeil = Math.max(topFloor, window.innerHeight - tooltipRect.height - gap)
  let top = pointerY + gap
  if (top > topCeil) top = pointerY - tooltipRect.height - gap
  top = Math.max(topFloor, Math.min(top, topCeil))
  return { left, top }
}

function animateTooltipPosition() {
  tooltipFrame = undefined
  if (!externalTooltipEl || externalTooltipEl.hidden) return
  if (tooltipHovered) return
  const target = getTooltipPositionTarget()
  if (!target) return

  if (!tooltipPositionReady) {
    tooltipPositionX = target.left
    tooltipPositionY = target.top
    tooltipPositionReady = true
  } else {
    const ease = 0.42
    const maxStep = 32
    const move = (current, destination) => {
      const delta = (destination - current) * ease
      if (Math.abs(delta) <= maxStep) return current + delta
      return current + Math.sign(delta) * maxStep
    }
    tooltipPositionX = move(tooltipPositionX, target.left)
    tooltipPositionY = move(tooltipPositionY, target.top)
  }
  externalTooltipEl.style.transform = `translate3d(${tooltipPositionX}px, ${tooltipPositionY}px, 0)`

  const distance = Math.hypot(target.left - tooltipPositionX, target.top - tooltipPositionY)
  if (distance > 0.25) tooltipFrame = requestAnimationFrame(animateTooltipPosition)
}

function scheduleTooltipPosition() {
  if (!externalTooltipEl || externalTooltipEl.hidden || tooltipHovered) return
  if (tooltipFrame === undefined) tooltipFrame = requestAnimationFrame(animateTooltipPosition)
}

// 多线图使用画布外的 HTML 悬浮层，避免数据较多时被 canvas 边界截断。
function renderExternalTooltip({ chart: activeChart, tooltip }) {
  if (!externalTooltipEl || !externalTooltipPanel) return
  if (!tooltip || tooltip.opacity === 0 || !tooltip.dataPoints?.length) {
    scheduleHideExternalTooltip()
    return
  }
  clearTimeout(tooltipHideTimer)
  const canvasRect = activeChart.canvas.getBoundingClientRect()
  tooltipCaretX = canvasRect.left + tooltip.caretX
  tooltipCaretY = canvasRect.top + tooltip.caretY

  const contentKey = [tooltip.title?.[0] || '', ...tooltip.dataPoints.map(point => [
    point.datasetIndex,
    point.dataIndex,
    point.dataset?.label || '',
    point.dataset?.group || '',
    props.fmt(point.parsed?.y),
  ].join(':'))].join('|')
  if (contentKey !== tooltipContentKey) {
    tooltipContentKey = contentKey
    externalTooltipPanel.replaceChildren()
    const title = document.createElement('div')
    title.className = 'chart-tooltip-title'
    title.textContent = tooltip.title?.[0] || ''
    externalTooltipPanel.append(title)

    const groups = new Map()
    for (const point of tooltip.dataPoints) {
      const name = point.dataset?.group || '未设置主标签'
      if (!groups.has(name)) groups.set(name, { name, color: point.dataset?.groupColor || 'gray', points: [] })
      groups.get(name).points.push(point)
    }
    const groupList = document.createElement('div')
    groupList.className = 'chart-tooltip-groups'
    for (const group of groups.values()) {
      const groupBox = document.createElement('section')
      groupBox.className = 'chart-tooltip-group'
      const groupTitle = document.createElement('div')
      groupTitle.className = 'chart-tooltip-group-title'
      const groupDot = document.createElement('i')
      groupDot.className = `chart-tooltip-group-dot tag-color-dot tag-${group.color}`
      const groupLabel = document.createElement('span')
      groupLabel.textContent = `${group.name} · ${group.points.length}`
      groupTitle.append(groupDot, groupLabel)
      const list = document.createElement('div')
      list.className = 'chart-tooltip-list'
      for (const point of group.points) {
        const row = document.createElement('div')
        row.className = 'chart-tooltip-row'
        const swatch = document.createElement('i')
        swatch.className = 'chart-tooltip-swatch'
        swatch.style.backgroundColor = point.dataset?.borderColor || props.color
        const text = document.createElement('span')
        text.textContent = `${point.dataset?.label || ''}: ${props.fmt(point.parsed?.y)}`
        row.append(swatch, text)
        list.append(row)
      }
      groupBox.append(groupTitle, list)
      groupList.append(groupBox)
    }
    externalTooltipPanel.append(groupList)
    externalTooltipPanel.scrollTop = 0
    tooltipScrollTarget = 0
  }
  externalTooltipEl.hidden = false
  scheduleTooltipPosition()
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
      group: item.group || '未设置主标签',
      groupColor: item.groupColor || 'gray',
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
          enabled: !props.sparkline && source.length <= 1,
          external: source.length > 1 && !props.sparkline ? renderExternalTooltip : undefined,
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

onMounted(() => {
  ensureExternalTooltip(props.datasets?.length > 1 && !props.sparkline)
  el.value.addEventListener('pointermove', handleCanvasPointerMove, { passive: true })
  chart = new Chart(el.value, cfg())
})
onBeforeUnmount(() => {
  chart?.destroy()
  el.value?.removeEventListener('pointermove', handleCanvasPointerMove)
  cancelAnimationFrame(tooltipFrame)
  cancelAnimationFrame(tooltipScrollFrame)
  clearTimeout(tooltipHideTimer)
  externalTooltipPanel?.removeEventListener('wheel', handleTooltipWheel)
  document.removeEventListener('pointermove', handleDocumentPointerMove)
  window.removeEventListener('wheel', handleDocumentWheel)
  externalTooltipEl?.remove()
  externalTooltipEl = null
  externalTooltipPanel = null
  canvasPointerX = undefined
  canvasPointerY = undefined
  tooltipCaretX = undefined
  tooltipCaretY = undefined
})
// 数据变化时复用 Chart 实例，避免轮询刷新导致 canvas 和监听器重复创建。
watch(() => [props.labels, props.data, props.datasets, props.max], () => {
  if (!chart) return
  chart.data.labels = props.labels
  const source = props.datasets?.length ? props.datasets : [{ data: props.data }]
  if (chart.data.datasets.length !== source.length) {
    chart.destroy()
    ensureExternalTooltip(source.length > 1 && !props.sparkline)
    chart = new Chart(el.value, cfg())
    return
  }
  source.forEach((item, index) => {
    if (chart.data.datasets[index]) {
      chart.data.datasets[index].data = item.data || []
      if (item.label != null) chart.data.datasets[index].label = item.label
      chart.data.datasets[index].group = item.group || '未设置主标签'
      chart.data.datasets[index].groupColor = item.groupColor || 'gray'
    }
  })
  const bounds = yBounds(source)
  chart.options.scales.y.min = bounds.min
  chart.options.scales.y.max = bounds.max
  chart.update()
}, { deep: true })
</script>

<template>
  <div class="chart-shell">
    <div class="chart-box" :class="{ spark: sparkline }"><canvas ref="el" /></div>
    <div v-if="showLegend && legendItems.length" class="chart-legend" role="list">
      <section v-for="group in legendGroups" :key="group.name" class="chart-legend-group">
        <div class="chart-legend-group-title">
          <i class="chart-legend-group-dot tag-color-dot" :class="`tag-${group.color}`"></i>
          <strong>{{ group.name }}</strong><small>{{ group.items.length }}</small>
        </div>
        <div class="chart-legend-items">
          <span v-for="item in group.items" :key="`${group.name}-${item.label}`" class="chart-legend-item" role="listitem" :title="item.label">
            <i class="chart-legend-dot" :style="{ backgroundColor: item.color || color }"></i>
            <span>{{ item.label }}</span>
          </span>
        </div>
      </section>
    </div>
  </div>
</template>

<style scoped>
.chart-shell { position: relative; width: 100%; }
.chart-box { position: relative; height: 180px; }
.chart-box.spark { height: 40px; }
.chart-legend {
  display: flex;
  flex-wrap: wrap;
  gap: 5px 12px;
  max-height: 60px;
  overflow-y: auto;
  padding: 7px 5px 0 2px;
  box-sizing: border-box;
  scrollbar-width: none;
}
.chart-legend::-webkit-scrollbar { width: 0; height: 0; }
.chart-legend-group + .chart-legend-group { margin-top: 6px; }
.chart-legend-group-title { display: flex; align-items: center; gap: 6px; min-height: 20px; padding: 2px 6px; border: 1px solid rgba(190,168,96,.25); border-radius: 5px; background: rgba(255,255,255,.72); color: #5d5362; font-size: 11px; }
.chart-legend-group-title strong { font-weight: 800; }
.chart-legend-group-title small { margin-left: auto; padding: 1px 5px; border-radius: 9px; background: #fff1bf; color: #8a6312; font-size: 9px; font-weight: 800; }
.chart-legend-group-dot { width: 8px; height: 8px; flex: 0 0 8px; }
.chart-legend-items { display: flex; flex-wrap: wrap; gap: 4px 12px; margin: 3px 0 0 5px; padding-left: 9px; border-left: 2px solid rgba(190,168,96,.18); }
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
:global(.chart-tooltip) {
  position: fixed;
  left: 0;
  top: 0;
  z-index: 120;
  width: min(458px, calc(100vw - 4px));
  box-sizing: border-box;
  padding: 14px;
  pointer-events: none;
  will-change: transform;
}
:global(.chart-tooltip-panel) {
  max-height: min(260px, calc(100vh - 32px));
  overflow: auto;
  box-sizing: border-box;
  padding: 8px 10px;
  border: 1px solid rgba(190,168,96,.38);
  border-radius: 7px;
  background: rgba(255,253,239,.96);
  backdrop-filter: blur(14px) saturate(115%);
  box-shadow: 0 8px 24px rgba(120,95,30,.16);
  color: #5d5060;
  font-size: 11px;
  line-height: 1.35;
  overscroll-behavior: contain;
  scroll-behavior: auto;
  scrollbar-width: none;
}
:global(.chart-tooltip-panel::-webkit-scrollbar) { width: 0; height: 0; }
:global(.chart-tooltip-title) { padding-bottom: 5px; color: #2a2330; font-weight: 800; }
:global(.chart-tooltip-group + .chart-tooltip-group) { margin-top: 7px; }
:global(.chart-tooltip-group-title) { display: flex; align-items: center; gap: 6px; min-height: 21px; padding: 2px 6px; border-left: 3px solid rgba(190,168,96,.65); border-radius: 3px; background: rgba(255,241,191,.76); color: #5d5060; font-size: 11px; font-weight: 800; }
:global(.chart-tooltip-group-title span) { overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
:global(.chart-tooltip-group-dot) { width: 8px; height: 8px; flex: 0 0 8px; }
:global(.chart-tooltip-list) { display: grid; grid-template-columns: repeat(2, minmax(0, 1fr)); gap: 3px 12px; margin: 3px 0 0 5px; padding-left: 8px; border-left: 2px solid rgba(190,168,96,.22); }
:global(.chart-tooltip-row) { display: flex; align-items: center; min-width: 0; gap: 5px; }
:global(.chart-tooltip-row span) { min-width: 0; overflow: hidden; color: #756d7c; text-overflow: ellipsis; white-space: nowrap; }
:global(.chart-tooltip-swatch) { width: 9px; height: 9px; flex: 0 0 9px; border-radius: 2px; }
</style>
