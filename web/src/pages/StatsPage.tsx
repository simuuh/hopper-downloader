import { useEffect, useState } from 'react'
import { PageHeader, PageState } from '../components/Page'
import { api, ApiError } from '../lib/api'
import { formatBytes, formatSpeed } from '../lib/format'
import type { PluginInfo, Stats } from '../lib/types'

type PeriodKey = 'session' | 'today' | 'week' | 'month' | 'year' | 'alltime'

const PERIOD_TABS: { key: PeriodKey; label: string }[] = [
  { key: 'session', label: 'Session' },
  { key: 'today', label: 'Today' },
  { key: 'week', label: 'Week' },
  { key: 'month', label: 'Month' },
  { key: 'year', label: 'Year' },
  { key: 'alltime', label: 'All' },
]

// PeriodData is the shape the overview row and the "by hoster" breakdown
// both need — common to SessionStats/DailyStats/AggregateStats/
// AllTimeStats regardless of which one actually backs a given period.
// peak_speed only exists on Session/AllTime (see internal/stats' Go
// types — a day/week/month/year bucket has no per-day peak to sum or
// max over), so it's optional here.
interface PeriodData {
  bytes_total: number
  files_total: number
  by_hoster: Record<string, number>
  peak_speed?: number
}

function periodData(stats: Stats, key: PeriodKey): PeriodData {
  switch (key) {
    case 'session':
      return stats.session
    case 'today':
      return stats.today
    case 'week':
      return stats.week
    case 'month':
      return stats.month
    case 'year':
      return stats.year
    case 'alltime':
      return stats.alltime
  }
}

function formatCount(n: number): string {
  if (n < 1000) return String(n)
  const v = n / 1000
  return `${v.toFixed(v < 10 ? 1 : 0)}k`
}

export function StatsPage({ onAuthError }: { onAuthError: () => void }) {
  const [stats, setStats] = useState<Stats | null>(null)
  const [plugins, setPlugins] = useState<PluginInfo[]>([])
  const [loadError, setLoadError] = useState<string | null>(null)
  const [loading, setLoading] = useState(false)
  const [period, setPeriod] = useState<PeriodKey>('month')

  async function load() {
    setLoading(true)
    setLoadError(null)
    try {
      const [s, plugs] = await Promise.all([api.getStats(), api.listPlugins()])
      setStats(s)
      setPlugins(plugs)
    } catch (err) {
      if (err instanceof ApiError && err.status === 401) onAuthError()
      setLoadError(err instanceof Error ? err.message : 'failed to load stats')
    } finally {
      setLoading(false)
    }
  }

  useEffect(() => {
    void load()
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [])

  if (loadError || !stats) return <PageState title="Statistics" error={loadError} />

  const displayName = (key: string): string => plugins.find((p) => p.key === key)?.name || key || '(unknown)'

  const overview: { key: PeriodKey; label: string }[] = [
    { key: 'session', label: 'Session' },
    { key: 'today', label: 'Today' },
    { key: 'week', label: 'Week' },
    { key: 'month', label: 'Month' },
    { key: 'alltime', label: 'Total' },
  ]

  const selected = periodData(stats, period)
  const hosterRows = Object.entries(selected.by_hoster)
    .filter(([, bytes]) => bytes > 0)
    .sort((a, b) => b[1] - a[1])
  const hosterTotal = selected.bytes_total || hosterRows.reduce((sum, [, bytes]) => sum + bytes, 0)

  const daily = stats.month.daily // always the last 30 days
  const maxDailyBytes = Math.max(1, ...daily.map((d) => d.bytes_total))

  return (
    <>
      <PageHeader
        title="Statistics"
        subtitle="Downloaded volume per period and hoster. Session resets when hopperd restarts."
        actions={
          <button type="button" className="btn small" onClick={() => void load()} disabled={loading}>
            {loading ? 'Refreshing…' : 'Refresh'}
          </button>
        }
      />
      <div className="panel">
        <h2>Overview</h2>
        <div className="stats-overview">
          {overview.map((p) => {
            const data = periodData(stats, p.key)
            return (
              <div className="stat-period" key={p.key}>
                <div className="stat-period-label">{p.label}</div>
                <div className="stat-period-bytes">{formatBytes(data.bytes_total)}</div>
                <div className="stat-period-files">{formatCount(data.files_total)} files</div>
                <div className="stat-period-peak">{data.peak_speed ? `${formatSpeed(data.peak_speed)} peak` : ' '}</div>
              </div>
            )
          })}
        </div>
      </div>

      <div className="panel">
        <div className="row panel-header-row">
          <h2>By hoster</h2>
          <div className="row period-tabs">
            {PERIOD_TABS.map((t) => (
              <button
                key={t.key}
                type="button"
                className={`stat-period stat-period-tab${period === t.key ? ' active' : ''}`}
                onClick={() => setPeriod(t.key)}
              >
                {t.label}
              </button>
            ))}
          </div>
        </div>
        {hosterRows.length === 0 ? (
          <p className="empty-state">No downloads in this period yet.</p>
        ) : (
          <div className="hoster-bars">
            {hosterRows.map(([key, bytes]) => {
              const pct = hosterTotal > 0 ? (bytes / hosterTotal) * 100 : 0
              return (
                <div className="hoster-bar-row" key={key}>
                  <span className="hoster-bar-name">{displayName(key)}</span>
                  <span className="hoster-bar-track">
                    <span className="hoster-bar" style={{ width: `${pct}%` }} />
                  </span>
                  <span className="hoster-bar-pct">{pct.toFixed(0)}%</span>
                  <span className="hoster-bar-bytes mono">{formatBytes(bytes)}</span>
                </div>
              )
            })}
          </div>
        )}
      </div>

      <div className="panel">
        <h2>Daily activity (last 30 days)</h2>
        <div className="daily-bars">
          {daily.map((d) => (
            <div
              className="daily-bar"
              key={d.date}
              data-tooltip={`${d.date}: ${formatBytes(d.bytes_total)}`}
              style={{ height: `${Math.max(2, (d.bytes_total / maxDailyBytes) * 100)}%` }}
            />
          ))}
        </div>
      </div>
    </>
  )
}
