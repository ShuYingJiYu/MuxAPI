const cssVariablePattern = /^var\(\s*(--[\w-]+)(?:\s*,\s*([^)]+))?\s*\)$/

const THEME_STORAGE_KEY = 'muxapi_theme'

export const THEMES = [
  {
    id: 'peach-soda',
    name: '蜜桃晴空',
    description: '蜜桃粉、薄荷绿与晴空蓝',
    swatches: ['#f58daf', '#45c9b6', '#85c8e5', '#fffaf2'],
    canvas: '#fffaf2',
    dark: false,
  },
  {
    id: 'sakura-milk',
    name: '樱莓牛乳',
    description: '樱花粉、莓果紫与牛乳白',
    swatches: ['#f47fa9', '#a58ae5', '#75c8e8', '#fff7fb'],
    canvas: '#fff7fb',
    dark: false,
  },
  {
    id: 'mint-salt',
    name: '薄荷海盐',
    description: '薄荷青、海盐蓝与珊瑚粉',
    swatches: ['#36cbb1', '#68bfe5', '#ff8eaa', '#f3fffb'],
    canvas: '#f3fffb',
    dark: false,
  },
  {
    id: 'starlight-candy',
    name: '星糖夜航',
    description: '夜色底、霓虹莓果与汽水青',
    swatches: ['#ff87b7', '#6edfd0', '#8bbcf5', '#1a171f'],
    canvas: '#1a171f',
    dark: true,
  },
]

const themeByID = new Map(THEMES.map(theme => [theme.id, theme]))

export function storedThemeID() {
  if (typeof localStorage === 'undefined') return THEMES[0].id
  const id = localStorage.getItem(THEME_STORAGE_KEY)
  return themeByID.has(id) ? id : THEMES[0].id
}

export function applyTheme(id, persist = true) {
  const theme = themeByID.get(id) || THEMES[0]
  if (typeof document === 'undefined') return theme
  document.documentElement.dataset.theme = theme.id
  document.documentElement.style.colorScheme = theme.dark ? 'dark' : 'light'
  document.querySelector('meta[name="theme-color"]')?.setAttribute('content', theme.canvas)
  if (persist && typeof localStorage !== 'undefined') localStorage.setItem(THEME_STORAGE_KEY, theme.id)
  window.dispatchEvent(new CustomEvent('muxapi-theme-change', { detail: theme }))
  return theme
}

export function initTheme() {
  return applyTheme(storedThemeID(), false)
}

export function themeValue(name, fallback = '') {
  if (typeof document === 'undefined') return fallback
  return getComputedStyle(document.documentElement).getPropertyValue(name).trim() || fallback
}

export function canvasColor(value, fallbackToken = '--chart-primary') {
  if (Array.isArray(value)) return value.map(item => canvasColor(item, fallbackToken))
  const source = String(value || '').trim()
  const match = source.match(cssVariablePattern)
  if (!match) return source || themeValue(fallbackToken)
  return themeValue(match[1], match[2]?.trim() || themeValue(fallbackToken))
}

export function colorWithAlpha(value, alpha) {
  const color = canvasColor(value)
  const hex = color.match(/^#([\da-f]{3}|[\da-f]{6})$/i)?.[1]
  if (hex) {
    const normalized = hex.length === 3 ? [...hex].map(part => part + part).join('') : hex
    const channels = normalized.match(/.{2}/g).map(part => Number.parseInt(part, 16))
    return `rgba(${channels.join(', ')}, ${alpha})`
  }
  const rgb = color.match(/^rgb\(\s*([\d.]+)\s*,\s*([\d.]+)\s*,\s*([\d.]+)\s*\)$/i)
  return rgb ? `rgba(${rgb.slice(1).join(', ')}, ${alpha})` : color
}
