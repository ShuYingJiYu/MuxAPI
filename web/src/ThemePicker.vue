<script setup>
import { computed, onBeforeUnmount, onMounted, ref } from 'vue'
import Icon from './Icon.vue'
import { THEMES, applyTheme, storedThemeID } from './theme.js'

const root = ref(null)
const open = ref(false)
const activeID = ref(storedThemeID())
const activeTheme = computed(() => THEMES.find(theme => theme.id === activeID.value) || THEMES[0])

function selectTheme(id) {
  activeID.value = applyTheme(id).id
  open.value = false
}

function handlePointerDown(event) {
  if (!root.value?.contains(event.target)) open.value = false
}

function handleKeydown(event) {
  if (event.key === 'Escape') open.value = false
}

onMounted(() => {
  document.addEventListener('pointerdown', handlePointerDown)
  document.addEventListener('keydown', handleKeydown)
})

onBeforeUnmount(() => {
  document.removeEventListener('pointerdown', handlePointerDown)
  document.removeEventListener('keydown', handleKeydown)
})
</script>

<template>
  <div ref="root" class="theme-picker" :class="{ open }">
    <button
      class="theme-trigger"
      type="button"
      title="切换项目主题"
      aria-label="切换项目主题"
      aria-haspopup="menu"
      :aria-expanded="open"
      @click="open = !open"
    >
      <Icon name="palette" :size="16" />
      <span class="theme-trigger-swatches" aria-hidden="true">
        <i v-for="color in activeTheme.swatches.slice(0, 3)" :key="color" :style="{ backgroundColor: color }"></i>
      </span>
    </button>

    <Transition name="theme-menu">
      <section v-if="open" class="theme-menu" role="menu" aria-label="项目主题">
        <header><strong>糖果主题</strong><small>{{ activeTheme.name }}</small></header>
        <div class="theme-options">
          <button
            v-for="theme in THEMES"
            :key="theme.id"
            class="theme-option"
            :class="{ active: theme.id === activeID }"
            type="button"
            role="menuitemradio"
            :aria-checked="theme.id === activeID"
            @click="selectTheme(theme.id)"
          >
            <span class="theme-preview" :class="{ dark: theme.dark }" :style="{ backgroundColor: theme.canvas }" aria-hidden="true">
              <i v-for="color in theme.swatches.slice(0, 3)" :key="color" :style="{ backgroundColor: color }"></i>
            </span>
            <span class="theme-option-copy"><strong>{{ theme.name }}</strong><small>{{ theme.description }}</small></span>
            <span v-if="theme.id === activeID" class="theme-option-check"><Icon name="check" :size="14" /></span>
          </button>
        </div>
      </section>
    </Transition>
  </div>
</template>

<style scoped>
.theme-picker { position: relative; }
.theme-picker.login-theme-picker { position: absolute; top: 20px; right: 20px; z-index: 2; }
.theme-trigger {
  width: 58px;
  height: 34px;
  display: flex;
  align-items: center;
  justify-content: center;
  gap: 6px;
  padding: 0 8px;
  border: 1px solid var(--line);
  border-radius: var(--r-action);
  background: var(--surface-raised);
  color: var(--g500);
  cursor: pointer;
  box-shadow: 0 2px 7px color-mix(in srgb, var(--g900) 7%, transparent);
  transition: border-color .16s, background .16s, color .16s, box-shadow .16s;
}
.theme-trigger:hover, .theme-picker.open .theme-trigger { border-color: var(--p400); background: var(--p50); color: var(--p700); box-shadow: 0 0 0 3px var(--focus-ring); }
.theme-trigger-swatches { display: flex; align-items: center; }
.theme-trigger-swatches i { width: 9px; height: 9px; margin-left: -2px; border: 1px solid var(--surface-raised); border-radius: 50%; }
.theme-trigger-swatches i:first-child { margin-left: 0; }
.theme-menu {
  position: absolute;
  z-index: 180;
  top: calc(100% + 10px);
  right: 0;
  width: min(310px, calc(100vw - 24px));
  padding: 10px;
  border: 1px solid var(--line-strong);
  border-radius: var(--r-card);
  background: var(--surface-glass);
  backdrop-filter: blur(18px) saturate(125%);
  box-shadow: var(--sh-glass);
  transform-origin: top right;
}
.theme-menu > header { display: flex; align-items: baseline; justify-content: space-between; gap: 10px; padding: 4px 6px 9px; }
.theme-menu > header strong { color: var(--g900); font-size: 13px; }
.theme-menu > header small { color: var(--g400); font-size: 10px; }
.theme-options { display: grid; gap: 5px; }
.theme-option {
  width: 100%;
  min-height: 57px;
  display: grid;
  grid-template-columns: 54px minmax(0, 1fr) 22px;
  align-items: center;
  gap: 10px;
  padding: 6px 8px;
  border: 1px solid transparent;
  border-radius: var(--r-control);
  background: transparent;
  color: var(--g700);
  text-align: left;
  cursor: pointer;
  transition: border-color .14s, background .14s;
}
.theme-option:hover { background: var(--surface-hover); }
.theme-option.active { border-color: var(--p100); background: var(--p50); }
.theme-preview {
  height: 40px;
  display: flex;
  align-items: flex-end;
  justify-content: center;
  gap: 3px;
  padding: 7px;
  border: 1px solid color-mix(in srgb, var(--g900) 10%, transparent);
  border-radius: 9px;
  box-shadow: inset 0 0 0 1px color-mix(in srgb, white 38%, transparent);
}
.theme-preview i { width: 10px; border-radius: 5px 5px 3px 3px; }
.theme-preview i:nth-child(1) { height: 24px; }
.theme-preview i:nth-child(2) { height: 18px; }
.theme-preview i:nth-child(3) { height: 12px; }
.theme-option-copy { min-width: 0; }
.theme-option-copy strong, .theme-option-copy small { display: block; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
.theme-option-copy strong { color: var(--g900); font-size: 12px; }
.theme-option-copy small { margin-top: 2px; color: var(--g400); font-size: 9.5px; }
.theme-option-check { width: 22px; height: 22px; display: grid; place-items: center; border-radius: 50%; background: var(--p500); color: var(--text-on-accent); }
.theme-menu-enter-active, .theme-menu-leave-active { transition: opacity .14s, transform .14s; }
.theme-menu-enter-from, .theme-menu-leave-to { opacity: 0; transform: translateY(-4px) scale(.98); }
@media (max-width: 760px) {
  .theme-trigger { width: 50px; padding: 0 6px; }
  .theme-menu { position: fixed; top: 64px; right: 8px; }
}
</style>
