<script setup>
// 原生 select 的样式化替代组件，保持 v-model 与 change 双事件兼容。
import { computed, onMounted, onUnmounted, ref } from 'vue'
import Icon from './Icon.vue'

const props = defineProps({
  modelValue: { type: [String, Number, Boolean], default: '' },
  options: { type: Array, default: () => [] },
  disabled: { type: Boolean, default: false },
})

const emit = defineEmits(['update:modelValue', 'change'])
const open = ref(false)
const root = ref(null)

const selected = computed(() => props.options.find(o => String(o.value) === String(props.modelValue)))
const label = computed(() => selected.value?.label ?? '请选择')

function toggle() {
  if (!props.disabled) open.value = !open.value
}
function pick(opt) {
  if (opt.disabled) return
  emit('update:modelValue', opt.value)
  emit('change', opt.value)
  open.value = false
}
function onDocClick(e) {
  if (root.value && !root.value.contains(e.target)) open.value = false
}
function onKey(e) {
  if (e.key === 'Escape') open.value = false
}

onMounted(() => {
  // 全局监听用于点击外部和 Escape 关闭，下线时必须成对移除。
  document.addEventListener('click', onDocClick)
  document.addEventListener('keydown', onKey)
})
onUnmounted(() => {
  document.removeEventListener('click', onDocClick)
  document.removeEventListener('keydown', onKey)
})
</script>

<template>
  <div ref="root" class="fancy-select" :class="{ open, disabled }">
    <button type="button" class="fancy-select-btn" :disabled="disabled" @click="toggle">
      <span>{{ label }}</span>
      <Icon name="chevron-right" :size="15" class="fancy-select-arrow" />
    </button>
    <Transition name="fancy-pop">
      <div v-if="open" class="fancy-select-menu">
        <button
          v-for="opt in options"
          :key="String(opt.value)"
          type="button"
          class="fancy-select-item"
          :class="{ active: String(opt.value) === String(modelValue), disabled: opt.disabled }"
          :disabled="opt.disabled"
          @click="pick(opt)"
        >
          <span>{{ opt.label }}</span>
          <Icon v-if="String(opt.value) === String(modelValue)" name="check" :size="14" />
        </button>
      </div>
    </Transition>
  </div>
</template>
