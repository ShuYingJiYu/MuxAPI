<script setup>
// 原生 select 的样式化替代组件，保持 v-model 与 change 双事件兼容。
import { computed, nextTick, onMounted, onUnmounted, ref } from 'vue'
import Icon from './Icon.vue'

const props = defineProps({
  modelValue: { type: [String, Number, Boolean, Array], default: '' },
  options: { type: Array, default: () => [] },
  disabled: { type: Boolean, default: false },
  variant: { type: String, default: '' },
  searchable: { type: Boolean, default: false },
  multiple: { type: Boolean, default: false },
})

const emit = defineEmits(['update:modelValue', 'change'])
const open = ref(false)
const root = ref(null)
const trigger = ref(null)
const menu = ref(null)
const searchInput = ref(null)
const menuStyle = ref({})
const searchText = ref('')

const selected = computed(() => props.options.find(o => String(o.value) === String(props.modelValue)))
const selectedOptions = computed(() => {
  if (!props.multiple) return selected.value ? [selected.value] : []
  const selectedValues = new Set((Array.isArray(props.modelValue) ? props.modelValue : []).map(value => String(value)))
  return props.options.filter(option => selectedValues.has(String(option.value)))
})
const label = computed(() => {
  if (!props.multiple) return selected.value?.label ?? '请选择'
  if (!selectedOptions.value.length) return '未选择'
  const names = selectedOptions.value.map(option => option.label)
  if (names.length <= 2) return names.join('、')
  return `${names[0]} 等 ${names.length} 个`
})
const filteredOptions = computed(() => {
  const query = searchText.value.trim().toLocaleLowerCase()
  if (!query) return props.options
  return props.options.filter(option => String(option.label || '').toLocaleLowerCase().includes(query))
})

function toggle() {
  if (props.disabled) return
  open.value = !open.value
  if (open.value) nextTick(() => { positionMenu(); searchInput.value?.focus() })
}
function pick(opt) {
  if (opt.disabled) return
  if (props.multiple) {
    const selectedValues = new Set((Array.isArray(props.modelValue) ? props.modelValue : []).map(value => String(value)))
    selectedValues.has(String(opt.value)) ? selectedValues.delete(String(opt.value)) : selectedValues.add(String(opt.value))
    const values = props.options.filter(option => selectedValues.has(String(option.value))).map(option => option.value)
    emit('update:modelValue', values)
    emit('change', values)
    return
  }
  emit('update:modelValue', opt.value)
  emit('change', opt.value)
  open.value = false
  searchText.value = ''
}
function onDocClick(e) {
  if (root.value && !root.value.contains(e.target) && !menu.value?.contains(e.target)) {
    open.value = false
    searchText.value = ''
  }
}
function onKey(e) {
  if (e.key === 'Escape') {
    open.value = false
    searchText.value = ''
  }
}
function positionMenu() {
  if (!open.value || !trigger.value) return
  const rect = trigger.value.getBoundingClientRect()
  const gap = 8
  const viewportPad = 12
  const below = window.innerHeight - rect.bottom - gap - viewportPad
  const above = rect.top - gap - viewportPad
  const openAbove = below < 180 && above > below
  const available = Math.max(80, openAbove ? above : below)
  menuStyle.value = {
    left: `${rect.left}px`,
    minWidth: `${rect.width}px`,
    maxWidth: `320px`,
    top: openAbove ? 'auto' : `${rect.bottom + gap}px`,
    bottom: openAbove ? `${window.innerHeight - rect.top + gap}px` : 'auto',
    maxHeight: `${Math.min(280, available)}px`,
  }
}
function onViewportChange() {
  if (open.value) positionMenu()
}

onMounted(() => {
  // 全局监听用于点击外部和 Escape 关闭，下线时必须成对移除。
  document.addEventListener('click', onDocClick)
  document.addEventListener('keydown', onKey)
  document.addEventListener('scroll', onViewportChange, true)
  window.addEventListener('resize', onViewportChange)
})
onUnmounted(() => {
  document.removeEventListener('click', onDocClick)
  document.removeEventListener('keydown', onKey)
  document.removeEventListener('scroll', onViewportChange, true)
  window.removeEventListener('resize', onViewportChange)
})
</script>

<template>
  <div ref="root" class="fancy-select" :class="[{ open, disabled, 'fancy-select-multiple': multiple }, variant ? `variant-${variant}` : '']">
    <button ref="trigger" type="button" class="fancy-select-btn" :disabled="disabled" aria-haspopup="listbox" :aria-expanded="open" @click="toggle">
      <span class="fancy-selected-label"><i v-if="variant === 'tag'" class="fancy-tag-dot" :class="multiple ? 'all' : (selected?.color || 'all')"></i><span>{{ label }}</span></span>
      <Icon name="chevron-right" :size="15" class="fancy-select-arrow" />
    </button>
    <Teleport to="body">
      <Transition name="fancy-pop">
        <div v-if="open" ref="menu" class="fancy-select-menu fancy-select-portal" :class="variant ? `fancy-select-menu-${variant}` : ''" role="listbox" :aria-multiselectable="multiple || undefined" :style="menuStyle">
        <div v-if="searchable" class="fancy-select-search"><Icon name="search" :size="14" /><input ref="searchInput" v-model="searchText" type="search" placeholder="搜索标签" @click.stop /></div>
        <button
          v-for="opt in filteredOptions"
          :key="String(opt.value)"
          type="button"
          class="fancy-select-item"
          :class="{ active: multiple ? selectedOptions.some(item => String(item.value) === String(opt.value)) : String(opt.value) === String(modelValue), disabled: opt.disabled }"
          :disabled="opt.disabled"
          role="option"
          :aria-selected="multiple ? selectedOptions.some(item => String(item.value) === String(opt.value)) : String(opt.value) === String(modelValue)"
          @click="pick(opt)"
        >
          <span class="fancy-option-main"><i v-if="variant === 'tag'" class="fancy-tag-dot" :class="opt.color || 'all'"></i><span>{{ opt.label }}</span></span>
          <Icon v-if="multiple ? selectedOptions.some(item => String(item.value) === String(opt.value)) : String(opt.value) === String(modelValue)" name="check" :size="14" />
        </button>
        <div v-if="searchable && !filteredOptions.length" class="fancy-select-empty">没有匹配标签</div>
        </div>
      </Transition>
    </Teleport>
  </div>
</template>
