<script setup>
import { ref, onMounted, onUnmounted } from 'vue'
import { api } from './api.js'
import { normalizeTs } from './api.generated.js'

const decisions = ref([])
const expanded = ref(null)
const detail = ref(null)
const loading = ref(false)
const err = ref('')

async function load() {
  try {
    decisions.value = await api.routeDecisions() || []
  } catch (e) {
    err.value = e.message || String(e)
  }
}

async function toggle(d) {
  if (expanded.value === d.id) { expanded.value = null; detail.value = null; return }
  expanded.value = d.id
  detail.value = null
  try {
    detail.value = await api.routeDecisionDetail(d.id)
  } catch (e) {
    detail.value = { error: e.message }
  }
}

let timer = null
onMounted(() => { load(); timer = setInterval(load, 5000) })
onUnmounted(() => { if (timer) clearInterval(timer) })

function timeAgo(ts) {
  const ms = normalizeTs(ts)
  if (!ms) return '—'
  const diff = Math.max(0, Math.floor((Date.now() - ms) / 1000))
  if (diff < 60) return diff + '秒前'
  if (diff < 3600) return Math.floor(diff / 60) + '分钟前'
  if (diff < 86400) return Math.floor(diff / 3600) + '小时前'
  return Math.floor(diff / 86400) + '天前'
}

function fmtCost(v) {
  if (v == null || v === 0) return '$0'
  if (v < 0.001) return '<$0.001'
  if (v < 1) return '$' + v.toFixed(4)
  return '$' + v.toFixed(2)
}

function fmtReason(d) {
  if (!d.reason) return '—'
  // Parse "lowest forecast cost via provider cache: 1.35 over 15m0s; saves 1.71 versus runner-up"
  if (d.cache_selected) {
    const saves = d.estimated_savings
    if (saves > 0) return `缓存省 ${fmtCost(saves)}（vs 次优）`
  }
  if (d.exploration) return '探索采样'
  if (d.reason.includes('ordinary input')) return '无缓存最低价'
  return d.reason.split(';')[0].replace('lowest forecast cost via ', '').replace(' over ', ' / ')
}

function maxCost(candidates) {
  if (!candidates || !candidates.length) return 1
  return Math.max(...candidates.map(c => c.forecast_total_cost || c.cost || 0), 0.0001)
}
</script>

<template>
  <p v-if="err" class="err-banner">{{ err }}</p>
  <p v-if="!decisions.length && !loading" class="routing-empty">暂无路由决策记录</p>

  <div class="routing-list">
    <div v-for="d in decisions" :key="d.id" class="routing-row" :class="{ expanded: expanded === d.id }">
      <div class="routing-summary" @click="toggle(d)">
        <span class="routing-time">{{ timeAgo(d.created_at) }}</span>
        <span class="routing-model">{{ d.model || '—' }}</span>
        <span class="routing-upstream selected">{{ d.selected_upstream || '—' }}</span>
        <span class="routing-cost">{{ fmtCost(d.selected_cost) }}</span>
        <span class="routing-badge" :class="d.cache_selected ? 'cache-hit' : 'cache-miss'">
          {{ d.cache_selected ? '缓存' : '直连' }}
        </span>
        <span class="routing-reason">{{ fmtReason(d) }}</span>
      </div>

      <div v-if="expanded === d.id" class="routing-detail">
        <div v-if="!detail" class="routing-loading">加载中…</div>
        <div v-else-if="detail.error" class="routing-err">{{ detail.error }}</div>
        <template v-else>
          <!-- Cost bar chart -->
          <div v-if="detail.candidates && detail.candidates.length" class="routing-candidates">
            <h4>候选成本对比（{{ detail.forecast_requests?.toFixed(0) || '?' }} 请求 / 15min 窗口）</h4>
            <div class="routing-bars">
              <div v-for="c in detail.candidates.filter(x => x.eligible)" :key="c.upstream_id || c.upstream_name"
                class="routing-bar-row">
                <span class="bar-label">{{ c.upstream_name || c.name }}</span>
                <div class="bar-track">
                  <div class="bar-fill" :class="{ accent: c.selected }"
                    :style="{ width: Math.max((c.forecast_total_cost || 0) / maxCost(detail.candidates.filter(x => x.eligible)) * 100, 3) + '%' }">
                  </div>
                </div>
                <span class="bar-cost">{{ fmtCost(c.forecast_total_cost) }}</span>
                <span v-if="c.selected" class="bar-note accent-text">✓ 已选</span>
                <span v-else-if="c.cache_supported" class="bar-note">有缓存</span>
                <span v-else class="bar-note">无缓存</span>
              </div>
            </div>
          </div>

          <!-- Actual result -->
          <div v-if="detail.actual_outcome" class="routing-section">
            <h4>实际结果</h4>
            <dl class="routing-dl">
              <div><dt>状态</dt><dd>{{ detail.actual_outcome }}</dd></div>
              <div v-if="detail.actual_cached_tokens"><dt>缓存命中</dt><dd>{{ (detail.actual_cached_tokens / 1000).toFixed(0) }}K tokens</dd></div>
              <div v-if="detail.actual_cache_creation_tokens"><dt>缓存创建</dt><dd>{{ (detail.actual_cache_creation_tokens / 1000).toFixed(1) }}K tokens</dd></div>
              <div v-if="detail.actual_input_tokens"><dt>输入</dt><dd>{{ (detail.actual_input_tokens / 1000).toFixed(0) }}K tokens</dd></div>
              <div v-if="detail.actual_output_tokens"><dt>输出</dt><dd>{{ detail.actual_output_tokens }} tokens</dd></div>
            </dl>
          </div>

          <!-- Decision context -->
          <div class="routing-section">
            <h4>决策上下文</h4>
            <dl class="routing-dl">
              <div><dt>策略</dt><dd>{{ detail.strategy || 'cost' }}</dd></div>
              <div><dt>置信度</dt><dd>{{ (detail.confidence * 100).toFixed(0) }}%</dd></div>
              <div><dt>前缀复用</dt><dd>{{ ((detail.reusable_prefix_tokens || 0) / 1000).toFixed(0) }}K / {{ ((detail.estimated_input_tokens || 0) / 1000).toFixed(0) }}K</dd></div>
              <div v-if="detail.exploration"><dt>探索</dt><dd>是（非最优采样）</dd></div>
            </dl>
          </div>
        </template>
      </div>
    </div>
  </div>
</template>

<style scoped>
.routing-empty { text-align: center; color: var(--g400); padding: 48px 0; }
.routing-list { display: flex; flex-direction: column; gap: 6px; }
.routing-row {
  background: rgba(255,255,255,.78); backdrop-filter: blur(8px);
  border: 1px solid var(--g100); border-radius: 14px;
  overflow: hidden; transition: box-shadow .2s;
}
.routing-row:hover { box-shadow: var(--sh-card-hover); }
.routing-row.expanded { box-shadow: var(--sh-card); }

.routing-summary {
  display: grid; grid-template-columns: 72px 1fr 1fr 80px 52px 1fr;
  align-items: center; gap: 10px; padding: 12px 16px; cursor: pointer;
  font-size: 13px;
}
.routing-time { color: var(--g400); font-size: 12px; font-variant-numeric: tabular-nums; }
.routing-model { font-weight: 600; color: var(--g700); overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
.routing-upstream { overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
.routing-upstream.selected { color: var(--p700); font-weight: 700; }
.routing-cost { font-variant-numeric: tabular-nums; color: var(--g600); font-weight: 600; }
.routing-badge {
  font-size: 11px; padding: 2px 8px; border-radius: 999px; text-align: center; font-weight: 600;
}
.routing-badge.cache-hit { background: #ecfdf5; color: #059669; }
.routing-badge.cache-miss { background: var(--g100); color: var(--g500); }
.routing-reason { color: var(--g500); font-size: 12px; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }

.routing-detail { padding: 0 16px 16px; border-top: 1px solid var(--g100); }
.routing-loading, .routing-err { padding: 12px 0; color: var(--g400); font-size: 13px; }
.routing-err { color: var(--red); }

.routing-candidates { margin-top: 12px; }
.routing-candidates h4, .routing-section h4 { font-size: 12px; color: var(--g500); font-weight: 700; margin-bottom: 8px; }

.routing-bars { display: flex; flex-direction: column; gap: 6px; }
.routing-bar-row { display: flex; align-items: center; gap: 8px; font-size: 12px; }
.bar-label { width: 110px; flex: none; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; color: var(--g700); font-weight: 600; }
.bar-track { flex: 1; height: 18px; background: var(--g100); border-radius: 9px; overflow: hidden; }
.bar-fill { height: 100%; border-radius: 9px; background: var(--g300); transition: width .3s; }
.bar-fill.accent { background: linear-gradient(90deg, var(--p400), var(--p600)); }
.bar-cost { width: 64px; flex: none; text-align: right; font-variant-numeric: tabular-nums; color: var(--g600); }
.bar-note { flex: none; font-size: 11px; color: var(--g400); width: 64px; }
.bar-note.accent-text { color: var(--p700); font-weight: 700; }

.routing-section { margin-top: 14px; }
.routing-dl { display: grid; grid-template-columns: repeat(auto-fill, minmax(160px, 1fr)); gap: 6px 20px; }
.routing-dl div { display: flex; gap: 8px; font-size: 12px; }
.routing-dl dt { color: var(--g400); white-space: nowrap; }
.routing-dl dd { margin: 0; color: var(--g700); font-weight: 600; font-variant-numeric: tabular-nums; }

@media (max-width: 768px) {
  .routing-summary { grid-template-columns: 60px 1fr 70px 46px; gap: 6px; padding: 10px 12px; }
  .routing-upstream, .routing-reason { display: none; }
  .bar-label { width: 80px; }
  .bar-note { display: none; }
}
</style>
