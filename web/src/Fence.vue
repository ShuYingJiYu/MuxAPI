<script setup>
// uptime 状态栅栏：trend 采样渲染成一排竖条，按状态染色。
// 不足 cap 根时左侧用「无数据」灰条补齐，宽度恒定（PAST → NOW）。
import { computed } from 'vue'

const props = defineProps({
  trend: { type: Array, default: () => [] }, // [{ts,status,total,succ,succ_rate}]
  cap: { type: Number, default: 24 },
  unit: { type: String, default: '请求' },   // 计数口径文案：分组="请求"，监控="探测"
})

const COLORS = { 0: 'var(--g200)', 1: '#10b981', 2: '#f59e0b', 3: '#ef4444' }
const LABEL = { 0: '无数据', 1: '正常', 2: '降级', 3: '熔断' }

// 固定输出 cap 个格子，避免不同数据量导致卡片宽度变化。
const bars = computed(() => {
  const pts = props.trend.slice(-props.cap)
  const pad = Math.max(0, props.cap - pts.length)
  const all = [...Array(pad).fill({ status: 0, _pad: true }), ...pts]
  return all.map((b) => ({ ...b, tip: tip(b) })) // 预计算 tooltip，模板只读不重算
})

// 该格代表的小时区间 "HH:MM–HH:MM"（ts 为整点起始）
function hourRange(ts) {
  const a = new Date(ts * 1000)
  const b = new Date((ts + 3600) * 1000)
  const hm = (d) => `${String(d.getHours()).padStart(2, '0')}:${String(d.getMinutes()).padStart(2, '0')}`
  return `${hm(a)}–${hm(b)}`
}

// tooltip 结构化内容：标题(时间段+状态) + 明细行 + 成功率
function tip(b) {
  if (b._pad || !b.ts) return { title: '无数据', rows: [] }
  const t = { title: hourRange(b.ts), label: LABEL[b.status] || '', status: b.status, rows: [] }
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
        <span class="bar" :style="{ background: COLORS[b.status] }" />
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
    <div class="fence-axis"><span>较早</span><span>现在</span></div>
  </div>
</template>

<style scoped>
.fence { margin: 14px 0 2px; }
.fence-bars { display: flex; gap: 4px; align-items: stretch; height: 36px; }
.bar-wrap { position: relative; flex: 1; min-width: 0; display: flex; }
.bar { flex: 1; border-radius: 3px; transition: opacity .12s; }
.bar-wrap:hover .bar { opacity: .6; }

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

.fence-axis { display: flex; justify-content: space-between; font-size: 10px; color: var(--g400); margin-top: 5px; letter-spacing: .03em; }
</style>
