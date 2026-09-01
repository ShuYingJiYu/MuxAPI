<script setup>
import { computed, nextTick, onMounted, onUnmounted, ref } from 'vue'
import Icon from './Icon.vue'

const props = defineProps({
  modelValue: { type: [String, Number], default: '' },
  upstreams: { type: Array, default: () => [] },
})

const emit = defineEmits(['update:modelValue'])
const root = ref(null)
const trigger = ref(null)
const menu = ref(null)
const searchInput = ref(null)
const open = ref(false)
const query = ref('')
const menuStyle = ref({})

const selected = computed(() => props.upstreams.find(item => String(item.id) === String(props.modelValue)))
const filtered = computed(() => {
  const keyword = query.value.trim().toLowerCase()
  if (!keyword) return props.upstreams
  return props.upstreams.filter(item => [
    item.name,
    item.source,
    item.base_url,
    item.protocol,
    ...(item.tags || []).map(tag => tag.name),
  ].some(value => String(value || '').toLowerCase().includes(keyword)))
})

function tagsFor(item) {
  return (item.tags || []).slice(0, 3)
}

function toggle() {
  if (!props.upstreams.length) return
  open.value = !open.value
  if (open.value) nextTick(() => {
    positionMenu()
    searchInput.value?.focus()
  })
}

function pick(item) {
  emit('update:modelValue', item.id)
  query.value = ''
  open.value = false
}

function close() {
  query.value = ''
  open.value = false
}

function onDocumentClick(event) {
  if (root.value && !root.value.contains(event.target) && !menu.value?.contains(event.target)) close()
}

function onDocumentKey(event) {
  if (event.key === 'Escape') close()
}

function positionMenu() {
  if (!open.value || !trigger.value) return
  const rect = trigger.value.getBoundingClientRect()
  const gap = 8
  const viewportPad = 12
  const below = window.innerHeight - rect.bottom - gap - viewportPad
  const above = rect.top - gap - viewportPad
  const openAbove = below < 250 && above > below
  const available = Math.max(128, openAbove ? above : below)
  const width = Math.min(rect.width, window.innerWidth - viewportPad * 2)
  const left = Math.min(Math.max(viewportPad, rect.left), window.innerWidth - width - viewportPad)
  menuStyle.value = {
    left: `${left}px`,
    width: `${width}px`,
    top: openAbove ? 'auto' : `${rect.bottom + gap}px`,
    bottom: openAbove ? `${window.innerHeight - rect.top + gap}px` : 'auto',
    maxHeight: `${Math.min(520, available)}px`,
  }
}

function onViewportChange() {
  if (open.value) positionMenu()
}

onMounted(() => {
  document.addEventListener('click', onDocumentClick)
  document.addEventListener('keydown', onDocumentKey)
  document.addEventListener('scroll', onViewportChange, true)
  window.addEventListener('resize', onViewportChange)
})
onUnmounted(() => {
  document.removeEventListener('click', onDocumentClick)
  document.removeEventListener('keydown', onDocumentKey)
  document.removeEventListener('scroll', onViewportChange, true)
  window.removeEventListener('resize', onViewportChange)
})
</script>

<template>
  <div ref="root" class="upstream-picker" :class="{ open }">
    <button
      type="button"
      class="upstream-picker-trigger"
      ref="trigger"
      :disabled="!upstreams.length"
      aria-haspopup="listbox"
      :aria-expanded="open"
      @click="toggle"
    >
      <span v-if="selected" class="upstream-picker-selected">
        <strong>{{ selected.name }}</strong>
        <small>{{ selected.base_url }}</small>
      </span>
      <span v-else class="upstream-picker-placeholder">搜索并选择上游</span>
      <Icon name="chevron-right" :size="15" class="upstream-picker-arrow" />
    </button>

    <Teleport to="body">
      <Transition name="picker-pop">
      <div v-if="open" ref="menu" class="upstream-picker-menu" :style="menuStyle">
        <div class="upstream-picker-search">
          <Icon name="search" :size="16" />
          <input
            ref="searchInput"
            v-model="query"
            type="search"
            aria-label="搜索上游"
            placeholder="名称、来源、地址或标签"
          />
          <span>{{ filtered.length }} / {{ upstreams.length }}</span>
        </div>

        <div class="upstream-picker-list" role="listbox">
          <button
            v-for="item in filtered"
            :key="item.id"
            type="button"
            role="option"
            class="upstream-picker-option"
            :class="{ selected: String(item.id) === String(modelValue) }"
            :aria-selected="String(item.id) === String(modelValue)"
            @click="pick(item)"
          >
            <Icon name="server" :size="17" class="upstream-picker-server" />
            <span class="upstream-picker-copy">
              <span class="upstream-picker-name">
                <strong>{{ item.name }}</strong>
                <em v-if="item.source">{{ item.source }}</em>
                <em>{{ item.protocol || 'passthrough' }}</em>
              </span>
              <small>{{ item.base_url }}</small>
              <span v-if="tagsFor(item).length" class="upstream-picker-tags">
                <i v-for="tag in tagsFor(item)" :key="tag.id" :class="`tag-${tag.color || 'gray'}`">{{ tag.name }}</i>
              </span>
            </span>
            <Icon v-if="String(item.id) === String(modelValue)" name="check" :size="16" class="upstream-picker-check" />
          </button>
          <div v-if="!filtered.length" class="upstream-picker-empty">
            <Icon name="search" :size="20" />
            <span>没有匹配的上游</span>
          </div>
        </div>
      </div>
      </Transition>
    </Teleport>
  </div>
</template>

<style scoped>
.upstream-picker { position: relative; width: 100%; }
.upstream-picker-trigger {
  width: 100%; min-height: 48px; display: flex; align-items: center; justify-content: space-between; gap: 10px;
  padding: 7px 12px; border: 1px solid var(--g200); border-radius: var(--r-cell); background: var(--surface-raised); color: var(--g900);
  text-align: left; cursor: pointer; transition: border-color .16s, box-shadow .16s;
}
.upstream-picker-trigger:hover, .upstream-picker.open .upstream-picker-trigger {
  border-color: var(--p500); box-shadow: 0 0 0 3px var(--focus-ring);
}
.upstream-picker-trigger:disabled { cursor: not-allowed; opacity: .55; box-shadow: none; }
.upstream-picker-selected { min-width: 0; display: flex; flex-direction: column; gap: 2px; }
.upstream-picker-selected strong { overflow: hidden; color: var(--g900); font-size: 13px; text-overflow: ellipsis; white-space: nowrap; }
.upstream-picker-selected small { overflow: hidden; color: var(--g400); font-size: 11px; text-overflow: ellipsis; white-space: nowrap; }
.upstream-picker-placeholder { color: var(--g400); font-size: 13px; }
.upstream-picker-arrow { flex: none; color: var(--g400); transform: rotate(90deg); transition: transform .16s; }
.upstream-picker.open .upstream-picker-arrow { transform: rotate(-90deg); }
.upstream-picker-menu {
  position: fixed; z-index: 200; display: flex; flex-direction: column; overflow: hidden;
  border: 1px solid var(--line-strong); border-radius: 16px; background: var(--surface-raised); box-shadow: var(--sh-dialog);
  animation: picker-menu-in .16s cubic-bezier(.22,1,.36,1) both;
}
.upstream-picker-search { display: grid; grid-template-columns: 20px minmax(0, 1fr) auto; align-items: center; gap: 9px; flex: none; min-height: 58px; padding: 10px 12px; border-bottom: 1px solid var(--g100); color: var(--g400); }
.upstream-picker-search > svg { color: var(--p600); }
.upstream-picker-search input { min-width: 0; height: 36px; padding: 0 10px; border: 1px solid var(--g200); border-radius: 10px; outline: 0; background: var(--g50); color: var(--g900); font-size: 12px; transition: border-color .16s, box-shadow .16s, background .16s; }
.upstream-picker-search input:focus { border-color: var(--p400); background: var(--surface-raised); box-shadow: 0 0 0 3px var(--focus-ring); }
.upstream-picker-search input::placeholder { color: var(--g400); }
.upstream-picker-search span { min-width: 38px; color: var(--g400); font-family: var(--font-data); font-size: 10px; font-variant-numeric: tabular-nums; text-align: right; }
.upstream-picker-list { min-height: 0; overflow-y: auto; padding: 7px; scrollbar-width: thin; scrollbar-color: var(--g300) transparent; }
.upstream-picker-list::-webkit-scrollbar { width: 7px; }
.upstream-picker-list::-webkit-scrollbar-thumb { border: 2px solid var(--surface-raised); border-radius: 99px; background: var(--g300); }
.upstream-picker-option {
  width: 100%; min-height: 62px; display: grid; grid-template-columns: 20px minmax(0, 1fr) 18px; align-items: start; gap: 9px;
  padding: 10px 9px; border: 1px solid transparent; border-radius: 11px; background: transparent; color: var(--g700); text-align: left; cursor: pointer; transition: background .14s, border-color .14s, transform .14s;
}
.upstream-picker-option:hover { border-color: var(--p100); background: var(--p50); transform: translateX(1px); }
.upstream-picker-option.selected { border-color: color-mix(in srgb, var(--p400) 38%, transparent); background: var(--selection-soft); }
.upstream-picker-server { margin-top: 1px; color: var(--p600); }
.upstream-picker-copy { min-width: 0; display: flex; flex-direction: column; gap: 3px; }
.upstream-picker-name { display: flex; align-items: center; gap: 5px; min-width: 0; }
.upstream-picker-name strong { overflow: hidden; color: var(--g900); font-size: 12px; text-overflow: ellipsis; white-space: nowrap; }
.upstream-picker-name em { flex: none; padding: 1px 5px; border-radius: 4px; background: var(--g100); color: var(--g500); font-size: 9px; font-style: normal; }
.upstream-picker-copy > small { overflow: hidden; color: var(--g400); font-size: 10px; text-overflow: ellipsis; white-space: nowrap; }
.upstream-picker-tags { display: flex; flex-wrap: wrap; gap: 4px; }
.upstream-picker-tags i { padding: 2px 6px; border: 1px solid var(--g150); border-left: 2px solid var(--tag-gray); border-radius: 5px; background: var(--surface-raised); color: var(--g500); font-size: 9px; font-style: normal; }
.upstream-picker-tags .tag-green { border-left-color: var(--tag-green); }
.upstream-picker-tags .tag-amber { border-left-color: var(--tag-amber); }
.upstream-picker-tags .tag-red { border-left-color: var(--tag-red); }
.upstream-picker-tags .tag-blue { border-left-color: var(--tag-blue); }
.upstream-picker-tags .tag-purple { border-left-color: var(--tag-purple); }
.upstream-picker-tags .tag-pink { border-left-color: var(--tag-pink); }
.upstream-picker-check { align-self: center; color: var(--p600); }
.upstream-picker-empty { min-height: 120px; display: flex; flex-direction: column; align-items: center; justify-content: center; gap: 7px; color: var(--g400); font-size: 12px; }
@keyframes picker-menu-in { from { opacity: 0; transform: translateY(-4px) scale(.985); } to { opacity: 1; transform: translateY(0) scale(1); } }
.picker-pop-enter-active, .picker-pop-leave-active { transition: opacity .13s, transform .13s; transform-origin: top; }
.picker-pop-enter-from, .picker-pop-leave-to { opacity: 0; transform: translateY(-4px) scale(.99); }
@media (prefers-reduced-motion: reduce) {
  .upstream-picker-menu, .picker-pop-enter-active, .picker-pop-leave-active { animation: none; transition: none; }
}
</style>
