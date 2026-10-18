import { useEffect, useMemo, useState, type DragEvent } from 'react'
import { AddLinkForm } from '../components/AddLinkForm'
import { ImportDropzone } from '../components/ImportDropzone'
import { ItemRow } from '../components/ItemRow'
import { PageHeader } from '../components/Page'
import { NARROW_SCREEN, useMediaQuery } from '../hooks/useMediaQuery'
import { api, ApiError, setToken, tokenNearExpiry } from '../lib/api'
import { formatBytes, formatCountdown, formatSpeed, itemName } from '../lib/format'
import { finishedLast, groupKeyOf, sortQueue } from '../lib/order'
import type { ItemState, Package, PackageAction, QueueItem } from '../lib/types'

type Action = 'pause' | 'resume' | 'retry' | 'cancel'
type Filter = 'all' | 'active' | 'done' | 'errors'

const ACTIVE_STATES: ItemState[] = ['queued', 'downloading', 'paused']

const FILTERS: { key: Filter; label: string }[] = [
  { key: 'all', label: 'All' },
  { key: 'active', label: 'Active' },
  { key: 'done', label: 'Finished' },
  { key: 'errors', label: 'Errors' },
]

// Priority order for a package group's roll-up badge: the first state
// found present in the group "wins" — error needs attention most, done
// needs it least.
const STATE_SEVERITY: ItemState[] = ['error', 'downloading', 'paused', 'queued', 'done']

interface Group {
  key: string
  name: string
  items: QueueItem[]
}

function isToday(iso: string): boolean {
  const d = new Date(iso)
  const now = new Date()
  return d.getFullYear() === now.getFullYear() && d.getMonth() === now.getMonth() && d.getDate() === now.getDate()
}

// finishedAt is when a finished group's last item completed.
function finishedAt(items: QueueItem[]): number {
  return items.reduce((max, i) => Math.max(max, new Date(i.updated_at).getTime()), 0)
}

function formatFinished(ms: number): string {
  const d = new Date(ms)
  const time = d.toLocaleTimeString([], { hour: '2-digit', minute: '2-digit' })
  return isToday(d.toISOString()) ? `today ${time}` : `${d.toLocaleDateString()} ${time}`
}

// extractSummary rolls a group's archive state up into one word for the
// finished view: failed wins, then extracted; null when nothing was
// extracted (no archives, or auto-extract off).
function extractSummary(items: QueueItem[]): 'extracted' | 'extract_failed' | null {
  if (items.some((i) => i.extract_state === 'extract_failed')) return 'extract_failed'
  if (items.some((i) => i.extract_state === 'extracted')) return 'extracted'
  return null
}

function worstState(items: QueueItem[]): ItemState {
  for (const s of STATE_SEVERITY) {
    if (items.some((i) => i.state === s)) return s
  }
  return 'queued'
}

// groupBadge is the package header's state label: like worstState, but a
// package that is only paused because it was never started (e.g. an
// import with auto-start off) reads 'ready' rather than 'paused'.
function groupBadge(items: QueueItem[]): string {
  const s = worstState(items)
  if (s === 'paused' && items.filter((i) => i.state === 'paused').every((i) => i.downloaded === 0)) return 'ready'
  return s
}

export function QueuePage({
  items,
  packages,
  sharedText,
  onSharedConsumed,
  onRefresh,
  onAuthError,
}: {
  items: QueueItem[]
  packages: Package[]
  sharedText: string | null
  onSharedConsumed: () => void
  onRefresh: () => void
  onAuthError: () => void
}) {
  const [selected, setSelected] = useState<Set<string>>(new Set())
  const [draggingId, setDraggingId] = useState<string | null>(null)
  const [busy, setBusy] = useState(false)
  const [toast, setToast] = useState<string | null>(null)
  // info is a neutral (non-error) toast for confirmations.
  const [info, setInfo] = useState<string | null>(() =>
    sharedText ? 'Shared links are in the add field — review them and press Add.' : null,
  )
  const [collapsed, setCollapsed] = useState<Set<string>>(new Set())
  const [filter, setFilter] = useState<Filter>('all')
  const [urls, setUrls] = useState(() => sharedText ?? '')
  const [saveTo, setSaveTo] = useState('')
  const [saveToDraft, setSaveToDraft] = useState('')
  const [showSaveTo, setShowSaveTo] = useState(false)
  const sortedItems = useMemo(() => sortQueue(items), [items])
  // On phones a package of one file is shown as just its file row: the
  // package header would repeat the same name and size.
  const narrow = useMediaQuery(NARROW_SCREEN)
  // In the Finished view groups start collapsed; expanded tracks the ones
  // opened there (collapsed above is for every other view).
  const [expanded, setExpanded] = useState<Set<string>>(new Set())
  const [addingTo, setAddingTo] = useState<string | null>(null)
  const [addToDraft, setAddToDraft] = useState('')

  // Shared links were taken over into urls by its initializer above.
  useEffect(() => {
    if (sharedText) onSharedConsumed()
  }, [sharedText, onSharedConsumed])

  // finishedKeys are the groups whose every item is done — the Finished
  // view lists whole packages, not single done files of a package that is
  // still downloading.
  const finishedKeys = useMemo(() => {
    const unfinished = new Set<string>()
    const all = new Set<string>()
    for (const i of items) {
      const key = groupKeyOf(i)
      all.add(key)
      if (i.state !== 'done') unfinished.add(key)
    }
    return new Set([...all].filter((k) => !unfinished.has(k)))
  }, [items])

  // Shown in start order (see lib/order.ts): top to bottom is the order
  // the dispatcher starts downloads in. In "All", finished packages follow
  // the ones still in progress.
  const filteredItems = useMemo(() => {
    const items = sortedItems
    switch (filter) {
      case 'active':
        return items.filter((i) => ACTIVE_STATES.includes(i.state))
      case 'done':
        return items.filter((i) => finishedKeys.has(groupKeyOf(i)))
      case 'errors':
        return items.filter((i) => i.state === 'error')
      default:
        return finishedLast(items, finishedKeys)
    }
  }, [sortedItems, filter, finishedKeys])

  const filterCounts = useMemo<Record<Filter, number>>(
    () => ({
      all: items.length,
      active: items.filter((i) => ACTIVE_STATES.includes(i.state)).length,
      done: finishedKeys.size,
      errors: items.filter((i) => i.state === 'error').length,
    }),
    [items, finishedKeys],
  )

  const allSelected = filteredItems.length > 0 && selected.size === filteredItems.length

  useEffect(() => {
    if (!toast) return
    const t = setTimeout(() => setToast(null), 6000)
    return () => clearTimeout(t)
  }, [toast])

  useEffect(() => {
    if (!info) return
    const t = setTimeout(() => setInfo(null), 6000)
    return () => clearTimeout(t)
  }, [info])

  // Packages stream over the websocket too (see App.tsx); refreshAll is
  // kept as the post-add hook name for the add/import components.
  const refreshAll = onRefresh

  function toggleSelect(id: string) {
    setSelected((prev) => {
      const next = new Set(prev)
      if (next.has(id)) next.delete(id)
      else next.add(id)
      return next
    })
  }

  function toggleSelectAll() {
    setSelected(allSelected ? new Set() : new Set(filteredItems.map((i) => i.id)))
  }

  function toggleGroup(key: string) {
    if (filter === 'done') {
      setExpanded((prev) => {
        const next = new Set(prev)
        if (next.has(key)) next.delete(key)
        else next.add(key)
        return next
      })
      return
    }
    setCollapsed((prev) => {
      const next = new Set(prev)
      if (next.has(key)) next.delete(key)
      else next.add(key)
      return next
    })
  }

  // performAction is the shared per-item worker for both a single-row
  // action and one item of a bulk batch. It never triggers onAuthError
  // itself — that decision depends on how *many* of a batch's parallel
  // requests actually failed with 401, which only the caller (runAction
  // for a batch of one, runBulkAction for many) can judge. 429 is kept
  // as its own outcome, distinct from 'unauthorized': a rate limit is
  // never a session problem, no matter how many of a batch hit it (see
  // runParallelActions).
  async function performAction(id: string, action: Action): Promise<'ok' | 'unauthorized' | 'ratelimited' | 'error'> {
    try {
      if (action === 'pause') await api.pause(id)
      else if (action === 'resume') await api.resume(id)
      else if (action === 'retry') await api.retry(id)
      else await api.cancel(id)
      return 'ok'
    } catch (err) {
      if (err instanceof ApiError && err.status === 401) return 'unauthorized'
      if (err instanceof ApiError && err.status === 429) return 'ratelimited'
      return 'error'
    }
  }

  async function runAction(id: string, action: Action) {
    const result = await performAction(id, action)
    if (result === 'unauthorized') {
      onAuthError()
      return
    }
    if (result === 'ratelimited') {
      setToast('Too many requests — try again in a moment.')
      return
    }
    if (result === 'ok') onRefresh()
  }

  // maybeRefreshToken proactively renews the token before a bulk action
  // if it's close to expiring (see tokenNearExpiry) — a batch of
  // parallel requests takes long enough, relative to a JWT nearing the
  // end of its window, that some could legitimately expire mid-flight
  // otherwise. A failed refresh is left for the batch itself to
  // surface (via the 401-aggregation below) rather than guessed at
  // here: if the token was already truly dead, refresh legitimately
  // 401s too, and that's not this function's call to make.
  async function maybeRefreshToken() {
    if (!tokenNearExpiry()) return
    try {
      const res = await api.refreshToken()
      setToken(res.token, res.expires_at)
    } catch {
      // Handled by the batch's own aggregation below.
    }
  }

  // runBulkAction applies a multi-select action in one request
  // (POST /queue/actions): resume/retry are applied atomically server-side
  // so the selection starts top to bottom, and a large selection can't
  // trip the rate limiter the way one request per item could.
  async function runSelectionAction(ids: string[], action: 'pause' | 'resume' | 'retry') {
    if (ids.length === 0) return
    setBusy(true)
    setToast(null)
    await maybeRefreshToken()
    try {
      await api.queueActions(ids, action)
      onRefresh()
    } catch (err) {
      if (err instanceof ApiError && err.status === 401) onAuthError()
      else if (err instanceof ApiError && err.status === 429) setToast('Too many requests — try again in a moment.')
      else setToast(err instanceof Error ? err.message : 'action failed')
    } finally {
      setBusy(false)
    }
  }

  // bulkCancel cancels every id in one request (DELETE /api/v1/queue)
  // instead of runParallelActions' N-parallel-requests approach — used
  // for cancel specifically (by both clearDone and runBulkAction's
  // 'cancel' branch) because firing one DELETE per item was exactly what
  // could trip the API's rate limiter on a large batch in the first
  // place; see the bulk endpoint's own doc comment in
  // api/queue_handlers.go. A per-id failure doesn't fail the whole
  // request (res.failed reports it) any more than one 401 used to fail
  // the whole batch above.
  async function bulkCancel(ids: string[]) {
    if (ids.length === 0) return
    setBusy(true)
    setToast(null)
    await maybeRefreshToken()

    try {
      const res = await api.bulkCancel(ids)
      if (res.failed.length > 0) {
        setToast(`${res.failed.length} of ${ids.length} items failed to clear.`)
      }
      if (res.deleted.length > 0) onRefresh()
    } catch (err) {
      if (err instanceof ApiError && err.status === 401) {
        onAuthError()
      } else if (err instanceof ApiError && err.status === 429) {
        setToast('Too many requests — try again in a moment.')
      } else {
        setToast(err instanceof Error ? err.message : 'failed to clear items')
      }
    } finally {
      setBusy(false)
    }
  }

  async function runBulkAction(action: Action) {
    const ids = [...selected]
    setSelected(new Set())
    if (action === 'cancel') {
      await bulkCancel(ids)
      return
    }
    await runSelectionAction(ids, action)
  }

  async function pauseAll() {
    try {
      await api.pauseAll()
      onRefresh()
    } catch (err) {
      if (err instanceof ApiError && err.status === 401) onAuthError()
    }
  }

  async function resumeAll() {
    try {
      await api.resumeAll()
      onRefresh()
    } catch (err) {
      if (err instanceof ApiError && err.status === 401) onAuthError()
    }
  }

  async function clearDone() {
    const doneIds = items.filter((i) => i.state === 'done').map((i) => i.id)
    await bulkCancel(doneIds)
  }

  async function setPriority(id: string, priority: QueueItem['priority']) {
    try {
      await api.setPriority(id, priority)
      onRefresh()
    } catch (err) {
      if (err instanceof ApiError && err.status === 401) onAuthError()
    }
  }

  async function editItem(id: string, edit: { url?: string; filename?: string }): Promise<string | null> {
    try {
      await api.editItem(id, edit)
      return null
    } catch (err) {
      if (err instanceof ApiError && err.status === 401) {
        onAuthError()
        return null
      }
      return err instanceof Error ? err.message : 'failed to save'
    }
  }

  async function addToPackage(id: string) {
    const text = addToDraft.trim()
    if (!text) return
    try {
      const res = await api.addToPackage(id, text)
      const parts = [`${res.items.length} added`]
      if (res.duplicates.length > 0) parts.push(`${res.duplicates.length} duplicate${res.duplicates.length === 1 ? '' : 's'} skipped`)
      setInfo(parts.join(' · '))
      setAddingTo(null)
      setAddToDraft('')
    } catch (err) {
      if (err instanceof ApiError && err.status === 401) onAuthError()
      else if (err instanceof ApiError) setToast(err.message)
    }
  }

  async function retryExtract(id: string, password?: string) {
    try {
      await api.retryExtract(id, password)
      onRefresh()
    } catch (err) {
      if (err instanceof ApiError && err.status === 401) onAuthError()
      else if (err instanceof ApiError) setToast(err.message)
    }
  }

  async function solveCaptcha(id: string, solution: string) {
    try {
      await api.solveCaptcha(id, solution)
      onRefresh()
    } catch (err) {
      if (err instanceof ApiError && err.status === 401) onAuthError()
      else if (err instanceof ApiError) setToast(err.message)
    }
  }

  async function runPackageAction(id: string, action: PackageAction | 'delete') {
    try {
      if (action === 'delete') await api.deletePackage(id)
      else await api.packageAction(id, action)
      onRefresh()
    } catch (err) {
      if (err instanceof ApiError && err.status === 401) onAuthError()
      else if (err instanceof ApiError) setToast(err.message)
    }
  }

  async function renamePackage(id: string, current: string) {
    const name = window.prompt('Package name', current)?.trim()
    if (!name || name === current) return
    try {
      await api.renamePackage(id, name)
    } catch (err) {
      if (err instanceof ApiError && err.status === 401) onAuthError()
      else if (err instanceof ApiError) setToast(err.message)
    }
  }

  function openSaveTo() {
    setSaveToDraft(saveTo)
    setShowSaveTo(true)
  }

  function confirmSaveTo() {
    setSaveTo(saveToDraft.trim())
    setShowSaveTo(false)
  }

  function handleDragStart(e: DragEvent, id: string) {
    setDraggingId(id)
    e.dataTransfer.effectAllowed = 'move'
  }

  function handleDragOver(e: DragEvent) {
    e.preventDefault()
  }

  async function handleDrop(e: DragEvent, targetId: string) {
    e.preventDefault()
    if (!draggingId || draggingId === targetId) {
      setDraggingId(null)
      return
    }
    const dragItem = items.find((i) => i.id === draggingId)
    const targetItem = items.find((i) => i.id === targetId)
    if (!dragItem || !targetItem || groupKeyOf(dragItem) !== groupKeyOf(targetItem)) {
      // Dragging between packages is out of scope.
      setDraggingId(null)
      return
    }
    const ids = items.map((i) => i.id)
    const from = ids.indexOf(draggingId)
    const to = ids.indexOf(targetId)
    ids.splice(from, 1)
    ids.splice(to, 0, draggingId)
    setDraggingId(null)
    try {
      await api.reorder(ids)
      onRefresh()
    } catch (err) {
      if (err instanceof ApiError && err.status === 401) onAuthError()
    }
  }

  const packagesById = useMemo(() => {
    const map = new Map<string, Package>()
    for (const p of packages) map.set(p.id, p)
    return map
  }, [packages])

  const groups = useMemo<Group[]>(() => {
    const order: string[] = []
    const map = new Map<string, Group>()
    for (const item of filteredItems) {
      const key = groupKeyOf(item)
      let group = map.get(key)
      if (!group) {
        const name = item.package_id ? packagesById.get(item.package_id)?.name || item.package_id : itemName(item)
        group = { key, name, items: [] }
        map.set(key, group)
        order.push(key)
      }
      group.items.push(item)
    }
    const result = order.map((k) => map.get(k)!)
    if (filter === 'done') result.sort((a, b) => finishedAt(b.items) - finishedAt(a.items))
    return result
  }, [filteredItems, packagesById, filter])

  const stats = useMemo(
    () => ({
      downloading: items.filter((i) => i.state === 'downloading').length,
      queued: items.filter((i) => i.state === 'queued').length,
      doneToday: items.filter((i) => i.state === 'done' && isToday(i.updated_at)).length,
      errors: items.filter((i) => i.state === 'error').length,
    }),
    [items],
  )

  const hasActiveItems = items.some((i) => i.state === 'queued' || i.state === 'downloading')
  const pausedCount = items.filter((i) => i.state === 'paused').length
  const hasDoneItems = items.some((i) => i.state === 'done')

  return (
    <>
      {info && !toast && (
        <div className="toast info" role="status">
          {info}
          <button type="button" className="toast-dismiss" onClick={() => setInfo(null)}>
            <i className="ti ti-x" />
          </button>
        </div>
      )}
      {toast && (
        <div className="toast" role="alert">
          {toast}
          <button type="button" className="toast-dismiss" onClick={() => setToast(null)}>
            <i className="ti ti-x" />
          </button>
        </div>
      )}
      <PageHeader
        title="Queue"
        subtitle="Paste links, drop .dlc files anywhere, or send them via Click'n'Load."
        actions={
          <>
            {hasActiveItems && (
              <button type="button" className="btn small" onClick={() => void pauseAll()}>
                <i className="ti ti-player-pause" /> Pause all
              </button>
            )}
            {pausedCount > 0 && (
              <button
                type="button"
                className="btn small primary"
                title="Start every paused or not yet started download, top to bottom"
                onClick={() => void resumeAll()}
              >
                <i className="ti ti-player-play" /> Start {pausedCount === 1 ? '1 download' : `all ${pausedCount}`}
              </button>
            )}
          </>
        }
      />
      <div className="row top-actions">
        <AddLinkForm urls={urls} setUrls={setUrls} onAdded={refreshAll} onAuthError={onAuthError} saveTo={saveTo} />
        <ImportDropzone onImported={refreshAll} onAuthError={onAuthError} saveTo={saveTo} />
        {showSaveTo ? (
          <span className="save-to-popover">
            <input
              type="text"
              className="save-to-input"
              placeholder={saveTo || 'Save to (optional subfolder)…'}
              value={saveToDraft}
              autoFocus
              onChange={(e) => setSaveToDraft(e.target.value)}
              onKeyDown={(e) => {
                if (e.key === 'Enter') {
                  e.preventDefault()
                  confirmSaveTo()
                } else if (e.key === 'Escape') {
                  setShowSaveTo(false)
                }
              }}
            />
            <button type="button" className="btn small" title="Confirm" onClick={confirmSaveTo}>
              ✓
            </button>
          </span>
        ) : (
          <button type="button" className="btn small" onClick={openSaveTo}>
            {saveTo ? `Save to: ${saveTo}` : 'Save to…'}
          </button>
        )}
      </div>

      <div className="stats-grid">
        <div className="stat-card">
          <div className="stat-label">Downloading</div>
          <div className="stat-value accent">{stats.downloading}</div>
        </div>
        <div className="stat-card">
          <div className="stat-label">Queued</div>
          <div className="stat-value dim">{stats.queued}</div>
        </div>
        <div className="stat-card">
          <div className="stat-label">Done today</div>
          <div className="stat-value done">{stats.doneToday}</div>
        </div>
        <div className="stat-card">
          <div className="stat-label">Errors</div>
          <div className="stat-value error">{stats.errors}</div>
        </div>
      </div>

      {selected.size > 0 && (
        <div className="row bulk-actions">
          <span className="mono">{selected.size} selected</span>
          <button className="btn small" disabled={busy} onClick={() => runBulkAction('pause')}>
            <i className="ti ti-player-pause" /> Pause
          </button>
          <button className="btn small" disabled={busy} onClick={() => runBulkAction('resume')}>
            <i className="ti ti-player-play" /> Resume
          </button>
          <button className="btn small" disabled={busy} onClick={() => runBulkAction('retry')}>
            <i className="ti ti-refresh" /> Retry
          </button>
          <button className="btn small danger" disabled={busy} onClick={() => runBulkAction('cancel')}>
            <i className="ti ti-x" /> Cancel
          </button>
        </div>
      )}

      <div className="row filter-tabs">
        {FILTERS.map(({ key, label }) => (
          <button
            key={key}
            type="button"
            className={`filter-tab${filter === key ? ' active' : ''}`}
            onClick={() => setFilter(key)}
          >
            {label}
            {filterCounts[key] > 0 && <span className="filter-count">{filterCounts[key]}</span>}
          </button>
        ))}
        {hasDoneItems && (
          <button type="button" className="btn small" disabled={busy} onClick={() => void clearDone()}>
            Clear done
          </button>
        )}
      </div>

      {items.length === 0 ? (
        <div className="empty-state">Queue is empty. Add a link above.</div>
      ) : filteredItems.length === 0 ? (
        <div className="empty-state">{filter === 'done' ? 'No finished packages yet.' : 'No items match this filter.'}</div>
      ) : (
        <>
          <div className="col-header">
            <span>
              <input type="checkbox" checked={allSelected} onChange={toggleSelectAll} />
            </span>
            <span>File</span>
            <span>Progress</span>
            <span>Speed</span>
            <span>Actions</span>
          </div>
          {groups.map((group) => {
            const single = narrow && group.items.length === 1
            const isCollapsed = !single && (filter === 'done' ? !expanded.has(group.key) : collapsed.has(group.key))
            const isFinished = finishedKeys.has(group.key)
            const extracted = isFinished ? extractSummary(group.items) : null
            const totalSize = group.items.reduce((sum, i) => sum + i.size, 0)
            const state = worstState(group.items)
            const badge = groupBadge(group.items)
            const pkgId = group.items[0]?.package_id
            const doneBytes = group.items.reduce(
              (sum, i) => sum + (i.state === 'done' ? Math.max(i.size, i.downloaded) : i.downloaded),
              0,
            )
            const groupSpeed = group.items.reduce((sum, i) => sum + (i.state === 'downloading' ? i.speed : 0), 0)
            const percent = totalSize > 0 ? Math.min(100, (doneBytes / totalSize) * 100) : 0
            const etaSeconds = groupSpeed > 0 && totalSize > doneBytes ? Math.round((totalSize - doneBytes) / groupSpeed) : 0
            const doneCount = group.items.filter((i) => i.state === 'done').length
            const has = (s: ItemState) => group.items.some((i) => i.state === s)
            const stop = (e: { stopPropagation: () => void }) => e.stopPropagation()
            return (
              <div className={`pkg-group${single ? ' single' : ''}`} key={group.key}>
                {!single && (
                  <div
                    className="pkg-header"
                    role="button"
                    tabIndex={0}
                    onClick={() => toggleGroup(group.key)}
                    onKeyDown={(e) => {
                      if (e.key === 'Enter' || e.key === ' ') {
                        e.preventDefault()
                        toggleGroup(group.key)
                      }
                    }}
                  >
                    <i className="ti ti-folder" />
                    <span className="pkg-name">{group.name}</span>
                    <span className="pkg-meta mono">
                      {doneCount}/{group.items.length} file{group.items.length === 1 ? '' : 's'} · {formatBytes(totalSize)}
                      {groupSpeed > 0 && ` · ${formatSpeed(groupSpeed)}`}
                      {etaSeconds > 0 && ` · ETA ${formatCountdown(etaSeconds)}`}
                      {isFinished && ` · finished ${formatFinished(finishedAt(group.items))}`}
                      {isFinished && pkgId && ` · ${packagesById.get(pkgId)?.path || group.name}/`}
                    </span>
                    {totalSize > 0 && (
                      <span className="pkg-progress" title={`${Math.round(percent)}%`}>
                        <span className={`progress-fill state-${state}`} style={{ width: `${percent}%` }} />
                      </span>
                    )}
                    {extracted && (
                      <span className={`badge ${extracted === 'extracted' ? 'state-done' : 'state-error'}`}>
                        {extracted === 'extracted' ? 'extracted' : 'extract failed'}
                      </span>
                    )}
                    <span className={`badge state-${badge}`}>{badge}</span>
                    {pkgId && (
                      <span className="pkg-actions" onClick={stop} onKeyDown={stop}>
                        {(has('queued') || has('downloading')) && (
                          <button className="icon-btn" title="Pause package" onClick={() => void runPackageAction(pkgId, 'pause')}>
                            <i className="ti ti-player-pause" />
                          </button>
                        )}
                        {has('paused') && (
                          <button className="icon-btn" title={badge === 'ready' ? 'Start package' : 'Resume package'} onClick={() => void runPackageAction(pkgId, 'resume')}>
                            <i className="ti ti-player-play" />
                          </button>
                        )}
                        {has('error') && (
                          <button className="icon-btn retry" title="Retry failed" onClick={() => void runPackageAction(pkgId, 'retry')}>
                            <i className="ti ti-refresh" />
                          </button>
                        )}
                        <button
                          className="icon-btn"
                          title="Add links to this package"
                          onClick={() => {
                            setAddToDraft('')
                            setAddingTo(addingTo === pkgId ? null : pkgId)
                          }}
                        >
                          +
                        </button>
                        <button className="icon-btn" title="Rename package" onClick={() => void renamePackage(pkgId, group.name)}>
                          ✎
                        </button>
                        <button
                          className="icon-btn danger"
                          title="Remove package from queue (files stay on disk)"
                          onClick={() => {
                            if (window.confirm(`Remove "${group.name}" (${group.items.length} items) from the queue?`)) {
                              void runPackageAction(pkgId, 'delete')
                            }
                          }}
                        >
                          <i className="ti ti-x" />
                        </button>
                      </span>
                    )}
                    <i className={`ti ${isCollapsed ? 'ti-chevron-right' : 'ti-chevron-down'}`} />
                  </div>
                )}
                {!single && pkgId && addingTo === pkgId && (
                  <form
                    className="pkg-add-links"
                    onSubmit={(e) => {
                      e.preventDefault()
                      void addToPackage(pkgId)
                    }}
                  >
                    <textarea
                      className="url-input"
                      rows={3}
                      autoFocus
                      placeholder={`Links to add to "${group.name}" (missing parts, mirrors)…`}
                      value={addToDraft}
                      onChange={(e) => setAddToDraft(e.target.value)}
                      onKeyDown={(e) => {
                        if (e.key === 'Escape') setAddingTo(null)
                        if (e.key === 'Enter' && (e.ctrlKey || e.metaKey)) {
                          e.preventDefault()
                          e.currentTarget.form?.requestSubmit()
                        }
                      }}
                    />
                    <button type="submit" className="btn small primary" disabled={!addToDraft.trim()}>
                      Add
                    </button>
                    <button type="button" className="btn small" onClick={() => setAddingTo(null)}>
                      Cancel
                    </button>
                  </form>
                )}
                {!isCollapsed && (
                  <div className="pkg-items">
                    {group.items.map((item) => (
                      <ItemRow
                        key={item.id}
                        item={item}
                        selected={selected.has(item.id)}
                        dragging={draggingId === item.id}
                        onToggleSelect={toggleSelect}
                        onAction={runAction}
                        onRetryExtract={retryExtract}
                        onSolveCaptcha={solveCaptcha}
                        onSetPriority={setPriority}
                        onEdit={editItem}
                        onDragStart={handleDragStart}
                        onDragOver={handleDragOver}
                        onDrop={handleDrop}
                      />
                    ))}
                  </div>
                )}
              </div>
            )
          })}
        </>
      )}
    </>
  )
}
