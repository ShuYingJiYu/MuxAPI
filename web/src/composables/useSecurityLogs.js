import { computed, reactive, ref } from 'vue'
import { api } from '../api.js'

export const securityRanges = [
  { value: '1h', label: '最近 1 小时' }, { value: '24h', label: '最近 24 小时' },
  { value: '7d', label: '最近 7 天' }, { value: '15d', label: '最近 15 天' },
]
export const securityResults = [
  { value: '', label: '全部结果' }, { value: 'failure', label: '验证失败' },
  { value: 'success', label: '验证成功' }, { value: 'attention', label: '需关注' },
]
export const securityKinds = [
  { value: '', label: '全部事件' }, { value: 'login', label: '登录验证' }, { value: 'access', label: '管理访问' },
]
export const securityPageSizes = [20, 50, 100].map(value => ({ value, label: `${value} 条` }))
const defaultFilters = () => ({ ip: '', result: '', kind: '', range: '24h' })
const normalize = filters => ({ ...filters, ip: filters.ip.trim() })

export function securityPageItems(total, current) {
  if (total <= 7) return Array.from({ length: total }, (_, i) => i + 1)
  const start = current <= 4 ? 2 : Math.max(2, Math.min(current - 1, total - 4))
  const end = current >= total - 3 ? total - 1 : Math.min(total - 1, Math.max(current + 1, 5))
  const items = [1]
  if (start > 2) items.push('left')
  for (let i = start; i <= end; i++) items.push(i)
  if (end < total - 1) items.push('right')
  return [...items, total]
}

// draft 只代表输入；翻页、切换视图与刷新始终使用查询成功后的 applied。
// 后端返回时间边界和实际页码，避免浏览器时钟及过期末页改变查询行为。
export function useSecurityLogs({ fetchEntries = api.securityLogs, onUnauthorized = () => {} } = {}) {
  const draft = reactive(defaultFilters())
  const applied = ref(defaultFilters())
  const view = ref('events')
  const data = ref(null)
  const page = ref(1)
  const pageSize = ref(20)
  const loading = ref(false)
  const error = ref('')
  const jump = ref('')
  const pages = computed(() => Math.max(1, Math.ceil((data.value?.total || 0) / pageSize.value)))
  const pageItems = computed(() => securityPageItems(pages.value, page.value))
  const pendingFilters = computed(() => JSON.stringify(normalize(draft)) !== JSON.stringify(applied.value))
  const rangeStart = computed(() => data.value?.total ? (page.value - 1) * pageSize.value + 1 : 0)
  const rangeEnd = computed(() => data.value?.total ? Math.min(page.value * pageSize.value, data.value.total) : 0)
  let controller
  let epoch = 0
  let window = null
  let lastAttempt

  async function request({ filters, targetView, targetPage, size, bounds }) {
    const attempt = { filters: { ...filters }, targetView, targetPage, size, bounds: bounds && { ...bounds } }
    lastAttempt = attempt
    const current = ++epoch
    controller?.abort()
    controller = new AbortController()
    loading.value = true
    error.value = ''
    try {
      const next = await fetchEntries({
        ip: filters.ip, result: filters.result, kind: filters.kind, view: targetView,
        page: targetPage, size, ...(bounds || { range: filters.range }),
      }, controller.signal)
      if (current !== epoch) return false
      // 只在成功后提交页码与筛选，失败时原记录、统计和页码保持一致。
      data.value = next
      applied.value = { ...filters }
      view.value = targetView
      pageSize.value = next.size
      page.value = next.page
      window = { from: next.from, to: next.to }
      jump.value = ''
      return true
    } catch (e) {
      if (current !== epoch || e.name === 'AbortError') return false
      if (e.status === 401) onUnauthorized()
      error.value = e.status === 400 ? '筛选条件无效，请检查来源 IP 和查询条件。' : '安全日志读取失败，请重试。'
      return false
    } finally {
      if (current === epoch) loading.value = false
    }
  }

  const query = (filters, targetView, targetPage = 1, size = pageSize.value, bounds = null) => request({ filters, targetView, targetPage, size, bounds })
  function applyFilters() { return query(normalize(draft), view.value) }
  function refresh() { return query(applied.value, view.value) }
  function resetFilters() {
    Object.assign(draft, defaultFilters())
    return applyFilters()
  }
  function changeView(value) {
    if (value === view.value || !['events', 'all'].includes(value)) return false
    return query(applied.value, value, 1, pageSize.value, window)
  }
  function changeSize(value) {
    const size = Number(value)
    if (![20, 50, 100].includes(size) || size === pageSize.value) return false
    return query(applied.value, view.value, 1, size, window)
  }
  function goPage(value) {
    if (String(value).trim() === '') return false
    const number = Number(value)
    if (!Number.isInteger(number)) return false
    const target = Math.max(1, Math.min(number, pages.value))
    if (target === page.value) { jump.value = ''; return false }
    return query(applied.value, view.value, target, pageSize.value, window)
  }
  function retry() { return lastAttempt ? request(lastAttempt) : refresh() }
  function dispose() { epoch++; controller?.abort(); loading.value = false }

  return {
    draft, applied, view, data, page, pageSize, loading, error, jump, pages, pageItems,
    pendingFilters, rangeStart, rangeEnd, applyFilters, refresh, resetFilters,
    changeView, changeSize, goPage, retry, dispose,
  }
}
