import test from 'node:test'
import assert from 'node:assert/strict'
import { mappingPageItems, mappingScope, mappingStatus, useModelMappings } from './useModelMappings.js'

const rows = () => [
  { id: 1, upstream_id: 0, source_model: 'global-source', target_model: 'global-target', mapping_type: 'static', upstream_missing: false },
  { id: 2, upstream_id: 10, upstream_name: 'Mint', source_model: 'claude-latest', target_model: 'claude', mapping_type: 'auto', upstream_missing: false },
  { id: 3, upstream_id: 10, upstream_name: 'Mint', source_model: 'pending', target_model: '', mapping_type: 'auto', upstream_missing: false },
  { id: 4, upstream_id: 22, source_model: 'old-model', target_model: 'old-target', mapping_type: 'static', upstream_missing: true },
  { id: 5, upstream_id: 10, upstream_name: 'Mint', source_model: 'expired', target_model: 'old', mapping_type: 'auto', expires_at: '2000-01-01T00:00:00Z', upstream_missing: false },
]

test('scope and status distinguish global, removed, expired and pending records', () => {
  const list = rows()
  assert.equal(mappingScope(list[0]), '全局规则')
  assert.equal(mappingScope(list[1]), 'Mint')
  assert.equal(mappingScope(list[3]), '上游已移除')
  assert.equal(mappingStatus(list[3]).key, 'orphan')
  assert.equal(mappingStatus(list[2]).key, 'pending')
  assert.equal(mappingStatus(list[4]).key, 'expired')
  assert.equal(mappingStatus(list[0]).key, 'active')
})

test('filtering and pagination keep orphan records separate without treating globals as missing', async () => {
  const model = useModelMappings({ client: { modelMappings: async () => rows() } })
  await model.load()
  assert.deepEqual(model.stats.value, { active: 2, manual: 1, auto: 3, orphan: 1 })
  assert.equal(model.filtered.value.length, 4)
  model.filters.query = 'mint'; assert.equal(model.filtered.value.length, 3)
  model.filters.type = 'static'; assert.equal(model.filtered.value.length, 0)
  model.reset(); model.filters.scope = 0; assert.equal(model.filtered.value.length, 1)
  model.reset(); model.filters.status = 'pending'; assert.equal(model.filtered.value[0].id, 3)
  model.tab.value = 'orphans'; assert.equal(model.filtered.value[0].id, 4)
  model.reset(); model.tab.value = 'rules'
  model.entries.value = Array.from({ length: 45 }, (_, i) => ({ ...rows()[0], id: i + 1 }))
  model.goPage(3); assert.equal(model.visible.value.length, 5)
  model.entries.value = rows(); assert.equal(model.page.value, 1)
  model.dispose()
})

test('failed delete preserves rows; successful delete refreshes and handles an already removed record', async () => {
  let failed = true
  const list = rows()
  const client = {
    modelMappings: async () => list,
    deleteModelMapping: async id => {
      if (failed) throw new Error('offline')
      list.splice(list.findIndex(row => row.id === id), 1)
      throw Object.assign(new Error('gone'), { status: 404 })
    },
  }
  const model = useModelMappings({ client }); await model.load()
  assert.equal(await model.remove(list[0]), false)
  assert.equal(model.entries.value.length, 5)
  assert.ok(model.actionError.value)
  failed = false
  assert.equal(await model.remove(list[0]), true)
  assert.equal(model.entries.value.length, 4)
  assert.equal(model.actionError.value, '')
  model.dispose()
})

test('cleanup sends only the confirmed ids and reports the actual server count', async () => {
  const calls = []
  const model = useModelMappings({ client: {
    modelMappings: async () => rows(),
    cleanupModelMappings: async ids => { calls.push([...ids]); return { deleted: 0 } },
  } })
  await model.load()
  assert.equal(await model.cleanup([4]), true)
  assert.deepEqual(calls, [[4]])
  assert.match(model.notice.value, /已清理 0 条/)
  assert.equal(model.rules.value.length, 4)
  model.dispose()
})

test('slow and failed refresh retain rows; stale and unmounted responses cannot overwrite them', async () => {
  const pending = []
  const model = useModelMappings({ client: { modelMappings: (_, signal) => new Promise((resolve, reject) => pending.push({ resolve, reject, signal })) } })
  const initial = model.load(); pending[0].resolve(rows()); await initial
  const original = model.entries.value
  const failed = model.load(); assert.equal(model.entries.value, original)
  pending[1].reject(new Error('offline')); await failed
  assert.equal(model.entries.value, original)
  assert.ok(model.loadError.value)
  const old = model.load(), latest = model.load()
  assert.equal(pending[2].signal.aborted, true)
  pending[3].resolve([rows()[0]]); await latest
  pending[2].resolve(rows()); await old
  assert.equal(model.entries.value.length, 1)
  const last = model.load(); model.dispose(); pending[4].resolve(rows()); await last
  assert.equal(model.entries.value.length, 1)
})

test('mutations cannot run twice or during a read, and a successful write is not mislabeled when refresh fails', async () => {
  let finish, readFail = false, writes = 0
  const model = useModelMappings({ client: {
    modelMappings: async () => { if (readFail) throw new Error('offline'); return rows() },
    createModelMapping: () => { writes++; return new Promise(resolve => { finish = resolve }) },
  } })
  await model.load()
  const save = model.save({ source_model: 'source' })
  assert.equal(await model.save({}), false)
  readFail = true; finish({ id: 10 }); assert.equal(await save, true)
  assert.equal(writes, 1)
  assert.match(model.notice.value, /规则已保存/)
  assert.ok(model.loadError.value)
  model.dispose()
})

test('401 signals logout and page controls stay bounded', async () => {
  let unauthorized = 0
  const model = useModelMappings({ client: { modelMappings: async () => { throw Object.assign(new Error('unauthorized'), { status: 401 }) } }, onUnauthorized: () => unauthorized++ })
  await model.load(); assert.equal(unauthorized, 1)
  assert.equal(model.loaded.value, false)
  assert.deepEqual(mappingPageItems(20, 1), [1, 2, 3, 4, 5, 'right', 20])
  assert.deepEqual(mappingPageItems(20, 10), [1, 'left', 9, 10, 11, 'right', 20])
  assert.deepEqual(mappingPageItems(20, 20), [1, 'left', 16, 17, 18, 19, 20])
  model.goPage(''); assert.equal(model.page.value, 1)
  model.dispose()
})
