// The active tab lives in the URL hash (#/stats, …), so the browser's Back
// button — on Android the system Back gesture — returns to the previous
// tab instead of leaving the app, and a reload stays on the same tab.

export type Tab = 'queue' | 'accounts' | 'stats' | 'settings'

const TABS: readonly Tab[] = ['queue', 'accounts', 'stats', 'settings']

// tabFromHash maps a location.hash to its tab; anything unknown (no hash,
// other fragments) is the queue.
export function tabFromHash(hash: string): Tab {
  const name = hash.replace(/^#\/?/, '')
  return (TABS as readonly string[]).includes(name) ? (name as Tab) : 'queue'
}

export function hashForTab(tab: Tab): string {
  return `#/${tab}`
}
