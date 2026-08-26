<script setup>
// uptime 梭子时间带：每格代表一小时，梭子高度表示请求量，颜色表示成功率状态。
import { computed } from 'vue'

const props = defineProps({
  trend: { type: Array, default: () => [] }, // [{ts,status,total,succ,succ_rate}]
  cap: { type: Number, default: 24 },
  unit: { type: String, default: '请求' },   // 计数口径文案：分组="请求"，监控="探测"
})

const COLORS = { 0: 'var(--g100)', 1: '#68bfae', 2: '#d9b463', 3: '#e18495' }
const LABEL = { 0: '无数据', 1: '正常', 2: '降级', 3: '熔断' }

// 固定输出 cap 个格子，避免不同数据量导致卡片宽度变化。
const bars = computed(() => {
  const pts = props.trend.slice(-props.cap)
  const pad = Math.max(0, props.cap - pts.length)
  const all = [...Array(pad).fill({ status: 0, _pad: true }), ...pts]
  const maxTotal = Math.max(0, ...all.map((point) => Number(point.total) || 0))
  return all.map((b) => ({
    ...b,
    height: b._pad || (Number(b.total) || 0) <= 0
      ? 0
      : Math.max(7, Math.round(Math.sqrt(Number(b.total) / maxTotal) * 30)),
    tip: tip(b),
  })) // 预计算梭子高度和 tooltip，模板只读不重算
})

// 该格代表的小时区间 "HH:MM–HH:MM"（ts 为整点起始）
function hourRange(ts, endTs) {
  const a = new Date(ts * 1000)
  const b = new Date((endTs || ts + 3600) * 1000)
  const hm = (d) => `${String(d.getHours()).padStart(2, '0')}:${String(d.getMinutes()).padStart(2, '0')}`
  return `${hm(a)}–${hm(b)}`
}

// tooltip 结构化内容：标题(时间段+状态) + 明细行 + 成功率
function tip(b) {
  if (b._pad || !b.ts) return { title: '无数据', rows: [] }
  const t = { title: hourRange(b.ts, b.end_ts), label: LABEL[b.status] || '', status: b.status, rows: [] }
  if (b.total > 0) {
    const succ = b.succ != null ? b.succ : Math.round(b.total * (b.succ_rate || 0))
    t.rows.push(`${b.total} 次${props.unit}，${succ} 次成功`)
    t.rate = `${((b.succ_rate || 0) * 100).toFixed(0)}%`
  } else {
    t.rows.push(`无${props.unit}`)
  }
  return t
}
</script>

<template>
  <div class="fence">
    <div class="fence-bars">
      <span v-for="(b, i) in bars" :key="i" class="bar-wrap">
        <span class="bar" :class="{ empty: !b.height }" :style="{ height: `${b.height}px`, background: COLORS[b.status] }" />
        <span class="tip" :data-edge="i < 4 ? 'l' : (i > bars.length - 5 ? 'r' : '')">
          <span class="tip-head">
            <i class="tip-dot" :style="{ background: COLORS[b.tip.status ?? 0] }" />
            <b>{{ b.tip.title }}</b>
            <em v-if="b.tip.label">{{ b.tip.label }}</em>
          </span>
          <span v-for="(r, k) in b.tip.rows" :key="k" class="tip-row">{{ r }}</span>
          <span v-if="b.tip.rate" class="tip-rate">成功率 <b>{{ b.tip.rate }}</b></span>
        </span>
      </span>
    </div>
  </div>
</template>

<style scoped>
.fence { margin: 14px 0 10px; }
.fence-bars { display: flex; align-items: flex-end; gap: 2px; height: 34px; padding-inline: 1px; }
.bar-wrap { position: relative; flex: 1; min-width: 0; height: 32px; display: flex; align-items: flex-end; justify-content: center; }
.bar {
  flex: none; width: 7px; border-radius: 1px; opacity: .9;
  clip-path: polygon(50% 0, 100% 18%, 100% 82%, 50% 100%, 0 82%, 0 18%);
  transform-origin: center bottom;
  transition: transform .12s ease, opacity .12s;
}
.bar.empty { display: none; }
.bar-wrap:hover .bar { transform: scaleX(1.25); opacity: 1; }

/* 自定义 tooltip：默认隐藏，hover 格子时上方弹出 */
.tip {
  position: absolute; bottom: calc(100% + 8px); left: 50%; transform: translateX(-50%) translateY(4px);
  display: flex; flex-direction: column; gap: 3px; width: max-content; max-width: 200px;
  padding: 8px 10px; border-radius: 8px; background: var(--g900, #1f2937); color: #fff;
  font-size: 11px; line-height: 1.45; white-space: nowrap; pointer-events: none; z-index: 40;
  opacity: 0; visibility: hidden; transition: opacity .14s, transform .14s;
  box-shadow: 0 8px 24px rgba(0, 0, 0, .22);
}
.bar-wrap:hover .tip { opacity: 1; visibility: visible; transform: translateX(-50%) translateY(0); }
/* 边缘格子贴边对齐，避免溢出裁剪 */
.tip[data-edge="l"] { left: 0; transform: translateX(0) translateY(4px); }
.bar-wrap:hover .tip[data-edge="l"] { transform: translateX(0) translateY(0); }
.tip[data-edge="r"] { left: auto; right: 0; transform: translateX(0) translateY(4px); }
.bar-wrap:hover .tip[data-edge="r"] { transform: translateX(0) translateY(0); }
/* 小箭头 */
.tip::after {
  content: ''; position: absolute; top: 100%; left: 50%; transform: translateX(-50%);
  border: 5px solid transparent; border-top-color: var(--g900, #1f2937);
}
.tip[data-edge="l"]::after { left: 14px; }
.tip[data-edge="r"]::after { left: auto; right: 14px; }

.tip-head { display: flex; align-items: center; gap: 6px; }
.tip-head b { font-weight: 600; font-variant-numeric: tabular-nums; }
.tip-head em { margin-left: auto; font-style: normal; font-size: 10px; opacity: .7; }
.tip-dot { width: 7px; height: 7px; border-radius: 50%; flex-shrink: 0; }
.tip-row { color: rgba(255, 255, 255, .82); font-variant-numeric: tabular-nums; }
.tip-rate { color: rgba(255, 255, 255, .82); }
.tip-rate b { color: #fff; font-weight: 600; }

</style>
