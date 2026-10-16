import { useEffect, useState } from 'react'
import {
  getThemePreference,
  setThemePreference,
  THEME_EVENT,
  THEME_LABELS,
  type ThemePreference,
} from '../lib/theme'

const NEXT: Record<ThemePreference, ThemePreference> = { system: 'light', light: 'dark', dark: 'system' }
const ICON: Record<ThemePreference, string> = { system: '◐', light: '☀', dark: '☾' }

// ThemeToggle cycles System → Light → Dark.
export function ThemeToggle() {
  const [pref, setPref] = useState<ThemePreference>(getThemePreference)

  useEffect(() => {
    const onChange = (e: Event) => setPref((e as CustomEvent<ThemePreference>).detail)
    window.addEventListener(THEME_EVENT, onChange)
    return () => window.removeEventListener(THEME_EVENT, onChange)
  }, [])

  return (
    <button
      type="button"
      className="icon-btn theme-toggle"
      title={`Theme: ${THEME_LABELS[pref]} (click to switch)`}
      aria-label={`Theme: ${THEME_LABELS[pref]}`}
      onClick={() => setThemePreference(NEXT[pref])}
    >
      {ICON[pref]}
    </button>
  )
}
