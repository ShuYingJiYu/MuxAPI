<script setup>
import { computed, nextTick, onMounted, onUnmounted, ref, watch } from 'vue'
import FancySelect from './FancySelect.vue'
import Icon from './Icon.vue'
import { useSecurityLogs, securityKinds, securityPageSizes, securityRanges, securityResults } from './composables/useSecurityLogs.js'

const emit = defineEmits(['unauthorized'])
const {
  draft, applied, view, data, page, pageSize, loading, error, jump, pages, pageItems,
  pendingFilters, rangeStart, rangeEnd, applyFilters, refresh, resetFilters,
  changeView, changeSize, goPage, retry, dispose,
} = useSecurityLogs({ onUnauthorized: () => emit('unauthorized') })
const expanded = ref(null)
const listViewport = ref(null)
const number = value => value == null ? '—' : value.toLocaleString('zh-CN')
const time = value => value ? new Date(value * 1000).toLocaleString('zh-CN', { hour12: false }) : '暂无'
const shortTime = value => value ? new Date(value * 1000).toLocaleString('zh-CN', {
  month: '2-digit', day: '2-digit', hour: '2-digit', minute: '2-digit', second: '2-digit', hour12: false,
}) : '暂无'
const appliedDescription = computed(() => [
  securityRanges.find(item => item.value === applied.value.range)?.label,
  applied.value.ip && `IP：${applied.value.ip}`,
  applied.value.kind && securityKinds.find(item => item.value === applied.value.kind)?.label,
  applied.value.result && securityResults.find(item => item.value === applied.value.result)?.label,
].filter(Boolean).join(' · '))
function labels(row) {
  return [row.frequent && '频繁尝试', row.after_failures && '失败后成功',
    row.new_source && '新来源', row.new_browser && '新浏览器'].filter(Boolean)
}
function toggleDetails(id) { expanded.value = expanded.value === id ? null : id }
// 仅在新结果成功显示后重置列表滚动；读取失败保留当前阅读位置。
watch(data, async () => {
  expanded.value = null
  await nextTick()
  if (listViewport.value) listViewport.value.scrollTop = 0
})
onMounted(refresh)
onUnmounted(dispose)
</script>

<template>
  <section class="security-view">
    <header class="security-heading">
      <div><span class="security-kicker">ACCESS SECURITY</span><h2>安全日志</h2><p>登录验证与管理访问 · 自动保留 15 天</p></div>
      <button class="btn btn-ghost" :disabled="loading" @click="refresh"><Icon name="refresh" :size="15" />刷新</button>
    </header>

    <div v-if="data && (!data.recording_enabled || data.dropped_since_start || data.write_failed)" class="security-notice" role="status">
      <Icon name="alert" :size="17" />
      <div v-if="!data.authentication_enabled">管理 Token 未配置，当前没有鉴权保护，也不记录成功验证。</div>
      <div v-else-if="!data.recording_enabled">当前为只读模式，不写入新的安全记录。</div>
      <div v-else>日志可能不完整：本次启动后丢失 {{ number(data.dropped_since_start) }} 次记录。{{ data.write_failed ? '数据库写入异常。' : '历史丢失计数保留至重启。' }}</div>
    </div>

    <div class="security-overview">
      <div class="security-stats">
        <div><span>成功登录</span><strong>{{ number(data?.summary?.login_successes) }}<small>次</small></strong></div>
        <div><span>管理访问</span><strong>{{ number(data?.summary?.management_accesses) }}<small>次</small></strong></div>
        <div><span>验证失败</span><strong :class="{ 'security-danger': data?.summary?.failures }">{{ number(data?.summary?.failures) }}<small>次</small></strong></div>
        <div><span>需关注来源</span><strong>{{ number(data?.summary?.attention_ips) }}<small>个</small></strong></div>
      </div>
      <p class="security-overview-note">概览仅按已查询的 IP 与时间统计<span>最近成功登录：{{ time(data?.summary?.latest_login) }}</span></p>
    </div>

    <form class="security-filters" @submit.prevent="applyFilters">
      <label><span>来源 IP</span><input v-model="draft.ip" name="security-ip" placeholder="完整 IPv4 / IPv6" autocomplete="off" spellcheck="false" /></label>
      <label><span>事件类型</span><FancySelect v-model="draft.kind" :options="securityKinds" /></label>
      <label><span>验证结果</span><FancySelect v-model="draft.result" :options="securityResults" /></label>
      <label><span>时间范围</span><FancySelect v-model="draft.range" :options="securityRanges" /></label>
      <div class="security-filter-actions">
        <span class="security-pending" role="status">{{ pendingFilters ? '筛选已修改，查询后生效' : '修改筛选后点击查询' }}</span>
        <button class="btn btn-ghost" type="button" :disabled="loading" @click="resetFilters">重置</button>
        <button class="btn" :disabled="loading" type="submit"><Icon name="search" :size="15" />查询</button>
      </div>
    </form>

    <div class="security-results">
      <div class="security-list-heading">
        <div class="security-tabs" role="group" aria-label="日志视图">
          <button :class="{ active: view === 'events' }" :aria-pressed="view === 'events'" :disabled="loading" @click="changeView('events')">安全事件</button>
          <button :class="{ active: view === 'all' }" :aria-pressed="view === 'all'" :disabled="loading" @click="changeView('all')">全部访问</button>
        </div>
        <span class="security-records">{{ data ? `${number(data.total)} 条合并记录` : '等待查询' }}</span>
      </div>
      <div class="security-query-note">
        <p>{{ appliedDescription }}</p>
        <p>{{ view === 'events' ? '登录、验证失败及来源变化；普通成功访问不在此列出。' : '包括普通管理访问和后台轮询。' }}</p>
        <p v-if="data" class="security-bounds">{{ time(data.from) }} — {{ time(data.to) }}</p>
      </div>

      <div v-if="error" class="security-error" role="alert">
        <Icon name="alert" :size="16" /><span>{{ error }}{{ data ? ' 下方保留上次查询结果。' : '' }}</span>
        <button class="btn-link sm" :disabled="loading" @click="retry">重试</button>
      </div>
      <div class="security-load-status" role="status" aria-live="polite">{{ loading ? (data ? '正在更新，原记录暂时保留…' : '正在读取安全日志…') : (data ? `第 ${page} / ${pages} 页 · ${rangeStart}–${rangeEnd} 条` : '') }}</div>
      <div class="security-list-shell" :aria-busy="loading">
        <div class="security-progress" :class="{ loading }" aria-hidden="true"></div>
        <div v-if="data?.entries?.length" ref="listViewport" class="security-list" tabindex="0" role="region" aria-label="安全日志列表，可滚动">
          <table aria-label="安全日志记录">
            <colgroup><col class="col-time" /><col class="col-ip" /><col class="col-kind" /><col class="col-result" /><col class="col-flags" /><col class="col-count" /><col class="col-detail" /></colgroup>
            <thead><tr><th scope="col">最近时间</th><th scope="col">来源 IP</th><th scope="col">事件</th><th scope="col">验证结果</th><th scope="col">关注提示</th><th scope="col" class="security-count">次数</th><th scope="col"><span class="security-sr-only">详情</span></th></tr></thead>
            <tbody>
              <template v-for="row in data.entries" :key="row.id">
                <tr class="security-row" :class="{ expanded: expanded === row.id }">
                  <td class="security-row-time"><time :title="time(row.last_at)">{{ shortTime(row.last_at) }}</time></td>
                  <td class="security-ip">{{ row.client_ip || '未知来源' }}</td>
                  <td class="security-kind">{{ row.kind === 'login' ? '登录验证' : '管理访问' }}</td>
                  <td class="security-result" :class="row.success ? 'success' : 'failure'"><span><i></i>{{ row.success ? '成功' : '失败' }}</span></td>
                  <td class="security-flags"><span v-for="label in labels(row)" :key="label">{{ label }}</span><span v-if="!labels(row).length" class="security-neutral">—</span></td>
                  <td class="security-count"><span class="security-mobile-label">累计 </span>{{ number(row.count) }}</td>
                  <td class="security-detail-action"><button class="security-detail-toggle" :aria-label="`${expanded === row.id ? '收起' : '展开'} ${row.client_ip || '未知来源'} 的记录详情`" :aria-expanded="expanded === row.id" :aria-controls="`security-detail-${row.id}`" @click="toggleDetails(row.id)"><Icon name="chevron-down" :size="15" /></button></td>
                </tr>
                <tr v-if="expanded === row.id" class="security-detail-row"><td colspan="7">
                  <dl :id="`security-detail-${row.id}`" class="security-details">
                    <div><dt>首次记录</dt><dd>{{ time(row.first_at) }}</dd></div>
                    <div><dt>最近记录</dt><dd>{{ time(row.last_at) }}</dd></div>
                    <div><dt>连接 IP</dt><dd class="security-data">{{ row.peer_ip || '未知' }}</dd></div>
                    <div><dt>来源依据</dt><dd>{{ row.forwarded ? '可信代理的 X-Forwarded-For' : '直接连接地址' }}</dd></div>
                    <div class="security-detail-wide"><dt>访问接口</dt><dd class="security-data">{{ row.method }} {{ row.endpoint }}</dd></div>
                    <div class="security-detail-wide"><dt>浏览器标识</dt><dd>{{ row.user_agent || '未提供' }}</dd></div>
                  </dl>
                </td></tr>
              </template>
            </tbody>
          </table>
        </div>
        <div v-else class="security-empty"><Icon name="key" :size="23" /><p>{{ loading && !data ? '正在读取记录' : data ? '当前条件暂无记录' : '暂未获取记录' }}</p><small v-if="data">可调整筛选条件或查看全部访问</small></div>
      </div>

      <footer class="security-footer">
        <label class="security-size"><span>每页</span><FancySelect :model-value="pageSize" :options="securityPageSizes" :disabled="loading || !data" @update:model-value="changeSize" /></label>
        <nav class="security-pagination" aria-label="安全日志分页">
          <button aria-label="上一页" :disabled="loading || !data || page <= 1" @click="goPage(page - 1)"><Icon name="chevron-right" :size="14" class="security-prev" /></button>
          <template v-for="item in pageItems" :key="item">
            <button v-if="typeof item === 'number'" :class="{ active: item === page }" :aria-label="`第 ${item} 页`" :aria-current="item === page ? 'page' : undefined" :disabled="loading || !data" @click="goPage(item)">{{ item }}</button>
            <span v-else aria-hidden="true">…</span>
          </template>
          <button aria-label="下一页" :disabled="loading || !data || page >= pages" @click="goPage(page + 1)"><Icon name="chevron-right" :size="14" /></button>
        </nav>
        <form class="security-jump" @submit.prevent="goPage(jump)"><label for="security-page-jump">前往</label><input id="security-page-jump" v-model="jump" type="number" min="1" :max="pages" step="1" inputmode="numeric" :disabled="loading || !data" aria-label="跳转页码" /><button :disabled="loading || !data || !jump" type="submit">跳转</button></form>
      </footer>
    </div>

    <div class="security-help">
      <p>次数为同一分钟内的合并访问量，不等于记录条数。管理访问不重复计为登录；来源变化仅供核查，不代表账号已被入侵。</p>
      <p v-if="data && !data.trusted_proxy_configured" class="security-proxy-note">未配置信任代理，来源使用直接连接 IP；经 Nginx 访问时可能显示代理地址。</p>
      <details><summary>记录规则</summary><p>日志异步写入，通常约 1 秒后可见。新来源和新浏览器与近 15 天记录比较；频繁尝试指当前分钟及前 4 分钟合计失败至少 10 次，不自动封禁。翻页沿用查询时间范围，刷新后查看新记录。</p></details>
    </div>
  </section>
</template>

<style scoped>
.security-view { min-width: 0; display: grid; gap: 20px; container-type: inline-size; }
.security-heading { display: flex; align-items: center; justify-content: space-between; gap: 16px; }
.security-heading > div { min-width: 0; }
.security-heading > button { flex-shrink: 0; }
.security-kicker { color: var(--p700); font-size: 10px; letter-spacing: .12em; font-weight: 700; }
.security-heading h2 { margin: 7px 0; font-size: 23px; color: var(--g800); }
.security-heading p, .security-help { margin: 0; color: var(--g500); font-size: 12px; line-height: 1.7; }
.security-overview { border: 1px solid var(--line); border-radius: var(--r-card); background: var(--surface); padding: 18px 20px 12px; min-width: 0; }
.security-stats { display: grid; grid-template-columns: repeat(4, minmax(0, 1fr)); gap: 20px; }
.security-stats > div { min-width: 0; display: grid; gap: 8px; }
.security-stats span { color: var(--g500); font-size: 12px; }
.security-stats strong { color: var(--g800); font-size: 24px; line-height: 1.2; font-weight: 650; overflow-wrap: anywhere; font-variant-numeric: tabular-nums; }
.security-stats small { margin-left: 5px; color: var(--g400); font-size: 11px; font-weight: 400; }
.security-overview-note { display: flex; flex-wrap: wrap; justify-content: space-between; gap: 5px 16px; margin: 14px 0 0; padding-top: 10px; border-top: 1px solid var(--line); color: var(--g500); font-size: 11px; line-height: 1.6; }
.security-filters { display: grid; grid-template-columns: minmax(0, 1.5fr) repeat(3, minmax(0, 1fr)); gap: 12px; align-items: end; }
.security-filters > label { display: grid; gap: 7px; min-width: 0; color: var(--g600); font-size: 12px; }
.security-view input { box-sizing: border-box; min-width: 0; height: 38px; padding: 8px 11px; border: 1px solid var(--control-border); border-radius: var(--r-control); background: var(--surface-raised); color: var(--g800); font: inherit; font-size: 12px; }
.security-filters input { width: 100%; }
.security-view input::placeholder { color: var(--g400); }
.security-view input:focus-visible { outline: 2px solid var(--p500); outline-offset: 2px; }
.security-view :deep(.fancy-select), .security-view :deep(.fancy-select-btn) { min-width: 0; width: 100%; }
.security-filters :deep(.fancy-select-btn) { height: 38px; font-size: 12px; }
.security-filter-actions { grid-column: 1 / -1; display: flex; align-items: center; gap: 8px; min-width: 0; }
.security-pending { margin-right: auto; color: var(--g500); font-size: 11px; line-height: 1.5; }
.security-filter-actions .btn { flex-shrink: 0; }
.security-results { min-width: 0; }
.security-list-heading { display: flex; align-items: center; justify-content: space-between; flex-wrap: wrap; gap: 10px; }
.security-tabs { display: inline-flex; padding: 3px; gap: 3px; border-radius: var(--r-control); background: var(--surface-muted); }
.security-tabs button { padding: 7px 13px; border: 0; border-radius: calc(var(--r-control) - 3px); background: transparent; color: var(--g500); font: inherit; font-size: 12px; cursor: pointer; transition: background .15s, color .15s; }
.security-tabs button.active { background: var(--surface-raised); color: var(--p700); box-shadow: 0 1px 3px var(--line); font-weight: 600; }
.security-records, .security-query-note { font-size: 11px; color: var(--g500); }
.security-query-note { margin: 10px 0 0; line-height: 1.7; overflow-wrap: anywhere; }
.security-query-note p { margin: 0; }
.security-query-note p:first-child { color: var(--g600); }
.security-bounds { font-variant-numeric: tabular-nums; }
.security-load-status { min-height: 18px; margin: 10px 0 6px; font-size: 11px; color: var(--g500); }
.security-list-shell { border-top: 1px solid var(--line); border-bottom: 1px solid var(--line); position: relative; }
.security-progress { position: absolute; top: -1px; inset-inline: 0; height: 2px; background: var(--p500); opacity: 0; z-index: 2; pointer-events: none; }
.security-progress.loading { opacity: 1; animation: security-pulse 1.2s ease-in-out infinite alternate; }
.security-list { max-block-size: max(18rem, 56dvh); overflow: auto; scrollbar-gutter: stable; overscroll-behavior: contain; }
.security-list table { width: 100%; table-layout: fixed; border-collapse: separate; border-spacing: 0; text-align: left; font-size: 12px; color: var(--g600); }
.col-time { width: 17%; } .col-ip { width: 23%; } .col-kind { width: 12%; } .col-result { width: 12%; } .col-flags { width: 21%; } .col-count { width: 8%; } .col-detail { width: 7%; }
.security-list th { position: sticky; top: 0; z-index: 1; background: var(--surface); color: var(--g500); font-size: 11px; font-weight: 500; }
.security-list th, .security-list td { padding: 12px 9px; overflow-wrap: anywhere; border-bottom: 1px solid var(--line); }
.security-list tbody > tr:last-child td { border-bottom: 0; }
.security-list .security-row > td { background: transparent; transition: background .15s; }
.security-list .security-row:hover > td, .security-list .security-row.expanded > td { background: var(--surface-muted); }
.security-row-time { font-family: var(--font-data); font-size: 11px; font-variant-numeric: tabular-nums; line-height: 1.7; }
.security-ip, .security-data { font-family: var(--font-data); color: var(--g800); }
.security-ip { font-weight: 500; line-height: 1.5; }
.security-result > span { display: inline-flex; gap: 6px; align-items: center; white-space: nowrap; }
.security-result i { width: 5px; height: 5px; background: currentColor; border-radius: 50%; }
.success { color: var(--state-success); }
.failure, .security-stats .security-danger { color: var(--state-danger); }
.security-flags { line-height: 1.7; }
.security-flags > span { display: inline-block; margin-right: 6px; font-size: 11px; color: var(--state-warning); }
.security-flags .security-neutral { color: var(--g400); }
.security-count { text-align: right; font-variant-numeric: tabular-nums; }
.security-detail-action { text-align: right; }
.security-detail-toggle { display: inline-flex; align-items: center; justify-content: center; width: 28px; height: 28px; padding: 0; border: 0; border-radius: var(--r-control); color: var(--g500); background: transparent; cursor: pointer; }
.security-detail-toggle:hover { color: var(--p700); background: var(--surface-raised); }
.security-detail-toggle svg { transition: transform .15s; }
.security-detail-toggle[aria-expanded="true"] svg { transform: rotate(180deg); }
.security-list .security-detail-row > td { background: var(--surface-muted); }
.security-details { margin: 0; padding: 4px 5px; display: grid; grid-template-columns: repeat(2, minmax(0, 1fr)); gap: 14px 24px; font-size: 11px; line-height: 1.7; }
.security-details > div { min-width: 0; }
.security-details dt { color: var(--g500); margin-bottom: 3px; }
.security-details dd { margin: 0; color: var(--g700); overflow-wrap: anywhere; white-space: pre-wrap; }
.security-detail-wide { grid-column: 1 / -1; }
.security-mobile-label { display: none; }
.security-notice, .security-error { display: flex; align-items: start; gap: 9px; padding: 12px 14px; border-radius: var(--r-control); font-size: 12px; line-height: 1.7; }
.security-notice { color: var(--state-warning); background: var(--state-warning-soft); }
.security-error { margin-top: 12px; color: var(--state-danger); background: var(--state-danger-soft); }
.security-notice svg, .security-error svg { flex-shrink: 0; margin-top: 2px; }
.security-error button { flex-shrink: 0; margin-left: auto; }
.security-empty { padding: 40px 12px; text-align: center; color: var(--g400); font-size: 13px; }
.security-empty p { margin: 10px 0 0; }
.security-empty small { display: block; margin-top: 7px; font-size: 11px; }
.security-footer { display: flex; justify-content: space-between; flex-wrap: wrap; gap: 12px; align-items: center; padding-top: 14px; color: var(--g500); font-size: 11px; }
.security-size { display: flex; align-items: center; gap: 7px; }
.security-size :deep(.fancy-select) { width: 82px; }
.security-size :deep(.fancy-select-btn) { height: 30px; padding: 5px 8px; font-size: 11px; }
.security-pagination { display: flex; align-items: center; gap: 3px; }
.security-pagination button, .security-jump button { display: inline-flex; align-items: center; justify-content: center; min-width: 28px; height: 30px; padding: 0 7px; border: 1px solid transparent; border-radius: var(--r-control); background: transparent; color: var(--g600); font: inherit; cursor: pointer; font-variant-numeric: tabular-nums; }
.security-pagination button.active { color: var(--p700); background: var(--p100); border-color: var(--line); font-weight: 600; }
.security-pagination button:hover:not(:disabled):not(.active), .security-jump button:hover:not(:disabled) { background: var(--surface-muted); }
.security-pagination > span { padding: 0 3px; }
.security-prev { transform: rotate(180deg); }
.security-jump { display: flex; align-items: center; gap: 6px; }
.security-jump input { width: 48px; height: 30px; padding: 4px; text-align: center; appearance: textfield; -moz-appearance: textfield; }
.security-jump input::-webkit-inner-spin-button, .security-jump input::-webkit-outer-spin-button { -webkit-appearance: none; margin: 0; }
.security-jump button { border-color: var(--line); }
.security-help { font-size: 11px; }
.security-help p { margin: 0 0 6px; }
.security-help summary { width: fit-content; cursor: pointer; }
.security-help details p { margin-top: 7px; }
.security-proxy-note { color: var(--state-warning); }
.security-view button:disabled { opacity: .5; cursor: not-allowed; }
.security-view button:focus-visible, .security-list:focus-visible { outline: 2px solid var(--p500); outline-offset: 2px; }
.security-sr-only { position: absolute; width: 1px; height: 1px; padding: 0; margin: -1px; overflow: hidden; clip-path: inset(50%); white-space: nowrap; }
@keyframes security-pulse { from { opacity: .35; } to { opacity: 1; } }
@container (max-width: 760px) {
  .security-filters { grid-template-columns: repeat(2, minmax(0, 1fr)); }
  .security-list table, .security-list tbody { display: block; }
  .security-list colgroup, .security-list thead { display: none; }
  .security-row { display: grid; grid-template-columns: minmax(0, 1fr) auto auto; gap: 7px 14px; padding: 14px 5px; border-bottom: 1px solid var(--line); }
  .security-list .security-row > td, .security-list .security-row:hover > td, .security-list .security-row.expanded > td { padding: 0; border: 0; background: transparent; }
  .security-row:hover, .security-row.expanded { background: var(--surface-muted); }
  .security-ip { grid-column: 1; grid-row: 1; }
  .security-result { grid-column: 2; grid-row: 1; }
  .security-detail-action { grid-column: 3; grid-row: 1 / 3; align-self: center; }
  .security-row-time { grid-column: 1; grid-row: 3; }
  .security-kind { grid-column: 1; grid-row: 2; font-size: 11px; }
  .security-count { grid-column: 2; grid-row: 2; font-size: 11px; }
  .security-mobile-label { display: inline; }
  .security-flags { grid-column: 1 / 3; grid-row: 4; }
  .security-flags:has(.security-neutral) { display: none; }
  .security-detail-row, .security-detail-row > td { display: block; }
  .security-list .security-detail-row > td { padding: 12px; }
  .security-footer { justify-content: flex-start; }
  .security-size { margin-right: auto; }
  .security-pagination { order: 3; flex-basis: 100%; justify-content: center; }
}
@container (max-width: 420px) {
  .security-stats { grid-template-columns: repeat(2, minmax(0, 1fr)); gap: 18px; }
  .security-overview { padding: 16px; }
  .security-heading h2 { font-size: 21px; }
  .security-details { grid-template-columns: minmax(0, 1fr); }
  .security-filters > label:first-child { grid-column: 1 / -1; }
  .security-filters > label:last-of-type { grid-column: 1 / -1; }
  .security-filter-actions { flex-wrap: wrap; }
  .security-pending { flex-basis: 100%; }
  .security-filter-actions .btn { flex: 1; justify-content: center; }
}
@media (prefers-reduced-motion: reduce) {
  .security-progress.loading { animation: none; }
  .security-view *, .security-detail-toggle svg { transition: none; }
}
</style>
