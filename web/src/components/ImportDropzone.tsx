import { useEffect, useRef, useState } from 'react'
import { api, ApiError } from '../lib/api'

const IMPORTABLE = /\.(dlc|txt)$/i

function hasFiles(e: DragEvent): boolean {
  return !!e.dataTransfer && Array.from(e.dataTransfer.types).includes('Files')
}

export function ImportDropzone({
  onImported,
  onAuthError,
  saveTo,
}: {
  onImported: () => void
  onAuthError: () => void
  saveTo: string
}) {
  const [dragging, setDragging] = useState(false)
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<string | null>(null)
  const [result, setResult] = useState<string | null>(null)
  // IDs of imported items that arrived paused (auto-start off) — offered
  // as a one-click "Start now" instead of hunting for "Start all".
  const [pausedIds, setPausedIds] = useState<string[]>([])
  const inputRef = useRef<HTMLInputElement>(null)
  const saveToRef = useRef(saveTo)
  useEffect(() => {
    saveToRef.current = saveTo
  }, [saveTo])

  async function importFiles(all: File[]) {
    const files = all.filter((f) => IMPORTABLE.test(f.name))
    if (files.length === 0) {
      setError('only .dlc and .txt files can be imported')
      return
    }
    setBusy(true)
    setError(null)
    setResult(null)
    try {
      // One request for all files: the server groups their links per
      // release and merges parts of the same release spread over several
      // DLCs (or added earlier via Click'n'Load) into one package.
      const res = await api.importFiles(files)
      const path = saveToRef.current.trim()
      if (path) {
        await Promise.allSettled(res.packages.map((pkg) => api.setPackagePath(pkg.id, path)))
      }
      const parts = [`${res.items.length} links imported`]
      if (res.duplicates.length > 0) parts.push(`${res.duplicates.length} duplicates skipped`)
      setResult(parts.join(' · '))
      setPausedIds(res.items.filter((i) => i.state === 'paused').map((i) => i.id))
      if (res.errors && res.errors.length > 0) setError(res.errors.join('; '))
      onImported()
    } catch (err) {
      if (err instanceof ApiError && err.status === 401) onAuthError()
      setError(err instanceof Error ? err.message : 'import failed')
    } finally {
      setBusy(false)
    }
  }

  // Accept .dlc/.txt files dropped anywhere on the page, not just on the
  // button. Only real file drags count — the queue's own drag-to-reorder
  // carries no "Files" type and is left alone.
  const importRef = useRef(importFiles)
  useEffect(() => {
    importRef.current = importFiles
  })
  useEffect(() => {
    let depth = 0
    const onEnter = (e: DragEvent) => {
      if (!hasFiles(e)) return
      depth++
      setDragging(true)
    }
    const onLeave = (e: DragEvent) => {
      if (!hasFiles(e)) return
      depth = Math.max(0, depth - 1)
      if (depth === 0) setDragging(false)
    }
    const onOver = (e: DragEvent) => {
      if (hasFiles(e)) e.preventDefault()
    }
    const onDrop = (e: DragEvent) => {
      if (!hasFiles(e)) return
      e.preventDefault()
      depth = 0
      setDragging(false)
      const files = Array.from(e.dataTransfer?.files ?? [])
      if (files.length > 0) void importRef.current(files)
    }
    window.addEventListener('dragenter', onEnter)
    window.addEventListener('dragleave', onLeave)
    window.addEventListener('dragover', onOver)
    window.addEventListener('drop', onDrop)
    return () => {
      window.removeEventListener('dragenter', onEnter)
      window.removeEventListener('dragleave', onLeave)
      window.removeEventListener('dragover', onOver)
      window.removeEventListener('drop', onDrop)
    }
  }, [])

  return (
    <>
      <button
        type="button"
        className="btn"
        disabled={busy}
        title="Import .dlc / .txt — or drop files anywhere on the page"
        onClick={() => inputRef.current?.click()}
      >
        {busy ? 'Importing…' : 'Import .dlc'}
      </button>
      <input
        ref={inputRef}
        type="file"
        accept=".dlc,.txt"
        multiple
        hidden
        onChange={(e) => {
          const files = Array.from(e.target.files ?? [])
          if (files.length > 0) void importFiles(files)
          e.target.value = ''
        }}
      />
      {result && <span className="ok-text">{result}</span>}
      {pausedIds.length > 0 && (
        <button
          type="button"
          className="btn small primary"
          title="Imports are added paused (Settings → start imported links immediately)"
          onClick={() => {
            const ids = pausedIds
            setPausedIds([])
            api
              .queueActions(ids, 'resume')
              .then(onImported)
              .catch((err) => {
                if (err instanceof ApiError && err.status === 401) onAuthError()
                setError(err instanceof Error ? err.message : 'failed to start')
              })
          }}
        >
          <i className="ti ti-player-play" /> Start now
        </button>
      )}
      {error && <span className="error-text">{error}</span>}
      {dragging && (
        <div className="drop-overlay">
          <div className="drop-overlay-box">
            <i className="ti ti-archive" /> Drop .dlc / .txt files to import
          </div>
        </div>
      )}
    </>
  )
}
