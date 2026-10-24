import { useEffect, useState, type DragEvent } from 'react'
import { literalText } from '../lib/inputProps'
import { isFirstArchivePart } from '../lib/archive'
import { splitName } from '../lib/filename'
import { formatBytes, formatCountdown, formatSpeed, itemName } from '../lib/format'
import type { Priority, QueueItem } from '../lib/types'

// FileName shows a file name that, when too long, is shortened in its
// head while the tail (".part3.rar", ".iso") stays visible — see
// lib/filename.ts.
function FileName({ name, title }: { name: string; title: string }) {
  const { head, tail } = splitName(name)
  return (
    <span className="item-fname" title={title}>
      <span className="item-fname-head">{head}</span>
      {tail && <span className="item-fname-tail">{tail}</span>}
    </span>
  )
}

function truncate(s: string, max: number): string {
  return s.length > max ? s.slice(0, max - 1) + '…' : s
}

// retryReason strips the server's " (auto-retry n/m)" suffix — the row
// already shows the attempt number.
function retryReason(err?: string): string {
  return (err ?? '').replace(/ \(auto-retry \d+\/\d+\)$/, '')
}

interface Props {
  item: QueueItem
  selected: boolean
  dragging: boolean
  onToggleSelect: (id: string) => void
  onAction: (id: string, action: 'pause' | 'resume' | 'retry' | 'cancel') => void
  onRetryExtract: (id: string, password?: string) => void
  onSolveCaptcha: (id: string, solution: string) => void
  onSetPriority: (id: string, priority: Priority) => void
  // onEdit saves a new URL and/or filename; resolves to an error message
  // to show inline, or null on success.
  onEdit: (id: string, edit: { url?: string; filename?: string }) => Promise<string | null>
  onDragStart: (e: DragEvent, id: string) => void
  onDragOver: (e: DragEvent, id: string) => void
  onDrop: (e: DragEvent, id: string) => void
}

export function ItemRow({
  item,
  selected,
  dragging,
  onToggleSelect,
  onAction,
  onRetryExtract,
  onSolveCaptcha,
  onSetPriority,
  onEdit,
  onDragStart,
  onDragOver,
  onDrop,
}: Props) {
  const percent = item.size > 0 ? Math.min(100, (item.downloaded / item.size) * 100) : item.state === 'done' ? 100 : 0
  const [showPasswordPrompt, setShowPasswordPrompt] = useState(false)
  const [passwordDraft, setPasswordDraft] = useState('')
  const [showCaptchaModal, setShowCaptchaModal] = useState(false)
  const [captchaDraft, setCaptchaDraft] = useState('')
  const [editing, setEditing] = useState(false)
  const [urlDraft, setUrlDraft] = useState('')
  const [nameDraft, setNameDraft] = useState('')
  const [editError, setEditError] = useState<string | null>(null)
  const editable = item.state === 'queued' || item.state === 'paused' || item.state === 'error'
  // A finished archive that was never extracted (auto-extract was off):
  // offered on its first part, which the server extracts the whole set from.
  const canExtract = item.state === 'done' && !item.extract_state && isFirstArchivePart(item.filename)

  // now ticks once per second only while a countdown is actually shown,
  // so the "Waiting X:XX" text below counts down live without waiting
  // for a WebSocket update — the item's own retry_after doesn't change
  // until the wait is actually over.
  const [now, setNow] = useState(() => Date.now())
  const retryAfterMs = item.retry_after ? new Date(item.retry_after).getTime() : null
  const waitingSecondsLeft = retryAfterMs ? Math.round((retryAfterMs - now) / 1000) : 0
  const isWaiting = item.state === 'queued' && retryAfterMs != null && waitingSecondsLeft > 0
  // An automatic retry after a transient failure (queue.auto_retry) is a
  // wait, not an error: shown in the waiting color with its reason.
  const isAutoRetry = isWaiting && !!item.attempts && !!item.error
  const isCaptchaPending =item.state === 'queued' && !!item.captcha_url

  useEffect(() => {
    if (!isWaiting) return
    const iv = setInterval(() => setNow(Date.now()), 1000)
    return () => clearInterval(iv)
  }, [isWaiting])

  // A paused item that never transferred anything was added paused (e.g. an
  // import with auto-start off): it is 'ready' to start, not interrupted.
  const neverStarted = item.state === 'paused' && item.downloaded === 0
  const displayState = isCaptchaPending ? 'captcha' : isWaiting ? 'waiting' : neverStarted ? 'ready' : item.state

  function submitCaptcha() {
    if (!captchaDraft.trim()) return
    onSolveCaptcha(item.id, captchaDraft.trim())
    setShowCaptchaModal(false)
    setCaptchaDraft('')
  }

  function cancelCaptchaModal() {
    setShowCaptchaModal(false)
    setCaptchaDraft('')
  }

  function openEdit() {
    setUrlDraft(item.url)
    setNameDraft(item.filename)
    setEditError(null)
    setEditing(true)
  }

  async function submitEdit() {
    const edit: { url?: string; filename?: string } = {}
    if (urlDraft.trim() !== item.url) edit.url = urlDraft.trim()
    if (nameDraft.trim() !== item.filename) edit.filename = nameDraft.trim()
    if (edit.url === undefined && edit.filename === undefined) {
      setEditing(false)
      return
    }
    const err = await onEdit(item.id, edit)
    if (err) setEditError(err)
    else setEditing(false)
  }

  function handleReExtractClick() {
    const mentionsPassword = /password/i.test(item.extract_error || '')
    if (mentionsPassword) {
      setShowPasswordPrompt(true)
    } else {
      onRetryExtract(item.id)
    }
  }

  function submitPassword() {
    onRetryExtract(item.id, passwordDraft)
    setShowPasswordPrompt(false)
    setPasswordDraft('')
  }

  function cancelPasswordPrompt() {
    setShowPasswordPrompt(false)
    setPasswordDraft('')
  }

  return (
    <div
      className={`item-row${dragging ? ' dragging' : ''}`}
      draggable
      onDragStart={(e) => onDragStart(e, item.id)}
      onDragOver={(e) => onDragOver(e, item.id)}
      onDrop={(e) => onDrop(e, item.id)}
    >
      <div className="item-icon-col">
        <input type="checkbox" checked={selected} onChange={() => onToggleSelect(item.id)} />
        <i className="ti ti-file item-icon" />
      </div>

      <div className="item-info">
        <div className="item-fname-row">
          <FileName name={itemName(item)} title={item.url} />
          <select
            className={`priority-select prio-${item.priority}`}
            title="Priority"
            value={item.priority}
            onClick={(e) => e.stopPropagation()}
            onChange={(e) => onSetPriority(item.id, e.target.value as Priority)}
          >
            <option value="low">low</option>
            <option value="normal">normal</option>
            <option value="high">high</option>
          </select>
        </div>
        <div className="item-meta mono">
          {item.hoster_key} · {formatBytes(item.size)}
        </div>
        {item.error && !isAutoRetry && (
          <div className="item-error" title={item.error}>
            {item.error}
          </div>
        )}
        {item.extract_state === 'extract_queued' && (
          <div className="item-extract-status queued" title="One archive is extracted at a time">
            ⏳ Waiting to extract — another archive is being extracted
          </div>
        )}
        {item.extract_state === 'extracting' && <div className="item-extract-status extracting">Extracting…</div>}
        {item.extract_state === 'extracted' && <div className="item-extract-status extracted">✓ Extracted</div>}
        {item.extract_state === 'extract_failed' && (
          <div className="item-extract-status extract-failed" title={item.extract_error}>
            ✗ Extract failed: {truncate(item.extract_error || 'unknown error', 60)}
          </div>
        )}
        {isAutoRetry ? (
          <div className="item-wait-status waiting" title={item.error}>
            ⏳ Retry {item.attempts} in {formatCountdown(waitingSecondsLeft)} — {retryReason(item.error)}
          </div>
        ) : (
          isWaiting && <div className="item-wait-status waiting">⏳ Waiting {formatCountdown(waitingSecondsLeft)}</div>
        )}
        {isCaptchaPending && (
          <div className="item-wait-status captcha">
            🔒 Captcha required
            <button type="button" className="link-button" onClick={() => setShowCaptchaModal(true)}>
              Solve →
            </button>
          </div>
        )}
      </div>

      <div className="item-progress">
        <div className="progress-bar">
          <div className={`progress-fill state-${item.state}`} style={{ width: `${percent}%` }} />
        </div>
        <div className="progress-text mono">
          <span>
            {formatBytes(item.downloaded)}
            {item.size > 0 ? ` / ${formatBytes(item.size)}` : ''}
          </span>
          <span>{Math.round(percent)}%</span>
        </div>
      </div>

      <div className="item-speed">
        {item.state === 'downloading' ? (
          <span className="item-speed-value mono">{formatSpeed(item.speed)}</span>
        ) : (
          <span className={`badge state-${displayState}`}>{displayState}</span>
        )}
      </div>

      <div className="item-actions">
        {(item.state === 'downloading' || item.state === 'queued') && (
          <button className="icon-btn" title="Pause" onClick={() => onAction(item.id, 'pause')}>
            <i className="ti ti-player-pause" />
          </button>
        )}
        {item.state === 'paused' && (
          <button className="icon-btn" title={neverStarted ? 'Start' : 'Resume'} onClick={() => onAction(item.id, 'resume')}>
            <i className="ti ti-player-play" />
          </button>
        )}
        {item.state === 'error' && (
          <button className="icon-btn retry" title="Retry" onClick={() => onAction(item.id, 'retry')}>
            <i className="ti ti-refresh" />
          </button>
        )}
        {item.extract_state === 'extract_failed' &&
          (showPasswordPrompt ? (
            <span className="extract-password-popover" onClick={(e) => e.stopPropagation()}>
              <input
                {...literalText}
                type="password"
                className="extract-password-input"
                placeholder="Archive password"
                value={passwordDraft}
                autoFocus
                onChange={(e) => setPasswordDraft(e.target.value)}
                onKeyDown={(e) => {
                  if (e.key === 'Enter') {
                    e.preventDefault()
                    submitPassword()
                  } else if (e.key === 'Escape') {
                    cancelPasswordPrompt()
                  }
                }}
              />
              <button className="icon-btn retry" title="Retry extraction" onClick={submitPassword}>
                <i className="ti ti-refresh" />
              </button>
            </span>
          ) : (
            <button className="icon-btn" title="Re-extract" onClick={handleReExtractClick}>
              <i className="ti ti-unpack" />
            </button>
          ))}
        {canExtract && (
          <button className="icon-btn" title="Extract now" onClick={() => onRetryExtract(item.id)}>
            <i className="ti ti-unpack" />
          </button>
        )}
        {editable && (
          <button className="icon-btn" title="Edit link / filename" onClick={openEdit}>
            ✎
          </button>
        )}
        <button className="icon-btn danger" title="Cancel" onClick={() => onAction(item.id, 'cancel')}>
          <i className="ti ti-x" />
        </button>
      </div>

      {editing && (
        <div className="captcha-modal-overlay" onClick={() => setEditing(false)}>
          <form
            className="captcha-modal edit-modal"
            onClick={(e) => e.stopPropagation()}
            onSubmit={(e) => {
              e.preventDefault()
              void submitEdit()
            }}
          >
            <h3>Edit link</h3>
            <label>
              URL
              <input
                type="url"
                className="captcha-modal-input"
                value={urlDraft}
                autoFocus
                onChange={(e) => setUrlDraft(e.target.value)}
                onKeyDown={(e) => e.key === 'Escape' && setEditing(false)}
              />
            </label>
            <label>
              Filename
              <input
                {...literalText}
                type="text"
                className="captcha-modal-input"
                placeholder="(from hoster)"
                value={nameDraft}
                onChange={(e) => setNameDraft(e.target.value)}
                onKeyDown={(e) => e.key === 'Escape' && setEditing(false)}
              />
            </label>
            <p className="edit-modal-hint">A new URL restarts a failed download, e.g. with a working mirror.</p>
            {editError && <div className="item-error">{editError}</div>}
            <div className="captcha-modal-actions">
              <button type="button" className="btn small" onClick={() => setEditing(false)}>
                Cancel
              </button>
              <button type="submit" className="btn small primary" disabled={!urlDraft.trim()}>
                Save
              </button>
            </div>
          </form>
        </div>
      )}

      {showCaptchaModal && (
        <div className="captcha-modal-overlay" onClick={cancelCaptchaModal}>
          <div className="captcha-modal" onClick={(e) => e.stopPropagation()}>
            <h3>Solve captcha</h3>
            <p className="captcha-modal-fname" title={itemName(item)}>
              {truncate(itemName(item), 60)}
            </p>
            {item.captcha_url && (
              // eslint-disable-next-line jsx-a11y/img-redundant-alt -- descriptive alt text is the whole point here
              <img className="captcha-modal-image" src={item.captcha_url} alt="Captcha challenge" />
            )}
            <input
              {...literalText}
              type="text"
              className="captcha-modal-input"
              placeholder="Enter the text shown above"
              value={captchaDraft}
              autoFocus
              onChange={(e) => setCaptchaDraft(e.target.value)}
              onKeyDown={(e) => {
                if (e.key === 'Enter') {
                  e.preventDefault()
                  submitCaptcha()
                } else if (e.key === 'Escape') {
                  cancelCaptchaModal()
                }
              }}
            />
            <div className="captcha-modal-actions">
              <button type="button" className="btn small" onClick={cancelCaptchaModal}>
                Cancel
              </button>
              <button type="button" className="btn small primary" onClick={submitCaptcha} disabled={!captchaDraft.trim()}>
                Submit
              </button>
            </div>
          </div>
        </div>
      )}
    </div>
  )
}
