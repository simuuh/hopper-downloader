// Theme preference: "system" follows the OS (prefers-color-scheme), the
// other two force a theme via <html data-theme="...">. The actual colors
// live in styles.css; this only sets the attribute. Stored per browser.
export type ThemePreference = 'system' | 'light' | 'dark'

const STORAGE_KEY = 'hopper_theme'

export function getThemePreference(): ThemePreference {
  const v = localStorage.getItem(STORAGE_KEY)
  return v === 'light' || v === 'dark' ? v : 'system'
}

// The top bar's background (--bg-1 in styles.css) per theme, for the
// browser/status bar color on phones. Keep in sync with styles.css.
const THEME_COLORS = { light: '#ffffff', dark: '#14171b' } as const

export function applyTheme(pref: ThemePreference = getThemePreference()): void {
  const root = document.documentElement
  if (pref === 'system') root.removeAttribute('data-theme')
  else root.setAttribute('data-theme', pref)
  // index.html has one theme-color meta per color scheme; a forced theme
  // gives both its color so the status bar matches the page.
  for (const meta of document.querySelectorAll<HTMLMetaElement>('meta[name="theme-color"]')) {
    const scheme = meta.media.includes('light') ? 'light' : 'dark'
    meta.content = THEME_COLORS[pref === 'system' ? scheme : pref]
  }
}

export function setThemePreference(pref: ThemePreference): void {
  if (pref === 'system') localStorage.removeItem(STORAGE_KEY)
  else localStorage.setItem(STORAGE_KEY, pref)
  applyTheme(pref)
  window.dispatchEvent(new CustomEvent(THEME_EVENT, { detail: pref }))
}

// THEME_EVENT is dispatched on window whenever the preference changes, so
// every control showing it (topbar toggle, Settings) stays in sync.
export const THEME_EVENT = 'hopper-theme'

export const THEME_LABELS: Record<ThemePreference, string> = {
  system: 'System',
  light: 'Light',
  dark: 'Dark',
}
