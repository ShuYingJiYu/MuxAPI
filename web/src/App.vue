<script setup>
import { ref, reactive, onMounted, onUnmounted, computed } from 'vue'
import Icon from './Icon.vue'
import Fence from './Fence.vue'
import { api } from './api.js'

const page = ref('groups')        // groups | upstreams | monitors
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
const probingId = ref(0)
async function probeOne(m) {
  probingId.value = m.id
  try {
    const sn = await api.probeMonitor(m.id)
    m.snapshot = sn
  } finally { probingId.value = 0 }
}
async function loadDetail(gid) {
  members.value = (await api.members(gid)) || []
  keys.value = (await api.keys(gid)) || []
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
  if (loggedIn.value) guard(async () => { await loadGroups(); await loadUpstreams(); await loadSettings() })
})

// 看板自动刷新：探测间隔 5min，这里每 60s 拉一次快照即可，离开即停
let monTimer = null
function startMonPoll() {
  stopMonPoll()
  monTimer = setInterval(() => { loadMonitors().catch(() => {}) }, 60000)
}
function stopMonPoll() { if (monTimer) { clearInterval(monTimer); monTimer = null } }
onUnmounted(stopMonPoll)

function go(p) {
  page.value = p; detailGroup.value = null
  stopMonPoll()
  guard(async () => {
    if (p === 'groups') await loadGroups()
    else if (p === 'upstreams') await loadUpstreams()
    else if (p === 'monitors') { await loadUpstreams(); await loadMonitors(); startMonPoll() }
  })
}
function openDetail(g) {
  detailGroup.value = g
  guard(async () => { await loadUpstreams(); await loadDetail(g.id) })
}
function backToGroups() { detailGroup.value = null; guard(loadGroups) }

// 上游池里未加入当前分组的（供"添加成员"下拉）
const memberIds = computed(() => new Set(members.value.map(m => m.upstream_id)))
const addable = computed(() => upstreams.value.filter(u => !memberIds.value.has(u.id)))

const pages = {
  groups: { title: '分组管理', desc: '每个分组是一个独立的调度池，拥有自己的上游与接入密钥' },
  upstreams: { title: '上游池', desc: '全局上游凭证，可被多个分组复用' },
  monitors: { title: '监控看板', desc: '为「渠道+模型」配置监控项，主动探测成功率与延迟' },
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

function newUpstream() { dlg.type = 'upstream'; dlg.form = { name: '', base_url: '', api_key: '', proxy: '', enabled: true } }
function editUpstream(u) { dlg.type = 'upstream'; dlg.form = { ...u, api_key: '' } }
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
    if (!testState.status) testState.status = { ok: true } // 流正常结束但没收到 complete
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

// 密钥
const newKey = ref('')   // 生成后明文展示一次
const copied = ref(0)    // 刚点击复制的密钥 id（短暂提示用）
const probeInterval = ref('')      // 路由探测间隔(页面可配)
const monitorInterval = ref('')    // 看板探测间隔(页面可配)
const effectiveProbeInterval = ref('')
const effectiveMonitorInterval = ref('')
const probeSource = ref('')
const monitorSource = ref('')
const apiBase = location.origin    // 当前访问地址，用于展示客户端接入端点
const settingsSaved = ref(false)
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
  probeInterval.value = s.probe_interval || ''
  monitorInterval.value = s.monitor_interval || ''
  effectiveProbeInterval.value = s.effective_probe_interval || ''
  effectiveMonitorInterval.value = s.effective_monitor_interval || ''
  probeSource.value = s.probe_source || ''
  monitorSource.value = s.monitor_source || ''
}
const sourceText = s => s === 'settings' ? '页面设置' : (s === 'env' ? '.env / 环境变量' : '默认值')
function saveSettings() {
  guard(async () => {
    await api.saveSettings({ probe_interval: probeInterval.value, monitor_interval: monitorInterval.value })
    await loadSettings()
    settingsSaved.value = true
    setTimeout(() => { settingsSaved.value = false }, 1500)
  })
}

// 监控项状态映射
const stateLabel = s => ({ OK: '正常', DEGRADED: '降级', DOWN: '故障' }[s] || '无数据')
const dotClass = s => ({ OK: 'closed', DEGRADED: 'half', DOWN: 'open' }[s] || 'nodata')
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

// 监控项 CRUD
const monModels = ref([])  // 当前对话框选中渠道的可选模型（datalist）
function loadMonModels(uid) {
  monModels.value = []
  if (!uid) return
  api.testUpstream(uid).then(r => { monModels.value = r.models || [] }).catch(() => {})
}
function newMonitor() {
  const uid = upstreams.value[0]?.id || 0
  dlg.type = 'monitor'; dlg.form = { upstream_id: uid, model: '', name: '', enabled: true }
  loadMonModels(uid)
}
function editMonitor(m) {
  dlg.type = 'monitor'
  dlg.form = { id: m.id, upstream_id: m.upstream_id, model: m.model, name: m.name, enabled: m.enabled }
  loadMonModels(m.upstream_id)
}
function saveMonitor() {
  guard(async () => {
    const f = { ...dlg.form, upstream_id: Number(dlg.form.upstream_id) }
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
  api.setToken(loginForm.token.trim())
  loggedIn.value = true
  guard(async () => { await loadGroups(); await loadUpstreams(); await loadSettings() })
}
function logout() {
  api.clearToken()
  loggedIn.value = false
  loginForm.token = ''
  groups.value = []; upstreams.value = []; members.value = []; keys.value = []; monitors.value = []
  stopMonPoll()
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
        <div class="nav-item" :class="{ active: page === 'groups' }" @click="go('groups')"><Icon class="ic" name="cube" :size="18" />分组管理</div>
        <div class="nav-item" :class="{ active: page === 'upstreams' }" @click="go('upstreams')"><Icon class="ic" name="server" :size="18" />上游池</div>
        <div class="nav-item" :class="{ active: page === 'monitors' }" @click="go('monitors')"><Icon class="ic" name="heart" :size="18" />监控看板</div>
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

        <!-- 分组列表 -->
        <template v-if="page === 'groups' && !detailGroup">
          <div class="toolbar">
            <div class="toolbar-left"><span class="count">{{ groups.length }} 个分组</span></div>
            <button class="btn" @click="newGroup"><Icon name="plus" :size="16" />新建分组</button>
          </div>
          <div class="cards">
            <div class="card group-card" v-for="g in groups" :key="g.id" @click="openDetail(g)">
              <div class="card-head">
                <span class="card-name">{{ g.name }}</span>
                <div class="card-actions">
                  <button class="icon-btn" @click.stop="editGroup(g)"><Icon name="edit" :size="16" /></button>
                  <button class="icon-btn danger" @click.stop="delGroup(g)"><Icon name="trash" :size="16" /></button>
                </div>
              </div>
              <p class="card-desc">{{ g.description || '无描述' }}</p>
              <div class="group-stats">
                <div><span>上游</span><b>{{ g.enabled_upstream_count || 0 }}/{{ g.upstream_count || 0 }}</b></div>
                <div><span>密钥</span><b>{{ g.enabled_key_count || 0 }}/{{ g.key_count || 0 }}</b></div>
                <div><span>近24h</span><b>{{ g.recent_total ? (g.success_rate + '% · ' + g.avg_latency_ms + 'ms') : '暂无调用' }}</b></div>
              </div>
              <div class="card-foot"><span>点击管理上游与密钥</span><Icon name="check" :size="14" /></div>
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
              <thead><tr><th>名称</th><th>地址</th><th>组内优先级</th><th>权重</th><th>状态</th><th>操作</th></tr></thead>
              <tbody>
                <tr v-for="m in members" :key="m.upstream_id">
                  <td class="cell-name">{{ m.name }}</td>
                  <td class="cell-url">{{ m.base_url }}</td>
                  <td>{{ m.priority }}</td>
                  <td>{{ m.weight }}</td>
                  <td><span class="tag" :class="m.enabled ? 'on' : 'off'">{{ m.enabled ? '启用' : '停用' }}</span></td>
                  <td>
                    <button class="icon-btn" @click="editMember(m)"><Icon name="edit" :size="16" /></button>
                    <button class="icon-btn danger" @click="removeMember(m)"><Icon name="trash" :size="16" /></button>
                  </td>
                </tr>
                <tr v-if="!members.length"><td colspan="6" class="empty-cell">暂无上游，从全局池添加。</td></tr>
              </tbody>
            </table>
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
          <div class="toolbar">
            <div class="toolbar-left"><span class="count">{{ upstreams.length }} 个上游</span></div>
            <button class="btn" @click="newUpstream"><Icon name="plus" :size="16" />新增上游</button>
          </div>
          <div class="table-wrap">
            <table>
              <thead><tr><th>名称</th><th>地址</th><th>凭证</th><th>状态</th><th>操作</th></tr></thead>
              <tbody>
                <tr v-for="u in upstreams" :key="u.id">
                  <td class="cell-name">{{ u.name }}</td>
                  <td class="cell-url">{{ u.base_url }}</td>
                  <td><code>{{ u.masked }}</code></td>
                  <td><span class="tag" :class="u.enabled ? 'on' : 'off'">{{ u.enabled ? '启用' : '停用' }}</span></td>
                  <td>
                    <button class="btn-link sm" @click="testUpstream(u)">测试</button>
                    <button class="icon-btn" @click="editUpstream(u)"><Icon name="edit" :size="16" /></button>
                    <button class="icon-btn danger" @click="delUpstream(u)"><Icon name="trash" :size="16" /></button>
                  </td>
                </tr>
                <tr v-if="!upstreams.length"><td colspan="5" class="empty-cell">还没有上游，点右上角新增。</td></tr>
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
            <div class="card mon-card" v-for="m in monitors" :key="m.id" :class="{ disabled: !m.enabled }">
              <div class="mon-head">
                <span class="mon-avatar" :class="dotClass(m.snapshot.state)">{{ initial(m) }}</span>
                <div class="mon-id">
                  <span class="mon-name">{{ monTitle(m) }}</span>
                  <span class="mon-sub">{{ m.upstream_name }} · {{ m.model }}</span>
                </div>
                <span class="state-badge" :class="dotClass(m.snapshot.state)">{{ m.enabled ? stateLabel(m.snapshot.state) : '已停用' }}</span>
              </div>
              <div class="card-metrics">
                <div class="metric-item"><span class="metric-label">成功率</span><span class="metric-value" :class="m.snapshot.reqs && m.snapshot.succ_rate < 1 ? 'warn' : ''">{{ m.snapshot.reqs ? (m.snapshot.succ_rate * 100).toFixed(0) + '%' : '—' }}</span></div>
                <div class="metric-item"><span class="metric-label">平均延迟</span><span class="metric-value">{{ m.snapshot.avg_ms || m.snapshot.last_ms || 0 }}<small>ms</small></span></div>
                <div class="metric-item"><span class="metric-label">最后探测</span><span class="metric-value sm">{{ sinceText(m.snapshot.last_ts) }}</span></div>
              </div>
              <Fence :trend="m.snapshot.trend || []" />
              <div class="mon-foot">
                <button class="btn-link sm" :disabled="probingId === m.id" @click="guard(() => probeOne(m))">{{ probingId === m.id ? '探测中…' : '立即探测' }}</button>
                <span class="hspacer" />
                <button class="btn-link sm" @click="toggleMonitor(m)">{{ m.enabled ? '停用' : '启用' }}</button>
                <button class="icon-btn" @click="editMonitor(m)"><Icon name="edit" :size="16" /></button>
                <button class="icon-btn danger" @click="delMonitor(m)"><Icon name="trash" :size="16" /></button>
              </div>
            </div>
            <div v-if="!monitors.length" class="empty">还没有监控项，点右上角新增。每个监控项 = 一个渠道 + 一个模型。</div>
          </div>
        </template>

        <!-- 设置页 -->
        <template v-else-if="page === 'settings'">
          <div class="card" style="max-width:520px">
            <div class="field"><label>路由探测间隔</label><input v-model="probeInterval" placeholder="如 30s / 2m / 1h（驱动熔断与回切）" /></div>
            <div class="field"><label>看板探测间隔</label><input v-model="monitorInterval" placeholder="如 5m / 1m（监控成功率与延迟）" /></div>
            <div class="settings-info">
              <div>路由实际生效：<b>{{ effectiveProbeInterval || '—' }}</b>，来源：{{ sourceText(probeSource) }}</div>
              <div>看板实际生效：<b>{{ effectiveMonitorInterval || '—' }}</b>，来源：{{ sourceText(monitorSource) }}</div>
            </div>
            <div class="dialog-foot">
              <button class="btn" @click="saveSettings"><Icon name="check" :size="16" />保存</button>
              <span class="save-status" :class="{ show: settingsSaved }">已保存 ✓</span>
            </div>
          </div>

          <div class="card" style="max-width:560px;margin-top:16px">
            <h3 style="margin:0 0 4px">接入地址</h3>
            <p class="hint" style="margin:0 0 12px">客户端用接入密钥访问（请求头 <code>Authorization: Bearer &lt;密钥&gt;</code>）：</p>
            <div class="field"><label>OpenAI</label><code>{{ apiBase }}/v1/chat/completions</code></div>
            <div class="field"><label>Responses（Codex）</label><code>{{ apiBase }}/v1/responses</code></div>
            <div class="field"><label>Claude</label><code>{{ apiBase }}/v1/messages</code></div>
          </div>
        </template>
      </main>
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
          <select v-model="testState.model" :disabled="testState.modelsLoading || testState.running" class="select">
            <option v-if="testState.modelsLoading" value="">加载模型中…</option>
            <option v-for="m in testState.models" :key="m" :value="m">{{ m }}</option>
            <option v-if="!testState.modelsLoading && !testState.models.length" :value="testState.model">{{ testState.model }}</option>
          </select>
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
          <div class="field"><label>api_key</label><input v-model="dlg.form.api_key" :placeholder="dlg.form.id ? '留空则不修改' : 'sk-...'" /></div>
          <div class="field"><label>代理</label><input v-model="dlg.form.proxy" placeholder="留空=直连/环境变量；如 http://127.0.0.1:7890 或 socks5://127.0.0.1:1080" /></div>
          <label class="check"><input type="checkbox" v-model="dlg.form.enabled" /> 启用</label>
          <div class="dialog-foot"><button class="btn btn-ghost" @click="closeDlg">取消</button><button class="btn" @click="saveUpstream">保存</button></div>
        </template>

        <template v-else-if="dlg.type === 'monitor'">
          <h3>{{ dlg.form.id ? '编辑监控项' : '新增监控项' }}</h3>
          <div class="field">
            <label>渠道</label>
            <select v-model="dlg.form.upstream_id" class="filter-select" style="width:100%" @change="loadMonModels(Number(dlg.form.upstream_id))">
              <option v-for="u in upstreams" :key="u.id" :value="u.id">{{ u.name }} — {{ u.base_url }}</option>
            </select>
          </div>
          <div class="field">
            <label>模型</label>
            <input v-model="dlg.form.model" list="mon-models" placeholder="如 gpt-4o，可从下拉选或手填" />
            <datalist id="mon-models"><option v-for="m in monModels" :key="m" :value="m" /></datalist>
          </div>
          <div class="field"><label>备注名</label><input v-model="dlg.form.name" placeholder="可选，留空则显示「渠道 · 模型」" /></div>
          <label class="check"><input type="checkbox" v-model="dlg.form.enabled" /> 启用</label>
          <div class="dialog-foot"><button class="btn btn-ghost" @click="closeDlg">取消</button><button class="btn" @click="saveMonitor">保存</button></div>
        </template>

        <template v-else-if="dlg.type === 'member'">
          <h3>{{ dlg.form.locked ? '调整组内策略' : '添加上游到分组' }}</h3>
          <div class="field" v-if="!dlg.form.locked">
            <label>选择上游</label>
            <select v-model="dlg.form.upstream_id" class="filter-select" style="width:100%">
              <option v-for="u in addable" :key="u.id" :value="u.id">{{ u.name }} — {{ u.base_url }}</option>
            </select>
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
