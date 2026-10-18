import { useEffect, useMemo, useState, type FormEvent } from 'react'
import { PageHeader, PageState } from '../components/Page'
import { literalText } from '../lib/inputProps'
import { api, ApiError } from '../lib/api'
import { formatBytes } from '../lib/format'
import type { Account, CredentialField, PluginInfo } from '../lib/types'

// Accounts expiring within this many days are highlighted and get a
// prominent "Extend" link.
const EXPIRY_WARN_DAYS = 7
const DAY_MS = 86_400_000

const FALLBACK_FIELDS: CredentialField[] = [
  { name: 'username', label: 'Username', secret: false, required: true },
  { name: 'password', label: 'Password', secret: true, required: true },
]

function daysLeft(acc: Account): number | null {
  if (!acc.expires_at) return null
  return Math.floor((new Date(acc.expires_at).getTime() - Date.now()) / DAY_MS)
}

function relativeTime(iso?: string): string {
  if (!iso) return 'never'
  const mins = Math.round((Date.now() - new Date(iso).getTime()) / 60_000)
  if (mins < 1) return 'just now'
  if (mins < 60) return `${mins} min ago`
  const hours = Math.round(mins / 60)
  if (hours < 48) return `${hours} h ago`
  return `${Math.round(hours / 24)} d ago`
}

type Health = 'ok' | 'expiring' | 'expired' | 'invalid' | 'unknown'

function health(acc: Account): Health {
  if (acc.valid === false) return 'invalid'
  const days = daysLeft(acc)
  if (days !== null && days < 0) return 'expired'
  if (days !== null && days <= EXPIRY_WARN_DAYS) return 'expiring'
  if (acc.valid === true) return 'ok'
  return 'unknown'
}

const HEALTH_BADGE: Record<Health, { label: string; cls: string }> = {
  ok: { label: 'valid', cls: 'state-done' },
  expiring: { label: 'expires soon', cls: 'state-paused' },
  expired: { label: 'expired', cls: 'state-error' },
  invalid: { label: 'invalid', cls: 'state-error' },
  unknown: { label: 'unchecked', cls: 'state-queued' },
}

function PremiumLink({ plugin, label }: { plugin?: PluginInfo; label: string }) {
  if (!plugin?.premium_url) return null
  return (
    <a
      className="premium-link"
      href={plugin.premium_url}
      target="_blank"
      rel="noopener noreferrer"
      title={
        plugin.is_referral
          ? 'Referral link — buying through it supports Hopper at no extra cost to you (accounts.referral_links)'
          : `${plugin.name} premium page`
      }
    >
      {label}
      {plugin.is_referral ? ' ♥' : ''} ↗
    </a>
  )
}

function CredentialInputs({
  fields,
  values,
  onChange,
  keepHint,
}: {
  fields: CredentialField[]
  values: Record<string, string>
  onChange: (name: string, value: string) => void
  keepHint?: boolean
}) {
  return (
    <>
      {fields.map((f) => (
        <label key={f.name} className="cred-field">
          {f.label}
          {!f.required && <span className="hint"> (optional)</span>}
          <input
            {...literalText}
            type={f.secret ? 'password' : 'text'}
            autoComplete="off"
            placeholder={keepHint && f.secret ? 'leave empty to keep' : f.placeholder}
            value={values[f.name] ?? ''}
            onChange={(e) => onChange(f.name, e.target.value)}
          />
          {f.help && <span className="hint">{f.help}</span>}
        </label>
      ))}
    </>
  )
}

export function AccountsPage({ onAuthError }: { onAuthError: () => void }) {
  const [accounts, setAccounts] = useState<Account[] | null>(null)
  const [plugins, setPlugins] = useState<PluginInfo[]>([])
  const [loadError, setLoadError] = useState<string | null>(null)
  const [showAdd, setShowAdd] = useState(false)
  const [hosterKey, setHosterKey] = useState('')
  const [values, setValues] = useState<Record<string, string>>({})
  const [busy, setBusy] = useState(false)
  const [formError, setFormError] = useState<string | null>(null)
  const [message, setMessage] = useState<{ ok: boolean; text: string } | null>(null)
  const [workingId, setWorkingId] = useState<string | null>(null)
  const [editingId, setEditingId] = useState<string | null>(null)
  const [editValues, setEditValues] = useState<Record<string, string>>({})

  const pluginByKey = useMemo(() => new Map(plugins.map((p) => [p.key, p])), [plugins])
  const selected = pluginByKey.get(hosterKey)
  const fields = selected?.fields?.length ? selected.fields : FALLBACK_FIELDS

  function handleError(err: unknown, fallback: string) {
    if (err instanceof ApiError && err.status === 401) onAuthError()
    setMessage({ ok: false, text: err instanceof Error ? err.message : fallback })
  }

  async function refresh() {
    try {
      const [accs, plugs] = await Promise.all([api.listAccounts(), api.listPlugins()])
      setAccounts(accs.sort((a, b) => a.hoster_key.localeCompare(b.hoster_key)))
      setPlugins(plugs)
      setHosterKey((cur) => cur || plugs[0]?.key || '')
      setLoadError(null)
    } catch (err) {
      if (err instanceof ApiError && err.status === 401) onAuthError()
      setLoadError(err instanceof Error ? err.message : 'failed to load accounts')
    }
  }

  useEffect(() => {
    void refresh()
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [])

  if (loadError || !accounts) return <PageState title="Accounts" error={loadError} />

  const missingRequired = fields.some((f) => f.required && !(values[f.name] ?? '').trim())

  async function handleAdd(e: FormEvent) {
    e.preventDefault()
    setBusy(true)
    setFormError(null)
    setMessage(null)
    try {
      const acc = await api.addAccount({
        hoster_key: hosterKey,
        username: (values.username ?? '').trim(),
        password: values.password ?? '',
      })
      const name = pluginByKey.get(acc.hoster_key)?.name ?? acc.hoster_key
      setMessage(
        acc.valid === false
          ? { ok: false, text: `${name} account added, but the hoster rejected it: ${acc.label || 'invalid credentials'}` }
          : { ok: true, text: `${name} account added${acc.label ? ` — ${acc.label}` : ''}` },
      )
      setValues({})
      setShowAdd(false)
      await refresh()
    } catch (err) {
      if (err instanceof ApiError && err.status === 401) onAuthError()
      setFormError(err instanceof Error ? err.message : 'failed to add account')
    } finally {
      setBusy(false)
    }
  }

  async function act(acc: Account, fn: () => Promise<unknown>, fallback: string) {
    setWorkingId(acc.id)
    setMessage(null)
    try {
      await fn()
      await refresh()
    } catch (err) {
      handleError(err, fallback)
    } finally {
      setWorkingId(null)
    }
  }

  function startEdit(acc: Account) {
    setEditingId(acc.id)
    setEditValues({ username: acc.username })
  }

  async function saveEdit(acc: Account) {
    const username = (editValues.username ?? '').trim()
    await act(
      acc,
      () =>
        api.updateAccount(acc.id, {
          username: username !== acc.username ? username : undefined,
          password: editValues.password || undefined,
        }),
      'failed to update account',
    )
    setEditingId(null)
    setEditValues({})
  }

  function remove(acc: Account) {
    const name = pluginByKey.get(acc.hoster_key)?.name ?? acc.hoster_key
    if (!window.confirm(`Delete the ${name} account${acc.username ? ` "${acc.username}"` : ''}?`)) return
    void act(acc, () => api.deleteAccount(acc.id), 'failed to delete account')
  }

  // Plugins with an enabled account, in priority order (GET /plugins is
  // sorted by rank) — only their relative order matters.
  const configured = plugins.filter((p) => accounts.some((a) => a.hoster_key === p.key && a.enabled))

  async function savePriority(order: string[]) {
    try {
      await api.updateSettings({ account_priority: order })
      await refresh()
    } catch (err) {
      handleError(err, 'failed to save the priority')
    }
  }

  function movePriority(index: number, delta: number) {
    const keys = configured.map((p) => p.key)
    const target = index + delta
    if (target < 0 || target >= keys.length) return
    ;[keys[index], keys[target]] = [keys[target], keys[index]]
    const rest = plugins.map((p) => p.key).filter((k) => !keys.includes(k))
    void savePriority([...keys, ...rest])
  }

  const attention = accounts.filter((a) => ['expiring', 'expired', 'invalid'].includes(health(a))).length

  return (
    <>
      <PageHeader
        title="Accounts"
        subtitle="Premium and debrid accounts. Credentials are stored encrypted; status is re-checked every 12 hours."
        actions={
          <button type="button" className="btn small primary" onClick={() => setShowAdd((v) => !v)}>
            {showAdd ? 'Close' : '+ Add account'}
          </button>
        }
      />

      {message && <div className={`validation-result ${message.ok ? 'ok' : 'bad'} page-message`}>{message.text}</div>}

      {showAdd && (
        <form className="panel" onSubmit={handleAdd}>
          <h2>Add account</h2>
          <div className="account-form">
            <label className="cred-field">
              Hoster
              <select
                value={hosterKey}
                onChange={(e) => {
                  setHosterKey(e.target.value)
                  setValues({})
                  setFormError(null)
                }}
              >
                {plugins.map((p) => (
                  <option key={p.key} value={p.key}>
                    {p.name}
                  </option>
                ))}
              </select>
            </label>
            <CredentialInputs fields={fields} values={values} onChange={(n, v) => setValues((s) => ({ ...s, [n]: v }))} />
          </div>
          <div className="row form-actions">
            <button className="btn primary" type="submit" disabled={busy || !hosterKey || missingRequired}>
              {busy ? 'Checking…' : 'Add account'}
            </button>
            {formError && <span className="error-text">{formError}</span>}
            <span className="spacer" />
            {selected?.premium_url && (
              <span className="hint">
                No {selected.name} premium yet? <PremiumLink plugin={selected} label="Get premium" />
              </span>
            )}
          </div>
        </form>
      )}

      <div className="panel">
        <h2>
          Your accounts ({accounts.length}){attention > 0 && <span className="attention"> · {attention} need attention</span>}
        </h2>
        {accounts.length === 0 ? (
          <div className="empty-state">
            No accounts yet. Without one, links from premium hosters fall back to plain HTTP (usually an info
            page, not the file).{' '}
            <button type="button" className="link-button" onClick={() => setShowAdd(true)}>
              Add an account →
            </button>
          </div>
        ) : (
          <div className="table-scroll">
            <table className="accounts-table">
              <thead>
                <tr>
                  <th>Hoster</th>
                  <th>Account</th>
                  <th>Status</th>
                  <th>Expires</th>
                  <th>Traffic left</th>
                  <th>Checked</th>
                  <th>Active</th>
                  <th />
                </tr>
              </thead>
              <tbody>
                {accounts.map((acc) => {
                  const plugin = pluginByKey.get(acc.hoster_key)
                  const h = health(acc)
                  const days = daysLeft(acc)
                  const working = workingId === acc.id
                  const editFields = plugin?.fields?.length ? plugin.fields : FALLBACK_FIELDS
                  return (
                    <tr key={acc.id} className={acc.enabled ? '' : 'disabled-row'}>
                      <td>
                        <span className="account-hoster">{plugin?.name ?? acc.hoster_key}</span>
                      </td>
                      <td className="mono">{acc.username || '–'}</td>
                      <td>
                        <span className={`badge ${HEALTH_BADGE[h].cls}`} title={acc.label || undefined}>
                          {HEALTH_BADGE[h].label}
                        </span>
                        {acc.label && <div className="hint account-label">{acc.label}</div>}
                        {acc.cooldown_until && (
                          <div
                            className="account-cooldown"
                            title="The hoster rejected this account during a download (expired or out of traffic). Other accounts of this hoster are used meanwhile; Re-check ends the pause early."
                          >
                            paused until{' '}
                            {new Date(acc.cooldown_until).toLocaleTimeString([], { hour: '2-digit', minute: '2-digit' })}
                          </div>
                        )}
                      </td>
                      <td className={`mono ${h === 'expired' ? 'account-expired' : h === 'expiring' ? 'account-expiring' : ''}`}>
                        {acc.expires_at ? (
                          <>
                            {new Date(acc.expires_at).toLocaleDateString()}
                            <div className="hint">{days !== null && (days < 0 ? 'expired' : `in ${days} d`)}</div>
                          </>
                        ) : (
                          '–'
                        )}
                      </td>
                      <td className="mono">{acc.traffic_left != null ? formatBytes(acc.traffic_left) : '–'}</td>
                      <td className="hint" title={acc.validated_at ? new Date(acc.validated_at).toLocaleString() : undefined}>
                        {relativeTime(acc.validated_at)}
                      </td>
                      <td>
                        <label className="switch" title={acc.enabled ? 'Used for downloads' : 'Not used'}>
                          <input
                            type="checkbox"
                            checked={acc.enabled}
                            disabled={working}
                            onChange={() =>
                              void act(acc, () => api.setAccountEnabled(acc.id, !acc.enabled), 'failed to update account')
                            }
                          />
                          <span />
                        </label>
                      </td>
                      <td className="account-actions">
                        {editingId === acc.id ? (
                          <span className="account-edit">
                            <CredentialInputs
                              fields={editFields}
                              values={editValues}
                              keepHint
                              onChange={(n, v) => setEditValues((s) => ({ ...s, [n]: v }))}
                            />
                            <span className="row">
                              <button className="btn small primary" disabled={working} onClick={() => void saveEdit(acc)}>
                                Save
                              </button>
                              <button className="btn small" onClick={() => setEditingId(null)}>
                                Cancel
                              </button>
                            </span>
                          </span>
                        ) : (
                          <>
                            {(h === 'expiring' || h === 'expired' || h === 'invalid') && (
                              <PremiumLink plugin={plugin} label="Extend" />
                            )}
                            <button
                              className="btn small"
                              disabled={working}
                              onClick={() => void act(acc, () => api.validateAccount(acc.id), 'failed to check account')}
                            >
                              {working ? 'Checking…' : 'Re-check'}
                            </button>
                            <button
                              className="btn small"
                              title="Replace credentials, e.g. a new API token after renewing"
                              onClick={() => startEdit(acc)}
                            >
                              Edit
                            </button>
                            <button className="btn small danger" disabled={working} onClick={() => remove(acc)}>
                              Delete
                            </button>
                            {h === 'ok' && <PremiumLink plugin={plugin} label="Extend" />}
                          </>
                        )}
                      </td>
                    </tr>
                  )
                })}
              </tbody>
            </table>
          </div>
        )}
      </div>

      {configured.length > 1 && (
        <div className="panel">
          <h2>Priority</h2>
          <p className="hint">
            When several of your accounts can download a link, the topmost is used. If it can&apos;t serve the link
            (host not supported or down, limit reached), the next one is tried.
          </p>
          <ol className="priority-list">
            {configured.map((p, i) => (
              <li key={p.key}>
                <span className="priority-rank mono">{i + 1}</span>
                <span className="priority-name">{p.name}</span>
                <button
                  type="button"
                  className="icon-btn"
                  title="Move up"
                  disabled={i === 0}
                  onClick={() => movePriority(i, -1)}
                >
                  ↑
                </button>
                <button
                  type="button"
                  className="icon-btn"
                  title="Move down"
                  disabled={i === configured.length - 1}
                  onClick={() => movePriority(i, 1)}
                >
                  ↓
                </button>
              </li>
            ))}
          </ol>
          <button type="button" className="link-button" onClick={() => void savePriority([])}>
            Reset to default order
          </button>
        </div>
      )}

      {plugins.length > 0 && (
        <div className="panel">
          <h2>Supported hosters</h2>
          <div className="hoster-list">
            {plugins.map((p) => {
              const has = accounts.some((a) => a.hoster_key === p.key)
              return (
                <div className="hoster-chip" key={p.key}>
                  <span className={`dot ${has ? 'on' : ''}`} />
                  <span className="hoster-chip-name">{p.name}</span>
                  {!has && (
                    <button
                      type="button"
                      className="link-button"
                      onClick={() => {
                        setHosterKey(p.key)
                        setValues({})
                        setShowAdd(true)
                      }}
                    >
                      add
                    </button>
                  )}
                  <PremiumLink plugin={p} label="premium" />
                </div>
              )
            })}
          </div>
          {plugins.some((p) => p.is_referral) && (
            <p className="hint">♥ = referral link: buying through it supports Hopper&apos;s development at no extra cost.</p>
          )}
        </div>
      )}
    </>
  )
}
