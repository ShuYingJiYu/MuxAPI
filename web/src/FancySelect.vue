<script setup>
// 原生 select 的样式化替代组件，保持 v-model 与 change 双事件兼容。
import { computed, nextTick, onMounted, onUnmounted, ref } from 'vue'
import Icon from './Icon.vue'

const props = defineProps({
  modelValue: { type: [String, Number, Boolean], default: '' },
  options: { type: Array, default: () => [] },
  disabled: { type: Boolean, default: false },
})

const emit = defineEmits(['update:modelValue', 'change'])
const open = ref(false)
const root = ref(null)
const trigger = ref(null)
const menu = ref(null)
const menuStyle = ref({})

const selected = computed(() => props.options.find(o => String(o.value) === String(props.modelValue)))
const label = computed(() => selected.value?.label ?? '请选择')

function toggle() {
  if (props.disabled) return
  open.value = !open.value
  if (open.value) nextTick(positionMenu)
}
function pick(opt) {
  if (opt.disabled) return
  emit('update:modelValue', opt.value)
  emit('change', opt.value)
  open.value = false
}
function onDocClick(e) {
  if (root.value && !root.value.contains(e.target) && !menu.value?.contains(e.target)) open.value = false
}
function onKey(e) {
  if (e.key === 'Escape') open.value = false
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
  <div ref="root" class="fancy-select" :class="{ open, disabled }">
    <button ref="trigger" type="button" class="fancy-select-btn" :disabled="disabled" aria-haspopup="listbox" :aria-expanded="open" @click="toggle">
      <span>{{ label }}</span>
      <Icon name="chevron-right" :size="15" class="fancy-select-arrow" />
    </button>
    <Teleport to="body">
      <Transition name="fancy-pop">
        <div v-if="open" ref="menu" class="fancy-select-menu fancy-select-portal" role="listbox" :style="menuStyle">
        <button
          v-for="opt in options"
          :key="String(opt.value)"
          type="button"
          class="fancy-select-item"
          :class="{ active: String(opt.value) === String(modelValue), disabled: opt.disabled }"
          :disabled="opt.disabled"
          role="option"
          :aria-selected="String(opt.value) === String(modelValue)"
          @click="pick(opt)"
        >
          <span>{{ opt.label }}</span>
          <Icon v-if="String(opt.value) === String(modelValue)" name="check" :size="14" />
        </button>
        </div>
      </Transition>
    </Teleport>
  </div>
</template>
