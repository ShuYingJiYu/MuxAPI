// 轻量 admin API 封装：所有请求经 vite proxy /admin → 后端 8080。
// adminToken 存在 localStorage（后端 AdminToken 为空时鉴权跳过，本地调试免填）。
const token = () => localStorage.getItem('muxapi_token') || ''

async function req(method, path, body) {
  const headers = { 'Content-Type': 'application/json' }
  const t = token()
  if (t) headers.Authorization = 'Bearer ' + t
  const res = await fetch('/admin' + path, {
    method,
    headers,
    body: body ? JSON.stringify(body) : undefined,
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
  logDetail: id => req('GET', '/logs/' + id),
  logOptions: () => req('GET', '/logs/options'),
}
