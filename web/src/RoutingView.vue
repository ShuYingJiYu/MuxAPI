<script setup>
import { ref, onMounted, onUnmounted, computed } from 'vue'
import { api } from './api.js'

const decisions = ref([])
const expanded = ref(null)
const detail = ref(null)
const loading = ref(false)
const err = ref('')

async function load() {
  try {
    const data = await api.routeDecisions()
    decisions.value = data || []
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

// Auto-refresh every 5s
let timer = null
onMounted(() => { load(); timer = setInterval(load, 5000) })
onUnmounted(() => { if (timer) clearInterval(timer) })

// Helpers
function timeAgo(ts) {
  if (!ts) return '—'
  const diff = Math.max(0, Math.floor((Date.now() - new Date(ts).getTime()) / 1000))
  if (diff < 60) return diff + '秒前'
  if (diff < 3600) return Math.floor(diff / 60) + '分钟前'
  if (diff < 86400) return Math.floor(diff / 3600) + '小时前'
  return Math.floor(diff / 86400) + '天前'
}

function fmtCost(v) {
  if (v == null || v === 0) return '$0'
  if (v < 0.001) return '<$0.001'
  return '$' + v.toFixed(4)
}

function maxCost(candidates) {
  if (!candidates || !candidates.length) return 1
  return Math.max(...candidates.map(c => c.cost || 0), 0.0001)
}
</script>

<template>
  <p v-if="err" class="err-banner">{{ err }}</p>
  <p v-if="!decisions.length && !loading" class="routing-empty">暂无路由决策记录</p>

  <div class="routing-list">
    <div v-for="d in decisions" :key="d.id" class="routing-row" :class="{ expanded: expanded === d.id }">
      <!-- Summary row -->
      <div class="routing-summary" @click="toggle(d)">
        <span class="routing-time">{{ timeAgo(d.created_at || d.timestamp) }}</span>
        <span class="routing-model">{{ d.model || '—' }}</span>
        <span class="routing-upstream selected">{{ d.selected_upstream || d.upstream_name || '—' }}</span>
        <span class="routing-cost">{{ fmtCost(d.cost) }}</span>
        <span class="routing-badge" :class="d.cached ? 'cache-hit' : 'cache-miss'">{{ d.cached ? '缓存' : '直连' }}</span>
        <span class="routing-reason">{{ d.reason || '—' }}</span>
      </div>

      <!-- Expanded detail -->
      <div v-if="expanded === d.id" class="routing-detail">
        <div v-if="!detail" class="routing-loading">加载中…</div>
        <div v-else-if="detail.error" class="routing-err">{{ detail.error }}</div>
        <template v-else>
          <!-- Cost bar chart -->
          <div v-if="detail.candidates && detail.candidates.length" class="routing-candidates">
            <h4>候选渠道成本对比</h4>
            <div class="routing-bars">
              <div v-for="c in detail.candidates" :key="c.upstream_id || c.name" class="routing-bar-row">
                <span class="bar-label">{{ c.name || c.upstream_name || c.upstream_id }}</span>
                <div class="bar-track">
                  <div class="bar-fill" :class="{ accent: c.selected }"
                    :style="{ width: (Math.max((c.cost || 0) / maxCost(detail.candidates) * 100, 2)) + '%' }"></div>
                </div>
                <span class="bar-cost">{{ fmtCost(c.cost) }}</span>
                <span class="bar-note" v-if="!c.selected">{{ c.reject_reason || '更贵' }}</span>
                <span class="bar-note accent-text" v-else>已选</span>
              </div>
            </div>
          </div>

          <!-- Forecast -->
          <div v-if="detail.forecast" class="routing-section">
            <h4>流量预测</h4>
            <dl class="routing-dl">
              <div v-if="detail.forecast.requests_15m != null"><dt>请求/15min</dt><dd>{{ detail.forecast.requests_15m }}</dd></div>
              <div v-if="detail.forecast.rpm != null"><dt>RPM</dt><dd>{{ detail.forecast.rpm }}</dd></div>
            </dl>
          </div>

          <!-- Cache info -->
          <div v-if="detail.cache" class="routing-section">
            <h4>缓存信息</h4>
            <dl class="routing-dl">
              <div v-if="detail.cache.hit_rate != null"><dt>命中率</dt><dd>{{ (detail.cache.hit_rate * 100).toFixed(1) }}%</dd></div>
              <div v-if="detail.cache.expires_in != null"><dt>过期倒计时</dt><dd>{{ detail.cache.expires_in }}s</dd></div>
              <div v-if="detail.cache.ttl_type"><dt>TTL 策略</dt><dd>{{ detail.cache.ttl_type }}</dd></div>
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
.bar-label { width: 120px; flex: none; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; color: var(--g700); font-weight: 600; }
.bar-track { flex: 1; height: 18px; background: var(--g100); border-radius: 9px; overflow: hidden; }
.bar-fill { height: 100%; border-radius: 9px; background: var(--g300); transition: width .3s; }
.bar-fill.accent { background: linear-gradient(90deg, var(--p400), var(--p600)); }
.bar-cost { width: 64px; flex: none; text-align: right; font-variant-numeric: tabular-nums; color: var(--g600); }
.bar-note { flex: none; font-size: 11px; color: var(--g400); width: 80px; }
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
