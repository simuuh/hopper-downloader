import { useCallback, useEffect, useRef, useState } from 'react'
import { Login } from './components/Login'
import { StatusBar } from './components/StatusBar'
import { ThemeToggle } from './components/ThemeToggle'
import { useQueueSocket } from './hooks/useQueueSocket'
import { api, clearToken, getToken } from './lib/api'
import { isFinished } from './lib/finished'
import { formatVersion } from './lib/format'
import { clearSharedLinks, peekSharedLinks } from './lib/share'
import { notificationsEnabled, notifyExtractFailed, notifyItemDone, notifyItemError, notifyPackageComplete } from './lib/notifications'
import { hashForTab, tabFromHash, type Tab } from './lib/route'
import type { Package, QueueItem, UpdateStatus } from './lib/types'
import { AccountsPage } from './pages/AccountsPage'
import { QueuePage } from './pages/QueuePage'
import { SettingsPage } from './pages/SettingsPage'
import { StatsPage } from './pages/StatsPage'

export function App() {
  const [loggedIn, setLoggedIn] = useState(() => getToken() !== null)
  // The tab follows the URL hash (see lib/route.ts): switching tabs adds a
  // history entry, and Back/Forward come in as hashchange events.
  const [tab, setTabState] = useState<Tab>(() => tabFromHash(window.location.hash))
  useEffect(() => {
    const onHashChange = () => setTabState(tabFromHash(window.location.hash))
    window.addEventListener('hashchange', onHashChange)
    return () => window.removeEventListener('hashchange', onHashChange)
  }, [])
  const setTab = (next: Tab) => {
    if (next !== tabFromHash(window.location.hash)) window.location.hash = hashForTab(next)
  }
  const [items, setItems] = useState<QueueItem[]>([])
  // Packages are kept here (not only in QueuePage) and updated from the
  // websocket's package_updated/package_removed events, so a package
  // created by Click'n'Load, hopperctl or the browser extension shows its
  // real name right away instead of its raw ID until the next reload.
  const [packages, setPackages] = useState<Package[]>([])
  const packagesRef = useRef<Package[]>([])
  useEffect(() => {
    packagesRef.current = packages
  }, [packages])
  const [version, setVersion] = useState<string | null>(null)
  // The last update check's result as hopperd cached it. Reading it never
  // reaches GitHub; it is re-read hourly so a long-open tab picks up the
  // daily check.
  const [update, setUpdate] = useState<UpdateStatus | null>(null)
  // Links shared from another app (PWA share target, iOS Shortcut — see
  // lib/share.ts). Kept until QueuePage has put them into its add form,
  // so they survive the login screen.
  const [sharedText, setSharedText] = useState<string | null>(peekSharedLinks)
  const consumeShared = useCallback(() => {
    clearSharedLinks()
    setSharedText(null)
  }, [])

  useEffect(() => {
    api
      .getVersion()
      .then((res) => setVersion(res.version))
      .catch(() => {})
  }, [])

  useEffect(() => {
    if (!loggedIn) return
    const load = () =>
      api
        .getUpdateStatus()
        .then(setUpdate)
        .catch(() => {})
    load()
    const id = setInterval(load, 60 * 60 * 1000)
    return () => clearInterval(id)
  }, [loggedIn])

  const handleAuthError = useCallback(() => {
    clearToken()
    setLoggedIn(false)
  }, [])

  const refresh = useCallback(() => {
    if (!loggedIn) return
    api
      .listQueue()
      .then(setItems)
      .catch(handleAuthError)
    api
      .listPackages()
      .then(setPackages)
      .catch(() => {})
  }, [loggedIn, handleAuthError])

  useEffect(() => {
    refresh()
  }, [refresh])

  const connection = useQueueSocket(
    loggedIn,
    (ev) => {
      if (ev.type === 'item_updated' && ev.item) {
        const updated = ev.item
        setItems((prev) => {
          const idx = prev.findIndex((i) => i.id === updated.id)
          const before = idx === -1 ? null : prev[idx]
          const next = idx === -1 ? [...prev, updated] : prev.map((i, j) => (j === idx ? updated : i))

          // "Done" means finished as lib/finished defines it: an archive
          // notifies once it is extracted, not when its download ends.
          if (notificationsEnabled()) {
            if (isFinished(updated) && !(before && isFinished(before))) {
              notifyItemDone(updated)
              if (updated.package_id) {
                const siblings = next.filter((i) => i.package_id === updated.package_id)
                if (siblings.length > 0 && siblings.every(isFinished)) {
                  const pkg = packagesRef.current.find((p) => p.id === updated.package_id)
                  notifyPackageComplete(pkg?.name ?? updated.package_id, siblings.length)
                }
              }
            } else if (updated.state === 'error' && before?.state !== 'error') {
              notifyItemError(updated)
            } else if (updated.extract_state === 'extract_failed' && before?.extract_state !== 'extract_failed') {
              notifyExtractFailed(updated)
            }
          }

          return next
        })
      } else if (ev.type === 'item_removed' && ev.item) {
        const removedId = ev.item.id
        setItems((prev) => prev.filter((i) => i.id !== removedId))
      } else if (ev.type === 'package_updated' && ev.package) {
        const pkg = ev.package
        setPackages((prev) => (prev.some((p) => p.id === pkg.id) ? prev.map((p) => (p.id === pkg.id ? pkg : p)) : [...prev, pkg]))
      } else if (ev.type === 'package_removed' && ev.package) {
        const removedId = ev.package.id
        setPackages((prev) => prev.filter((p) => p.id !== removedId))
      }
    },
    // Force a full GET /api/v1/queue on every (re)connect, not just the
    // initial mount's own refresh() call above: a dropped connection
    // (network blip, or hopperd restarting) can miss state changes that
    // happened while disconnected — most notably an item stuck showing
    // "Extracting…" because hopperd restarted mid-extraction and reset it
    // to extract_failed server-side (see queue.Manager.Start) — and the
    // websocket itself only streams events going forward, it never
    // replays what was missed.
    refresh,
  )

  if (!loggedIn) {
    return <Login onLoggedIn={() => setLoggedIn(true)} />
  }

  return (
    <div className="app">
      <header className="topbar">
        <div className="brand">
          HOPPER {version && <span className="version">{formatVersion(version)}</span>}
          {update?.available && update.latest && (
            <a
              className="update-badge"
              href={update.release_url}
              target="_blank"
              rel="noopener noreferrer"
              title={`Hopper ${formatVersion(update.latest)} is available — release notes`}
              aria-label={`Update available: ${formatVersion(update.latest)}`}
            >
              <i className="ti ti-arrow-up" />
            </a>
          )}
          <small>power-user download manager</small>
        </div>
        <nav className="nav">
          <button className={tab === 'queue' ? 'active' : ''} onClick={() => setTab('queue')}>
            Queue
          </button>
          <button className={tab === 'accounts' ? 'active' : ''} onClick={() => setTab('accounts')}>
            Accounts
          </button>
          <button className={tab === 'stats' ? 'active' : ''} onClick={() => setTab('stats')}>
            Stats
          </button>
          <button className={tab === 'settings' ? 'active' : ''} onClick={() => setTab('settings')}>
            Settings
          </button>
        </nav>
        <div className="spacer" />
        <StatusBar items={items} connection={connection} />
        <ThemeToggle />
        <button
          className="logout-btn"
          onClick={() => {
            clearToken()
            setLoggedIn(false)
          }}
        >
          Sign out
        </button>
      </header>

      <main className="main">
        {tab === 'queue' && (
          <QueuePage
            items={items}
            packages={packages}
            sharedText={sharedText}
            onSharedConsumed={consumeShared}
            onRefresh={refresh}
            onAuthError={handleAuthError}
          />
        )}
        {tab === 'accounts' && <AccountsPage onAuthError={handleAuthError} />}
        {tab === 'stats' && <StatsPage onAuthError={handleAuthError} />}
        {tab === 'settings' && <SettingsPage updateStatus={update} onUpdateStatus={setUpdate} onAuthError={handleAuthError} />}
      </main>
    </div>
  )
}
