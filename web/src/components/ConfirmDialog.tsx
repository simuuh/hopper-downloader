import { useEffect, type ReactNode } from 'react'

interface Props {
  title: string
  children: ReactNode
  confirmLabel: string
  onConfirm: () => void
  onCancel: () => void
}

// ConfirmDialog asks before a destructive action, in the app's own modal
// style instead of the browser's window.confirm. Escape or a click on the
// backdrop cancels; the confirm button has focus, so Enter confirms.
export function ConfirmDialog({ title, children, confirmLabel, onConfirm, onCancel }: Props) {
  useEffect(() => {
    function onKey(e: KeyboardEvent) {
      if (e.key === 'Escape') onCancel()
    }
    window.addEventListener('keydown', onKey)
    return () => window.removeEventListener('keydown', onKey)
  }, [onCancel])

  return (
    <div className="captcha-modal-overlay" onClick={onCancel}>
      <div className="captcha-modal confirm-modal" role="alertdialog" aria-modal="true" aria-label={title} onClick={(e) => e.stopPropagation()}>
        <h3>{title}</h3>
        <div className="confirm-modal-body">{children}</div>
        <div className="captcha-modal-actions">
          <button type="button" className="btn small" onClick={onCancel}>
            Cancel
          </button>
          <button type="button" className="btn small danger" autoFocus onClick={onConfirm}>
            {confirmLabel}
          </button>
        </div>
      </div>
    </div>
  )
}
