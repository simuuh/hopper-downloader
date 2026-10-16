export function formatBytes(n: number): string {
  if (n <= 0) return '0 B'
  const units = ['B', 'KB', 'MB', 'GB', 'TB']
  const i = Math.min(units.length - 1, Math.floor(Math.log(n) / Math.log(1024)))
  return `${(n / 1024 ** i).toFixed(i === 0 ? 0 : 1)} ${units[i]}`
}

export function formatSpeed(n: number): string {
  return n > 0 ? `${formatBytes(n)}/s` : '0 B/s'
}

// formatCountdown renders a remaining-seconds count as m:ss (e.g. "3:45",
// "0:09") for the queue's "waiting" retry_after display — not h:mm:ss,
// since a hoster-imposed wait is expected to be short (typically well
// under an hour); a negative/zero input clamps to "0:00" rather than
// showing a negative countdown for the brief window before the item's
// next state update arrives.
export function formatCountdown(totalSeconds: number): string {
  const s = Math.max(0, Math.round(totalSeconds))
  const hours = Math.floor(s / 3600)
  const minutes = Math.floor((s % 3600) / 60)
  const seconds = s % 60
  const ss = seconds.toString().padStart(2, '0')
  // Package ETAs (unlike hoster waits) can exceed an hour: h:mm:ss then.
  if (hours > 0) return `${hours}:${minutes.toString().padStart(2, '0')}:${ss}`
  return `${minutes}:${ss}`
}

// itemName is how a queue item is labelled everywhere: its real filename
// once known, else the last path segment of its URL (minus a hoster's
// ".html" info-page suffix) — plain links only learn their filename when
// the download starts.
export function itemName(item: { filename: string; url: string }): string {
  if (item.filename) return item.filename
  try {
    const seg = decodeURIComponent(new URL(item.url).pathname.split('/').filter(Boolean).pop() ?? '')
    return seg.replace(/\.html$/i, '') || item.url
  } catch {
    return item.url
  }
}
