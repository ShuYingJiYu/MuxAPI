import test from 'node:test'
import assert from 'node:assert/strict'
import { tokenRatio, tokenRatioText, tokenAccountingReason, tokenCountSource, reportedInputText, outboundInputText, uncachedInputText } from './tokenAccounting.js'

const measured = (ratio = 1) => ({
  token_inflation: 99, estimated_input_tokens: 10,
  token_accounting: { version: 1, status: 'comparable', ratio,
    estimate: { tokens: 1000, covered: true, source: 'local_tiktoken' },
    report: { input_total: 1000 },
  },
})

test('both views use the backend ratio and preserve authoritative zero', () => {
  assert.equal(tokenRatio(measured()), 1)
  assert.equal(tokenRatioText(measured()), '1.00×')
  assert.equal(tokenRatioText(measured(0)), '0.00×')
  assert.equal(reportedInputText(measured()), '1,000')
  assert.equal(outboundInputText(measured()), '1,000')
  assert.equal(tokenCountSource(measured()), '本地分词估算')
  assert.equal(tokenRatio({ token_inflation: 1.9 }), null)
})

test('unavailable, legacy and malformed results cannot look like valid ratios', () => {
  for (const status of ['legacy', 'invalid_usage', 'incomplete_usage', 'server_context']) {
    const entry = measured()
    entry.token_accounting.status = status
    assert.equal(tokenRatioText(entry), '—')
    assert.notEqual(tokenAccountingReason(entry), '暂无可比较的计量数据')
  }
  for (const value of [null, undefined, '1', Infinity, NaN, -1]) {
    const entry = measured()
    entry.token_accounting.ratio = value
    assert.equal(tokenRatio(entry), null)
  }
  const entry = measured()
  entry.token_accounting.estimate.covered = false
  assert.equal(outboundInputText(entry), '1,000（局部估算）')
  entry.token_accounting.estimate.source = 'legacy'
  assert.equal(outboundInputText(entry), '—')
  entry.token_accounting.report.input_total = 0
  assert.equal(reportedInputText(entry), '0')
  assert.equal(uncachedInputText(entry), '—')
  Object.assign(entry.token_accounting.report, { present: true, quality: 'legacy', normalized: true, uncached_input: 100 })
  assert.equal(uncachedInputText(entry), '100')
})
