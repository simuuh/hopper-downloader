import { useState, type FormEvent } from 'react'
import { api, ApiError } from '../lib/api'

// countLinks mirrors the server's URL extraction closely enough for the
// button label; the server (dlc.ExtractURLs) has the final say.
function countLinks(text: string): number {
  const matches = text.match(/https?:\/\/[^\s<>"'`]+/gi)
  return matches ? new Set(matches).size : 0
}

export function AddLinkForm({
  urls,
  setUrls,
  onAdded,
  onAuthError,
  saveTo,
}: {
  urls: string
  setUrls: (value: string) => void
  onAdded: () => void
  onAuthError: () => void
  saveTo: string
}) {
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<string | null>(null)
  const [result, setResult] = useState<string | null>(null)
  const [password, setPassword] = useState('')
  const [expanded, setExpanded] = useState(false)
  const linkCount = countLinks(urls)

  async function handleSubmit(e: FormEvent) {
    e.preventDefault()
    if (linkCount === 0) return

    setBusy(true)
    setError(null)
    setResult(null)
    try {
      // One request for any number of links: the server groups archive
      // parts per release, merges further parts into an already-queued
      // release's package and skips duplicates (POST /links/bulk).
      const res = await api.addLinks({ text: urls, path: saveTo.trim() || undefined, password: password || undefined })
      const parts = [`${res.items.length} added`]
      if (res.packages.length > 0) parts.push(`${res.packages.length} package${res.packages.length === 1 ? '' : 's'}`)
      if (res.duplicates.length > 0) parts.push(`${res.duplicates.length} duplicate${res.duplicates.length === 1 ? '' : 's'} skipped`)
      setResult(parts.join(' · '))
      setUrls('')
      setPassword('')
      setExpanded(false)
      onAdded()
    } catch (err) {
      if (err instanceof ApiError && err.status === 401) onAuthError()
      setError(err instanceof Error ? err.message : 'failed to add links')
    } finally {
      setBusy(false)
    }
  }

  return (
    <form className={`add-link-row${expanded ? ' expanded' : ''}`} onSubmit={handleSubmit}>
      <textarea
        className="url-input"
        placeholder="Paste download links — one or many, any text containing URLs…"
        value={urls}
        rows={expanded ? 6 : 1}
        onFocus={() => setExpanded(true)}
        onChange={(e) => {
          setUrls(e.target.value)
          setResult(null)
        }}
        onKeyDown={(e) => {
          if (e.key === 'Enter' && (e.ctrlKey || e.metaKey)) {
            e.preventDefault()
            e.currentTarget.form?.requestSubmit()
          }
        }}
      />
      {expanded && (
        <input
          type="text"
          className="archive-password-input"
          placeholder="Archive password (optional)"
          value={password}
          onChange={(e) => setPassword(e.target.value)}
        />
      )}
      <button className="btn primary" type="submit" disabled={busy || linkCount === 0} title="Ctrl+Enter">
        {busy ? 'Adding…' : linkCount > 1 ? `Add ${linkCount} links` : 'Add'}
      </button>
      {error && <span className="error-text">{error}</span>}
      {result && <span className="ok-text">{result}</span>}
    </form>
  )
}
