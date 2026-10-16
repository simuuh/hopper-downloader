import type { ReactNode } from 'react'

// PageHeader is the common top of every page: title, a one-line
// description, and page-level actions on the right.
export function PageHeader({
  title,
  subtitle,
  actions,
  sticky,
}: {
  title: string
  subtitle?: ReactNode
  actions?: ReactNode
  sticky?: boolean
}) {
  return (
    <header className={`page-header${sticky ? ' sticky' : ''}`}>
      <div className="page-title">
        <h1>{title}</h1>
        {subtitle && <p className="page-subtitle">{subtitle}</p>}
      </div>
      {actions && <div className="page-actions">{actions}</div>}
    </header>
  )
}

// PageState renders a page's loading or load-error state with the same
// header the loaded page uses, so switching tabs doesn't make the layout
// jump.
export function PageState({ title, error }: { title: string; error?: string | null }) {
  return (
    <>
      <PageHeader title={title} />
      <div className="panel">
        {error ? <p className="error-text">{error}</p> : <p className="empty-state">Loading…</p>}
      </div>
    </>
  )
}
