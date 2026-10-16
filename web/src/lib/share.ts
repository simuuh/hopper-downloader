// Links shared to Hopper from another app arrive as a page load of
// "/?share_text=...&share_url=...&share_title=..." — either from the
// installed PWA's Web Share Target (public/manifest.json, Android) or from
// any URL-opening automation (an iOS Shortcut, a bookmarklet). They are
// parked in sessionStorage so they survive the login screen, and the
// query string is removed so a reload doesn't add them again.

const PENDING_KEY = 'hopper_pending_share'
const PARAMS = ['share_url', 'share_text', 'share_title']

// captureSharedLinks moves any share_* query parameters into
// sessionStorage. Call once, before the first render.
export function captureSharedLinks(): void {
  const params = new URLSearchParams(window.location.search)
  // The title is usually just the page title; keep it only if it carries
  // a link itself (some apps put the URL there).
  const parts: string[] = []
  for (const p of PARAMS) {
    const v = params.get(p)?.trim()
    if (v && (p !== 'share_title' || /https?:\/\//i.test(v))) parts.push(v)
  }
  if (parts.length === 0) return
  try {
    sessionStorage.setItem(PENDING_KEY, parts.join('\n'))
  } catch {
    // storage unavailable (private mode): nothing to keep
  }
  for (const p of PARAMS) params.delete(p)
  const rest = params.toString()
  window.history.replaceState(null, '', window.location.pathname + (rest ? `?${rest}` : '') + window.location.hash)
}

// peekSharedLinks returns the parked shared text (or null) without
// removing it — safe to call from a state initializer.
export function peekSharedLinks(): string | null {
  try {
    return sessionStorage.getItem(PENDING_KEY)
  } catch {
    return null
  }
}

// clearSharedLinks forgets the parked shared text once it was used.
export function clearSharedLinks(): void {
  try {
    sessionStorage.removeItem(PENDING_KEY)
  } catch {
    // nothing stored
  }
}
