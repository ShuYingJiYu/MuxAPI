<script setup>
// Chart.js 薄封装：按 props 画图，数据变化时更新。配色走 pastel 风格，去网格线。
import { computed, onMounted, onBeforeUnmount, ref, watch } from 'vue'
import {
  Chart, LineController, BarController, LineElement, BarElement,
  PointElement, LinearScale, CategoryScale, Tooltip, Filler,
} from 'chart.js'
import { canvasColor, colorWithAlpha, themeValue } from './theme.js'

const thresholdLinePlugin = {
  id: 'thresholdLine',
  afterDraw(activeChart, _args, options) {
    const value = Number(options?.value)
    const yScale = activeChart.scales?.y
    if (!Number.isFinite(value) || !yScale) return
    const y = yScale.getPixelForValue(value)
    const { left, right, top, bottom } = activeChart.chartArea
    if (y < top || y > bottom) return

    const { ctx } = activeChart
    ctx.save()
    ctx.strokeStyle = options.color || themeValue('--chart-threshold')
    ctx.lineWidth = 1
    ctx.setLineDash([4, 4])
    ctx.beginPath()
    ctx.moveTo(left, y)
    ctx.lineTo(right, y)
    ctx.stroke()
    if (options.label) {
      ctx.fillStyle = options.textColor || themeValue('--chart-threshold-text')
      ctx.font = `600 10px ${themeValue('--font-body', 'sans-serif')}`
      ctx.textAlign = 'right'
      ctx.textBaseline = 'bottom'
      ctx.fillText(options.label, right, Math.max(top + 10, y - 4))
    }
    ctx.restore()
  },
}

function fitCanvasText(ctx, text, maxWidth) {
  const source = String(text || '')
  if (ctx.measureText(source).width <= maxWidth) return source
  let result = source
  while (result.length > 1 && ctx.measureText(`${result}…`).width > maxWidth) result = result.slice(0, -1)
  return `${result}…`
}

function roundedRectPath(ctx, x, y, width, height, radius) {
  const right = x + width
  const bottom = y + height
  ctx.beginPath()
  ctx.moveTo(x + radius, y)
  ctx.lineTo(right - radius, y)
  ctx.quadraticCurveTo(right, y, right, y + radius)
  ctx.lineTo(right, bottom - radius)
  ctx.quadraticCurveTo(right, bottom, right - radius, bottom)
  ctx.lineTo(x + radius, bottom)
  ctx.quadraticCurveTo(x, bottom, x, bottom - radius)
  ctx.lineTo(x, y + radius)
  ctx.quadraticCurveTo(x, y, x + radius, y)
  ctx.closePath()
}

function rectanglesOverlap(a, b) {
  return a.x < b.x + b.width + 4 && a.x + a.width + 4 > b.x
    && a.y < b.y + b.height + 4 && a.y + a.height + 4 > b.y
}

const inlineLabelsPlugin = {
  id: 'inlineLabels',
  afterDatasetsDraw(activeChart, _args, options) {
    if (!options?.display || activeChart.config.type !== 'line') return
    const { ctx, chartArea } = activeChart
    const occupied = []
    const preferredFractions = [0.68, 0.56, 0.78, 0.46, 0.86]

    ctx.save()
    ctx.font = `600 9px ${themeValue('--font-body', 'sans-serif')}`
    ctx.textAlign = 'left'
    ctx.textBaseline = 'middle'
    activeChart.data.datasets.forEach((dataset, datasetIndex) => {
      const meta = activeChart.getDatasetMeta(datasetIndex)
      if (meta.hidden || !dataset.label) return
      const validIndexes = (dataset.data || []).flatMap((value, index) => {
        const point = meta.data[index]
        return value != null && Number.isFinite(Number(value)) && point && Number.isFinite(point.x) && Number.isFinite(point.y)
          ? [index]
          : []
      })
      if (!validIndexes.length) return

      const label = fitCanvasText(ctx, dataset.lineLabel || dataset.label, options.maxWidth || 72)
      const width = Math.ceil(ctx.measureText(label).width) + 15
      const height = 17
      const fractions = preferredFractions.map((_, index) => preferredFractions[(index + datasetIndex) % preferredFractions.length])
      let placement
      for (const fraction of fractions) {
        const pointIndex = validIndexes[Math.round((validIndexes.length - 1) * fraction)]
        const point = meta.data[pointIndex]
        for (const direction of [-1, 1]) {
          const x = Math.max(chartArea.left + 2, Math.min(point.x - width / 2, chartArea.right - width - 2))
          const y = direction < 0 ? point.y - height - 8 : point.y + 8
          const rect = { x, y, width, height }
          const inside = rect.y >= chartArea.top + 2 && rect.y + rect.height <= chartArea.bottom - 2
          if (inside && !occupied.some(item => rectanglesOverlap(rect, item))) {
            placement = { point, rect, direction, label }
            break
          }
        }
        if (placement) break
      }
      if (!placement) return
      occupied.push(placement.rect)

      const color = dataset.borderColor || themeValue('--chart-axis')
      const connectorY = placement.direction < 0 ? placement.rect.y + placement.rect.height : placement.rect.y
      ctx.strokeStyle = color
      ctx.lineWidth = 1
      ctx.setLineDash([])
      ctx.globalAlpha = 0.68
      ctx.beginPath()
      ctx.moveTo(placement.point.x, placement.point.y)
      ctx.lineTo(placement.point.x, connectorY)
      ctx.stroke()
      ctx.globalAlpha = 1
      ctx.fillStyle = color
      ctx.beginPath()
      ctx.arc(placement.point.x, placement.point.y, 2.5, 0, Math.PI * 2)
      ctx.fill()

      roundedRectPath(ctx, placement.rect.x, placement.rect.y, placement.rect.width, placement.rect.height, 4)
      ctx.fillStyle = themeValue('--chart-label-bg')
      ctx.fill()
      ctx.strokeStyle = color
      ctx.globalAlpha = 0.72
      ctx.stroke()
      ctx.globalAlpha = 1
      ctx.fillStyle = color
      ctx.fillRect(placement.rect.x + 5, placement.rect.y + 5, 2, 7)
      ctx.fillStyle = themeValue('--g600')
      ctx.fillText(placement.label, placement.rect.x + 10, placement.rect.y + placement.rect.height / 2)
    })
    ctx.restore()
  },
}

Chart.register(LineController, BarController, LineElement, BarElement,
  PointElement, LinearScale, CategoryScale, Tooltip, Filler, thresholdLinePlugin, inlineLabelsPlugin)

const props = defineProps({
  type: { type: String, default: 'line' }, // line | bar
  labels: { type: Array, default: () => [] },
  data: { type: Array, default: () => [] },
  datasets: { type: Array, default: null },
  color: { type: String, default: 'var(--chart-primary)' },
  sparkline: Boolean,      // 迷你模式：隐藏坐标轴/图例
  fill: Boolean,           // 面积填充
  min: Number,             // y 轴下限
  max: Number,             // y 轴上限（如成功率固定 1）
  threshold: Number,       // 水平参考线
  thresholdLabel: String,
  lineLabels: Boolean,     // 在折线中段直接标注数据集名称
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
  if (props.min != null || props.max != null) return { min: props.min ?? 0, max: props.max }
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
  const c = canvasColor(props.color)
  const source = props.datasets?.length ? props.datasets : [{ data: props.data, color: c }]
  const bounds = yBounds(source)
  const datasets = source.map((item, index) => {
    const color = canvasColor(item.color || c)
    const fill = item.fill ?? props.fill
    return {
      label: item.label || '',
      lineLabel: item.lineLabel || item.label || '',
      group: item.group || '未设置主标签',
      groupColor: item.groupColor || 'gray',
      data: item.data || [],
      borderColor: color,
      backgroundColor: props.type === 'bar' ? colorWithAlpha(color, .6) : (fill ? colorWithAlpha(color, .13) : color),
      borderWidth: item.borderWidth ?? (source.length > 1 ? 1.6 : 2),
      borderRadius: props.type === 'bar' ? 6 : 0,
      pointRadius: item.pointRadius ?? 0,
      pointHoverRadius: item.pointHoverRadius ?? (source.length > 1 ? 4 : 3),
      pointBackgroundColor: canvasColor(item.pointBackgroundColor ?? color),
      pointBorderColor: canvasColor(item.pointBorderColor ?? color),
      pointBorderWidth: item.pointBorderWidth ?? 1,
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
        thresholdLine: {
          value: props.threshold,
          label: props.thresholdLabel,
          color: themeValue('--chart-threshold'),
          textColor: themeValue('--chart-threshold-text'),
        },
        inlineLabels: {
          display: props.lineLabels,
          maxWidth: 72,
        },
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
        x: { display: !props.sparkline, grid: { display: false }, ticks: { display: props.axisLabels && !props.sparkline, color: themeValue('--chart-axis'), font: { size: 10 }, maxTicksLimit: 6, maxRotation: 0 }, border: { display: false } },
        y: {
          display: !props.sparkline, beginAtZero: false, min: bounds.min, max: bounds.max,
          grid: { color: themeValue('--chart-grid') }, border: { display: false },
          ticks: { font: { size: 10 }, color: themeValue('--chart-axis'), maxTicksLimit: 4 },
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
watch(() => [props.labels, props.data, props.datasets, props.min, props.max, props.threshold, props.thresholdLabel, props.lineLabels], () => {
  if (!chart) return
  const source = props.datasets?.length ? props.datasets : [{ data: props.data }]
  const next = cfg()
  chart.data.labels = props.labels
  if (chart.data.datasets.length !== source.length) {
    chart.destroy()
    ensureExternalTooltip(source.length > 1 && !props.sparkline)
    chart = new Chart(el.value, cfg())
    return
  }
  next.data.datasets.forEach((item, index) => {
    if (chart.data.datasets[index]) {
      Object.assign(chart.data.datasets[index], item)
    }
  })
  chart.options.scales.y.min = next.options.scales.y.min
  chart.options.scales.y.max = next.options.scales.y.max
  chart.options.plugins.thresholdLine = next.options.plugins.thresholdLine
  chart.options.plugins.inlineLabels = next.options.plugins.inlineLabels
  chart.update()
}, { deep: true })
</script>

<template>
  <div class="chart-shell">
    <div class="chart-box" :class="{ spark: sparkline }"><canvas ref="el" /></div>
    <div v-if="showLegend && legendItems.length" class="chart-legend" role="list" aria-label="标签汇总">
      <section v-for="group in legendGroups" :key="group.name" class="chart-legend-group">
        <div class="chart-legend-group-title">
          <i class="chart-legend-group-dot tag-color-dot" :class="`tag-${group.color}`"></i>
          <span><strong>{{ group.name }}</strong><small>{{ group.items.length }} 项</small></span>
        </div>
        <div class="chart-legend-items">
          <span v-for="item in group.items" :key="`${group.name}-${item.label}`" class="chart-legend-item" role="listitem" :title="item.label">
            <i class="chart-legend-dot" :style="{ backgroundColor: item.color || color }"></i>
            <span class="chart-legend-copy">
              <strong>{{ item.legendLabel || item.label }}</strong>
              <small v-if="item.legendMeta">{{ item.legendMeta }}</small>
            </span>
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
  max-height: 94px;
  overflow-y: auto;
  margin-top: 7px;
  padding: 9px 2px 0;
  border-top: 1px solid var(--line);
  box-sizing: border-box;
  scrollbar-width: none;
}
.chart-legend::-webkit-scrollbar { width: 0; height: 0; }
.chart-legend-group { display: grid; grid-template-columns: minmax(82px, 108px) minmax(0, 1fr); align-items: start; gap: 12px; width: 100%; }
.chart-legend-group + .chart-legend-group { margin-top: 8px; padding-top: 8px; border-top: 1px solid var(--line); }
.chart-legend-group-title { display: flex; align-items: flex-start; gap: 7px; min-width: 0; padding-top: 3px; color: var(--g600); }
.chart-legend-group-title > span { min-width: 0; }
.chart-legend-group-title strong, .chart-legend-group-title small { display: block; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
.chart-legend-group-title strong { font-size: 11px; font-weight: 700; }
.chart-legend-group-title small { margin-top: 1px; color: var(--g400); font-size: 9px; font-weight: 500; }
.chart-legend-group-dot { width: 8px; height: 8px; flex: 0 0 8px; }
.chart-legend-items { display: grid; grid-template-columns: repeat(auto-fit, minmax(112px, 1fr)); gap: 3px 14px; min-width: 0; }
.chart-legend-item {
  display: grid;
  grid-template-columns: 15px minmax(0, 1fr);
  align-items: center;
  gap: 5px;
  min-width: 0;
  min-height: 25px;
  color: var(--g500);
  font-size: 10px;
}
.chart-legend-copy { display: flex; align-items: baseline; gap: 5px; min-width: 0; }
.chart-legend-copy strong { min-width: 0; overflow: hidden; color: var(--g600); font-size: 10px; font-weight: 650; text-overflow: ellipsis; white-space: nowrap; }
.chart-legend-copy small { flex: none; color: var(--g400); font-size: 8.5px; white-space: nowrap; }
.chart-legend-dot { width: 14px; height: 2px; border-radius: 1px; }
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
  border: 1px solid var(--chart-tooltip-border);
  border-radius: 7px;
  background: var(--chart-tooltip-bg);
  backdrop-filter: blur(14px) saturate(115%);
  box-shadow: var(--chart-tooltip-shadow);
  color: var(--chart-tooltip-text);
  font-size: 11px;
  line-height: 1.35;
  overscroll-behavior: contain;
  scroll-behavior: auto;
  scrollbar-width: none;
}
:global(.chart-tooltip-panel::-webkit-scrollbar) { width: 0; height: 0; }
:global(.chart-tooltip-title) { padding-bottom: 5px; color: var(--chart-tooltip-title); font-weight: 800; }
:global(.chart-tooltip-group + .chart-tooltip-group) { margin-top: 7px; }
:global(.chart-tooltip-group-title) { display: flex; align-items: center; gap: 6px; min-height: 21px; padding: 2px 6px; border-left: 3px solid var(--chart-tooltip-accent); border-radius: 3px; background: var(--chart-tooltip-group-bg); color: var(--chart-tooltip-text); font-size: 11px; font-weight: 800; }
:global(.chart-tooltip-group-title span) { overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
:global(.chart-tooltip-group-dot) { width: 8px; height: 8px; flex: 0 0 8px; }
:global(.chart-tooltip-list) { display: grid; grid-template-columns: repeat(2, minmax(0, 1fr)); gap: 3px 12px; margin: 3px 0 0 5px; padding-left: 8px; border-left: 2px solid var(--chart-tooltip-line); }
:global(.chart-tooltip-row) { display: flex; align-items: center; min-width: 0; gap: 5px; }
:global(.chart-tooltip-row span) { min-width: 0; overflow: hidden; color: var(--g500); text-overflow: ellipsis; white-space: nowrap; }
:global(.chart-tooltip-swatch) { width: 9px; height: 9px; flex: 0 0 9px; border-radius: 2px; }
</style>
