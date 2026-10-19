import { useEffect, useState } from 'react'
import { PageHeader, PageState } from '../components/Page'
import { literalText } from '../lib/inputProps'
import { api, ApiError } from '../lib/api'
import { formatVersion } from '../lib/format'
import {
  notificationsEnabled,
  notificationsSupported,
  requestPermission,
  setNotificationsEnabled,
} from '../lib/notifications'
import { getThemePreference, setThemePreference, THEME_EVENT, THEME_LABELS, type ThemePreference } from '../lib/theme'
import type { Priority, Settings, UpdateStatus } from '../lib/types'

const MB = 1024 * 1024

export function SettingsPage({
  updateStatus,
  onUpdateStatus,
  onAuthError,
}: {
  updateStatus: UpdateStatus | null
  onUpdateStatus: (s: UpdateStatus) => void
  onAuthError: () => void
}) {
  const [settings, setSettings] = useState<Settings | null>(null)
  const [saved, setSaved] = useState<Settings | null>(null)
  const [loadError, setLoadError] = useState<string | null>(null)
  const [saveError, setSaveError] = useState<string | null>(null)
  const [justSaved, setJustSaved] = useState(false)
  const [busy, setBusy] = useState(false)
  const [newPassword, setNewPassword] = useState('')
  const [theme, setTheme] = useState<ThemePreference>(getThemePreference)
  const [notifyOn, setNotifyOn] = useState(notificationsEnabled)
  useEffect(() => {
    const onChange = (e: Event) => setTheme((e as CustomEvent<ThemePreference>).detail)
    window.addEventListener(THEME_EVENT, onChange)
    return () => window.removeEventListener(THEME_EVENT, onChange)
  }, [])
  const [notifyPermission, setNotifyPermission] = useState<NotificationPermission>(() =>
    notificationsSupported() ? Notification.permission : 'denied',
  )
  // Server-side push (webhook/ntfy, configured in hopper.yaml): only
  // whether it's configured is exposed, never the URLs/tokens.
  const [pushEnabled, setPushEnabled] = useState<boolean | null>(null)
  const [pushTest, setPushTest] = useState<{ ok: boolean; text: string } | null>(null)
  const [checking, setChecking] = useState(false)
  const [checkError, setCheckError] = useState<string | null>(null)

  useEffect(() => {
    api
      .getSettings()
      .then((s) => {
        setSettings(s)
        setSaved(s)
      })
      .catch((err) => {
        if (err instanceof ApiError && err.status === 401) onAuthError()
        setLoadError(err instanceof Error ? err.message : 'failed to load settings')
      })
    api
      .getNotifyStatus()
      .then((res) => setPushEnabled(res.enabled))
      .catch(() => setPushEnabled(null))
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [])

  if (loadError || !settings) return <PageState title="Settings" error={loadError} />

  const dirty = JSON.stringify(settings) !== JSON.stringify(saved)

  function update<K extends keyof Settings>(key: K, value: Settings[K]) {
    setSettings((prev) => (prev ? { ...prev, [key]: value } : prev))
    setJustSaved(false)
  }

  async function handleSave() {
    if (!settings) return
    setBusy(true)
    setSaveError(null)
    try {
      const updated = await api.updateSettings(settings)
      const turnedOnUpdates = updated.update_check && !saved?.update_check
      setSettings(updated)
      setSaved(updated)
      setJustSaved(true)
      // Show a result right away instead of after the next hourly tick.
      if (turnedOnUpdates) void checkForUpdate()
    } catch (err) {
      if (err instanceof ApiError && err.status === 401) onAuthError()
      setSaveError(err instanceof Error ? err.message : 'failed to save settings')
    } finally {
      setBusy(false)
    }
  }

  function addPassword() {
    const p = newPassword.trim()
    if (!p || !settings) return
    if (!settings.extract_passwords.includes(p)) update('extract_passwords', [...settings.extract_passwords, p])
    setNewPassword('')
  }

  async function toggleNotifications(on: boolean) {
    if (on) {
      const permission = await requestPermission()
      setNotifyPermission(permission)
      if (permission !== 'granted') return
    }
    setNotificationsEnabled(on)
    setNotifyOn(on)
  }

  async function sendTestPush() {
    setPushTest(null)
    try {
      await api.testNotify()
      setPushTest({ ok: true, text: 'Test notification sent' })
    } catch (err) {
      if (err instanceof ApiError && err.status === 401) onAuthError()
      setPushTest({ ok: false, text: err instanceof Error ? err.message : 'failed to send' })
    }
  }

  async function checkForUpdate() {
    setChecking(true)
    setCheckError(null)
    try {
      onUpdateStatus(await api.checkForUpdate())
    } catch (err) {
      if (err instanceof ApiError && err.status === 401) onAuthError()
      setCheckError(err instanceof Error ? err.message : 'update check failed')
    } finally {
      setChecking(false)
    }
  }

  // A "dev" build can't be compared with a release, so it is never
  // reported as up to date either.
  const comparable = updateStatus ? /^v?\d/.test(updateStatus.current) : false
  const updateError = checkError ?? updateStatus?.error ?? null

  // The backend rejects an update atomically with one error string;
  // download_path problems are shown at the field since they're the
  // actionable case ("doesn't exist" / "not writable").
  const downloadPathError = saveError?.toLowerCase().includes('download_path') ? saveError : null
  const pathEmpty = settings.download_path.trim() === ''

  return (
    <>
      <PageHeader
        title="Settings"
        subtitle="Changes apply immediately after saving — no restart needed."
        sticky
        actions={
          <>
            {dirty && !busy && <span className="hint">Unsaved changes</span>}
            {justSaved && !dirty && <span className="ok-text">Saved</span>}
            {saveError && !downloadPathError && <span className="error-text">{saveError}</span>}
            <button type="button" className="btn small" disabled={!dirty || busy} onClick={() => setSettings(saved)}>
              Discard
            </button>
            <button
              type="button"
              className="btn small primary"
              disabled={!dirty || busy || pathEmpty}
              onClick={() => void handleSave()}
            >
              {busy ? 'Saving…' : 'Save'}
            </button>
          </>
        }
      />

      <div className="panel">
        <h2>Downloads</h2>
        <div className="settings-grid">
          <label>
            Download folder
            <input
              {...literalText}
              type="text"
              value={settings.download_path}
              onChange={(e) => update('download_path', e.target.value)}
            />
            <span className="hint">Must exist and be writable. In Docker: /downloads or a subfolder.</span>
            {pathEmpty && <span className="error-text">Must not be empty</span>}
            {downloadPathError && <span className="error-text">{downloadPathError}</span>}
          </label>
          <label>
            Bandwidth limit (MB/s)
            <input
              type="number"
              min={0}
              step={0.5}
              value={settings.bandwidth_limit > 0 ? +(settings.bandwidth_limit / MB).toFixed(2) : 0}
              onChange={(e) => update('bandwidth_limit', Math.max(0, Math.round(Number(e.target.value) * MB)))}
            />
            <span className="hint">0 = unlimited. Also applies to downloads already running.</span>
          </label>
          <label>
            Parallel files
            <input
              type="number"
              min={1}
              max={10}
              value={settings.parallel_files}
              onChange={(e) => update('parallel_files', Number(e.target.value))}
            />
          </label>
          <label>
            Connections per file
            <input
              type="number"
              min={1}
              max={16}
              value={settings.parallel_chunks}
              onChange={(e) => update('parallel_chunks', Number(e.target.value))}
            />
            <span className="hint">Files under 4 MiB per connection use fewer.</span>
          </label>
          <label>
            Retries per connection
            <input
              type="number"
              min={0}
              max={10}
              value={settings.retry_max}
              onChange={(e) => update('retry_max', Number(e.target.value))}
            />
          </label>
          <label>
            Default priority
            <select
              value={settings.default_priority}
              onChange={(e) => update('default_priority', e.target.value as Priority)}
            >
              <option value="low">low</option>
              <option value="normal">normal</option>
              <option value="high">high</option>
            </select>
          </label>
        </div>
        <label className="settings-checkbox">
          <input
            type="checkbox"
            checked={settings.auto_start_imports}
            onChange={(e) => update('auto_start_imports', e.target.checked)}
          />
          Start imported .dlc/.txt links immediately instead of adding them paused (Click&apos;n&apos;Load links always arrive paused)
        </label>
      </div>

      <div className="panel">
        <h2>Auto-extract</h2>
        <label className="settings-checkbox">
          <input
            type="checkbox"
            checked={settings.extract_enabled}
            onChange={(e) => update('extract_enabled', e.target.checked)}
          />
          Extract archives (.rar incl. multi-part, .zip, .tar.gz, .tar.bz2) after download
        </label>
        <label className="settings-checkbox">
          <input
            type="checkbox"
            checked={settings.extract_delete_after}
            onChange={(e) => update('extract_delete_after', e.target.checked)}
            disabled={!settings.extract_enabled}
          />
          Delete the archive after successful extraction
        </label>

        <h3>Archive passwords</h3>
        <p className="hint">Tried in order, after the package&apos;s own password.</p>
        <div className="row">
          <input
            {...literalText}
            type="text"
            placeholder="Add a password"
            value={newPassword}
            onChange={(e) => setNewPassword(e.target.value)}
            onKeyDown={(e) => {
              if (e.key === 'Enter') {
                e.preventDefault()
                addPassword()
              }
            }}
          />
          <button type="button" className="btn small" onClick={addPassword} disabled={newPassword.trim() === ''}>
            Add
          </button>
        </div>
        {settings.extract_passwords.length > 0 && (
          <div className="password-chips">
            {settings.extract_passwords.map((p) => (
              <span className="password-chip mono" key={p}>
                {p}
                <button
                  type="button"
                  title="Remove"
                  onClick={() =>
                    update(
                      'extract_passwords',
                      settings.extract_passwords.filter((x) => x !== p),
                    )
                  }
                >
                  <i className="ti ti-x" />
                </button>
              </span>
            ))}
          </div>
        )}
      </div>

      <div className="panel">
        <h2>Logging</h2>
        <label>
          Log level
          <select
            value={settings.log_level}
            onChange={(e) => update('log_level', e.target.value as Settings['log_level'])}
          >
            <option value="debug">debug</option>
            <option value="info">info</option>
            <option value="warn">warn</option>
            <option value="error">error</option>
          </select>
        </label>
      </div>

      <div className="panel">
        <h2>Push notifications</h2>
        <p className="hint">
          Sent by the server when a package is finished (downloaded, and extracted if applicable) — also with no
          browser open. Set <code>HOPPER_NOTIFY_WEBHOOK_URL</code> and/or <code>HOPPER_NOTIFY_NTFY_URL</code> (or{' '}
          <code>notify.*</code> in hopper.yaml).
        </p>
        {pushEnabled === false && <p className="hint">Not configured.</p>}
        {pushEnabled && (
          <div className="row">
            <button type="button" className="btn small" onClick={() => void sendTestPush()}>
              Send test notification
            </button>
            {pushTest && <span className={pushTest.ok ? 'ok-text' : 'error-text'}>{pushTest.text}</span>}
          </div>
        )}
      </div>

      <div className="panel">
        <h2>Updates</h2>
        <p className="hint">
          Installed <span className="mono">{updateStatus ? formatVersion(updateStatus.current) : '…'}</span>
          {updateStatus?.latest && (
            <>
              {' · '}latest release{' '}
              <a href={updateStatus.release_url} target="_blank" rel="noopener noreferrer" className="mono">
                {formatVersion(updateStatus.latest)}
              </a>
            </>
          )}
          {updateStatus?.checked_at && ` · checked ${new Date(updateStatus.checked_at).toLocaleString()}`}
        </p>
        {updateStatus?.available && updateStatus.latest && (
          <p>
            <a href={updateStatus.release_url} target="_blank" rel="noopener noreferrer">
              Hopper {formatVersion(updateStatus.latest)} is available — release notes
            </a>
          </p>
        )}
        {updateStatus && !updateStatus.available && updateStatus.latest && comparable && (
          <p className="ok-text">Up to date.</p>
        )}
        <label className="settings-checkbox">
          <input
            type="checkbox"
            checked={settings.update_check}
            onChange={(e) => update('update_check', e.target.checked)}
          />
          Check for a new release once a day
        </label>
        <p className="hint">
          Asks api.github.com for the latest release. GitHub sees this server&apos;s IP address; nothing about the
          installation is sent. &quot;Check now&quot; works with this off.
        </p>
        <div className="row">
          <button type="button" className="btn small" disabled={checking} onClick={() => void checkForUpdate()}>
            {checking ? 'Checking…' : 'Check now'}
          </button>
          {updateError && <span className="error-text">{updateError}</span>}
        </div>
      </div>

      <div className="panel">
        <h2>This browser</h2>
        <p className="hint">Stored only in this browser — not saved on the server, not synced.</p>
        <div className="settings-grid">
          <label>
            Theme
            <select
              value={theme}
              onChange={(e) => {
                const next = e.target.value as ThemePreference
                setThemePreference(next)
                setTheme(next)
              }}
            >
              {(Object.keys(THEME_LABELS) as ThemePreference[]).map((k) => (
                <option key={k} value={k}>
                  {THEME_LABELS[k]}
                </option>
              ))}
            </select>
          </label>
        </div>
        {!notificationsSupported() ? (
          <p className="hint">Desktop notifications aren&apos;t supported in this browser.</p>
        ) : (
          <label className="settings-checkbox">
            <input
              type="checkbox"
              checked={notifyOn && notifyPermission === 'granted'}
              disabled={notifyPermission === 'denied'}
              onChange={(e) => void toggleNotifications(e.target.checked)}
            />
            Desktop notifications when a file or package finishes, or a download fails
            {notifyPermission === 'denied' && <span className="error-text"> — blocked in the browser settings</span>}
          </label>
        )}
      </div>
    </>
  )
}
