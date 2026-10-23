<script setup>
import { computed, nextTick, onMounted, onUnmounted, reactive, ref, watch } from 'vue'
import FancySelect from './FancySelect.vue'
import Icon from './Icon.vue'
import { mappingScope, mappingStatus, useModelMappings } from './composables/useModelMappings.js'

const props = defineProps({ upstreams: { type: Array, default: () => [] } })
const emit = defineEmits(['unauthorized'])
const {
  entries, loaded, loading, busy, loadError, actionError, notice, tab, filters, now,
  page, pageSize, orphans, rules, stats, filtered, pages, pageItems, visible,
  load, save, remove, cleanup, reset, goPage, dispose,
} = useModelMappings({ onUnauthorized: () => emit('unauthorized') })
const types = [{ value: '', label: '全部类型' }, { value: 'static', label: '手动规则' }, { value: 'auto', label: '自动学习' }]
const states = [{ value: '', label: '全部状态' }, { value: 'active', label: '有效' }, { value: 'pending', label: '尚未生成目标' }, { value: 'expired', label: '已过期' }]
const sizes = [20, 50, 100].map(value => ({ value, label: `${value} 条` }))
const scopeOptions = computed(() => [{ value: 0, label: '全局 · 所有上游' }, ...props.upstreams.map(row => ({ value: row.id, label: row.name }))])
const filterScopes = computed(() => [{ value: '', label: '全部上游与全局' }, ...scopeOptions.value])
const formScopes = computed(() => [{ value: '', label: '请选择生效范围', disabled: true }, ...scopeOptions.value])
const expanded = ref(null)
const viewport = ref(null)
const jump = ref('')
const dialog = ref(null)
const panel = ref('')
const selection = ref(null)
const cleanupRows = ref([])
const overwrite = ref(null)
const formError = ref('')
const form = reactive({ id: null, upstream_id: '', source_model: '', target_model: '' })
const title = computed(() => panel.value === 'form' ? (form.id ? '编辑映射' : '新增映射') : panel.value === 'cleanup' ? '清理失效规则' : '删除映射')
const number = value => Number(value || 0).toLocaleString('zh-CN')
const time = value => value ? new Date(value).toLocaleString('zh-CN', { hour12: false }) : '—'
const formScope = computed(() => scopeOptions.value.find(item => Number(item.value) === Number(form.upstream_id))?.label || '请选择生效范围')
const expiry = row => row.expires_at ? time(row.expires_at) : row.target_model ? '长期有效' : '—'
let timer

watch([page, pageSize, tab, filtered], async () => {
  expanded.value = null
  jump.value = ''
  await nextTick()
  if (viewport.value) viewport.value.scrollTop = 0
})
watch(form, () => { overwrite.value = null; formError.value = ''; actionError.value = '' })
async function openPanel(value) {
  actionError.value = ''
  formError.value = ''
  panel.value = value
  await nextTick()
  dialog.value?.showModal()
}
function closePanel() {
  if (busy.value) return
  dialog.value?.close()
  panel.value = ''
  overwrite.value = null
}
function openNew() {
  if (form.id) Object.assign(form, { id: null, upstream_id: '', source_model: '', target_model: '' })
  openPanel('form')
}
function edit(row) {
  Object.assign(form, { id: row.id, upstream_id: row.upstream_id, source_model: row.source_model, target_model: row.target_model })
  openPanel('form')
}
function askDelete(row) { selection.value = { ...row }; openPanel('delete') }
function askCleanup() { cleanupRows.value = filtered.value.slice(0, 1000).map(row => ({ ...row })); openPanel('cleanup') }
async function submitRule() {
  formError.value = ''
  if (form.upstream_id === '' || !form.source_model.trim() || !form.target_model.trim()) {
    formError.value = '请选择生效范围，并填写请求模型和实际模型。'
    return
  }
  const payload = { upstream_id: Number(form.upstream_id), source_model: form.source_model.trim(), target_model: form.target_model.trim(), mapping_type: 'static' }
  const existing = entries.value.find(row => row.upstream_id === payload.upstream_id && row.source_model === payload.source_model)
  if (existing && !overwrite.value) { overwrite.value = { ...existing }; return }
  if (await save(payload)) {
    closePanel()
    Object.assign(form, { id: null, upstream_id: '', source_model: '', target_model: '' })
  }
}
async function confirmDelete() { if (await remove(selection.value)) closePanel() }
async function confirmCleanup() { if (await cleanup(cleanupRows.value.map(row => row.id))) closePanel() }
function jumpPage() { goPage(jump.value); jump.value = '' }
onMounted(() => { load(); timer = setInterval(() => { now.value = Date.now() }, 60000) })
onUnmounted(() => { clearInterval(timer); dialog.value?.close(); dispose() })
</script>

<template>
  <section class="model-mappings">
    <header class="mm-heading">
      <div><span class="mm-kicker">MODEL MAPPING</span><h2>模型映射</h2><p>清晰管理客户端模型名与上游模型名的转换关系。</p></div>
      <div class="mm-actions"><button class="btn btn-ghost" :disabled="loading || busy" @click="load"><Icon name="refresh" :size="15" />刷新</button><button class="btn" :disabled="!loaded || loading || busy || !!loadError" @click="openNew"><Icon name="plus" :size="15" />新增规则</button></div>
    </header>

    <div class="mm-stats"><div><span>有效映射</span><strong>{{ loaded ? number(stats.active) : '—' }}</strong></div><div><span>手动规则</span><strong>{{ loaded ? number(stats.manual) : '—' }}</strong></div><div><span>自动学习</span><strong>{{ loaded ? number(stats.auto) : '—' }}</strong></div><div><span>失效归属</span><strong :class="{ 'mm-warning': stats.orphan }">{{ loaded ? number(stats.orphan) : '—' }}</strong></div></div>
    <div v-if="orphans.length && tab !== 'orphans'" class="mm-orphan-note"><Icon name="alert" :size="16" /><span>{{ number(orphans.length) }} 条历史记录的上游已移除，已从规则列表分开。</span><button class="btn-link" @click="tab = 'orphans'">查看失效规则</button></div>
    <div v-if="notice" class="mm-notice" role="status"><Icon name="check" :size="15" /><span>{{ notice }}</span></div>
    <div v-if="loadError" class="mm-error" role="alert"><span>{{ loadError }}{{ loaded ? ' 当前保留上次读取的列表。' : '' }}</span><button class="btn-link" :disabled="busy || loading" @click="load">重试</button></div>

    <div class="mm-toolbar">
      <div class="mm-tabs" role="group" aria-label="映射分类"><button :class="{ active: tab === 'rules' }" :aria-pressed="tab === 'rules'" @click="tab = 'rules'">映射规则<span>{{ number(rules.length) }}</span></button><button :class="{ active: tab === 'orphans' }" :aria-pressed="tab === 'orphans'" @click="tab = 'orphans'">失效规则<span>{{ number(orphans.length) }}</span></button></div>
      <button v-if="tab === 'orphans'" class="btn btn-ghost mm-cleanup" :disabled="loading || busy || !filtered.length || !!loadError" @click="askCleanup"><Icon name="trash" :size="14" />清理失效规则</button>
    </div>
    <div class="mm-filters">
      <label class="mm-search"><Icon name="search" :size="15" /><input v-model="filters.query" aria-label="搜索模型或上游" placeholder="搜索模型或上游名称" /></label>
      <label v-if="tab !== 'orphans'"><span class="mm-sr-only">生效范围筛选</span><FancySelect v-model="filters.scope" :options="filterScopes" /></label>
      <label><span class="mm-sr-only">规则类型筛选</span><FancySelect v-model="filters.type" :options="types" /></label>
      <label v-if="tab !== 'orphans'"><span class="mm-sr-only">规则状态筛选</span><FancySelect v-model="filters.status" :options="states" /></label>
      <button class="btn btn-ghost" @click="reset">重置</button>
    </div>
    <p class="mm-list-note" role="status">{{ loading ? (loaded ? '正在更新，原列表暂时保留…' : '正在读取映射规则…') : `当前 ${number(filtered.length)} 条记录` }}<span v-if="tab === 'orphans'">仅包含所属上游已移除的记录，不包含全局规则。</span></p>

    <div ref="viewport" class="mm-list" :aria-busy="loading" tabindex="0" role="region" aria-label="模型映射列表，可滚动">
      <article v-for="row in visible" :key="row.id" class="mm-rule">
        <div class="mm-rule-main">
          <div class="mm-conversion"><div><small>请求模型</small><span>{{ row.source_model }}</span></div><Icon class="mm-direction" name="arrow-right" :size="18" /><div><small>上游实际模型</small><span :class="{ 'mm-no-target': !row.target_model }">{{ row.target_model || '尚未生成目标' }}</span></div></div>
          <div class="mm-row-meta"><span class="mm-scope"><Icon :name="row.upstream_missing ? 'alert' : 'cube'" :size="13" />{{ mappingScope(row) }}</span><span>{{ row.mapping_type === 'auto' ? '自动学习' : '手动规则' }}</span><span class="mm-state" :class="mappingStatus(row, now).key"><i></i>{{ mappingStatus(row, now).label }}</span><span v-if="row.target_model && !row.upstream_missing" class="mm-expiry">{{ expiry(row) }}</span></div>
        </div>
        <div class="mm-rule-actions"><button v-if="!row.upstream_missing" class="mm-text-button" :disabled="loading || busy || !!loadError" :aria-label="`编辑 ${row.source_model} 的映射`" @click="edit(row)">编辑</button><button class="mm-icon-button mm-delete" :disabled="loading || busy" :aria-label="`删除 ${row.source_model} 的映射`" @click="askDelete(row)"><Icon name="trash" :size="15" /></button><button class="mm-icon-button mm-detail-toggle" :aria-label="`${expanded === row.id ? '收起' : '展开'} ${row.source_model} 的详情`" :aria-expanded="expanded === row.id" :aria-controls="`mapping-detail-${row.id}`" @click="expanded = expanded === row.id ? null : row.id"><Icon name="chevron-down" :size="15" /></button></div>
        <dl v-if="expanded === row.id" :id="`mapping-detail-${row.id}`" class="mm-details"><div><dt>创建时间</dt><dd>{{ time(row.created_at) }}</dd></div><div><dt>最近更新</dt><dd>{{ time(row.updated_at) }}</dd></div><div><dt>失败计数</dt><dd>{{ number(row.failure_count) }}</dd></div><div><dt>{{ row.upstream_missing ? '原上游 ID' : '有效期限' }}</dt><dd>{{ row.upstream_missing ? row.upstream_id : expiry(row) }}</dd></div></dl>
      </article>
      <div v-if="!visible.length" class="mm-empty"><Icon name="link" :size="25" /><strong>{{ loading && !loaded ? '正在读取规则' : !loaded ? '暂未获取规则' : tab === 'orphans' && !orphans.length ? '没有失效归属的记录' : '没有符合条件的规则' }}</strong><p>{{ tab === 'orphans' ? '已移除上游的残留规则会单独列在这里。' : '可调整筛选，或新增一条模型转换规则。' }}</p></div>
    </div>

    <footer class="mm-footer"><label class="mm-page-size"><span>每页</span><FancySelect v-model="pageSize" :options="sizes" /></label><nav class="mm-pages" aria-label="模型映射分页"><button aria-label="上一页" :disabled="page <= 1" @click="goPage(page - 1)"><Icon class="mm-prev" name="chevron-right" :size="14" /></button><template v-for="item in pageItems" :key="item"><button v-if="typeof item === 'number'" :aria-label="`第 ${item} 页`" :aria-current="page === item ? 'page' : undefined" :class="{ active: page === item }" @click="goPage(item)">{{ item }}</button><span v-else>…</span></template><button aria-label="下一页" :disabled="page >= pages" @click="goPage(page + 1)"><Icon name="chevron-right" :size="14" /></button></nav><form class="mm-jump" @submit.prevent="jumpPage"><label for="mapping-page-jump">前往</label><input id="mapping-page-jump" v-model="jump" type="number" min="1" :max="pages" step="1" inputmode="numeric" aria-label="映射跳转页码" /><button type="submit" :disabled="!jump">跳转</button></form></footer>
    <p class="mm-help">指定上游的有效映射优先于全局规则；没有有效映射时使用原模型名。手动与自动为规则来源，不代表模型已通过可用性测试。</p>

    <dialog ref="dialog" class="mm-dialog" :aria-label="title" @cancel.prevent="closePanel" @click="event => { if (event.target === dialog) closePanel() }">
      <header class="mm-dialog-heading"><div><span class="mm-kicker">MODEL MAPPING</span><h3>{{ title }}</h3></div><button class="mm-icon-button" :disabled="busy" aria-label="关闭映射对话框" @click="closePanel"><Icon name="x" :size="19" /></button></header>
      <form v-if="panel === 'form'" class="mm-editor" @submit.prevent="submitRule">
        <div class="mm-dialog-body">
          <label class="mm-field"><span>生效范围</span><FancySelect v-model="form.upstream_id" :options="formScopes" :disabled="busy || !!form.id" :teleport-target="dialog || 'body'" /></label>
          <div class="mm-model-fields"><label class="mm-field"><span>请求模型</span><input v-model="form.source_model" placeholder="客户端请求的模型名" :readonly="!!form.id" :disabled="busy" required spellcheck="false" /></label><Icon name="arrow-right" :size="18" /><label class="mm-field"><span>上游实际模型</span><input v-model="form.target_model" placeholder="上游接收的模型名" :disabled="busy" required spellcheck="false" /></label></div>
          <div class="mm-preview"><small>{{ form.upstream_id === '' ? '请选择生效范围' : formScope }}</small><div><span>{{ form.source_model.trim() || '请求模型' }}</span><Icon name="arrow-right" :size="16" /><span>{{ form.target_model.trim() || '实际模型' }}</span></div></div>
          <p class="mm-help">保存为长期有效的手动规则。{{ form.id ? '生效范围与请求模型不可修改；修改目标会覆盖原规则。' : '相同生效范围与请求模型已存在时，需再次确认覆盖。' }}</p>
          <div v-if="overwrite" class="mm-overwrite" role="status"><strong>该范围已有规则，确认覆盖？</strong><p>{{ overwrite.mapping_type === 'auto' ? '自动学习' : '手动规则' }} · 原目标：{{ overwrite.target_model || '尚未生成目标' }}</p><p>新目标：{{ form.target_model.trim() }} · 长期有效</p></div>
          <p v-if="formError || actionError" class="mm-form-error" role="alert">{{ formError || actionError }}</p>
        </div>
        <footer class="mm-dialog-footer"><button class="btn btn-ghost" :disabled="busy" type="button" @click="closePanel">取消</button><button class="btn" :disabled="busy || loading || !!loadError" type="submit">{{ busy ? '正在保存…' : overwrite ? '确认覆盖' : '保存规则' }}</button></footer>
      </form>
      <template v-else-if="panel === 'delete' && selection">
        <div class="mm-dialog-body"><p class="mm-confirm-copy">删除以下映射？此操作不会删除上游。</p><div class="mm-preview"><small>{{ mappingScope(selection) }}</small><div><span>{{ selection.source_model }}</span><Icon name="arrow-right" :size="16" /><span>{{ selection.target_model || '尚未生成目标' }}</span></div></div><p class="mm-help">{{ selection.mapping_type === 'auto' ? '此记录由自动学习产生，后续请求仍可能重新学习。' : '删除后，可能回退到全局规则或使用原模型名。' }}</p><p v-if="actionError" class="mm-form-error" role="alert">{{ actionError }}</p></div>
        <footer class="mm-dialog-footer"><button class="btn btn-ghost" :disabled="busy" @click="closePanel">取消</button><button class="btn btn-danger" :disabled="busy" @click="confirmDelete">{{ busy ? '正在删除…' : '确认删除' }}</button></footer>
      </template>
      <template v-else-if="panel === 'cleanup'">
        <div class="mm-dialog-body"><p class="mm-confirm-copy">将清理当前筛选中的 {{ number(cleanupRows.length) }} 条失效规则。</p><p class="mm-help">只删除所属上游已移除的记录。全局规则与现有上游的规则均保留；每次最多清理 1,000 条。</p><ul class="mm-cleanup-list"><li v-for="row in cleanupRows" :key="row.id"><span>{{ row.source_model }} → {{ row.target_model || '尚未生成目标' }}</span><small>原上游 ID {{ row.upstream_id }}</small></li></ul><p v-if="actionError" class="mm-form-error" role="alert">{{ actionError }}</p></div>
        <footer class="mm-dialog-footer"><button class="btn btn-ghost" :disabled="busy" @click="closePanel">取消</button><button class="btn btn-danger" :disabled="busy" @click="confirmCleanup">{{ busy ? '正在清理…' : `确认清理 ${number(cleanupRows.length)} 条` }}</button></footer>
      </template>
    </dialog>
  </section>
</template>

<style scoped>
.model-mappings { min-width: 0; display: grid; gap: 18px; container-type: inline-size; color: var(--g700); }
.mm-heading { display: flex; justify-content: space-between; align-items: center; gap: 16px; }
.mm-heading > div:first-child { min-width: 0; }
.mm-kicker { color: var(--p700); font-size: 10px; font-weight: 700; letter-spacing: .12em; }
.mm-heading h2 { margin: 7px 0; color: var(--g800); font-size: 23px; }
.mm-heading p, .mm-help { margin: 0; color: var(--g500); font-size: 12px; line-height: 1.8; }
.mm-actions { display: flex; gap: 8px; flex-shrink: 0; }
.mm-stats { display: grid; grid-template-columns: repeat(4, minmax(0, 1fr)); padding: 18px 0; border-block: 1px solid var(--line); gap: 18px; }
.mm-stats > div { min-width: 0; display: grid; gap: 8px; }
.mm-stats span { color: var(--g500); font-size: 12px; }
.mm-stats strong { color: var(--g800); font-size: 25px; font-weight: 650; font-variant-numeric: tabular-nums; }
.mm-stats .mm-warning { color: var(--state-warning); }
.mm-toolbar { display: flex; align-items: center; justify-content: space-between; flex-wrap: wrap; gap: 10px; }
.mm-tabs { display: inline-flex; gap: 4px; padding: 3px; background: var(--surface-muted); border-radius: var(--r-control); }
.mm-tabs button { display: flex; align-items: center; gap: 8px; border: 0; border-radius: calc(var(--r-control) - 3px); padding: 8px 12px; color: var(--g500); background: transparent; cursor: pointer; font: inherit; font-size: 12px; }
.mm-tabs button.active { color: var(--p700); background: var(--surface-raised); box-shadow: 0 1px 3px var(--line); font-weight: 600; }
.mm-tabs span { font-family: var(--font-data); font-size: 11px; opacity: .8; }
.mm-filters { display: grid; grid-template-columns: minmax(0, 1.5fr) repeat(3, minmax(0, 1fr)) auto; align-items: center; gap: 10px; }
.mm-filters label { min-width: 0; }
.mm-search { display: flex; align-items: center; gap: 8px; border: 1px solid var(--control-border); border-radius: var(--r-control); padding: 0 11px; background: var(--surface-raised); color: var(--g400); }
.mm-search:focus-within { outline: 2px solid var(--p500); outline-offset: 2px; }
.mm-search input { width: 100%; min-width: 0; height: 38px; padding: 0; border: 0; outline: 0; background: transparent; font: inherit; color: var(--g800); font-size: 12px; }
.model-mappings :deep(.fancy-select), .model-mappings :deep(.fancy-select-btn) { min-width: 0; width: 100%; }
.mm-filters :deep(.fancy-select-btn) { height: 40px; font-size: 12px; }
.mm-list-note { min-height: 18px; margin: 0; display: flex; justify-content: space-between; flex-wrap: wrap; gap: 5px 16px; font-size: 11px; color: var(--g500); line-height: 1.7; }
.mm-list { max-block-size: max(20rem, 58dvh); overflow: auto; scrollbar-gutter: stable; overscroll-behavior: contain; border-block: 1px solid var(--line); min-width: 0; }
.mm-rule { display: grid; grid-template-columns: minmax(0, 1fr) auto; gap: 15px; padding: 18px 4px; border-bottom: 1px solid var(--line); }
.mm-rule:last-child { border-bottom: 0; }
.mm-rule-main { min-width: 0; }
.mm-conversion { display: grid; grid-template-columns: minmax(0, 1fr) 18px minmax(0, 1fr); gap: 16px; align-items: center; }
.mm-conversion > div { min-width: 0; display: grid; gap: 6px; }
.mm-conversion small { color: var(--g500); font-size: 10px; }
.mm-conversion span { color: var(--g800); font: 500 13px/1.7 var(--font-data); overflow-wrap: anywhere; }
.mm-conversion .mm-no-target { color: var(--g400); font-family: inherit; font-weight: 400; }
.mm-direction { color: var(--p600); margin-top: 14px; }
.mm-row-meta { display: flex; flex-wrap: wrap; align-items: center; gap: 8px 15px; margin-top: 11px; color: var(--g500); font-size: 11px; line-height: 1.7; }
.mm-scope, .mm-state { display: inline-flex; align-items: center; gap: 5px; min-width: 0; overflow-wrap: anywhere; }
.mm-scope { color: var(--g600); }
.mm-state i { display: block; flex-shrink: 0; width: 4px; height: 4px; border-radius: 50%; background: currentColor; }
.mm-state.active { color: var(--state-success); }
.mm-state.orphan, .mm-state.expired { color: var(--state-warning); }
.mm-expiry { color: var(--g400); font-variant-numeric: tabular-nums; }
.mm-rule-actions { display: flex; gap: 6px; align-items: center; align-self: start; padding-top: 21px; }
.mm-icon-button, .mm-text-button { display: inline-flex; align-items: center; justify-content: center; padding: 0; height: 30px; min-width: 30px; border: 0; border-radius: var(--r-control); background: transparent; color: var(--g500); font: inherit; font-size: 12px; cursor: pointer; }
.mm-text-button { padding-inline: 5px; }
.mm-icon-button:hover, .mm-text-button:hover { background: var(--surface-muted); color: var(--p700); }
.mm-delete:hover { background: var(--state-danger-soft); color: var(--state-danger); }
.mm-detail-toggle svg { transition: transform .15s; }
.mm-detail-toggle[aria-expanded="true"] svg { transform: rotate(180deg); }
.mm-details { grid-column: 1 / -1; display: grid; grid-template-columns: repeat(4, minmax(0, 1fr)); gap: 14px; margin: 0; padding: 14px; border-radius: var(--r-control); background: var(--surface-muted); font-size: 11px; line-height: 1.7; }
.mm-details dt { color: var(--g500); margin-bottom: 4px; }
.mm-details dd { margin: 0; overflow-wrap: anywhere; color: var(--g700); }
.mm-empty { display: grid; justify-items: center; gap: 12px; padding: 48px 16px; color: var(--g400); text-align: center; font-size: 13px; }
.mm-empty strong { color: var(--g600); font-size: 13px; font-weight: 500; }
.mm-empty p { margin: 0; font-size: 12px; line-height: 1.7; }
.mm-orphan-note, .mm-error, .mm-notice { display: flex; align-items: start; gap: 9px; padding: 12px 14px; border-radius: var(--r-control); font-size: 12px; line-height: 1.7; }
.mm-orphan-note { background: var(--state-warning-soft); color: var(--state-warning); }
.mm-orphan-note button, .mm-error button { flex-shrink: 0; margin-left: auto; font-size: 12px; }
.mm-orphan-note > svg, .mm-notice > svg { margin-top: 3px; }
.mm-error { background: var(--state-danger-soft); color: var(--state-danger); }
.mm-notice { background: var(--state-success-soft); color: var(--state-success); }
.mm-cleanup { color: var(--state-danger); }
.mm-footer { display: flex; align-items: center; justify-content: space-between; gap: 12px; flex-wrap: wrap; color: var(--g500); font-size: 11px; }
.mm-page-size { display: flex; gap: 7px; align-items: center; }
.mm-page-size :deep(.fancy-select) { width: 82px; }
.mm-page-size :deep(.fancy-select-btn) { height: 30px; padding: 5px 8px; font-size: 11px; }
.mm-pages, .mm-jump { display: flex; gap: 4px; align-items: center; }
.mm-pages button, .mm-jump button { display: inline-flex; align-items: center; justify-content: center; height: 30px; min-width: 28px; padding: 0 6px; border: 1px solid transparent; border-radius: var(--r-control); background: transparent; color: var(--g600); font: inherit; cursor: pointer; }
.mm-pages button.active { color: var(--p700); background: var(--p100); border-color: var(--line); font-weight: 600; }
.mm-pages button:hover:not(:disabled), .mm-jump button:hover:not(:disabled) { background: var(--surface-muted); }
.mm-jump input { width: 46px; height: 30px; padding: 4px; border: 1px solid var(--control-border); border-radius: var(--r-control); background: var(--surface-raised); color: var(--g800); text-align: center; font: inherit; appearance: textfield; -moz-appearance: textfield; }
.mm-jump input::-webkit-inner-spin-button, .mm-jump input::-webkit-outer-spin-button { -webkit-appearance: none; margin: 0; }
.mm-jump button { border-color: var(--line); }
.mm-prev { transform: rotate(180deg); }
.mm-dialog { inset: 0; margin: auto; width: min(38rem, calc(100% - 24px)); max-height: 86dvh; padding: 0; overflow-y: auto; border: 1px solid var(--dialog-border); border-radius: var(--r-card); background: var(--dialog-surface); color: var(--g700); box-shadow: var(--sh-dialog); }
.mm-dialog::backdrop { background: rgba(40,35,43,.30); backdrop-filter: blur(5px); }
.mm-dialog-heading { position: sticky; top: 0; z-index: 2; display: flex; justify-content: space-between; align-items: center; padding: 20px 24px 17px; background: var(--dialog-surface); border-bottom: 1px solid var(--line); }
.mm-dialog-heading h3 { margin: 5px 0 0; font-size: 19px; color: var(--g800); }
.mm-dialog-body { padding: 22px 24px; display: grid; gap: 18px; }
.mm-field { display: grid; gap: 9px; min-width: 0; font-size: 12px; }
.mm-field > span { color: var(--g600); }
.mm-field input { box-sizing: border-box; min-width: 0; width: 100%; height: 41px; padding: 9px 11px; border: 1px solid var(--control-border); border-radius: var(--r-control); background: var(--surface-raised); color: var(--g800); font: 12px var(--font-data); }
.mm-field input:read-only { background: var(--surface-muted); color: var(--g500); }
.mm-model-fields { display: grid; grid-template-columns: minmax(0, 1fr) 18px minmax(0, 1fr); gap: 12px; align-items: end; }
.mm-model-fields > svg { align-self: end; margin-bottom: 11px; color: var(--p600); }
.mm-preview { min-width: 0; padding: 15px; border-radius: var(--r-control); background: var(--surface-muted); border: 1px solid var(--line); }
.mm-preview small { display: block; color: var(--g500); font-size: 11px; margin-bottom: 10px; overflow-wrap: anywhere; }
.mm-preview > div { display: flex; align-items: center; gap: 12px; color: var(--g800); font: 12px/1.8 var(--font-data); }
.mm-preview span { min-width: 0; overflow-wrap: anywhere; }
.mm-confirm-copy { margin: 0; font-size: 13px; line-height: 1.7; }
.mm-dialog-footer { position: sticky; bottom: 0; z-index: 1; padding: 16px 24px; display: flex; justify-content: flex-end; gap: 10px; background: var(--dialog-surface); border-top: 1px solid var(--line); }
.mm-overwrite { padding: 13px; border-radius: var(--r-control); background: var(--state-warning-soft); color: var(--state-warning); font-size: 12px; line-height: 1.8; overflow-wrap: anywhere; }
.mm-overwrite p { margin: 5px 0 0; }
.mm-form-error { margin: 0; color: var(--state-danger); font-size: 12px; line-height: 1.7; }
.mm-cleanup-list { display: grid; gap: 12px; max-height: 32dvh; overflow-y: auto; padding: 12px; margin: 0; list-style: none; border: 1px solid var(--line); border-radius: var(--r-control); }
.mm-cleanup-list li { display: grid; gap: 4px; font-size: 12px; line-height: 1.7; overflow-wrap: anywhere; }
.mm-cleanup-list small { color: var(--g500); font-size: 11px; }
.model-mappings button:disabled { opacity: .5; cursor: not-allowed; }
.model-mappings button:focus-visible, .model-mappings input:focus-visible, .mm-list:focus-visible { outline: 2px solid var(--p500); outline-offset: 2px; }
.mm-sr-only { position: absolute; width: 1px; height: 1px; padding: 0; margin: -1px; overflow: hidden; clip-path: inset(50%); white-space: nowrap; }
@container (max-width: 860px) { .mm-filters { grid-template-columns: repeat(2, minmax(0, 1fr)); } .mm-search { grid-column: 1 / -1; } .mm-filters > button { justify-content: center; } .mm-rule { gap: 8px; } .mm-conversion { gap: 10px; } .mm-details { grid-template-columns: repeat(2, minmax(0, 1fr)); } }
@container (max-width: 580px) { .mm-heading { align-items: start; flex-direction: column; } .mm-heading h2 { font-size: 21px; } .mm-actions { align-self: stretch; } .mm-actions > button { flex: 1; justify-content: center; } .mm-rule { grid-template-columns: minmax(0, 1fr); } .mm-rule-actions { grid-row: 2; justify-content: flex-end; padding: 0; } .mm-conversion { grid-template-columns: minmax(0, 1fr) 16px minmax(0, 1fr); } .mm-stats { gap: 12px; } .mm-stats strong { font-size: 22px; } .mm-orphan-note { flex-wrap: wrap; } .mm-orphan-note > span { flex: 1; } .mm-orphan-note button { margin-left: 25px; } .mm-page-size { margin-right: auto; } .mm-pages { order: 3; justify-content: center; flex-basis: 100%; } }
@media (max-width: 480px) { .mm-dialog-body { padding: 18px; } .mm-dialog-heading, .mm-dialog-footer { padding: 16px 18px; } .mm-model-fields { grid-template-columns: minmax(0, 1fr); gap: 14px; } .mm-model-fields > svg { display: none; } }
@media (prefers-reduced-motion: reduce) { .mm-detail-toggle svg { transition: none; } }
</style>
