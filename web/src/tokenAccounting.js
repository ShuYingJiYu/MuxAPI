// Both views consume the same server measurement; never reconstruct a ratio.
export function tokenRatio(entry) {
  const m = entry?.token_accounting
  const value = m?.ratio
  return m?.version === 1 && m.status === 'comparable' && typeof value === 'number' && Number.isFinite(value) && value >= 0 ? value : null
}
export function tokenRatioText(entry) {
  const value = tokenRatio(entry)
  return value === null ? '—' : `${value.toFixed(2)}×`
}
const reasons = {
  comparable: '上游报告输入 ÷ 出站估算；仅供参考',
  legacy: '历史记录缺少出站计数基准',
  missing_usage: '上游未返回用量', invalid_usage: '上游用量字段矛盾或异常',
  unclassified_usage: '无法确认上游用量的分类方式', incomplete_usage: '响应未包含完整用量',
  unverified_tokenizer: '该模型的分词规则尚未验证', unsupported_content: '含暂未覆盖的图片、音频或文件等内容',
  server_context: '含无法独立计数的服务器上下文', server_tools: '含服务器执行的工具，无法完整比较',
  unsupported_tools: '暂未覆盖该工具格式', incomplete_estimate: '出站估算覆盖不完整',
  empty_estimate: '没有可比较的输入基准', response_model_mismatch: '响应模型与出站模型不同',
  invalid_request: '请求内容无法计数', unknown_protocol: '暂未覆盖该协议', tokenizer_unavailable: '分词器不可用',
}
export const tokenAccountingReason = entry => reasons[entry?.token_accounting?.status] || '暂无可比较的计量数据'
export function tokenCountSource(entry) {
  const m = entry?.token_accounting
  return ({ local_tiktoken: '本地分词估算', local_tiktoken_fallback: '近似分词（未验证）', legacy: '历史记录' })[m?.estimate?.source] || '—'
}
const fmtCount = value => typeof value === 'number' && Number.isFinite(value) && value >= 0 ? value.toLocaleString('zh-CN') : '—'
export const reportedInputText = entry => fmtCount(entry?.token_accounting?.report?.input_total)
export const uncachedInputText = entry => {
  const report = entry?.token_accounting?.report
  return report?.present && report.normalized && ['valid', 'legacy'].includes(report.quality)
    ? fmtCount(report.uncached_input) : '—'
}
export const outboundInputText = entry => {
  const estimate = entry?.token_accounting?.estimate
  if (!estimate?.source || estimate.source === 'legacy') return '—'
  return fmtCount(estimate.tokens) + (estimate.covered ? '' : '（局部估算）')
}
