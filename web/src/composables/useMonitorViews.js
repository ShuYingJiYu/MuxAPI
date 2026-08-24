import { computed, reactive, ref } from 'vue'

const CHANNEL_STATE_RANK = { DOWN: 0, DEGRADED: 1, NODATA: 2, OK: 3, DISABLED: 4 }

// 组装监控页与总览页共享的只读视图状态。
export function useMonitorViews({ monitors, upstreams, tags, probeOne }) {
  const primaryTagFor = upstream => upstream?.tags?.find(tag => tag.is_primary) || null
  const auxiliaryTagsFor = upstream => (upstream?.tags || []).filter(tag => !tag.is_primary)
  const tagGroupKey = tag => tag ? `tag-${tag.id}` : 'untagged'
  const tagGroupName = tag => tag?.name || '未分类'

  function monitorItemState(monitor, upstream) {
    if (!monitor.enabled || !upstream?.enabled) return 'DISABLED'
    if (upstream.health?.state === 'OPEN') return 'DOWN'
    if (upstream.health?.state === 'HALF_OPEN') return 'DEGRADED'
    return monitor.snapshot?.state || 'NODATA'
  }

  // 将监控项与上游元数据合并，后续筛选和汇总只消费这一种视图对象。
  const monitorItems = computed(() => {
    const upstreamByID = new Map(upstreams.value.map(item => [item.id, item]))
    return monitors.value.map(monitor => {
      const upstream = upstreamByID.get(monitor.upstream_id) || { id: monitor.upstream_id, name: monitor.upstream_name, enabled: true, tags: [] }
      const primaryTag = primaryTagFor(upstream)
      return { ...monitor, upstream, primaryTag, groupKey: tagGroupKey(primaryTag), state: monitorItemState(monitor, upstream) }
    })
  })

  const monitorSearch = ref('')
  const monitorStatusFilter = ref('')
  const monitorTagFilter = ref('')
  const collapsedMonitorTags = reactive(new Set())
  const monitorStatusOptions = [
    { value: '', label: '状态：全部' },
    { value: 'OK', label: '可用' },
    { value: 'DEGRADED', label: '波动' },
    { value: 'DOWN', label: '不可用' },
    { value: 'NODATA', label: '待检测' },
    { value: 'DISABLED', label: '已停用' },
  ]
  const monitorTagOptions = computed(() => [
    { value: '', label: '主标签：全部' },
    { value: 'untagged', label: '未分类' },
    ...tags.value.map(tag => ({ value: `tag-${tag.id}`, label: tag.name })),
  ])
  function aggregateChannelTrend(items) {
    const buckets = new Map()
    for (const item of items) {
      for (const point of item.snapshot?.trend || []) {
        const ts = Number(point.ts) || 0
        if (!ts) continue
        if (!buckets.has(ts)) buckets.set(ts, { ts, total: 0, succ: 0 })
        const bucket = buckets.get(ts)
        bucket.total += Number(point.total) || 0
        bucket.succ += Number(point.succ) || 0
      }
    }
    return [...buckets.values()].sort((a, b) => a.ts - b.ts).map(point => {
      const succRate = point.total ? point.succ / point.total : 0
      const status = !point.total ? 0 : succRate >= .95 ? 1 : succRate >= .8 ? 2 : 3
      return { ...point, succ_rate: succRate, status }
    })
  }

  // 渠道是监控页的主实体；模型探测只用于组成渠道的当前状态和历史可用率。
  const monitorChannels = computed(() => {
    const upstreamOrder = new Map(upstreams.value.map((item, index) => [Number(item.id), index]))
    const grouped = new Map()
    for (const item of monitorItems.value) {
      const id = Number(item.upstream_id)
      if (!grouped.has(id)) grouped.set(id, [])
      grouped.get(id).push(item)
    }
    return [...grouped.entries()].map(([id, items]) => {
      const upstream = items[0].upstream
      const enabled = items.filter(item => item.state !== 'DISABLED')
      const states = enabled.map(item => item.state)
      let state = 'DISABLED'
      if (enabled.length) {
        if (states.every(value => value === 'DOWN')) state = 'DOWN'
        else if (states.every(value => value === 'NODATA')) state = 'NODATA'
        else if (states.some(value => value !== 'OK')) state = 'DEGRADED'
        else state = 'OK'
      }
      const reqs = enabled.reduce((sum, item) => sum + (Number(item.snapshot?.reqs) || 0), 0)
      const success = enabled.reduce((sum, item) => sum + (Number(item.snapshot?.reqs) || 0) * (Number(item.snapshot?.succ_rate) || 0), 0)
      const latencyWeight = enabled.reduce((sum, item) => sum + ((Number(item.snapshot?.avg_ms) || 0) > 0 ? (Number(item.snapshot?.reqs) || 1) : 0), 0)
      const latencyTotal = enabled.reduce((sum, item) => {
        const avg = Number(item.snapshot?.avg_ms) || 0
        return sum + (avg > 0 ? avg * (Number(item.snapshot?.reqs) || 1) : 0)
      }, 0)
      const latest = enabled.reduce((result, item) => Number(item.snapshot?.last_ts) > Number(result?.snapshot?.last_ts || 0) ? item : result, null)
      const primaryTag = primaryTagFor(upstream)
      return {
        id, upstream, primaryTag, groupKey: tagGroupKey(primaryTag), monitors: items,
        enabledCount: enabled.length, state, reqs, rate: reqs ? success / reqs : 0,
        avgMs: latencyWeight ? Math.round(latencyTotal / latencyWeight) : 0,
        lastTS: Number(latest?.snapshot?.last_ts) || 0,
        lastMs: Number(latest?.snapshot?.last_ms) || 0,
        trend: aggregateChannelTrend(enabled), order: upstreamOrder.get(id) ?? Number.MAX_SAFE_INTEGER,
      }
    }).sort((a, b) => a.order - b.order)
  })

  const monitorSections = computed(() => {
    const query = monitorSearch.value.trim().toLowerCase()
    const tagOrder = new Map(tags.value.map((tag, index) => [tag.id, index]))
    const filtered = monitorChannels.value.filter(channel => {
      if (monitorStatusFilter.value && channel.state !== monitorStatusFilter.value) return false
      if (monitorTagFilter.value && channel.groupKey !== monitorTagFilter.value) return false
      if (!query) return true
      return [channel.upstream.name, channel.upstream.base_url, ...channel.monitors.map(item => item.model), ...(channel.upstream.tags || []).map(tag => tag.name)]
        .some(value => String(value || '').toLowerCase().includes(query))
    })
    const sections = new Map()
    for (const channel of filtered) {
      if (!sections.has(channel.groupKey)) sections.set(channel.groupKey, { tag: channel.primaryTag, items: [] })
      sections.get(channel.groupKey).items.push(channel)
    }
    return [...sections.entries()].map(([key, section]) => {
      const items = section.items
      const enabled = items.filter(channel => channel.state !== 'DISABLED')
      const reqs = enabled.reduce((sum, channel) => sum + channel.reqs, 0)
      const success = enabled.reduce((sum, channel) => sum + channel.reqs * channel.rate, 0)
      return {
        key, tag: section.tag, name: tagGroupName(section.tag),
        items: [...items].sort((a, b) => (CHANNEL_STATE_RANK[a.state] ?? 5) - (CHANNEL_STATE_RANK[b.state] ?? 5) || a.order - b.order),
        up: items.filter(channel => channel.state === 'OK').length,
        down: items.filter(channel => channel.state === 'DOWN').length,
        degraded: items.filter(channel => channel.state === 'DEGRADED').length,
        nodata: items.filter(channel => channel.state === 'NODATA').length,
        enabled: enabled.length, rate: reqs ? success / reqs : 0, reqs,
      }
    }).sort((a, b) => (tagOrder.get(a.tag?.id) ?? Number.MAX_SAFE_INTEGER) - (tagOrder.get(b.tag?.id) ?? Number.MAX_SAFE_INTEGER))
  })

  const monitorVisibleCount = computed(() => monitorSections.value.reduce((sum, section) => sum + section.items.length, 0))
  const summary = computed(() => {
    const items = monitorChannels.value.filter(channel => channel.state !== 'DISABLED')
    const down = items.filter(channel => channel.state === 'DOWN').length
    const degraded = items.filter(channel => channel.state === 'DEGRADED').length
    const nodata = items.filter(channel => channel.state === 'NODATA').length
    const reqs = items.reduce((sum, channel) => sum + channel.reqs, 0)
    const success = items.reduce((sum, channel) => sum + channel.reqs * channel.rate, 0)
    return {
      total: items.length, down, degraded, nodata,
      up: items.filter(channel => channel.state === 'OK').length,
      rate: reqs ? success / reqs : 0,
      probeReqs: reqs,
      allOk: items.length > 0 && down === 0 && degraded === 0 && nodata === 0,
    }
  })
  function toggleMonitorTag(key) {
    if (collapsedMonitorTags.has(key)) collapsedMonitorTags.delete(key)
    else collapsedMonitorTags.add(key)
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
  return {
    primaryTagFor, auxiliaryTagsFor, tagGroupKey, tagGroupName,
    monitorItems, monitorChannels, monitorSearch, monitorStatusFilter,
    monitorTagFilter, collapsedMonitorTags, monitorStatusOptions, monitorTagOptions,
    monitorSections, monitorVisibleCount, summary, toggleMonitorTag, matrix, ovSummary,
    cellDrawerId, cellDrawer, openCell, closeCell, probeCell,
  }
}
