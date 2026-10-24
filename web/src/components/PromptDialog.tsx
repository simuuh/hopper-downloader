import { useState } from 'react'
import { literalText } from '../lib/inputProps'

interface Props {
  title: string
  label: string
  initialValue: string
  confirmLabel: string
  onConfirm: (value: string) => void
  onCancel: () => void
}

// PromptDialog asks for one line of text, in the app's own modal style
// instead of the browser's window.prompt. Enter confirms, Escape or a
// click on the backdrop cancels; the value is passed on trimmed.
export function PromptDialog({ title, label, initialValue, confirmLabel, onConfirm, onCancel }: Props) {
  const [value, setValue] = useState(initialValue)
  const trimmed = value.trim()

  return (
    <div className="captcha-modal-overlay" onClick={onCancel}>
      <form
        className="captcha-modal confirm-modal"
        aria-label={title}
        onClick={(e) => e.stopPropagation()}
        onSubmit={(e) => {
          e.preventDefault()
          if (trimmed) onConfirm(trimmed)
        }}
      >
        <h3>{title}</h3>
        <label className="confirm-modal-body">
          {label}
          <input
            {...literalText}
            type="text"
            className="captcha-modal-input"
            value={value}
            autoFocus
            onFocus={(e) => e.target.select()}
            onChange={(e) => setValue(e.target.value)}
            onKeyDown={(e) => e.key === 'Escape' && onCancel()}
          />
        </label>
        <div className="captcha-modal-actions">
          <button type="button" className="btn small" onClick={onCancel}>
            Cancel
          </button>
          <button type="submit" className="btn small primary" disabled={!trimmed}>
            {confirmLabel}
          </button>
        </div>
      </form>
    </div>
  )
}
