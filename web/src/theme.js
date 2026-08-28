const cssVariablePattern = /^var\(\s*(--[\w-]+)(?:\s*,\s*([^)]+))?\s*\)$/

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
