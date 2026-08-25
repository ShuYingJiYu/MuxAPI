// 轻量 admin API 封装：所有请求经 vite proxy /admin → 后端 8080。
// adminToken 存在 localStorage（后端 AdminToken 为空时鉴权跳过，本地调试免填）。
const token = () => localStorage.getItem('muxapi_token') || ''
const REQUEST_TIMEOUT_MS = 15000

async function req(method, path, body) {
  const headers = { 'Content-Type': 'application/json' }
  const t = token()
  if (t) headers.Authorization = 'Bearer ' + t
  const controller = new AbortController()
  const timeout = window.setTimeout(() => controller.abort(), REQUEST_TIMEOUT_MS)
  try {
    const res = await fetch('/admin' + path, {
      method,
      headers,
      body: body ? JSON.stringify(body) : undefined,
      signal: controller.signal,
    })
    // 保留状态码供页面统一处理 401，响应文本作为具体错误信息。
    if (!res.ok) {
      const e = new Error((await res.text()) || String(res.status))
      e.status = res.status
      throw e
    }
    // DELETE/PUT 可能返回空正文，只对 JSON 响应调用解析器。
    const ct = res.headers.get('content-type') || ''
    return ct.includes('json') ? res.json() : null
  } catch (e) {
    if (controller.signal.aborted) throw new Error('服务器响应超时，请重新加载。')
    throw e
  } finally {
    window.clearTimeout(timeout)
  }
}

function groupTestRequest(protocol, model) {
  if (protocol === 'claude') {
    return { path: '/v1/messages', body: { model, messages: [{ role: 'user', content: 'hi' }], max_tokens: 32, stream: true } }
  }
  if (protocol === 'chat') {
    return { path: '/v1/chat/completions', body: { model, messages: [{ role: 'user', content: 'hi' }], max_tokens: 32, stream: true } }
  }
  return { path: '/v1/responses', body: { model, input: 'hi', max_output_tokens: 32, stream: true } }
}

function groupTestText(protocol, payload) {
  if (protocol === 'claude') return payload?.delta?.text || ''
  if (protocol === 'chat') return payload?.choices?.[0]?.delta?.content || ''
  return payload?.type === 'response.output_text.delta' ? (payload.delta || '') : ''
}

function groupTestBodyText(protocol, payload) {
  if (protocol === 'claude') return (payload?.content || []).filter(item => item.type === 'text').map(item => item.text || '').join('')
  if (protocol === 'chat') return payload?.choices?.[0]?.message?.content || ''
  return (payload?.output || []).flatMap(item => item.content || []).filter(item => item.type === 'output_text').map(item => item.text || '').join('')
}

async function testGroupStream({ key, protocol, model }, onText) {
  const request = groupTestRequest(protocol, model)
  const response = await fetch(request.path, {
    method: 'POST',
    headers: { Authorization: 'Bearer ' + key, 'Content-Type': 'application/json', Accept: 'text/event-stream' },
    body: JSON.stringify(request.body),
  })
  const requestId = response.headers.get('x-request-id') || ''
  if (!response.ok) {
    const error = new Error((await response.text()) || `HTTP ${response.status}`)
    error.status = response.status
    error.requestId = requestId
    throw error
  }
  const contentType = response.headers.get('content-type') || ''
  if (!contentType.includes('text/event-stream')) {
    const payload = await response.json()
    const text = groupTestBodyText(protocol, payload)
    if (text) onText(text)
    return { requestId, status: response.status }
  }
  const reader = response.body.getReader()
  const decoder = new TextDecoder()
  let buffer = ''
  const consume = block => {
    const data = block.split(/\r?\n/).filter(line => line.startsWith('data:')).map(line => line.slice(5).trim()).join('\n')
    if (!data || data === '[DONE]') return
    let payload
    try { payload = JSON.parse(data) } catch { return }
    const message = payload?.error?.message || (payload?.type === 'error' ? payload?.error?.message : '')
    if (message) throw new Error(message)
    const text = groupTestText(protocol, payload)
    if (text) onText(text)
  }
  for (;;) {
    const { done, value } = await reader.read()
    if (done) break
    buffer += decoder.decode(value, { stream: true })
    const blocks = buffer.split(/\r?\n\r?\n/)
    buffer = blocks.pop()
    blocks.forEach(consume)
  }
  buffer += decoder.decode()
  if (buffer.trim()) consume(buffer)
  return { requestId, status: response.status }
}

export const api = {
  getToken: token,
  setToken: t => localStorage.setItem('muxapi_token', t),
  clearToken: () => localStorage.removeItem('muxapi_token'),
  // 上游全局池
  upstreams: () => req('GET', '/upstreams'),
  createUpstream: u => req('POST', '/upstreams', u),
  updateUpstream: (id, u) => req('PUT', '/upstreams/' + id, u),
  deleteUpstream: id => req('DELETE', '/upstreams/' + id),
  batchUpdateUpstreams: payload => req('POST', '/upstreams/batch', payload),
  reorderUpstreams: ids => req('POST', '/upstreams/reorder', { ids }),
  testUpstream: id => req('GET', `/upstreams/${id}/models`),
  recoverUpstream: id => req('POST', `/upstreams/${id}/recover`),
  recoverUpstreamModel: (id, model) => req('POST', `/upstreams/${id}/models/recover`, { model }),
  refreshUpstreamBilling: id => req('POST', `/upstreams/${id}/billing/refresh`),
  upstreamBillingAudit: (id, window) => req('GET', `/upstreams/${id}/billing/audit?window=${encodeURIComponent(window || '')}`),
  overviewTrends: ({ window = '24h', tag_id = 0 } = {}) => {
    const p = new URLSearchParams({ window, _ts: String(Date.now()) })
    if (Number(tag_id) > 0) p.set('tag_id', String(tag_id))
    return req('GET', `/overview/trends?${p.toString()}`)
  },
  overviewSummary: () => req('GET', `/overview/summary?_ts=${Date.now()}`),
  createMonitorsBatch: (id, payload) => req('POST', `/upstreams/${id}/monitors`, payload),
  // 管理标签
  tags: () => req('GET', '/tags'),
  createTag: tag => req('POST', '/tags', tag),
  updateTag: (id, tag) => req('PUT', '/tags/' + id, tag),
  deleteTag: id => req('DELETE', '/tags/' + id),
  // 真实对话测试：发 hi 请求，SSE 逐块回调 onEvent({type,text,...})。EventSource 不能带鉴权头，故用 fetch 流式解析。
  testUpstreamStream: async (id, model, onEvent, signal) => {
    const headers = {}
    const t = token()
    if (t) headers.Authorization = 'Bearer ' + t
    const res = await fetch(`/admin/upstreams/${id}/test?model=${encodeURIComponent(model)}`, {
      method: 'POST', headers, signal,
    })
    if (!res.ok) throw new Error((await res.text()) || res.status)
    const reader = res.body.getReader()
    const dec = new TextDecoder()
    let buf = ''
    for (;;) {
      const { done, value } = await reader.read()
      if (done) break
      buf += dec.decode(value, { stream: true })
      const parts = buf.split('\n\n')
      buf = parts.pop()
      for (const p of parts) {
        const line = p.trim()
        if (line.startsWith('data:')) {
          try { onEvent(JSON.parse(line.slice(5).trim())) } catch {}
        }
      }
    }
  },
  // 分组
  groups: () => req('GET', '/groups'),
  createGroup: g => req('POST', '/groups', g),
  updateGroup: (id, g) => req('PUT', '/groups/' + id, g),
  deleteGroup: id => req('DELETE', '/groups/' + id),
  reorderGroups: ids => req('POST', '/groups/reorder', { ids }),
  groupModels: async key => {
    const response = await fetch('/v1/models', { headers: { Authorization: 'Bearer ' + key } })
    if (!response.ok) throw new Error((await response.text()) || String(response.status))
    const payload = await response.json()
    return (payload.data || []).map(item => item.id).filter(Boolean)
  },
  testGroupStream,
  // 组成员
  members: gid => req('GET', `/groups/${gid}/upstreams`),
  addMember: (gid, m) => req('POST', `/groups/${gid}/upstreams`, m),
  setMemberEnabled: (gid, uid, enabled) => req('PUT', `/groups/${gid}/upstreams/${uid}`, { enabled }),
  removeMember: (gid, uid) => req('DELETE', `/groups/${gid}/upstreams/${uid}`),
  // 组密钥
  keys: gid => req('GET', `/groups/${gid}/keys`),
  createKey: (gid, name) => req('POST', `/groups/${gid}/keys`, { name }),
  setKeyEnabled: (id, enabled) => req('PUT', '/keys/' + id, { enabled }),
  deleteKey: id => req('DELETE', '/keys/' + id),
  // 监控项（渠道+模型，主动探测）
  monitors: () => req('GET', '/monitors'),
  createMonitor: m => req('POST', '/monitors', m),
  updateMonitor: (id, m) => req('PUT', '/monitors/' + id, m),
  deleteMonitor: id => req('DELETE', '/monitors/' + id),
  reorderMonitors: ids => req('POST', '/monitors/reorder', { ids }), // 持久化拖拽顺序
  probeMonitor: id => req('POST', `/monitors/${id}/probe`), // 立即探测一次，返回最新快照
  // 运行时设置
  getSettings: () => req('GET', '/settings'),
  saveSettings: s => req('PUT', '/settings', s),

  // 请求记录（游标分页 + 服务端筛选）。
  logs: (opts = {}) => {
    const p = new URLSearchParams()
    for (const key of ['before', 'offset', 'limit', 'model', 'group', 'status', 'key', 'endpoint', 'error_kind',
      'q', 'stream', 'upstream_id', 'since', 'until', 'slow_ms']) {
      if (opts[key] !== undefined && opts[key] !== null && opts[key] !== '') p.set(key, opts[key])
    }
    if (opts.retried) p.set('retried', 'true')
    const qs = p.toString()
    return req('GET', '/logs' + (qs ? '?' + qs : ''))
  },
  logStats: (opts = {}) => {
    const p = new URLSearchParams()
    for (const key of ['model', 'group', 'status', 'key', 'endpoint', 'error_kind', 'q', 'stream',
      'upstream_id', 'since', 'until', 'slow_ms']) {
      if (opts[key] !== undefined && opts[key] !== null && opts[key] !== '') p.set(key, opts[key])
    }
    if (opts.retried) p.set('retried', 'true')
    const qs = p.toString()
    return req('GET', '/logs/stats' + (qs ? '?' + qs : ''))
  },
  logCacheStats: (opts = {}) => {
    const p = new URLSearchParams()
    for (const key of ['model', 'group', 'status', 'key', 'endpoint', 'error_kind', 'q', 'stream',
      'upstream_id', 'since', 'until', 'slow_ms']) {
      if (opts[key] !== undefined && opts[key] !== null && opts[key] !== '') p.set(key, opts[key])
    }
    if (opts.retried) p.set('retried', 'true')
    const qs = p.toString()
    return req('GET', '/logs/cache-stats' + (qs ? '?' + qs : ''))
  },
  logDetail: id => req('GET', '/logs/' + id),
  logOptions: () => req('GET', '/logs/options'),

  // 数据备份
  backupConfig: () => req('GET', '/backup/config'),
  saveBackupConfig: cfg => req('PUT', '/backup/config', cfg),
  testBackupConfig: cfg => req('POST', '/backup/config/test', cfg),
  backupSchedule: () => req('GET', '/backup/schedule'),
  saveBackupSchedule: s => req('PUT', '/backup/schedule', s),
  triggerBackup: () => req('POST', '/backup', {}),
  listBackups: () => req('GET', '/backup'),
  deleteBackup: id => req('DELETE', '/backup/records/' + id),
  backupDownloadURL: id => req('GET', '/backup/records/' + id + '/download'),
}
