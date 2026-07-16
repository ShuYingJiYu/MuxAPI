<script setup>
import { ref, reactive, onMounted, onUnmounted, computed } from 'vue'
import Icon from './Icon.vue'
import Fence from './Fence.vue'
import FancySelect from './FancySelect.vue'
import { api } from './api.js'

const page = ref('overview')      // overview | groups | upstreams | monitors
const detailGroup = ref(null)     // 进入分组详情时设置

const groups = ref([])
const upstreams = ref([])         // 全局上游池
const members = ref([])           // 当前详情分组的成员
const keys = ref([])              // 当前详情分组的密钥
const monitors = ref([])          // 监控项（含探测快照）
const err = ref('')
const loggedIn = ref(!!api.getToken())
const loginForm = reactive({ token: api.getToken() })

async function loadGroups() { groups.value = (await api.groups()) || [] }
async function loadUpstreams() { upstreams.value = (await api.upstreams()) || [] }
async function loadMonitors() { monitors.value = (await api.monitors()) || [] }

// —— 监控卡片拖拽排序（仅监控管理页）——
const dragId = ref(null)   // 正在拖的监控 id
const dragOverId = ref(null)
function onDragStart(m, e) {
  dragId.value = m.id
  e.dataTransfer.effectAllowed = 'move'
}
function onDragOver(m, e) {
  e.preventDefault()
  if (m.id !== dragId.value) dragOverId.value = m.id
}
function onDrop(target) {
  const from = monitors.value.findIndex(x => x.id === dragId.value)
  const to = monitors.value.findIndex(x => x.id === target.id)
  if (from < 0 || to < 0 || from === to) { dragId.value = dragOverId.value = null; return }
  const arr = monitors.value.slice()
  const [moved] = arr.splice(from, 1)
  arr.splice(to, 0, moved)
  monitors.value = arr // 乐观更新
  dragId.value = dragOverId.value = null
  // 持久化失败时主动重拉还原顺序（而非等下次轮询才校正）
  guard(() => api.reorderMonitors(arr.map(x => x.id)).catch(e => { loadMonitors().catch(() => {}); throw e }))
}
function onDragEnd() { dragId.value = dragOverId.value = null }

// 看板汇总：监控项可用性概览（顶部状态条用）
const summary = computed(() => {
  const ms = monitors.value.filter(m => m.enabled)
  const st = m => m.snapshot.state
  const down = ms.filter(m => st(m) === 'DOWN').length
  const degraded = ms.filter(m => st(m) === 'DEGRADED').length
  const rated = ms.filter(m => m.snapshot.reqs > 0)
  const rate = rated.length ? rated.reduce((a, m) => a + m.snapshot.succ_rate, 0) / rated.length : 1
  return {
    total: ms.length, down, degraded,
    up: ms.length - down - degraded, rate,
    allOk: down === 0 && degraded === 0,
  }
})
// 单项「立即探测」：探完用返回的快照原地更新该卡片
// 用 Set 记录正在探测的卡片 id，支持多卡并发互不串台
const probing = reactive(new Set())
async function probeOne(m) {
  probing.add(m.id)
  try {
    const sn = await api.probeMonitor(m.id)
    m.snapshot = sn
  } finally { probing.delete(m.id) }
}

// ===== 总览：按上游分组的模型探活状态 =====
// 数据源 = monitors（每项 = 一个 (上游,模型)，含探测快照）。
// 监控只是探活子集，各上游监控的模型数不同 —— 故不拼矩阵，
// 而是按上游分组，每组平铺它实际监控的模型芯片（有几个显几个）。
const stateRank = { DOWN: 0, DEGRADED: 1, OK: 2 } // 故障置顶用
const matrix = computed(() => {
  const byUp = new Map()
  for (const m of monitors.value) {
    if (!byUp.has(m.upstream_id)) byUp.set(m.upstream_id, { id: m.upstream_id, name: m.upstream_name, items: [] })
    byUp.get(m.upstream_id).items.push(m)
  }
  const rows = [...byUp.values()].map(g => {
    // 组内健康汇总 + 模型按状态排序（故障/降级置顶，停用沉底）
    let down = 0, degraded = 0
    for (const m of g.items) {
      if (!m.enabled) continue
      const s = m.snapshot.state
      if (s === 'DOWN') down++
      else if (s === 'DEGRADED') degraded++
    }
    // 排序副本，避免在 computed 内原地改 monitors 派生数组（保 computed 纯净）
    const items = [...g.items].sort((a, b) => {
      if (a.enabled !== b.enabled) return a.enabled ? -1 : 1
      const ra = stateRank[a.snapshot.state] ?? 3, rb = stateRank[b.snapshot.state] ?? 3
      return ra - rb || a.model.localeCompare(b.model)
    })
    return { ...g, items, down, degraded, worst: down ? 0 : degraded ? 1 : 2 }
  })
  // 组排序：有故障的上游置顶，其次降级，再按名字
  rows.sort((a, b) => a.worst - b.worst || a.name.localeCompare(b.name))
  return { rows }
})
// 总览汇总卡（粉/薄荷/黄/紫 四色块）
const ovSummary = computed(() => {
  const ms = monitors.value.filter(m => m.enabled)
  const st = m => m.snapshot.state
  const down = ms.filter(m => st(m) === 'DOWN').length
  const degraded = ms.filter(m => st(m) === 'DEGRADED').length
  const rated = ms.filter(m => m.snapshot.reqs > 0)
  const rate = rated.length ? rated.reduce((a, m) => a + m.snapshot.succ_rate, 0) / rated.length : 1
  const lats = rated.filter(m => m.snapshot.avg_ms).map(m => m.snapshot.avg_ms)
  const avgLat = lats.length ? Math.round(lats.reduce((a, b) => a + b, 0) / lats.length) : 0
  return {
    upstreams: matrix.value.rows.length,
    combos: ms.length,
    models: new Set(ms.map(m => m.model)).size,
    healthy: ms.length - down - degraded, degraded, down,
    rate, avgLat,
  }
})
// 点格子 → 抽屉看详情（含趋势 + 立即探测）
// 只存 id，抽屉对象用 computed 从 monitors 实时 find——
// 这样 60s 轮询整替 monitors 后，抽屉内容随之刷新而非冻结在旧引用。
const cellDrawerId = ref(null)
const cellDrawer = computed(() => cellDrawerId.value == null ? null : monitors.value.find(x => x.id === cellDrawerId.value) || null)
function openCell(m) { if (m) cellDrawerId.value = m.id }
function closeCell() { cellDrawerId.value = null }
async function probeCell() {
  const m = cellDrawer.value
  if (!m) return
  await probeOne(m)
}
// 分组详情加载守卫：每次切换详情/返回都自增 epoch，
// 参数化加载(members/keys)返回时校验 epoch 未变才写入，避免快速切换短暂错配。
let loadEpoch = 0
async function loadMembers(gid) {
  const ep = loadEpoch
  const data = (await api.members(gid)) || []
  if (ep === loadEpoch) members.value = data
}
// 流量分配预估：把生效层成员的 route_preview 按模型重组（同模型各成员占比加和≈100）。
const routeDist = computed(() => {
  const byModel = new Map()
  for (const m of members.value) {
    if (!m.effective || !m.route_preview) continue
    for (const rp of m.route_preview) {
      if (!byModel.has(rp.model)) byModel.set(rp.model, [])
      byModel.get(rp.model).push({ name: m.name, ...rp })
    }
  }
  return [...byModel.entries()]
    .map(([model, rows]) => ({ model, rows: rows.sort((a, b) => b.share_pct - a.share_pct) }))
    .sort((a, b) => a.model.localeCompare(b.model))
})
const mhTitle = mh => mh.model + ' · 当前渠道暂不支持，5 分钟后重新尝试'
async function loadDetail(gid) {
  const ep = loadEpoch
  await loadMembers(gid)
  const ks = (await api.keys(gid)) || []
  if (ep === loadEpoch) keys.value = ks
}

async function guard(fn) {
  err.value = ''
  try { await fn() } catch (e) {
    if (e.status === 401) {
      loggedIn.value = false
      api.clearToken()
      err.value = '未授权，请输入管理 Token'
      return
    }
    err.value = String(e.message || e)
  }
}

onMounted(() => {
  if (loggedIn.value) guard(async () => { await loadUpstreams(); await loadMonitors(); await loadSettings(); startMonPoll() })
})

// 看板自动刷新：探测间隔 5min，这里每 60s 拉一次快照即可，离开即停
let monTimer = null
function startMonPoll() {
  stopMonPoll()
  monTimer = setInterval(() => { if (!dragId.value) loadMonitors().catch(() => {}) }, 60000)
}
function stopMonPoll() { if (monTimer) { clearInterval(monTimer); monTimer = null } }

// 运行时状态轮询：分组列表/分组详情/上游池页，每 8s 刷新健康，离开即停。
let rtTimer = null
function startRtPoll(fn) {
  stopRtPoll()
  rtTimer = setInterval(() => { fn().catch(() => {}) }, 8000)
}
function stopRtPoll() { if (rtTimer) { clearInterval(rtTimer); rtTimer = null } }
function stopAllPoll() { stopMonPoll(); stopRtPoll(); stopLogPoll() }
onUnmounted(() => { stopAllPoll() })

function go(p) {
  page.value = p; detailGroup.value = null
  cellDrawerId.value = null; logDetail.value = null
  loadEpoch++   // 离开详情，作废在途的 members/keys 加载
  stopAllPoll()
  guard(async () => {
    if (p === 'overview') { await loadUpstreams(); await loadMonitors(); startMonPoll() }
    else if (p === 'groups') { await loadGroups(); startRtPoll(loadGroups) }
    else if (p === 'upstreams') { await loadUpstreams(); startRtPoll(loadUpstreams) }
    else if (p === 'monitors') { await loadUpstreams(); await loadMonitors(); startMonPoll() }
    else if (p === 'logs') { await loadLogOptions(); await loadLogs(true); startLogPoll() }
    else if (p === 'settings') { await loadSettings() }
  })
}
function openDetail(g) {
  detailGroup.value = g
  loadEpoch++   // 切入新分组详情，作废上一组在途加载
  stopAllPoll()
  guard(async () => { await loadUpstreams(); await loadDetail(g.id); startRtPoll(() => loadMembers(g.id)) })
}
function backToGroups() {
  detailGroup.value = null; loadEpoch++; stopAllPoll()
  guard(async () => { await loadGroups(); startRtPoll(loadGroups) })
}

// 上游池里未加入当前分组的（供"添加成员"下拉）
const memberIds = computed(() => new Set(members.value.map(m => m.upstream_id)))
const addable = computed(() => upstreams.value.filter(u => !memberIds.value.has(u.id)))

const pages = {
  overview: { title: '总览', desc: '上游 × 模型健康矩阵，一屏看全所有渠道的实时状态' },
  groups: { title: '分组管理', desc: '每个分组是一个独立的调度池，拥有自己的上游与接入密钥' },
  upstreams: { title: '上游池', desc: '全局上游凭证，可被多个分组复用' },
  monitors: { title: '监控看板', desc: '为「渠道+模型」配置监控项，主动探测成功率与延迟' },
  logs: { title: '请求记录', desc: '每一次转发请求的真实去向：模型 → 选中渠道 → 状态 → 延迟' },
  settings: { title: '设置', desc: '运行时配置，保存后即时生效（无需重启）' },
}

// --- 弹窗状态 ---
const dlg = reactive({ type: '', form: {} })
function closeDlg() { dlg.type = '' }

// 通用确认弹窗：confirm(消息, 危险操作回调)
const confirmState = reactive({ show: false, msg: '', onOk: null })
function ask(msg, onOk) { confirmState.show = true; confirmState.msg = msg; confirmState.onOk = onOk }
function confirmOk() { confirmState.show = false; confirmState.onOk?.() }

function newGroup() { dlg.type = 'group'; dlg.form = { name: '', description: '' } }
function editGroup(g) { dlg.type = 'group'; dlg.form = { id: g.id, name: g.name, description: g.description } }
function saveGroup() {
  guard(async () => {
    const f = { ...dlg.form }
    if (f.id) await api.updateGroup(f.id, f)
    else await api.createGroup(f)
    closeDlg(); await loadGroups()
  })
}

const protocolOptions = [
  { value: 'passthrough', label: '透传' },
  { value: 'openai', label: 'OpenAI Chat Completions' },
  { value: 'openai-response', label: 'OpenAI Responses' },
  { value: 'claude', label: 'Anthropic Messages' },
  { value: 'codex', label: 'Codex Responses' },
]
const protocolLabels = Object.fromEntries(protocolOptions.map(option => [option.value, option.label]))
function protocolLabel(protocol) { return protocolLabels[protocol || 'passthrough'] || protocol }
function newUpstream() { dlg.type = 'upstream'; dlg.form = { name: '', base_url: '', api_key: '', proxy: '', protocol: 'passthrough', enabled: true, channel_probe: false } }
function editUpstream(u) { dlg.type = 'upstream'; dlg.form = { ...u, protocol: u.protocol || 'passthrough', api_key: '' } }
function saveUpstream() {
  guard(async () => {
    const f = { ...dlg.form }
    if (f.id) await api.updateUpstream(f.id, f)
    else await api.createUpstream(f)
    closeDlg(); await loadUpstreams()
  })
}
function delUpstream(u) {
  ask(`删除上游「${u.name}」？将同时从所有分组移除。`, () =>
    guard(async () => { await api.deleteUpstream(u.id); await loadUpstreams() }))
}

const upstreamRunFilter = ref('')
const upstreamEnabledFilter = ref('')
const upstreamDragId = ref(null)
const upstreamDragOverId = ref(null)
const upstreamRunOptions = [
  { value: '', label: '运行状态：全部' },
  { value: 'CLOSED', label: '正常' },
  { value: 'HALF_OPEN', label: '半开' },
  { value: 'OPEN', label: '熔断' },
  { value: 'UNPROBED', label: '待探测' },
]
const upstreamEnabledOptions = [
  { value: '', label: '启停状态：全部' },
  { value: 'enabled', label: '启用' },
  { value: 'disabled', label: '停用' },
]

const upstreamRunValue = u => rtUnprobed(u.health) ? 'UNPROBED' : (u.health?.state || '')
const upstreamsFiltered = computed(() => upstreams.value.filter(u => {
  if (upstreamEnabledFilter.value === 'enabled' && !u.enabled) return false
  if (upstreamEnabledFilter.value === 'disabled' && u.enabled) return false
  if (upstreamRunFilter.value && upstreamRunValue(u) !== upstreamRunFilter.value) return false
  return true
}))

function onUpstreamDragStart(u, e) {
  upstreamDragId.value = u.id
  e.dataTransfer.effectAllowed = 'move'
}
function onUpstreamDragOver(u, e) {
  e.preventDefault()
  if (u.id !== upstreamDragId.value) upstreamDragOverId.value = u.id
}
function onUpstreamDrop(target) {
  const visible = upstreamsFiltered.value
  const from = visible.findIndex(x => x.id === upstreamDragId.value)
  const to = visible.findIndex(x => x.id === target.id)
  if (from < 0 || to < 0 || from === to) { onUpstreamDragEnd(); return }
  const reorderedVisible = visible.slice()
  const [moved] = reorderedVisible.splice(from, 1)
  reorderedVisible.splice(to, 0, moved)
  const visibleIds = new Set(visible.map(x => x.id))
  let vi = 0
  const arr = upstreams.value.map(u => visibleIds.has(u.id) ? reorderedVisible[vi++] : u)
  upstreams.value = arr
  onUpstreamDragEnd()
  guard(() => api.reorderUpstreams(arr.map(x => x.id)).catch(e => { loadUpstreams().catch(() => {}); throw e }))
}
function onUpstreamDragEnd() { upstreamDragId.value = upstreamDragOverId.value = null }

// 连通测试 + 模型列表
const testState = reactive({
  show: false, id: 0, name: '',
  modelsLoading: false, models: [], model: '', modelsErr: '',
  running: false, output: '', status: null, // status: {ok,latency_ms,code,error}
})
// 打开测试弹窗：先拉模型列表供选择
function testUpstream(u) {
  Object.assign(testState, {
    show: true, id: u.id, name: u.name,
    modelsLoading: true, models: [], model: '', modelsErr: '',
    running: false, output: '', status: null,
  })
  api.testUpstream(u.id)
    .then(r => {
      testState.models = r.models || []
      testState.model = testState.models[0] || 'gpt-5.5'
      if (!r.ok && r.error) testState.modelsErr = r.error
    })
    .catch(e => { testState.modelsErr = String(e.message || e); testState.model = 'gpt-5.5' })
    .finally(() => { testState.modelsLoading = false })
}
// 真实对话测试：流式回显上游回复
async function runTest() {
  if (!testState.model || testState.running) return
  testState.running = true; testState.output = ''; testState.status = null
  try {
    await api.testUpstreamStream(testState.id, testState.model, e => {
      if (e.type === 'content') testState.output += e.text
      else if (e.type === 'test_complete') testState.status = { ok: true, latency_ms: e.latency_ms }
      else if (e.type === 'error') testState.status = { ok: false, code: e.status, error: e.error, latency_ms: e.latency_ms }
    })
  } catch (e) {
    testState.status = { ok: false, error: String(e.message || e) }
  } finally {
    testState.running = false
    // 流正常结束但没收到 test_complete：
    //  - 有 content → 视为「部分成功」（连上了且有回复，只是缺收尾事件）
    //  - 完全无数据 → 不能判成功，标记失败避免假阳性
    if (!testState.status) {
      testState.status = testState.output
        ? { ok: true, partial: true }
        : { ok: false, error: '上游无任何响应（未收到内容或完成事件）' }
    }
  }
}

function delGroup(g) {
  ask(`删除分组「${g.name}」？其成员关联与密钥都会被清除。`, () =>
    guard(async () => { await api.deleteGroup(g.id); await loadGroups() }))
}

// 组成员
function addMember() { dlg.type = 'member'; dlg.form = { upstream_id: addable.value[0]?.id, priority: 50, weight: 1 } }
function saveMember() {
  guard(async () => {
    await api.addMember(detailGroup.value.id, { ...dlg.form, upstream_id: Number(dlg.form.upstream_id) })
    closeDlg(); await loadDetail(detailGroup.value.id)
  })
}
function editMember(m) { dlg.type = 'member'; dlg.form = { upstream_id: m.upstream_id, priority: m.priority, weight: m.weight, locked: true } }
function removeMember(m) {
  guard(async () => { await api.removeMember(detailGroup.value.id, m.upstream_id); await loadDetail(detailGroup.value.id) })
}
function toggleMember(m) {
  guard(async () => { await api.setMemberEnabled(detailGroup.value.id, m.upstream_id, !m.group_enabled); await loadDetail(detailGroup.value.id) })
}

// 密钥
const newKey = ref('')   // 生成后明文展示一次
const copied = ref(0)    // 刚点击复制的密钥 id（短暂提示用）
const logRetention = ref('')           // 请求记录保留天数
const effectiveLogRetention = ref('')
const logRetentionSource = ref('')
const alertWebhook = ref('')           // 告警 Webhook URL(空=关闭)
const alertDebounce = ref('')          // 告警去抖窗口
const effectiveAlertWebhook = ref('')
const effectiveAlertDebounce = ref('')
const alertWebhookSource = ref('')
const alertDebounceSource = ref('')
const routeSmart = ref('on')               // 兼容旧设置字段；调度固定使用标准 P2C
const firstResponseTimeoutSec = ref('')    // 首字节超时(秒，后端存毫秒)
const effectiveFirstResponseTimeoutMs = ref('')
const firstResponseTimeoutSource = ref('')
const apiBase = location.origin    // 当前访问地址，用于展示客户端接入端点
const settingsSaved = ref(false)
const settingsSection = ref('logs')  // 设置页左锚点：logs | alert | endpoint
function createKey() { dlg.type = 'keygen'; dlg.form = { name: '' } }
function saveKey() {
  guard(async () => {
    const r = await api.createKey(detailGroup.value.id, dlg.form.name || '')
    closeDlg()
    newKey.value = r.key
    await loadDetail(detailGroup.value.id)
  })
}
function toggleKey(k) {
  guard(async () => { await api.setKeyEnabled(k.id, !k.enabled); await loadDetail(detailGroup.value.id) })
}
function delKey(k) {
  ask('吊销该密钥？使用它的客户端将立即失效。', () =>
    guard(async () => { await api.deleteKey(k.id); await loadDetail(detailGroup.value.id) }))
}
function copyKey() { navigator.clipboard?.writeText(newKey.value); newKey.value = '' }
function copyText(t, id) {
  navigator.clipboard?.writeText(t)
  copied.value = id
  setTimeout(() => { if (copied.value === id) copied.value = 0 }, 1200)
}
async function loadSettings() {
  const s = await api.getSettings()
  effectiveLogRetention.value = s.effective_log_retention || ''
  effectiveAlertWebhook.value = s.effective_alert_webhook || ''
  effectiveAlertDebounce.value = s.effective_alert_debounce || ''
  logRetention.value = s.log_retention || effectiveLogRetention.value
  alertWebhook.value = s.alert_webhook || ''
  alertDebounce.value = s.alert_debounce || effectiveAlertDebounce.value
  logRetentionSource.value = s.log_retention_source || ''
  alertWebhookSource.value = s.alert_webhook_source || ''
  alertDebounceSource.value = s.alert_debounce_source || ''
  routeSmart.value = s.route_smart || 'on'
  effectiveFirstResponseTimeoutMs.value = s.effective_first_response_timeout_ms || ''
  // 后端存毫秒，UI 展示秒：优先取页面设置值，否则用 effective
  firstResponseTimeoutSec.value = String(Math.round((Number(s.first_response_timeout_ms || s.effective_first_response_timeout_ms) || 120000) / 1000))
  firstResponseTimeoutSource.value = s.first_response_timeout_ms_source || ''
}
const sourceText = s => s === 'settings' ? '页面设置' : '默认值'
// 设置页左锚点点击：滚动到对应 section 并高亮
function gotoSection(id) {
  settingsSection.value = id
  document.getElementById('set-' + id)?.scrollIntoView({ behavior: 'smooth', block: 'start' })
}
function saveSettings() {
  guard(async () => {
    await api.saveSettings({
      log_retention: String(logRetention.value),
      alert_webhook: alertWebhook.value,
      alert_debounce: alertDebounce.value,
      route_smart: routeSmart.value,
      first_response_timeout_ms: String((Number(firstResponseTimeoutSec.value) || 120) * 1000),
    })
    await loadSettings()
    settingsSaved.value = true
    setTimeout(() => { settingsSaved.value = false }, 1500)
  })
}

// 监控项状态映射
const stateLabel = s => ({ OK: '正常', DEGRADED: '降级', DOWN: '故障' }[s] || '无数据')
const dotClass = s => ({ OK: 'closed', DEGRADED: 'half', DOWN: 'open' }[s] || 'nodata')

// 路由熔断状态映射（成员表/上游池运行时列用）。未探测过(last_probe=0 且无请求)显示「待探测」。
const rtUnprobed = h => h && h.state === 'CLOSED' && !h.last_probe && !h.reqs
const rtLabel = h => rtUnprobed(h) ? '待探测' : ({ CLOSED: '正常', HALF_OPEN: '半开', OPEN: '熔断' }[h?.state] || '待探测')
const rtClass = h => rtUnprobed(h) ? 'nodata' : ({ CLOSED: 'closed', HALF_OPEN: 'half', OPEN: 'open' }[h?.state] || 'nodata')
const rtRate = h => (h && h.reqs) ? (h.succ_rate * 100).toFixed(0) + '%' : '—'

// 模型徽章只表示短期能力排除，不再表示独立熔断状态。
const mhClass = mh => mh?.state === 'UNSUPPORTED' ? 'open' : 'nodata'
const visibleDots = item => item?.model_health || []

// 分组卡片：生效渠道文本 + 健康概览文本
const effText = rt => (rt && rt.effective && rt.effective.length) ? rt.effective.join(' / ') : '无可用'
// 分组卡片成功率数值配色，阈值与栅栏一致：绿≥95 / 黄≥80 / 红<80
function rateClass(g) {
  if (!g.recent_total) return 'rate-none'
  const r = Number(g.success_rate) || 0
  return r >= 95 ? 'rate-ok' : r >= 80 ? 'rate-warn' : 'rate-bad'
}
const healthSummary = rt => {
  if (!rt || !rt.total) return '无成员'
  const parts = [`${rt.normal} 正常`]
  if (rt.half_open) parts.push(`${rt.half_open} 半开`)
  if (rt.open) parts.push(`${rt.open} 熔断`)
  return parts.join(' · ')
}
const upName = id => upstreams.value.find(u => u.id === id)?.name || ('#' + id)
const monTitle = m => m.name || (m.upstream_name + ' · ' + m.model)
const initial = m => (m.model || '?').replace(/[^a-zA-Z0-9]/g, '').charAt(0).toUpperCase() || '?'
const sinceText = ts => {
  if (!ts) return '从未'
  const s = Math.floor(Date.now() / 1000) - ts
  if (s < 60) return s + ' 秒前'
  if (s < 3600) return Math.floor(s / 60) + ' 分钟前'
  return Math.floor(s / 3600) + ' 小时前'
}

// --- 请求记录（标准分页 + 服务端筛选）---
const logs = ref([])
const logPageSize = ref(20)
const logCurrentPage = ref(1)
const logLoading = ref(false)
const logDetail = ref(null)
const logDetailLoading = ref(false)
const logStats = ref({})
const logSearch = ref('')
const logFTime = ref('24h')
const logFGroup = ref('')
const logFModel = ref('')
const logFStatus = ref('')
const logFUpstream = ref('')
const logFKey = ref('')
const logFEndpoint = ref('')
const logFErrorKind = ref('')
const logFStream = ref('')
const logFSlow = ref('')
const logFRetried = ref(false)
const logAutoRefresh = ref(false)
const logMoreFilters = ref(false)
const logModelOpts = ref([])
const logGroupOpts = ref([])
const logKeyOpts = ref([])
const logEndpointOpts = ref([])
const logErrorKindOpts = ref([])
const logUpstreamOpts = ref([])
const logPageSizeOptions = [
  { value: 20, label: '20 条' },
  { value: 50, label: '50 条' },
  { value: 100, label: '100 条' },
]
const logTotalPages = computed(() => Math.max(1, Math.ceil((Number(logStats.value.total) || 0) / Number(logPageSize.value))))
const logPageItems = computed(() => {
  const total = logTotalPages.value
  const current = Math.min(logCurrentPage.value, total)
  if (total <= 7) return Array.from({ length: total }, (_, index) => ({ type: 'page', value: index + 1, key: `page-${index + 1}` }))
  let start = Math.max(2, current - 1)
  let end = Math.min(total - 1, current + 1)
  if (current <= 4) end = 5
  if (current >= total - 3) start = total - 4
  const items = [{ type: 'page', value: 1, key: 'page-1' }]
  if (start > 2) items.push({ type: 'ellipsis', key: 'ellipsis-left' })
  for (let value = start; value <= end; value++) items.push({ type: 'page', value, key: `page-${value}` })
  if (end < total - 1) items.push({ type: 'ellipsis', key: 'ellipsis-right' })
  items.push({ type: 'page', value: total, key: `page-${total}` })
  return items
})
const logGroupSelectOptions = computed(() => [{ value: '', label: '全部分组' }, ...logGroupOpts.value.map(g => ({ value: g, label: g }))])
const logModelSelectOptions = computed(() => [{ value: '', label: '全部模型' }, ...logModelOpts.value.map(m => ({ value: m, label: m }))])
const logKeySelectOptions = computed(() => [{ value: '', label: '全部密钥' }, ...logKeyOpts.value.map(k => ({ value: k, label: k }))])
const logEndpointSelectOptions = computed(() => [{ value: '', label: '全部端点' }, ...logEndpointOpts.value.map(e => ({ value: e, label: fmtEndpoint(e) }))])
const logErrorSelectOptions = computed(() => [{ value: '', label: '全部错误' }, ...logErrorKindOpts.value.map(e => ({ value: e, label: errorKindText(e) }))])
const logUpstreamSelectOptions = computed(() => [{ value: '', label: '全部渠道' }, ...logUpstreamOpts.value.map(u => ({ value: u.id, label: u.name }))])
const logTimeOptions = [
  { value: '1h', label: '最近 1 小时' },
  { value: '24h', label: '最近 24 小时' },
  { value: '7d', label: '最近 7 天' },
  { value: 'all', label: '全部记录' },
]
const logStatusOptions = [
  { value: '', label: '全部状态' },
  { value: 'direct_success', label: '直接成功' },
  { value: 'failover_success', label: '切换后成功' },
  { value: 'failed', label: '失败' },
  { value: 'partial', label: '流中断' },
  { value: 'canceled', label: '客户端取消' },
  { value: 'client_error', label: '请求错误' },
]
const logStreamOptions = [
  { value: '', label: '全部模式' },
  { value: 'stream', label: '流式请求' },
  { value: 'nonstream', label: '非流式请求' },
]
const logSlowOptions = [
  { value: '', label: '全部耗时' },
  { value: 5000, label: '耗时 ≥ 5 秒' },
  { value: 30000, label: '耗时 ≥ 30 秒' },
  { value: 120000, label: '耗时 ≥ 2 分钟' },
]

function logFilterParams() {
  const now = Math.floor(Date.now() / 1000)
  const since = ({ '1h': now - 3600, '24h': now - 86400, '7d': now - 7 * 86400 })[logFTime.value] || ''
  return {
    q: logSearch.value.trim(), since, group: logFGroup.value, model: logFModel.value,
    status: logFStatus.value, upstream_id: logFUpstream.value, key: logFKey.value,
    endpoint: logFEndpoint.value, error_kind: logFErrorKind.value, stream: logFStream.value,
    slow_ms: logFSlow.value, retried: logFRetried.value,
  }
}

let logLoadEpoch = 0
async function fetchLogPage(targetPage, refreshStats) {
  const epoch = ++logLoadEpoch
  logLoading.value = true
  try {
    const pageSize = Number(logPageSize.value)
    const params = { ...logFilterParams(), offset: (targetPage - 1) * pageSize, limit: pageSize }
    const [page, stats] = refreshStats
      ? await Promise.all([api.logs(params), api.logStats(params)])
      : [await api.logs(params), null]
    if (epoch !== logLoadEpoch) return false
    const rows = (page && page.entries) || []
    logs.value = rows
    logCurrentPage.value = targetPage
    if (stats) logStats.value = stats
    return true
  } finally {
    if (epoch === logLoadEpoch) logLoading.value = false
  }
}
async function loadLogs(resetPagination = false) {
  return fetchLogPage(resetPagination ? 1 : logCurrentPage.value, true)
}
async function goLogPage(targetPage) {
  if (logLoading.value) return
  const normalized = Math.max(1, Math.min(Number(targetPage) || 1, logTotalPages.value))
  if (normalized === logCurrentPage.value) return
  await fetchLogPage(normalized, false)
}
function onLogPageSizeChange() { guard(() => loadLogs(true)) }
async function loadLogOptions() {
  const o = await api.logOptions()
  logModelOpts.value = (o && o.models) || []
  logGroupOpts.value = (o && o.groups) || []
  logKeyOpts.value = (o && o.keys) || []
  logEndpointOpts.value = (o && o.endpoints) || []
  logErrorKindOpts.value = (o && o.error_kinds) || []
  logUpstreamOpts.value = (o && o.upstreams) || []
}
function onLogFilterChange() { guard(() => loadLogs(true)) }
function resetLogFilters() {
  logSearch.value = ''; logFTime.value = '24h'; logFGroup.value = ''; logFModel.value = ''
  logFStatus.value = ''; logFUpstream.value = ''; logFKey.value = ''; logFEndpoint.value = ''
  logFErrorKind.value = ''; logFStream.value = ''; logFSlow.value = ''; logFRetried.value = false
  logMoreFilters.value = false
  onLogFilterChange()
}
let logDetailEpoch = 0
async function openLogDetail(entry) {
  const epoch = ++logDetailEpoch
  logDetailLoading.value = true
  logDetail.value = entry
  try {
    const detail = await api.logDetail(entry.id)
    if (epoch === logDetailEpoch) logDetail.value = detail
  } finally {
    if (epoch === logDetailEpoch) logDetailLoading.value = false
  }
}
function closeLogDetail() { logDetailEpoch++; logDetail.value = null; logDetailLoading.value = false }

let logTimer = null
function startLogPoll() {
  stopLogPoll()
  if (!logAutoRefresh.value) return
  logTimer = setInterval(() => {
    if (page.value === 'logs' && logCurrentPage.value === 1 && !logLoading.value && !logDetail.value) loadLogs(false).catch(() => {})
  }, 10000)
}
function stopLogPoll() { if (logTimer) { clearInterval(logTimer); logTimer = null } }
function toggleLogAutoRefresh() { startLogPoll() }

const logActiveFilters = computed(() => [logSearch.value, logFGroup.value, logFModel.value, logFStatus.value,
  logFUpstream.value, logFKey.value, logFEndpoint.value, logFErrorKind.value, logFStream.value,
  logFSlow.value, logFRetried.value].filter(Boolean).length)
const logAdvancedFilters = computed(() => [logFGroup.value, logFModel.value, logFKey.value, logFEndpoint.value,
  logFErrorKind.value, logFStream.value, logFSlow.value, logFRetried.value].filter(Boolean).length)
const requestShort = id => id ? id.slice(0, 8) : '—'
const fmtMs = ms => {
  const value = Number(ms) || 0
  if (!value) return '—'
  return value >= 1000 ? (value / 1000).toFixed(1) + 's' : value + 'ms'
}
const fmtBytes = bytes => {
  const value = Number(bytes) || 0
  if (!value) return '—'
  if (value >= 1048576) return (value / 1048576).toFixed(1) + ' MB'
  if (value >= 1024) return (value / 1024).toFixed(1) + ' KB'
  return value + ' B'
}
const fmtNum = value => new Intl.NumberFormat('zh-CN').format(Number(value) || 0)
const outcomeText = outcome => ({
  success: '成功', failed: '失败', canceled: '已取消', partial: '流中断',
  client_error: '请求错误', unsupported: '不支持', unavailable: '无可用渠道',
}[outcome] || outcome || '未知')
const requestOutcomeText = entry => entry.outcome === 'success' && entry.attempt_count > 1 ? '切换后成功' : outcomeText(entry.outcome)
const requestOutcomeClass = entry => entry.outcome === 'success' ? (entry.attempt_count > 1 ? 'warn' : 'ok')
  : entry.outcome === 'canceled' || entry.outcome === 'client_error' ? 'muted' : 'fail'
const errorKindText = kind => ({
  request_build: '请求构建失败', upstream_network: '上游网络失败', first_response_timeout: '首字节超时',
  upstream_read: '上游读取失败', model_unsupported: '模型不支持', client_request: '客户端请求错误',
  upstream_http: '上游 HTTP 错误', downstream_write: '下游写入失败', client_canceled: '客户端取消',
  empty_response: '上游空响应', error_payload: '成功状态错误体', upstream_disconnect: '上游断流',
  no_upstream: '无可用渠道', upstream_error: '上游失败', auth: '接入鉴权失败',
  request_too_large: '请求体过大', request_read: '请求体读取失败',
}[kind] || kind || '—')
const errorSourceText = source => ({ upstream: '上游', client: '客户端', gateway: 'MuxAPI' }[source] || source || '—')
const selectionText = reason => ({ initial: '首次选择', failover: '故障切换', recovery_trial: '恢复验证' }[reason] || reason || '—')
const streamStateText = entry => {
  if (!entry.stream) return '非流式'
  if (entry.stream_completed) return entry.last_event ? `完整 · ${entry.last_event}` : '完整结束'
  return entry.last_event ? `EOF · ${entry.last_event}` : '未见完成事件'
}
// 绝对时间 MM-DD HH:MM:SS（请求记录看具体时刻，不用相对）
const fmtTime = ts => {
  if (!ts) return '—'
  const d = new Date(ts * 1000), p = n => String(n).padStart(2, '0')
  return `${p(d.getMonth() + 1)}-${p(d.getDate())} ${p(d.getHours())}:${p(d.getMinutes())}:${p(d.getSeconds())}`
}
// 完整时间(含年份)，供请求记录 hover 查看
const fmtTimeFull = ts => {
  if (!ts) return '—'
  const d = new Date(ts * 1000), p = n => String(n).padStart(2, '0')
  return `${d.getFullYear()}-${p(d.getMonth() + 1)}-${p(d.getDate())} ${p(d.getHours())}:${p(d.getMinutes())}:${p(d.getSeconds())}`
}
// 状态展示文案：0=网络失败，否则状态码
const statusText = s => s === 0 ? '网络失败' : s === 499 ? '客户端取消' : String(s)
// 端点路径简化展示：去掉 /v1/ 前缀，空则 —
const fmtEndpoint = p => !p ? '—' : p.replace(/^\/v1\//, '')

// 监控项 CRUD
const monModels = ref([])  // 当前对话框选中渠道的可选模型（datalist）
const testModelOptions = computed(() => {
  if (testState.modelsLoading) return [{ value: '', label: '加载模型中…', disabled: true }]
  const opts = testState.models.map(m => ({ value: m, label: m }))
  if (!opts.length && testState.model) opts.push({ value: testState.model, label: testState.model })
  return opts
})
const upstreamSelectOptions = computed(() => upstreams.value.map(u => ({ value: u.id, label: `${u.name} — ${u.base_url}` })))
const addableSelectOptions = computed(() => addable.value.map(u => ({ value: u.id, label: `${u.name} — ${u.base_url}` })))
function loadMonModels(uid) {
  monModels.value = []
  if (!uid) return
  api.testUpstream(uid).then(r => { monModels.value = r.models || [] }).catch(() => {})
}

// 批量建监控对话框状态：拉模型/勾选/共享探测参数
const batchMon = reactive({ loading: false, error: '', models: [], picked: {}, monitored: {} })
const notice = ref('')  // 轻量成功提示，3s 自动消失
function flash(msg) { notice.value = msg; setTimeout(() => { if (notice.value === msg) notice.value = '' }, 3000) }

function openBatchMonitors(u) {
  dlg.type = 'upstream-monitors'
  dlg.form = { upstream_id: u.id, upstream_name: u.name, enabled: true, stream: false, probe_text: '', max_tokens: 0, interval_sec: 0, path: '' }
  // 该上游已建监控的模型集合（前端标“已监控”、避免重复勾选）
  batchMon.monitored = {}
  monitors.value.filter(m => m.upstream_id === u.id).forEach(m => { batchMon.monitored[m.model] = true })
  batchMon.models = []; batchMon.picked = {}; batchMon.error = ''; batchMon.loading = true
  api.testUpstream(u.id)
    .then(r => { batchMon.models = r.models || []; if (r.error) batchMon.error = r.error })
    .catch(e => { batchMon.error = String(e.message || e) })
    .finally(() => { batchMon.loading = false })
}
// 可勾选模型（排除已监控的）
const batchSelectable = computed(() => batchMon.models.filter(m => !batchMon.monitored[m]))
const batchPickedCount = computed(() => batchSelectable.value.filter(m => batchMon.picked[m]).length)
function batchToggleAll() {
  const all = batchPickedCount.value === batchSelectable.value.length && batchSelectable.value.length > 0
  batchSelectable.value.forEach(m => { batchMon.picked[m] = !all })
}
function saveBatchMonitors() {
  const models = batchSelectable.value.filter(m => batchMon.picked[m])
  if (!models.length) return
  guard(async () => {
    const f = dlg.form
    const r = await api.createMonitorsBatch(f.upstream_id, {
      models, enabled: f.enabled, stream: f.stream, probe_text: f.probe_text,
      max_tokens: Number(f.max_tokens) || 0, interval_sec: Number(f.interval_sec) || 0, path: f.path,
    })
    closeDlg(); await loadMonitors()
    flash(`已为「${f.upstream_name}」创建 ${r.created} 个监控${r.skipped ? `，跳过 ${r.skipped} 个已存在` : ''}`)
  })
}
function newMonitor() {
  const uid = upstreams.value[0]?.id || 0
  dlg.type = 'monitor'; dlg.form = { upstream_id: uid, model: '', name: '', enabled: true, stream: false, probe_text: '', max_tokens: 0, interval_sec: 0, path: '' }
  loadMonModels(uid)
}
function editMonitor(m) {
  dlg.type = 'monitor'
  dlg.form = { id: m.id, upstream_id: m.upstream_id, model: m.model, name: m.name, enabled: m.enabled, stream: !!m.stream, probe_text: m.probe_text || '', max_tokens: m.max_tokens || 0, interval_sec: m.interval_sec || 0, path: m.path || '' }
  loadMonModels(m.upstream_id)
}
function saveMonitor() {
  guard(async () => {
    const f = { ...dlg.form, upstream_id: Number(dlg.form.upstream_id), max_tokens: Number(dlg.form.max_tokens) || 0, interval_sec: Number(dlg.form.interval_sec) || 0 }
    if (f.id) await api.updateMonitor(f.id, f)
    else await api.createMonitor(f)
    closeDlg(); await loadMonitors()
  })
}
function delMonitor(m) {
  ask(`删除监控项「${monTitle(m)}」？`, () =>
    guard(async () => { await api.deleteMonitor(m.id); await loadMonitors() }))
}
function toggleMonitor(m) {
  guard(async () => { await api.updateMonitor(m.id, { ...m, enabled: !m.enabled }); await loadMonitors() })
}

function login() {
  if (!loginForm.token.trim()) { err.value = '请输入管理 Token'; return }
  api.setToken(loginForm.token.trim())
  loggedIn.value = true
  guard(async () => { await loadUpstreams(); await loadMonitors(); await loadSettings(); startMonPoll() })
}
function logout() {
  api.clearToken()
  loggedIn.value = false
  loginForm.token = ''
  groups.value = []; upstreams.value = []; members.value = []; keys.value = []; monitors.value = []
  stopAllPoll()
}
</script>

<template>
  <div v-if="!loggedIn" class="login-page">
    <div class="login-card">
      <div class="logo login-logo"><Icon name="bolt" :size="22" /><span class="logo-text">MuxAPI</span></div>
      <h1>管理后台登录</h1>
      <p>输入服务器环境变量 <code>MUXAPI_TOKEN</code>。</p>
      <div class="field"><label>Token</label><input v-model="loginForm.token" type="password" placeholder="MUXAPI_TOKEN" @keyup.enter="login" autofocus /></div>
      <p v-if="err" class="err-banner">{{ err }}</p>
      <button class="btn login-btn" @click="login"><Icon name="check" :size="16" />进入后台</button>
    </div>
  </div>

  <div v-else class="layout">
    <aside class="sidebar">
      <div class="logo"><Icon name="bolt" :size="22" /><span class="logo-text">MuxAPI</span></div>
      <nav class="nav">
        <div class="nav-item" :class="{ active: page === 'overview' }" @click="go('overview')"><Icon class="ic" name="bolt" :size="18" />总览</div>
        <div class="nav-item" :class="{ active: page === 'groups' }" @click="go('groups')"><Icon class="ic" name="cube" :size="18" />分组管理</div>
        <div class="nav-item" :class="{ active: page === 'upstreams' }" @click="go('upstreams')"><Icon class="ic" name="server" :size="18" />上游池</div>
        <div class="nav-item" :class="{ active: page === 'monitors' }" @click="go('monitors')"><Icon class="ic" name="heart" :size="18" />监控看板</div>
        <div class="nav-item" :class="{ active: page === 'logs' }" @click="go('logs')"><Icon class="ic" name="refresh" :size="18" />请求记录</div>
        <div class="nav-item" :class="{ active: page === 'settings' }" @click="go('settings')"><Icon class="ic" name="cog" :size="18" />设置</div>
      </nav>
    </aside>

    <div class="main-wrap">
      <header class="header">
        <div class="header-left">
          <h1 class="header-title">{{ detailGroup ? detailGroup.name : pages[page].title }}</h1>
          <p class="header-desc">{{ detailGroup ? '管理该分组的上游成员与接入密钥' : pages[page].desc }}</p>
        </div>
        <div class="header-actions">
          <span class="header-badge">v0.1.0</span>
          <button class="btn-link sm" @click="logout">退出</button>
        </div>
      </header>

      <main class="main">
        <p v-if="err" class="err-banner">{{ err }}</p>
        <p v-if="notice" class="ok-banner">{{ notice }}</p>

        <!-- 总览：上游 × 模型健康矩阵 -->
        <template v-if="page === 'overview'">
          <div class="ov-stats">
            <div class="ov-stat mint"><div class="ov-num">{{ ovSummary.healthy }}</div><div class="ov-lbl">正常组合</div></div>
            <div class="ov-stat amber"><div class="ov-num">{{ ovSummary.degraded }}</div><div class="ov-lbl">降级</div></div>
            <div class="ov-stat pink"><div class="ov-num">{{ ovSummary.down }}</div><div class="ov-lbl">故障</div></div>
            <div class="ov-stat violet"><div class="ov-num">{{ (ovSummary.rate * 100).toFixed(0) }}<small>%</small></div><div class="ov-lbl">平均成功率</div></div>
            <div class="ov-stat blue"><div class="ov-num">{{ ovSummary.avgLat || '—' }}<small v-if="ovSummary.avgLat">ms</small></div><div class="ov-lbl">平均延迟</div></div>
          </div>

          <div class="ov-meta">{{ ovSummary.upstreams }} 个上游 · {{ ovSummary.models }} 个模型 · {{ ovSummary.combos }} 个监控组合 · 点芯片看详情</div>

          <div v-if="!matrix.rows.length" class="empty">还没有监控项，去「监控看板」为渠道+模型建监控，这里就会亮起来 ✨</div>

          <div v-else class="ov-groups">
            <section class="ov-group" v-for="g in matrix.rows" :key="g.id">
              <header class="ovg-head">
                <span class="ovg-dot" :class="dotClass(g.down ? 'DOWN' : g.degraded ? 'DEGRADED' : 'OK')"></span>
                <span class="ovg-name" :title="g.name">{{ g.name }}</span>
                <span class="ovg-meta">{{ g.items.length }} 模型<template v-if="g.down"> · {{ g.down }} 故障</template><template v-if="g.degraded"> · {{ g.degraded }} 降级</template></span>
              </header>
              <div class="ovg-chips">
                <button v-for="m in g.items" :key="m.id" class="ov-chip"
                  :class="[dotClass(m.snapshot.state), { off: !m.enabled }]"
                  :title="m.model + ' · ' + (m.enabled ? stateLabel(m.snapshot.state) : '已停用') + (m.snapshot.avg_ms ? ' · ' + m.snapshot.avg_ms + 'ms' : '')"
                  @click="openCell(m)">
                  <span class="ovc-led" :class="dotClass(m.snapshot.state)"></span>
                  <span class="ovc-name">{{ m.model }}</span>
                  <span class="ovc-lat" v-if="m.snapshot.avg_ms || m.snapshot.last_ms">{{ m.snapshot.avg_ms || m.snapshot.last_ms }}ms</span>
                </button>
              </div>
            </section>
          </div>
        </template>

        <!-- 分组列表 -->
        <template v-if="page === 'groups' && !detailGroup">
          <div class="toolbar">
            <div class="toolbar-left"><span class="count">{{ groups.length }} 个分组</span></div>
            <button class="btn" @click="newGroup"><Icon name="plus" :size="16" />新建分组</button>
          </div>
          <div class="cards group-cards">
            <div class="card group-card" v-for="g in groups" :key="g.id" @click="openDetail(g)">
              <div class="card-head">
                <div class="gc-id">
                  <span class="gc-avatar">{{ (g.name || '?').slice(0,1) }}</span>
                  <div class="gc-titlewrap">
                    <span class="card-name">{{ g.name }}</span>
                    <p class="card-desc">{{ g.description || '无描述' }}</p>
                  </div>
                </div>
                <div class="card-actions">
                  <button class="icon-btn" @click.stop="editGroup(g)"><Icon name="edit" :size="16" /></button>
                  <button class="icon-btn danger" @click.stop="delGroup(g)"><Icon name="trash" :size="16" /></button>
                </div>
              </div>

              <div class="gc-body">
                <div class="gc-stat">
                  <b :class="rateClass(g)">{{ g.recent_total ? g.success_rate + '%' : '—' }}</b>
                  <span>近 24h 成功率</span>
                </div>
                <div class="gc-stat-sub">
                  <div><span>调用</span><b>{{ g.recent_total || 0 }}</b></div>
                  <div><span>延迟</span><b>{{ g.recent_total ? g.avg_latency_ms + 'ms' : '—' }}</b></div>
                </div>
              </div>
              <Fence :trend="g.trend || []" />
              <div class="gc-fence-cap">每格 = 最近 1 小时成功率</div>
              <div class="gc-pills">
                <span class="gc-pill mint"><i></i>{{ healthSummary(g.runtime) }}</span>
                <span class="gc-pill blue">上游 {{ g.enabled_upstream_count || 0 }}/{{ g.upstream_count || 0 }}</span>
                <span class="gc-pill violet">密钥 {{ g.enabled_key_count || 0 }}/{{ g.key_count || 0 }}</span>
                <span class="gc-pill" :class="g.runtime?.effective?.length ? 'amber' : 'gray'">生效 {{ effText(g.runtime) }}</span>
              </div>

              <div class="card-foot"><span>点击管理上游与密钥</span><Icon name="chevron-right" :size="15" /></div>
            </div>
            <div v-if="!groups.length" class="empty">还没有分组，点右上角新建一个。</div>
          </div>
        </template>

        <!-- 分组详情 -->
        <template v-else-if="detailGroup">
          <button class="btn-link" @click="backToGroups">← 返回分组列表</button>

          <div class="section-head">
            <h3 class="section-title">上游成员</h3>
            <button class="btn btn-sm" @click="addMember" :disabled="!addable.length"><Icon name="plus" :size="14" />从池中添加</button>
          </div>
          <div class="table-wrap">
            <table>
              <thead><tr><th>名称</th><th>地址</th><th>组内优先级</th><th>权重</th><th>运行时</th><th>成功率</th><th>延迟</th><th>组内开关</th><th>操作</th></tr></thead>
              <tbody>
                <tr v-for="m in members" :key="m.upstream_id" :class="{ 'row-eff': m.effective }">
                  <td class="cell-name">{{ m.name }}<span v-if="m.effective" class="eff-badge">生效中</span></td>
                  <td class="cell-url">{{ m.base_url }}</td>
                  <td>{{ m.priority }}</td>
                  <td>{{ m.weight }}</td>
                  <td>
                    <span class="state-badge" :class="rtClass(m.health)">{{ m.enabled && m.group_enabled ? rtLabel(m.health) : '已停用' }}</span>
                    <div v-if="visibleDots(m).length" class="model-dots">
                      <span v-for="mh in visibleDots(m)" :key="mh.model" class="model-dot" :class="mhClass(mh)" :title="mhTitle(mh)">{{ mh.model }}</span>
                    </div>
                  </td>
                  <td>{{ rtRate(m.health) }}</td>
                  <td>{{ m.health && m.health.avg_lat_ms ? m.health.avg_lat_ms + 'ms' : '—' }}</td>
                  <td>
                    <span v-if="!m.enabled" class="tag off" title="该上游已全局停用，请到上游池页启用">全局停用</span>
                    <span v-else class="tag" :class="m.group_enabled ? 'on' : 'off'">{{ m.group_enabled ? '启用' : '停用' }}</span>
                  </td>
                  <td>
                    <button v-if="m.enabled" class="btn-link sm" @click="toggleMember(m)">{{ m.group_enabled ? '停用' : '启用' }}</button>
                    <button class="icon-btn" @click="editMember(m)"><Icon name="edit" :size="16" /></button>
                    <button class="icon-btn danger" @click="removeMember(m)"><Icon name="trash" :size="16" /></button>
                  </td>
                </tr>
                <tr v-if="!members.length"><td colspan="9" class="empty-cell">暂无上游，从全局池添加。</td></tr>
              </tbody>
            </table>
          </div>

          <div class="section-head" style="margin-top:28px">
            <h3 class="section-title">流量分配预估</h3>
            <span class="route-flag on">标准 P2C</span>
          </div>
          <div class="card route-card">
            <template>
              <p class="route-cap">同优先级层内独立抽取两个渠道，按 TTFT 与当前并发选择更优者。</p>
              <div v-if="!routeDist.length" class="empty">生效层暂无路由数据</div>
              <div v-for="md in routeDist" :key="md.model" class="route-model">
                <div class="route-model-name">{{ md.model }}<span class="route-model-cnt">{{ md.rows.length }} 个渠道</span></div>
                <div v-for="r in md.rows" :key="r.name" class="route-row">
                  <div class="route-row-head">
                    <span class="route-up">{{ r.name }}</span>
                    <span class="route-pct">{{ r.share_pct.toFixed(0) }}%</span>
                  </div>
                  <span class="route-bar"><i :style="{ width: Math.max(r.share_pct, 2) + '%' }"></i></span>
                  <span class="route-metrics">
                    <template v-if="r.lat_ewma_ms > 0">TTFT {{ Math.round(r.lat_ewma_ms) }}ms</template>
                    <template v-else>待数据</template>
                  </span>
                </div>
              </div>
            </template>
          </div>

          <div class="section-head" style="margin-top:28px">
            <h3 class="section-title">接入密钥</h3>
            <button class="btn btn-sm" @click="createKey"><Icon name="plus" :size="14" />生成密钥</button>
          </div>
          <p class="hint">客户端用这里的密钥访问 MuxAPI，请求即路由到本分组的上游池。密钥仅在生成时明文显示一次。</p>
          <div class="table-wrap">
            <table>
              <thead><tr><th>名称</th><th>密钥</th><th>状态</th><th>操作</th></tr></thead>
              <tbody>
                <tr v-for="k in keys" :key="k.id">
                  <td>{{ k.name || '—' }}</td>
                  <td><code class="key-cell" title="点击复制" @click="copyText(k.key, k.id)">{{ k.key }}</code><span v-if="copied === k.id" style="color:#16a34a;font-size:12px;margin-left:6px">已复制 ✓</span></td>
                  <td><span class="tag" :class="k.enabled ? 'on' : 'off'">{{ k.enabled ? '启用' : '停用' }}</span></td>
                  <td>
                    <button class="btn-link sm" @click="toggleKey(k)">{{ k.enabled ? '停用' : '启用' }}</button>
                    <button class="icon-btn danger" @click="delKey(k)"><Icon name="trash" :size="16" /></button>
                  </td>
                </tr>
                <tr v-if="!keys.length"><td colspan="4" class="empty-cell">暂无密钥，点生成一个。</td></tr>
              </tbody>
            </table>
          </div>
        </template>

        <!-- 上游池 -->
        <template v-else-if="page === 'upstreams'">
          <div class="toolbar upstream-toolbar">
            <div class="toolbar-left">
              <span class="count">{{ upstreamsFiltered.length }} / {{ upstreams.length }} 个上游</span>
              <FancySelect v-model="upstreamRunFilter" :options="upstreamRunOptions" />
              <FancySelect v-model="upstreamEnabledFilter" :options="upstreamEnabledOptions" />
            </div>
            <button class="btn" @click="newUpstream"><Icon name="plus" :size="16" />新增上游</button>
          </div>
          <div class="table-wrap">
            <table>
              <thead><tr><th></th><th>名称</th><th>地址</th><th>协议</th><th>凭证</th><th>运行时</th><th>成功率</th><th>人工开关</th><th>操作</th></tr></thead>
              <tbody>
                <tr v-for="u in upstreamsFiltered" :key="u.id"
                  :class="{ disabled: !u.enabled, dragging: upstreamDragId === u.id, dragover: upstreamDragOverId === u.id }"
                  draggable="true"
                  @dragstart="onUpstreamDragStart(u, $event)" @dragover="onUpstreamDragOver(u, $event)"
                  @drop="onUpstreamDrop(u)" @dragend="onUpstreamDragEnd">
                  <td class="drag-cell"><span class="mon-grip" title="拖拽调整顺序"><Icon name="grip" :size="16" /></span></td>
                  <td class="cell-name">{{ u.name }}</td>
                  <td class="cell-url">{{ u.base_url }}</td>
                  <td><span class="tag">{{ protocolLabel(u.protocol) }}</span></td>
                  <td><code>{{ u.masked }}</code></td>
                  <td>
                    <span class="state-badge" :class="rtClass(u.health)">{{ u.enabled ? rtLabel(u.health) : '已停用' }}</span>
                    <div v-if="visibleDots(u).length" class="model-dots">
                      <span v-for="mh in visibleDots(u)" :key="mh.model" class="model-dot" :class="mhClass(mh)" :title="mhTitle(mh)">{{ mh.model }}</span>
                    </div>
                  </td>
                  <td>{{ rtRate(u.health) }}</td>
                  <td><span class="tag" :class="u.enabled ? 'on' : 'off'">{{ u.enabled ? '启用' : '停用' }}</span></td>
                  <td>
                    <button class="btn-link sm" @click="testUpstream(u)">测试</button>
                    <button class="btn-link sm" @click="openBatchMonitors(u)">建监控</button>
                    <button class="icon-btn" @click="editUpstream(u)"><Icon name="edit" :size="16" /></button>
                    <button class="icon-btn danger" @click="delUpstream(u)"><Icon name="trash" :size="16" /></button>
                  </td>
                </tr>
                <tr v-if="!upstreamsFiltered.length"><td colspan="9" class="empty-cell">{{ upstreams.length ? '没有符合筛选的上游。' : '还没有上游，点右上角新增。' }}</td></tr>
              </tbody>
            </table>
          </div>
        </template>

        <!-- 监控看板 -->
        <template v-else-if="page === 'monitors'">
          <!-- 探测设置已移至「设置」页 -->

          <!-- 顶部整体状态横幅 -->
          <div class="hbanner" :class="summary.allOk ? 'ok' : (summary.down ? 'down' : 'warn')">
            <div class="hb-icon"><Icon :name="summary.allOk ? 'check' : 'alert'" :size="20" /></div>
            <div class="hb-text">
              <div class="hb-title">{{ summary.allOk ? '所有监控项运行正常' : (summary.down ? `${summary.down} 个监控项故障` : `${summary.degraded} 个监控项降级`) }}</div>
              <div class="hb-sub">{{ summary.total }} 个监控 · {{ summary.up }} 正常 · {{ summary.degraded }} 降级 · {{ summary.down }} 故障 · 平均成功率 {{ (summary.rate * 100).toFixed(1) }}%</div>
            </div>
            <button class="btn" @click="newMonitor"><Icon name="plus" :size="16" />新增监控</button>
          </div>

          <!-- 监控项卡片网格 -->
          <div class="cards cards-sm">
            <div class="card mon-card" v-for="m in monitors" :key="m.id"
              :class="{ disabled: !m.enabled, dragging: dragId === m.id, dragover: dragOverId === m.id }"
              draggable="true"
              @dragstart="onDragStart(m, $event)" @dragover="onDragOver(m, $event)"
              @drop="onDrop(m)" @dragend="onDragEnd">
              <div class="mon-head">
                <span class="mon-grip" title="拖拽调整顺序"><Icon name="grip" :size="16" /></span>
                <span class="mon-avatar" :class="dotClass(m.snapshot.state)">{{ initial(m) }}</span>
                <div class="mon-id">
                  <span class="mon-name">{{ monTitle(m) }}</span>
                  <span class="mon-sub">{{ m.upstream_name }} · {{ m.model }}<span v-if="m.stream" class="tag on" style="margin-left:6px">流式</span></span>
                </div>
                <span class="state-badge" :class="dotClass(m.snapshot.state)">{{ m.enabled ? stateLabel(m.snapshot.state) : '已停用' }}</span>
              </div>
              <div class="card-metrics">
                <div class="metric-item"><span class="metric-label">成功率<small class="mh">24h</small></span><span class="metric-value" :class="m.snapshot.reqs && m.snapshot.succ_rate < 1 ? 'warn' : ''">{{ m.snapshot.reqs ? (m.snapshot.succ_rate * 100).toFixed(0) + '%' : '—' }}</span></div>
                <div class="metric-item"><span class="metric-label">平均延迟<small class="mh">24h</small></span><span class="metric-value">{{ m.snapshot.avg_ms || m.snapshot.last_ms || 0 }}<small>ms</small></span></div>
                <div class="metric-item"><span class="metric-label">最后探测</span><span class="metric-value sm">{{ sinceText(m.snapshot.last_ts) }}</span></div>
              </div>
              <Fence :trend="m.snapshot.trend || []" unit="探测" />
              <div class="mon-foot">
                <button class="btn-link sm" :disabled="probing.has(m.id)" @click="guard(() => probeOne(m))">{{ probing.has(m.id) ? '探测中…' : '立即探测' }}</button>
                <span class="hspacer" />
                <button class="btn-link sm" @click="toggleMonitor(m)">{{ m.enabled ? '停用' : '启用' }}</button>
                <button class="icon-btn" @click="editMonitor(m)"><Icon name="edit" :size="16" /></button>
                <button class="icon-btn danger" @click="delMonitor(m)"><Icon name="trash" :size="16" /></button>
              </div>
            </div>
            <div v-if="!monitors.length" class="empty">还没有监控项，点右上角新增。每个监控项 = 一个渠道 + 一个模型。</div>
          </div>
        </template>

        <!-- 请求记录页：范围统计 + 服务端筛选 + 按需详情 -->
        <template v-else-if="page === 'logs'">
          <div class="log-summary">
            <div class="log-stat"><span>请求</span><b>{{ fmtNum(logStats.total) }}</b><em>{{ ((logStats.success_rate || 0) * 100).toFixed(1) }}% 成功</em></div>
            <div class="log-stat success"><span>直接成功</span><b>{{ fmtNum(logStats.direct_success) }}</b><em>未切换渠道</em></div>
            <div class="log-stat warn"><span>切换后成功</span><b>{{ fmtNum(logStats.failover_success) }}</b><em>{{ fmtNum(logStats.retried) }} 次发生重试</em></div>
            <div class="log-stat danger"><span>异常</span><b>{{ fmtNum((logStats.failed || 0) + (logStats.partial || 0)) }}</b><em>{{ fmtNum(logStats.partial) }} 次流中断</em></div>
            <div class="log-stat"><span>P95 TTFT</span><b>{{ fmtMs(logStats.p95_ttft_ms) }}</b><em>P50 {{ fmtMs(logStats.p50_ttft_ms) }}</em></div>
            <div class="log-stat"><span>P95 总耗时</span><b>{{ fmtMs(logStats.p95_duration_ms) }}</b><em>所选时间范围</em></div>
            <div class="log-stat tokens"><span>Token</span><b>{{ fmtNum((logStats.input_tokens || 0) + (logStats.output_tokens || 0)) }}</b><em>入 {{ fmtNum(logStats.input_tokens) }} · 出 {{ fmtNum(logStats.output_tokens) }} · 缓存 {{ fmtNum(logStats.cached_tokens) }}</em></div>
          </div>

          <div class="log-filter-band">
            <div class="log-filter-primary">
              <div class="search-box log-search">
                <Icon class="ic" name="search" :size="16" />
                <input v-model="logSearch" class="search-input" placeholder="请求 ID 前缀" @keyup.enter="onLogFilterChange" />
              </div>
              <FancySelect v-model="logFTime" :options="logTimeOptions" @change="onLogFilterChange" />
              <FancySelect v-model="logFStatus" :options="logStatusOptions" @change="onLogFilterChange" />
              <FancySelect v-model="logFUpstream" :options="logUpstreamSelectOptions" @change="onLogFilterChange" />
              <button class="btn btn-sm log-more-filter-button" @click="logMoreFilters = !logMoreFilters"><Icon name="filter" :size="15" />筛选<span v-if="logAdvancedFilters">{{ logAdvancedFilters }}</span></button>
              <label class="log-auto"><input v-model="logAutoRefresh" type="checkbox" @change="toggleLogAutoRefresh" /><span></span>自动刷新</label>
              <span class="log-count">共 {{ fmtNum(logStats.total) }} 条</span>
              <button class="icon-btn log-refresh" :disabled="logLoading" title="刷新当前页" @click="guard(() => loadLogs(false))"><Icon name="refresh" :size="17" /></button>
            </div>
            <div class="log-filter-secondary" :class="{ open: logMoreFilters }">
              <FancySelect v-model="logFGroup" :options="logGroupSelectOptions" @change="onLogFilterChange" />
              <FancySelect v-model="logFModel" :options="logModelSelectOptions" @change="onLogFilterChange" />
              <FancySelect v-model="logFKey" :options="logKeySelectOptions" @change="onLogFilterChange" />
              <FancySelect v-model="logFEndpoint" :options="logEndpointSelectOptions" @change="onLogFilterChange" />
              <FancySelect v-model="logFErrorKind" :options="logErrorSelectOptions" @change="onLogFilterChange" />
              <FancySelect v-model="logFStream" :options="logStreamOptions" @change="onLogFilterChange" />
              <FancySelect v-model="logFSlow" :options="logSlowOptions" @change="onLogFilterChange" />
              <label class="log-check"><input v-model="logFRetried" type="checkbox" @change="onLogFilterChange" />仅看重试</label>
              <button v-if="logActiveFilters" class="btn-link sm log-reset" @click="resetLogFilters">清除 {{ logActiveFilters }} 项</button>
            </div>
          </div>

          <div class="table-wrap log-table-wrap">
            <table class="log-table">
              <thead><tr><th>时间 / 请求</th><th>接入</th><th>端点 / 模型</th><th>路由链</th><th>结果</th><th>性能</th><th>Token</th><th>流量</th></tr></thead>
              <tbody>
                <tr v-for="l in logs" :key="l.id" class="log-row" @click="openLogDetail(l)">
                  <td>
                    <div class="log-time" :title="fmtTimeFull(l.created_at)">{{ fmtTime(l.created_at) }}</div>
                    <div class="log-id" :title="l.request_id">{{ requestShort(l.request_id) }}</div>
                  </td>
                  <td><b class="log-main">{{ l.group_name || '未知分组' }}</b><span class="log-sub">{{ l.key_name || '未知密钥' }}</span></td>
                  <td><b class="log-main log-endpoint">{{ fmtEndpoint(l.endpoint) }}</b><span class="log-sub log-model" :title="l.model">{{ l.model || '未知模型' }}<i v-if="l.stream">流</i></span></td>
                  <td>
                    <div v-if="l.route?.length" class="route-chain">
                      <template v-for="(step, index) in l.route" :key="step.attempt_no">
                        <Icon v-if="index" name="chevron-right" :size="12" />
                        <span class="route-step" :class="step.outcome === 'success' ? 'ok' : 'fail'" :title="`${outcomeText(step.outcome)} · ${statusText(step.status)}`">{{ step.upstream_name || ('#' + step.upstream_id) }}</span>
                      </template>
                    </div>
                    <span v-else class="log-sub">{{ l.final_upstream_name || '未选择渠道' }}</span>
                  </td>
                  <td><span class="log-status" :class="requestOutcomeClass(l)">{{ requestOutcomeText(l) }} · {{ statusText(l.status) }}</span><span v-if="l.error_kind" class="log-sub error-kind">{{ errorKindText(l.error_kind) }}</span></td>
                  <td><b class="log-metric">{{ fmtMs(l.ttft_ms) }}</b><span class="log-sub">总计 {{ fmtMs(l.duration_ms) }}</span></td>
                  <td><b class="log-metric">{{ fmtNum(l.input_tokens) }} / {{ fmtNum(l.output_tokens) }}</b><span class="log-sub">缓存 {{ fmtNum(l.cached_tokens) }}</span></td>
                  <td><b class="log-metric">{{ fmtBytes(l.response_bytes) }}</b><span class="log-sub">{{ streamStateText(l) }}</span></td>
                </tr>
                <tr v-if="!logs.length"><td colspan="8" class="empty-cell">{{ logLoading ? '加载中…' : '没有符合条件的请求记录。' }}</td></tr>
              </tbody>
            </table>
          </div>
          <div v-if="Number(logStats.total) > 0" class="log-pager">
            <div class="log-page-size">
              <span>每页</span>
              <FancySelect v-model="logPageSize" :options="logPageSizeOptions" :disabled="logLoading" @change="onLogPageSizeChange" />
            </div>
            <div class="log-page-controls">
              <button class="icon-btn log-page-arrow prev" title="上一页" aria-label="上一页" :disabled="logLoading || logCurrentPage === 1" @click="guard(() => goLogPage(logCurrentPage - 1))">
                <Icon name="chevron-right" :size="15" />
              </button>
              <template v-for="item in logPageItems" :key="item.key">
                <button v-if="item.type === 'page'" class="log-page-number" :class="{ active: item.value === logCurrentPage }"
                  :aria-current="item.value === logCurrentPage ? 'page' : undefined" :disabled="logLoading" @click="guard(() => goLogPage(item.value))">{{ item.value }}</button>
                <span v-else class="log-page-ellipsis">…</span>
              </template>
              <button class="icon-btn log-page-arrow" title="下一页" aria-label="下一页" :disabled="logLoading || logCurrentPage === logTotalPages" @click="guard(() => goLogPage(logCurrentPage + 1))">
                <Icon name="chevron-right" :size="15" />
              </button>
            </div>
          </div>
        </template>

        <!-- 设置页：左锚点菜单 + 右内容 -->
        <template v-else-if="page === 'settings'">
          <div class="settings-layout">
            <aside class="settings-nav">
              <div class="set-navitem" :class="{ active: settingsSection === 'logs' }" @click="gotoSection('logs')"><Icon name="refresh" :size="16" />日志清理</div>
              <div class="set-navitem" :class="{ active: settingsSection === 'route' }" @click="gotoSection('route')"><Icon name="link" :size="16" />渠道路由</div>
              <div class="set-navitem" :class="{ active: settingsSection === 'alert' }" @click="gotoSection('alert')"><Icon name="alert" :size="16" />健康告警</div>
              <div class="set-navitem" :class="{ active: settingsSection === 'endpoint' }" @click="gotoSection('endpoint')"><Icon name="link" :size="16" />接入地址</div>
              <p class="set-navhint">探测间隔 / 路径已下放到各监控项，在「监控看板」逐项配置。</p>
            </aside>

            <div class="settings-body">
              <section id="set-logs" class="card settings-card">
                <div class="settings-title"><h3>日志清理</h3><p>按完成时间保留请求记录，过期记录与尝试链自动删除。</p></div>
                <div class="settings-fields">
                  <div class="field"><label>保留天数</label><input v-model="logRetention" type="number" min="1" max="365" placeholder="7" /></div>
                </div>
                <div class="settings-info">
                  <div><span>请求记录</span><b>{{ effectiveLogRetention ? effectiveLogRetention + ' 天' : '—' }}</b><em>{{ sourceText(logRetentionSource) }}</em></div>
                </div>
                <div class="settings-actions">
                  <span class="save-status" :class="{ show: settingsSaved }">已保存 ✓</span>
                  <button class="btn" @click="saveSettings"><Icon name="check" :size="16" />保存</button>
                </div>
              </section>

              <section id="set-route" class="card settings-card">
                <div class="settings-title"><h3>渠道路由</h3><p>首字节前允许故障切换，流开始后保持透明传输。</p></div>
                <div class="settings-fields">
                  <div class="field"><label>首字节超时（秒）</label><input v-model="firstResponseTimeoutSec" type="number" min="1" max="600" placeholder="120" /></div>
                </div>
                <div class="settings-info">
                  <div><span>算法</span><b>标准 P2C</b><em>渠道级</em></div>
                  <div><span>首字节超时</span><b>{{ effectiveFirstResponseTimeoutMs ? Math.round(effectiveFirstResponseTimeoutMs / 1000) + ' 秒' : '—' }}</b><em>{{ sourceText(firstResponseTimeoutSource) }}</em></div>
                </div>
                <p class="hint">收到任何响应字节前可切换渠道；开始传输后不再解析或强制等待结束事件。</p>
                <div class="settings-actions">
                  <span class="save-status" :class="{ show: settingsSaved }">已保存 ✓</span>
                  <button class="btn" @click="saveSettings"><Icon name="check" :size="16" />保存</button>
                </div>
              </section>

              <section id="set-alert" class="card settings-card">
                <div class="settings-title"><h3>健康告警</h3><p>上游渠道熔断翻转时推送 Webhook，URL 留空则关闭。</p></div>
                <div class="settings-fields">
                  <div class="field"><label>告警 Webhook</label><input v-model="alertWebhook" placeholder="https://... 留空关闭" /></div>
                  <div class="field"><label>去抖间隔</label><input v-model="alertDebounce" placeholder="60s / 5m" /></div>
                </div>
                <div class="settings-info">
                  <div><span>Webhook</span><b>{{ effectiveAlertWebhook || '已关闭' }}</b><em>{{ sourceText(alertWebhookSource) }}</em></div>
                  <div><span>去抖</span><b>{{ effectiveAlertDebounce || '—' }}</b><em>{{ sourceText(alertDebounceSource) }}</em></div>
                </div>
                <div class="settings-actions">
                  <span class="save-status" :class="{ show: settingsSaved }">已保存 ✓</span>
                  <button class="btn" @click="saveSettings"><Icon name="check" :size="16" />保存</button>
                </div>
              </section>

              <section id="set-endpoint" class="card settings-card">
                <div class="settings-title"><h3>接入地址</h3><p>客户端使用接入密钥访问。</p></div>
                <div class="endpoint-list">
                  <div><span>OpenAI</span><code>{{ apiBase }}/v1/chat/completions</code></div>
                  <div><span>Responses</span><code>{{ apiBase }}/v1/responses</code></div>
                  <div><span>Claude</span><code>{{ apiBase }}/v1/messages</code></div>
                </div>
                <p class="hint">请求头：<code>Authorization: Bearer &lt;密钥&gt;</code></p>
              </section>
            </div>
          </div>
        </template>
      </main>
    </div>

    <!-- 矩阵格子详情抽屉 -->
    <div class="drawer-mask" v-if="cellDrawer" @click.self="closeCell">
      <div class="drawer">
        <div class="dw-head">
          <span class="mx-dot lg" :class="dotClass(cellDrawer.snapshot.state)"></span>
          <div class="dw-id">
            <div class="dw-title">{{ cellDrawer.model }}</div>
            <div class="dw-sub">{{ cellDrawer.upstream_name }}<span v-if="cellDrawer.stream" class="tag on" style="margin-left:6px">流式</span></div>
          </div>
          <button class="icon-btn" @click="closeCell"><Icon name="x" :size="18" /></button>
        </div>
        <div class="dw-metrics">
          <div class="dw-m"><span>状态</span><b :class="dotClass(cellDrawer.snapshot.state)">{{ cellDrawer.enabled ? stateLabel(cellDrawer.snapshot.state) : '已停用' }}</b></div>
          <div class="dw-m"><span>成功率</span><b>{{ cellDrawer.snapshot.reqs ? (cellDrawer.snapshot.succ_rate * 100).toFixed(0) + '%' : '—' }}</b></div>
          <div class="dw-m"><span>平均延迟</span><b>{{ cellDrawer.snapshot.avg_ms || cellDrawer.snapshot.last_ms || 0 }}<small>ms</small></b></div>
          <div class="dw-m"><span>最后探测</span><b>{{ sinceText(cellDrawer.snapshot.last_ts) }}</b></div>
        </div>
        <Fence :trend="cellDrawer.snapshot.trend || []" unit="探测" />
        <div class="dw-foot">
          <button class="btn" :disabled="probing.has(cellDrawer.id)" @click="guard(probeCell)">{{ probing.has(cellDrawer.id) ? '探测中…' : '立即探测' }}</button>
        </div>
      </div>
    </div>

    <!-- 请求审计详情 -->
    <div class="drawer-mask" v-if="logDetail" @click.self="closeLogDetail">
      <aside class="drawer log-detail-drawer">
        <div class="dw-head log-detail-head">
          <span class="log-status" :class="requestOutcomeClass(logDetail)">{{ requestOutcomeText(logDetail) }} · {{ statusText(logDetail.status) }}</span>
          <div class="dw-id">
            <div class="dw-title">{{ requestShort(logDetail.request_id) }}</div>
            <div class="dw-sub">{{ fmtTimeFull(logDetail.created_at) }}</div>
          </div>
          <button class="icon-btn" title="复制请求 ID" @click="copyText(logDetail.request_id, 'request-id')"><Icon name="copy" :size="17" /></button>
          <button class="icon-btn" title="关闭" @click="closeLogDetail"><Icon name="x" :size="18" /></button>
        </div>

        <div v-if="logDetailLoading" class="log-detail-loading"><Icon name="loader" :size="18" />加载详情</div>
        <div v-else class="log-detail-scroll">
          <section class="log-detail-section">
            <h4>请求上下文</h4>
            <dl class="log-detail-grid">
              <div><dt>请求 ID</dt><dd class="mono">{{ logDetail.request_id }}</dd></div>
              <div><dt>分组 / 密钥</dt><dd>{{ logDetail.group_name || '—' }} / {{ logDetail.key_name || '—' }}</dd></div>
              <div><dt>端点</dt><dd class="mono">{{ logDetail.endpoint || '—' }}</dd></div>
              <div><dt>模型</dt><dd>{{ logDetail.model || '—' }}</dd></div>
              <div><dt>请求模式</dt><dd>{{ logDetail.stream ? '流式' : '非流式' }}</dd></div>
              <div><dt>上游 Request ID</dt><dd class="mono">{{ logDetail.upstream_request_id || '—' }}</dd></div>
              <div><dt>开始</dt><dd>{{ fmtTimeFull(logDetail.created_at) }}</dd></div>
              <div><dt>完成</dt><dd>{{ fmtTimeFull(logDetail.completed_at) }}</dd></div>
            </dl>
          </section>

          <section class="log-detail-section">
            <h4>性能与用量</h4>
            <div class="log-detail-metrics">
              <div><span>TTFT</span><b>{{ fmtMs(logDetail.ttft_ms) }}</b></div>
              <div><span>总耗时</span><b>{{ fmtMs(logDetail.duration_ms) }}</b></div>
              <div><span>请求体</span><b>{{ fmtBytes(logDetail.request_bytes) }}</b></div>
              <div><span>响应体</span><b>{{ fmtBytes(logDetail.response_bytes) }}</b></div>
              <div><span>输入 Token</span><b>{{ fmtNum(logDetail.input_tokens) }}</b></div>
              <div><span>输出 Token</span><b>{{ fmtNum(logDetail.output_tokens) }}</b></div>
              <div><span>缓存 Token</span><b>{{ fmtNum(logDetail.cached_tokens) }}</b></div>
              <div><span>流结束</span><b>{{ streamStateText(logDetail) }}</b></div>
            </div>
          </section>

          <section class="log-detail-section">
            <h4>渠道尝试 <span>{{ logDetail.attempts?.length || 0 }}</span></h4>
            <div v-if="logDetail.attempts?.length" class="attempt-timeline">
              <article v-for="attempt in logDetail.attempts" :key="attempt.id" class="attempt-item">
                <div class="attempt-marker" :class="attempt.outcome === 'success' ? 'ok' : 'fail'">{{ attempt.attempt_no }}</div>
                <div class="attempt-content">
                  <header>
                    <div><b>{{ attempt.upstream_name || ('#' + attempt.upstream_id) }}</b><span>{{ selectionText(attempt.selection_reason) }} · 优先级 {{ attempt.priority || '—' }}</span></div>
                    <span class="log-status" :class="attempt.outcome === 'success' ? 'ok' : attempt.outcome === 'canceled' ? 'muted' : 'fail'">{{ outcomeText(attempt.outcome) }} · {{ statusText(attempt.status) }}</span>
                  </header>
                  <div class="attempt-facts">
                    <span>熔断 {{ attempt.health_before || '—' }} → {{ attempt.health_after || '—' }}</span>
                    <span>TTFT {{ fmtMs(attempt.ttft_ms) }}</span>
                    <span>耗时 {{ fmtMs(attempt.duration_ms) }}</span>
                    <span>响应 {{ fmtBytes(attempt.response_bytes) }}</span>
                    <span>Token {{ fmtNum(attempt.input_tokens) }} / {{ fmtNum(attempt.output_tokens) }}</span>
                    <span v-if="attempt.stream">{{ attempt.stream_completed ? '流完成' : '未见完成事件' }} · {{ attempt.last_event || 'EOF' }}</span>
                  </div>
                  <div v-if="attempt.error_kind" class="attempt-error-meta"><b>{{ errorKindText(attempt.error_kind) }}</b><span>来源：{{ errorSourceText(attempt.error_source) }}</span></div>
                  <button v-if="attempt.error" class="log-error-block" title="复制错误" @click="copyText(attempt.error, 'attempt-' + attempt.id)">
                    <code>{{ attempt.error }}</code><Icon name="copy" :size="14" />
                  </button>
                  <div v-if="attempt.upstream_request_id" class="attempt-request-id">Upstream ID <code>{{ attempt.upstream_request_id }}</code></div>
                </div>
              </article>
            </div>
            <div v-else class="empty log-detail-empty">没有渠道尝试记录</div>
          </section>

          <section v-if="logDetail.error" class="log-detail-section">
            <h4>最终错误</h4>
            <div class="log-final-error-head"><b>{{ errorKindText(logDetail.error_kind) }}</b><span>来源：{{ errorSourceText(logDetail.error_source) }}</span></div>
            <button class="log-error-block final" title="复制错误" @click="copyText(logDetail.error, 'final-error')">
              <code>{{ logDetail.error }}</code><Icon name="copy" :size="14" />
            </button>
          </section>
        </div>
      </aside>
    </div>

    <!-- 新密钥明文展示（生成后一次性） -->
    <div class="mask" v-if="newKey" @click.self="copyKey">
      <div class="dialog">
        <h3>密钥已生成</h3>
        <p class="hint">请立即复制保存，关闭后将无法再次查看完整密钥。</p>
        <div class="key-reveal"><code>{{ newKey }}</code></div>
        <div class="dialog-foot"><button class="btn" @click="copyKey"><Icon name="check" :size="16" />复制并关闭</button></div>
      </div>
    </div>

    <!-- 真实对话测试 -->
    <div class="mask" v-if="testState.show" @click.self="testState.show = false">
      <div class="dialog">
        <h3>测试上游 · {{ testState.name }}</h3>
        <p class="hint" style="margin:0 0 10px">发一条真实对话请求，验证能否端到端跑通并查看回复。</p>

        <div class="test-row">
          <FancySelect v-model="testState.model" :options="testModelOptions" :disabled="testState.modelsLoading || testState.running" />
          <button class="btn" :disabled="testState.running || !testState.model" @click="runTest">
            <Icon :name="testState.running ? 'loader' : 'play'" :size="16" />{{ testState.running ? '测试中…' : '开始测试' }}
          </button>
        </div>
        <p v-if="testState.modelsErr" class="test-err" style="margin:0 0 8px">列模型失败：{{ testState.modelsErr }}（仍可手动测试默认模型）</p>

        <div v-if="testState.running || testState.output || testState.status" class="test-output">
          <span v-if="testState.output">{{ testState.output }}</span>
          <span v-if="testState.running" class="cursor">▋</span>
          <span v-else-if="!testState.output && testState.status?.ok" class="hint">（上游无文本输出，但连接成功）</span>
        </div>

        <div v-if="testState.status" class="test-status" :class="testState.status.ok ? 'ok' : 'fail'">
          <Icon :name="testState.status.ok ? 'check' : 'x'" :size="16" />
          <span>{{ testState.status.ok ? '测试通过' : '测试失败' }}</span>
          <small v-if="testState.status.latency_ms != null">{{ testState.status.latency_ms }}ms</small>
          <small v-if="testState.status.code">HTTP {{ testState.status.code }}</small>
        </div>
        <p v-if="testState.status && !testState.status.ok && testState.status.error" class="test-err">{{ testState.status.error }}</p>

        <div class="dialog-foot"><button class="btn btn-ghost" @click="testState.show = false">关闭</button></div>
      </div>
    </div>

    <!-- 确认弹窗 -->
    <div class="mask" v-if="confirmState.show" @click.self="confirmState.show = false">
      <div class="dialog dialog-sm">
        <h3>确认操作</h3>
        <p class="confirm-msg">{{ confirmState.msg }}</p>
        <div class="dialog-foot">
          <button class="btn btn-ghost" @click="confirmState.show = false">取消</button>
          <button class="btn btn-danger" @click="confirmOk"><Icon name="trash" :size="16" />确认删除</button>
        </div>
      </div>
    </div>

    <!-- 表单弹窗 -->
    <div class="mask" v-if="dlg.type" @click.self="closeDlg">
      <div class="dialog">
        <template v-if="dlg.type === 'group'">
          <h3>{{ dlg.form.id ? '编辑分组' : '新建分组' }}</h3>
          <div class="field"><label>名称</label><input v-model="dlg.form.name" placeholder="如 Claude 池" /></div>
          <div class="field"><label>描述</label><input v-model="dlg.form.description" placeholder="可选" /></div>
          <div class="dialog-foot"><button class="btn btn-ghost" @click="closeDlg">取消</button><button class="btn" @click="saveGroup">保存</button></div>
        </template>

        <template v-else-if="dlg.type === 'keygen'">
          <h3>生成接入密钥</h3>
          <div class="field"><label>名称</label><input v-model="dlg.form.name" placeholder="备注用，如「客户端A」，可留空" @keyup.enter="saveKey" /></div>
          <p class="hint" style="margin:0">密钥由系统生成，绑定当前分组，仅在生成后明文显示一次。</p>
          <div class="dialog-foot"><button class="btn btn-ghost" @click="closeDlg">取消</button><button class="btn" @click="saveKey"><Icon name="plus" :size="16" />生成</button></div>
        </template>

        <template v-else-if="dlg.type === 'upstream'">
          <h3>{{ dlg.form.id ? '编辑上游' : '新增上游' }}</h3>
          <div class="field"><label>名称</label><input v-model="dlg.form.name" /></div>
          <div class="field"><label>base_url</label><input v-model="dlg.form.base_url" placeholder="https://..." /></div>
          <div class="field"><label>协议</label><FancySelect v-model="dlg.form.protocol" :options="protocolOptions" /></div>
          <div class="field"><label>api_key</label><input v-model="dlg.form.api_key" :placeholder="dlg.form.id ? '留空则不修改' : 'sk-...'" /></div>
          <div class="field"><label>代理</label><input v-model="dlg.form.proxy" placeholder="留空=直连/环境变量；如 http://127.0.0.1:7890 或 socks5://127.0.0.1:1080" /></div>
          <label class="check"><input type="checkbox" v-model="dlg.form.enabled" /> 启用</label>
          <div class="dialog-foot"><button class="btn btn-ghost" @click="closeDlg">取消</button><button class="btn" @click="saveUpstream">保存</button></div>
        </template>

        <template v-else-if="dlg.type === 'monitor'">
          <h3>{{ dlg.form.id ? '编辑监控项' : '新增监控项' }}</h3>
          <div class="field">
            <label>渠道</label>
            <FancySelect v-model="dlg.form.upstream_id" :options="upstreamSelectOptions" @change="loadMonModels(Number(dlg.form.upstream_id))" />
          </div>
          <div class="field">
            <label>模型</label>
            <input v-model="dlg.form.model" list="mon-models" placeholder="如 gpt-4o，可从下拉选或手填" />
            <datalist id="mon-models"><option v-for="m in monModels" :key="m" :value="m" /></datalist>
          </div>
          <div class="field"><label>备注名</label><input v-model="dlg.form.name" placeholder="可选，留空则显示「渠道 · 模型」" /></div>
          <div class="field-row">
            <div class="field"><label>探测间隔(秒)</label><input v-model="dlg.form.interval_sec" type="number" min="0" placeholder="留空/0 用默认 5 分钟" /></div>
            <div class="field"><label>max_tokens</label><input v-model="dlg.form.max_tokens" type="number" min="0" placeholder="留空/0 用默认 1" /></div>
          </div>
          <div class="field"><label>探测路径</label><input v-model="dlg.form.path" placeholder="留空用默认 /v1/chat/completions，Claude 填 /v1/messages" /></div>
          <div class="field"><label>探测消息</label><input v-model="dlg.form.probe_text" placeholder="留空用默认「hi」" /></div>
          <label class="check"><input type="checkbox" v-model="dlg.form.stream" /> 流式探测（请求体加 stream:true）</label>
          <label class="check"><input type="checkbox" v-model="dlg.form.enabled" /> 启用</label>
          <div class="dialog-foot"><button class="btn btn-ghost" @click="closeDlg">取消</button><button class="btn" @click="saveMonitor">保存</button></div>
        </template>

        <template v-else-if="dlg.type === 'upstream-monitors'">
          <h3>为「{{ dlg.form.upstream_name }}」批量建监控</h3>
          <p class="hint">勾选要探活的模型，下方探测参数对所有选中项共享。已监控的模型自动跳过。监控只是探活子集，不影响下游 /v1/models 能看到的全部模型。</p>
          <div v-if="batchMon.loading" class="hint">正在拉取模型列表…</div>
          <div v-else-if="batchMon.error" class="err-banner">拉取模型失败：{{ batchMon.error }}</div>
          <div v-else-if="!batchMon.models.length" class="hint">该上游 /v1/models 没有返回任何模型。</div>
          <template v-else>
            <div class="batch-tools">
              <label class="check"><input type="checkbox" :checked="batchPickedCount === batchSelectable.length && batchSelectable.length > 0" @change="batchToggleAll" /> 全选可用（{{ batchPickedCount }}/{{ batchSelectable.length }}）</label>
            </div>
            <div class="batch-list">
              <label v-for="m in batchMon.models" :key="m" class="batch-item" :class="{ done: batchMon.monitored[m] }">
                <input type="checkbox" :disabled="batchMon.monitored[m]" v-model="batchMon.picked[m]" />
                <span class="bm-name">{{ m }}</span>
                <span v-if="batchMon.monitored[m]" class="tag off">已监控</span>
              </label>
            </div>
            <div class="field-row">
              <div class="field"><label>探测间隔(秒)</label><input v-model="dlg.form.interval_sec" type="number" min="0" placeholder="留空/0 用默认 5 分钟" /></div>
              <div class="field"><label>max_tokens</label><input v-model="dlg.form.max_tokens" type="number" min="0" placeholder="留空/0 用默认 1" /></div>
            </div>
            <div class="field"><label>探测路径</label><input v-model="dlg.form.path" placeholder="留空用默认 /v1/chat/completions，Claude 填 /v1/messages" /></div>
            <div class="field"><label>探测消息</label><input v-model="dlg.form.probe_text" placeholder="留空用默认「hi」" /></div>
            <label class="check"><input type="checkbox" v-model="dlg.form.stream" /> 流式探测（请求体加 stream:true）</label>
            <label class="check"><input type="checkbox" v-model="dlg.form.enabled" /> 启用</label>
          </template>
          <div class="dialog-foot">
            <button class="btn btn-ghost" @click="closeDlg">取消</button>
            <button class="btn" :disabled="batchPickedCount === 0" @click="saveBatchMonitors">为选中 {{ batchPickedCount }} 个模型创建监控</button>
          </div>
        </template>

        <template v-else-if="dlg.type === 'member'">
          <h3>{{ dlg.form.locked ? '调整组内策略' : '添加上游到分组' }}</h3>
          <div class="field" v-if="!dlg.form.locked">
            <label>选择上游</label>
            <FancySelect v-model="dlg.form.upstream_id" :options="addableSelectOptions" />
          </div>
          <div class="field" v-else><label>上游</label><input :value="upName(dlg.form.upstream_id)" disabled /></div>
          <div class="field-row">
            <div class="field" style="flex:1"><label>组内优先级</label><input type="number" v-model.number="dlg.form.priority" /></div>
            <div class="field" style="flex:1"><label>权重</label><input type="number" v-model.number="dlg.form.weight" /></div>
          </div>
          <p class="hint" style="margin:0">优先级越小越先用；同优先级按权重分流。</p>
          <div class="dialog-foot"><button class="btn btn-ghost" @click="closeDlg">取消</button><button class="btn" @click="saveMember">保存</button></div>
        </template>
      </div>
    </div>
  </div>
</template>
