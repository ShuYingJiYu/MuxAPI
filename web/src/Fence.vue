<script setup>
// uptime 状态栅栏：trend 采样渲染成一排竖条，按状态染色。
// 不足 cap 根时左侧用「无数据」灰条补齐，宽度恒定（PAST → NOW）。
import { computed } from 'vue'

const props = defineProps({
  trend: { type: Array, default: () => [] }, // [{ts,status,lat_ms,succ_rate}]
  cap: { type: Number, default: 24 },
})

const COLORS = { 0: 'var(--g200)', 1: '#10b981', 2: '#f59e0b', 3: '#ef4444' }
const LABEL = { 0: '无数据', 1: '正常', 2: '降级', 3: '熔断' }

const bars = computed(() => {
  const pts = props.trend.slice(-props.cap)
  const pad = Math.max(0, props.cap - pts.length)
  return [...Array(pad).fill({ status: 0, _pad: true }), ...pts]
})

function tip(b) {
  if (b._pad) return '无数据'
  const t = b.ts ? new Date(b.ts * 1000).toLocaleTimeString() : ''
  const rate = b.succ_rate != null ? ` · ${(b.succ_rate * 100).toFixed(0)}%` : ''
  const lat = b.lat_ms ? ` · ${b.lat_ms}ms` : ''
  return `${t} ${LABEL[b.status] || ''}${rate}${lat}`.trim()
}
</script>

<template>
  <div class="fence">
    <div class="fence-bars">
      <span v-for="(b, i) in bars" :key="i" class="bar"
        :style="{ background: COLORS[b.status] }" :title="tip(b)" />
    </div>
    <div class="fence-axis"><span>较早</span><span>现在</span></div>
  </div>
</template>

<style scoped>
.fence { margin: 14px 0 2px; }
.fence-bars { display: flex; gap: 4px; align-items: stretch; height: 36px; }
.bar { flex: 1; min-width: 0; border-radius: 3px; transition: opacity .12s; }
.bar:hover { opacity: .65; }
.fence-axis { display: flex; justify-content: space-between; font-size: 10px; color: var(--g400); margin-top: 5px; letter-spacing: .03em; }
</style>
