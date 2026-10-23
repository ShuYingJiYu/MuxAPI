<script setup>
import { computed, onMounted, onUnmounted, reactive, ref } from 'vue'
import { api } from './api.js'
import FancySelect from './FancySelect.vue'
import Icon from './Icon.vue'

const emit = defineEmits(['unauthorized'])
const filters = reactive({ ip: '', result: '', range: '15' })
const ranges = [{ value: '1', label: '最近 24 小时' }, { value: '7', label: '最近 7 天' }, { value: '15', label: '最近 15 天' }]
const results = [{ value: '', label: '全部结果' }, { value: 'failure', label: '验证失败' }, { value: 'success', label: '验证成功' }, { value: 'attention', label: '需关注' }]
const data = ref(null)
const page = ref(1)
const pageSize = 20
const pages = computed(() => Math.max(1, Math.ceil((data.value?.total || 0) / pageSize)))
const loading = ref(false)
const error = ref('')
let controller
let epoch = 0

function time(value) { return value ? new Date(value * 1000).toLocaleString('zh-CN', { hour12: false }) : '暂无' }
function labels(row) {
  return [row.frequent && '频繁尝试', row.after_failures && '失败后成功', row.new_source && '近 15 天新来源', row.new_browser && '新浏览器标识'].filter(Boolean)
}
async function load(reset = false) {
  if (reset) page.value = 1
  const current = ++epoch
  controller?.abort()
  controller = new AbortController()
  loading.value = true
  error.value = ''
  try {
    const next = await api.securityLogs({
      ip: filters.ip.trim(), result: filters.result, page: page.value, size: pageSize,
      from: Math.floor(Date.now() / 1000) - Number(filters.range) * 86400,
    }, controller.signal)
    if (current !== epoch) return
    data.value = next
    if (page.value > pages.value) {
      page.value = pages.value
      return load()
    }
  } catch (e) {
    if (current !== epoch || e.name === 'AbortError') return
    if (e.status === 401) emit('unauthorized')
    error.value = e.status === 400 ? '筛选条件无效，请检查来源 IP。' : '安全日志读取失败，请重试。'
  } finally {
    if (current === epoch) loading.value = false
  }
}
function turn(delta) { page.value += delta; load() }
onMounted(() => load())
onUnmounted(() => { epoch++; controller?.abort() })
</script>

<template>
  <section class="security-view" :aria-busy="loading">
    <header class="security-heading">
      <div><span class="security-kicker">ACCESS SECURITY</span><h2>安全日志</h2><p>查看登录验证与管理访问，自动保留 15 天。</p></div>
      <button class="btn btn-ghost" :disabled="loading" @click="load()"><Icon name="refresh" :size="15" />刷新</button>
    </header>

    <div v-if="data && (!data.recording_enabled || data.dropped_since_start || data.write_failed)" class="security-notice" role="status">
      <Icon name="alert" :size="17" />
      <div v-if="!data.authentication_enabled">管理 Token 未配置，当前没有鉴权保护，也不记录成功验证。</div>
      <div v-else-if="!data.recording_enabled">当前为只读模式，不写入新的安全记录。</div>
      <div v-else>日志可能不完整：本次启动后丢失 {{ data.dropped_since_start }} 次记录。{{ data.write_failed ? '数据库写入异常。' : '历史丢失计数保留至重启。' }}</div>
    </div>

    <div class="security-stats">
      <div><span>验证失败次数</span><strong>{{ data?.summary?.failures ?? '—' }}</strong><small>按当前筛选范围累计</small></div>
      <div><span>需关注来源</span><strong>{{ data?.summary?.attention_ips ?? '—' }}</strong><small>来源变化或异常尝试</small></div>
      <div><span>最近验证成功</span><strong class="security-date">{{ time(data?.summary?.latest_success) }}</strong><small>不代表已确认使用者身份</small></div>
    </div>

    <form class="security-filters" @submit.prevent="load(true)">
      <label><span>来源 IP</span><input v-model="filters.ip" placeholder="完整 IPv4 / IPv6" autocomplete="off" /></label>
      <label><span>验证结果</span><FancySelect v-model="filters.result" :options="results" /></label>
      <label><span>时间范围</span><FancySelect v-model="filters.range" :options="ranges" /></label>
      <button class="btn" :disabled="loading" type="submit"><Icon name="search" :size="15" />查询</button>
    </form>

    <p v-if="error" class="security-error" role="alert">{{ error }}<button class="btn-link sm" @click="load()">重试</button></p>
    <p v-if="loading" class="security-empty" role="status">正在读取安全日志…</p>
    <template v-else-if="!error">
      <div v-if="data?.entries?.length" class="security-list">
        <article v-for="row in data.entries" :key="row.id" class="security-entry">
          <div class="security-entry-main">
            <span class="security-result" :class="row.success ? 'success' : 'failure'"><i></i>{{ row.success ? '验证成功' : '验证失败' }}</span>
            <strong class="security-ip">{{ row.client_ip || '未知来源' }}</strong>
            <span class="security-kind">{{ row.kind === 'login' ? '登录' : '管理访问' }}</span>
            <span class="security-count">{{ row.count }} 次</span>
          </div>
          <div v-if="labels(row).length" class="security-flags"><span v-for="label in labels(row)" :key="label">{{ label }}</span></div>
          <div class="security-meta"><time>{{ time(row.first_at) }}</time><span v-if="row.first_at !== row.last_at">至 {{ time(row.last_at) }}</span><code>{{ row.method }} {{ row.endpoint }}</code></div>
          <details class="security-details"><summary>来源与浏览器</summary><dl><dt>连接 IP</dt><dd>{{ row.peer_ip || '未知' }}</dd><dt>来源依据</dt><dd>{{ row.forwarded ? '可信代理的 X-Forwarded-For' : '直接连接地址' }}</dd><dt>浏览器标识</dt><dd>{{ row.user_agent || '未提供' }}</dd></dl></details>
        </article>
      </div>
      <div v-else class="security-empty"><Icon name="key" :size="24" /><p>当前范围暂无安全记录</p></div>
    </template>

    <footer class="security-footer">
      <span>共 {{ data?.total ?? 0 }} 条合并记录 · {{ page }} / {{ pages }} 页</span>
      <div><button class="btn btn-ghost btn-sm" :disabled="loading || page <= 1" @click="turn(-1)">上一页</button><button class="btn btn-ghost btn-sm" :disabled="loading || page >= pages" @click="turn(1)">下一页</button></div>
    </footer>
    <p class="security-help">同一分钟的重复访问合并计数；后台轮询记为管理访问，不重复记为登录。来源与浏览器变化仅供核查，不代表账号已被入侵。</p>
    <p v-if="data && !data.trusted_proxy_configured" class="security-help">未配置信任代理，来源使用直接连接 IP；经 Nginx 访问时可能显示代理地址。</p>
    <p class="security-help">日志异步写入，通常约 1 秒后可见。频繁尝试：当前分钟及前 4 分钟合计失败至少 10 次，不自动封禁。</p>
  </section>
</template>

<style scoped>
.security-view { min-width: 0; display: grid; gap: 20px; }
.security-heading { display: flex; align-items: center; justify-content: space-between; gap: 16px; }
.security-kicker { color: var(--p700); font-size: 10px; letter-spacing: .12em; font-weight: 700; }
.security-heading h2 { margin: 7px 0; font-size: 23px; color: var(--g800); }
.security-heading p, .security-help { margin: 0; color: var(--g500); font-size: 12px; line-height: 1.7; }
.security-stats { display: grid; grid-template-columns: repeat(auto-fit, minmax(min(100%, 170px), 1fr)); gap: 12px; }
.security-stats > div { display: grid; align-content: start; gap: 10px; padding: 18px; background: var(--surface); border: 1px solid var(--line); border-radius: var(--r-card); min-width: 0; }
.security-stats span { color: var(--g500); font-size: 12px; }
.security-stats strong { font-size: 28px; color: var(--g800); font-variant-numeric: tabular-nums; }
.security-stats .security-date { font-size: 15px; line-height: 2; overflow-wrap: anywhere; }
.security-stats small { color: var(--g400); font-size: 11px; }
.security-filters { display: grid; grid-template-columns: minmax(0, 1.4fr) repeat(2, minmax(0, 1fr)) auto; gap: 12px; align-items: end; }
.security-filters label { display: grid; gap: 7px; min-width: 0; color: var(--g600); font-size: 12px; }
.security-filters input { width: 100%; box-sizing: border-box; min-width: 0; height: 40px; padding: 9px 12px; border: 1px solid var(--control-border); border-radius: var(--r-control); background: var(--surface-raised); color: var(--g800); font-size: 12px; outline: none; }
.security-filters input::placeholder { color: var(--g400); }
.security-filters input:focus { border-color: var(--p500); box-shadow: 0 0 0 2px var(--p100); }
.security-filters :deep(.fancy-select) { min-width: 0; width: 100%; }
.security-filters :deep(.fancy-select-btn) { min-width: 0; width: 100%; }
.security-list { border-top: 1px solid var(--line); }
.security-entry { padding: 18px 0; border-bottom: 1px solid var(--line); min-width: 0; }
.security-entry-main, .security-meta, .security-flags { display: flex; flex-wrap: wrap; align-items: center; gap: 10px; }
.security-result { display: inline-flex; align-items: center; gap: 6px; font-size: 12px; }
.security-result i { width: 6px; height: 6px; background: currentColor; border-radius: 50%; }
.success { color: var(--state-success); }
.failure, .security-error { color: var(--state-danger); }
.security-ip { font-size: 14px; color: var(--g800); overflow-wrap: anywhere; }
.security-kind, .security-count, .security-meta { font-size: 11px; color: var(--g500); }
.security-count { margin-left: auto; font-variant-numeric: tabular-nums; }
.security-meta { margin-top: 10px; line-height: 1.6; }
.security-meta code { overflow-wrap: anywhere; font-size: 11px; }
.security-flags { margin-top: 10px; gap: 6px; }
.security-flags span { padding: 3px 8px; color: var(--state-warning); background: var(--state-warning-soft); border-radius: var(--r-control); font-size: 11px; }
.security-details { color: var(--g500); font-size: 11px; margin-top: 9px; }
.security-details summary { cursor: pointer; width: fit-content; }
.security-details dl { display: grid; grid-template-columns: auto minmax(0, 1fr); gap: 8px 14px; padding: 12px; background: var(--surface-muted); border-radius: var(--r-control); }
.security-details dd { margin: 0; overflow-wrap: anywhere; }
.security-notice { display: flex; align-items: start; gap: 10px; color: var(--state-warning); background: var(--state-warning-soft); padding: 13px 15px; border-radius: var(--r-control); font-size: 12px; line-height: 1.7; }
.security-notice svg { flex-shrink: 0; margin-top: 2px; }
.security-empty { padding: 32px 12px; text-align: center; color: var(--g400); font-size: 13px; }
.security-footer { display: flex; justify-content: space-between; flex-wrap: wrap; gap: 12px; align-items: center; color: var(--g500); font-size: 11px; }
.security-footer > div { display: flex; gap: 8px; }
.security-view button:disabled { opacity: .5; cursor: not-allowed; }
@media (max-width: 720px) { .security-filters { grid-template-columns: repeat(2, minmax(0, 1fr)); } .security-filters label:first-child { grid-column: 1 / -1; } .security-filters > button { grid-column: 1 / -1; justify-content: center; } }
</style>
