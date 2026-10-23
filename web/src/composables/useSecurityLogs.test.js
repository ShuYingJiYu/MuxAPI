import test from 'node:test'
import assert from 'node:assert/strict'
import { useSecurityLogs, securityPageItems } from './useSecurityLogs.js'

function response(params, overrides = {}) {
  return { entries: [{ id: params.page }], total: 121, size: params.size, page: params.page, from: 100, to: 200, ...overrides }
}

test('draft filters never leak into pagination, size, view or refresh', async () => {
  const calls = []
  const model = useSecurityLogs({ fetchEntries: async params => { calls.push(params); return response(params) } })
  await model.refresh()
  assert.equal(calls[0].view, 'events')
  assert.equal(calls[0].range, '24h')
  Object.assign(model.draft, { ip: '192.0.2.1', kind: 'login', result: 'failure', range: '7d' })
  assert.equal(model.pendingFilters.value, true)
  await model.goPage(2)
  assert.equal(calls[1].ip, '')
  assert.equal(calls[1].kind, '')
  assert.equal(calls[1].from, 100)
  assert.equal(calls[1].to, 200)
  assert.equal('range' in calls[1], false)
  await model.changeSize(50)
  assert.equal(calls[2].page, 1)
  assert.equal(calls[2].size, 50)
  assert.equal(calls[2].ip, '')
  await model.changeView('all')
  assert.equal(calls[3].view, 'all')
  assert.equal(calls[3].kind, '')
  await model.refresh()
  assert.equal(calls[4].range, '24h')
  assert.equal('from' in calls[4], false)
  await model.applyFilters()
  assert.equal(calls[5].ip, '192.0.2.1')
  assert.equal(calls[5].kind, 'login')
  assert.equal(calls[5].range, '7d')
  assert.equal(calls[5].view, 'all')
  assert.equal(calls[5].page, 1)
  assert.equal(model.pendingFilters.value, false)
  await model.resetFilters()
  assert.equal(model.draft.ip, '')
  assert.equal(calls[6].range, '24h')
  assert.equal(calls[6].view, 'all')
  model.dispose()
})

test('loading and errors retain current rows and committed page; retry repeats failed request', async () => {
  let resolve, reject
  const calls = []
  let manual = false
  const model = useSecurityLogs({ fetchEntries: params => {
    calls.push(params)
    return manual ? new Promise((ok, fail) => { resolve = ok; reject = fail }) : Promise.resolve(response(params))
  } })
  await model.refresh()
  const original = model.data.value
  manual = true
  const pending = model.goPage(2)
  assert.equal(model.loading.value, true)
  assert.equal(model.data.value, original)
  assert.equal(model.page.value, 1)
  reject(new Error('offline'))
  await pending
  assert.equal(model.loading.value, false)
  assert.equal(model.data.value, original)
  assert.equal(model.page.value, 1)
  const retry = model.retry()
  assert.deepEqual(calls[2], calls[1])
  resolve(response(calls[2]))
  await retry
  assert.equal(model.page.value, 2)
  assert.equal(model.error.value, '')
  model.dispose()
})

test('out-of-order results and unmounted responses cannot overwrite current data', async () => {
  const pending = []
  const model = useSecurityLogs({ fetchEntries: (params, signal) => new Promise(resolve => pending.push({ params, signal, resolve })) })
  const first = model.refresh()
  model.draft.ip = '192.0.2.2'
  const second = model.applyFilters()
  assert.equal(pending[0].signal.aborted, true)
  pending[1].resolve(response(pending[1].params, { total: 3 }))
  await second
  pending[0].resolve(response(pending[0].params, { total: 99 }))
  await first
  assert.equal(model.applied.value.ip, '192.0.2.2')
  assert.equal(model.data.value.total, 3)
  const last = model.refresh()
  model.dispose()
  pending[2].resolve(response(pending[2].params, { total: 55 }))
  await last
  assert.equal(model.data.value.total, 3)
})

test('server-normalized pages, invalid controls, and unauthorized results', async () => {
  let unauthorized = 0
  let failed = false
  const model = useSecurityLogs({
    fetchEntries: async params => {
      if (failed) throw Object.assign(new Error('unauthorized'), { status: 401 })
      return response(params, { page: 1, total: 0, entries: [] })
    },
    onUnauthorized: () => unauthorized++,
  })
  await model.refresh()
  assert.equal(model.pages.value, 1)
  assert.equal(model.rangeStart.value, 0)
  assert.equal(model.rangeEnd.value, 0)
  assert.equal(model.goPage('invalid'), false)
  assert.equal(model.goPage(''), false)
  assert.equal(model.goPage('  '), false)
  assert.equal(model.changeSize(999), false)
  assert.equal(model.changeView('invalid'), false)
  failed = true
  await model.refresh()
  assert.equal(unauthorized, 1)
  assert.equal(model.data.value.total, 0)
  model.dispose()
})

test('page numbers include first, last, neighbors and bounded ellipses', () => {
  assert.deepEqual(securityPageItems(1, 1), [1])
  assert.deepEqual(securityPageItems(7, 4), [1, 2, 3, 4, 5, 6, 7])
  assert.deepEqual(securityPageItems(20, 1), [1, 2, 3, 4, 5, 'right', 20])
  assert.deepEqual(securityPageItems(20, 10), [1, 'left', 9, 10, 11, 'right', 20])
  assert.deepEqual(securityPageItems(20, 20), [1, 'left', 16, 17, 18, 19, 20])
})

test('empty page jumps preserve the current page without making a request', async () => {
  let calls = 0
  const model = useSecurityLogs({ fetchEntries: async params => { calls++; return response(params) } })
  await model.refresh()
  await model.goPage(2)
  assert.equal(model.goPage(''), false)
  assert.equal(model.goPage('  '), false)
  assert.equal(model.page.value, 2)
  assert.equal(calls, 2)
  model.dispose()
})

test('failed view and page-size changes do not commit controls or discard draft filters', async () => {
  let failed = false
  const calls = []
  const model = useSecurityLogs({ fetchEntries: async params => {
    calls.push(params)
    if (failed) throw new Error('offline')
    return response(params)
  } })
  await model.refresh()
  await model.goPage(2)
  model.draft.ip = '192.0.2.3'
  failed = true
  await model.changeView('all')
  assert.equal(model.view.value, 'events')
  assert.equal(model.page.value, 2)
  await model.changeSize(100)
  assert.equal(model.pageSize.value, 20)
  assert.equal(model.page.value, 2)
  assert.equal(model.draft.ip, '192.0.2.3')
  assert.equal(calls.at(-1).ip, '')
  failed = false
  await model.retry()
  assert.equal(model.pageSize.value, 100)
  assert.equal(model.page.value, 1)
  assert.equal(model.pendingFilters.value, true)
  model.dispose()
})
