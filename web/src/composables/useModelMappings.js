import { computed, reactive, ref, watch } from 'vue'
import { api } from '../api.js'

export function mappingScope(row) {
  if (Number(row.upstream_id) === 0) return '全局规则'
  return row.upstream_missing ? '上游已移除' : row.upstream_name || `上游 ${row.upstream_id}`
}

export function mappingStatus(row, now = Date.now()) {
  if (row.upstream_missing) return { key: 'orphan', label: '失效归属' }
  if (!row.target_model) return { key: 'pending', label: '尚未生成目标' }
  if (row.expires_at && new Date(row.expires_at).getTime() < now) return { key: 'expired', label: '已过期' }
  return { key: 'active', label: '有效' }
}

export function mappingPageItems(total, current) {
  if (total <= 7) return Array.from({ length: total }, (_, i) => i + 1)
  const from = Math.max(2, Math.min(current - 1, total - 4))
  const to = Math.min(total - 1, Math.max(current + 1, 5))
  const items = [1]
  if (from > 2) items.push('left')
  for (let i = from; i <= to; i++) items.push(i)
  if (to < total - 1) items.push('right')
  return [...items, total]
}

export function useModelMappings({ client = api, onUnauthorized = () => {} } = {}) {
  const entries = ref([])
  const loaded = ref(false)
  const loading = ref(false)
  const busy = ref(false)
  const loadError = ref('')
  const actionError = ref('')
  const notice = ref('')
  const tab = ref('rules')
  const filters = reactive({ query: '', scope: '', type: '', status: '' })
  const now = ref(Date.now())
  const page = ref(1)
  const pageSize = ref(20)
  const orphans = computed(() => entries.value.filter(row => row.upstream_missing && Number(row.upstream_id) !== 0))
  const rules = computed(() => entries.value.filter(row => !row.upstream_missing))
  const stats = computed(() => ({
    active: rules.value.filter(row => mappingStatus(row, now.value).key === 'active').length,
    manual: rules.value.filter(row => row.mapping_type !== 'auto').length,
    auto: rules.value.filter(row => row.mapping_type === 'auto').length,
    orphan: orphans.value.length,
  }))
  const filtered = computed(() => {
    const query = filters.query.trim().toLowerCase()
    return (tab.value === 'orphans' ? orphans.value : rules.value).filter(row => {
      if (query && ![row.source_model, row.target_model, mappingScope(row)].join(' ').toLowerCase().includes(query)) return false
      if (tab.value !== 'orphans' && filters.scope !== '' && Number(filters.scope) !== Number(row.upstream_id)) return false
      if (filters.type && row.mapping_type !== filters.type) return false
      return tab.value === 'orphans' || !filters.status || mappingStatus(row, now.value).key === filters.status
    })
  })
  const pages = computed(() => Math.max(1, Math.ceil(filtered.value.length / pageSize.value)))
  const pageItems = computed(() => mappingPageItems(pages.value, page.value))
  const visible = computed(() => filtered.value.slice((page.value - 1) * pageSize.value, page.value * pageSize.value))
  const stops = [watch([tab, () => filters.query, () => filters.scope, () => filters.type, () => filters.status, pageSize], () => { page.value = 1 }, { flush: 'sync' }),
    watch(pages, value => { page.value = Math.min(page.value, value) }, { flush: 'sync' })]
  let epoch = 0
  let controller
  let disposed = false

  function fail(error, fallback) {
    if (error.status === 401) onUnauthorized()
    if (error.status === 423) return '当前为只读模式，无法修改规则。'
    if (error.status === 400) return '操作未完成，请检查模型名称及上游是否仍存在。'
    return fallback
  }

  async function load() {
    const current = ++epoch
    controller?.abort()
    controller = new AbortController()
    loading.value = true
    loadError.value = ''
    try {
      const next = await client.modelMappings(undefined, controller.signal)
      if (disposed || current !== epoch) return false
      entries.value = next || []
      loaded.value = true
      now.value = Date.now()
      return true
    } catch (error) {
      if (disposed || current !== epoch || error.name === 'AbortError') return false
      loadError.value = fail(error, '规则读取失败，请重试。')
      return false
    } finally {
      if (!disposed && current === epoch) loading.value = false
    }
  }

  // 修改请求不随弹窗关闭而取消；仅阻止组件卸载后的响应修改页面。
  async function mutate(action, success) {
    if (disposed || busy.value || loading.value) return false
    busy.value = true
    actionError.value = ''
    notice.value = ''
    try {
      const result = await action()
      if (disposed) return false
      notice.value = success(result)
      await load()
      return !disposed
    } catch (error) {
      if (!disposed) actionError.value = fail(error, '操作未确认成功，请刷新核实后重试。')
      return false
    } finally {
      if (!disposed) busy.value = false
    }
  }

  const save = payload => mutate(() => client.createModelMapping(payload), () => '规则已保存。')
  const remove = row => mutate(async () => {
    try { await client.deleteModelMapping(row.id) }
    catch (error) { if (error.status !== 404) throw error }
    if (!disposed) entries.value = entries.value.filter(item => item.id !== row.id)
  }, () => '规则已删除。')
  const cleanup = ids => mutate(() => client.cleanupModelMappings(ids), result => `已清理 ${result.deleted} 条失效规则；全局规则与现有上游规则保留。`)
  function reset() { Object.assign(filters, { query: '', scope: '', type: '', status: '' }) }
  function goPage(value) {
    const target = Number(value)
    if (String(value).trim() && Number.isInteger(target)) page.value = Math.max(1, Math.min(target, pages.value))
  }
  function dispose() { disposed = true; epoch++; controller?.abort(); stops.forEach(stop => stop()) }

  return { entries, loaded, loading, busy, loadError, actionError, notice, tab, filters, now,
    page, pageSize, orphans, rules, stats, filtered, pages, pageItems, visible, load, save, remove, cleanup, reset, goPage, dispose }
}
