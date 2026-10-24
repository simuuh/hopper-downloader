export type Priority = 'low' | 'normal' | 'high'
export type ItemState = 'queued' | 'downloading' | 'paused' | 'done' | 'error'
export type ExtractState = 'extract_pending' | 'extract_queued' | 'extracting' | 'extracted' | 'extract_failed'

export interface QueueItem {
  id: string
  package_id: string
  url: string
  filename: string
  password?: string
  hoster_key: string
  priority: Priority
  state: ItemState
  position: number
  size: number
  downloaded: number
  speed: number
  error?: string
  extract_state?: ExtractState
  extract_error?: string
  scheduled_at?: string
  // retry_after/captcha_url reflect a hoster's free-download fallback
  // asking the client to wait or solve a captcha (see
  // internal/queue/manager.go's tryFreeFallback) — the item's own state
  // stays 'queued' the whole time, not a distinct ItemState value, the
  // same way extract_state layers extraction status on top of state
  // without becoming a state itself. At most one of the two is set.
  retry_after?: string
  captcha_url?: string
  attempts?: number
  created_at: string
  updated_at: string
}

export interface Package {
  id: string
  name: string
  password?: string
  // path is an optional "save to" override, relative to the downloads
  // root — see PATCH /api/v1/packages/:id. Empty/absent means "use
  // name" (the existing default).
  path?: string
  created_at: string
}

export interface Account {
  id: string
  hoster_key: string
  username: string
  enabled: boolean
  created_at: string
  updated_at: string
  // Validation state cached from the hoster's own API. valid is null
  // when never validated or the hoster plugin doesn't support
  // validation at all (e.g. directhttp/rapidgator) — distinct from
  // false (checked and found invalid).
  valid: boolean | null
  expires_at?: string
  traffic_left?: number
  label?: string
  validated_at?: string
  // Set while the account is skipped after a hoster rejected it during a
  // download (expired, out of traffic) — the next account of the same
  // hoster is used meanwhile. In-memory server-side, not persisted.
  cooldown_until?: string
}

export interface PluginInfo {
  name: string
  key: string
  domains: string[]
  // Hoster page to buy/extend premium; is_referral marks the Hopper
  // developer's referral link (accounts.referral_links in hopper.yaml).
  premium_url?: string
  is_referral?: boolean
  // Account form fields (username/password slots) as the plugin describes them.
  fields: CredentialField[]
  // Place in the account priority (1 = tried first when several plugins
  // with an account can handle a link). GET /plugins is sorted by it.
  rank: number
}

export interface CredentialField {
  name: 'username' | 'password'
  label: string
  secret: boolean
  required: boolean
  placeholder?: string
  help?: string
}

export interface AddLinksResult {
  packages: Package[]
  items: QueueItem[]
  duplicates: string[]
  errors?: string[]
}

export type PackageAction = 'pause' | 'resume' | 'retry'

// DailyStats/AllTimeStats/SessionStats/AggregateStats mirror
// internal/stats' Go types exactly (same JSON field names).

export interface DailyStats {
  date: string // "2026-07-27"
  bytes_total: number
  files_total: number
  errors: number
  by_hoster: Record<string, number>
}

export interface AllTimeStats {
  bytes_total: number
  files_total: number
  errors: number
  peak_speed: number
  largest_file: number
  by_hoster: Record<string, number>
}

// SessionStats is in-memory on the server too — it resets whenever
// hopperd restarts, which is correct behavior, not something to work
// around client-side.
export interface SessionStats {
  started_at: string
  bytes_total: number
  files_total: number
  errors: number
  peak_speed: number
  by_hoster: Record<string, number>
}

export interface AggregateStats {
  days: number
  bytes_total: number
  files_total: number
  errors: number
  by_hoster: Record<string, number>
  // One entry per day in the range, oldest first — a day with no
  // activity still appears (zero-valued, date set), not omitted.
  daily: DailyStats[]
}

export interface Stats {
  session: SessionStats
  today: DailyStats
  week: AggregateStats
  month: AggregateStats
  year: AggregateStats
  alltime: AllTimeStats
}

// BulkCancelResult mirrors DELETE /api/v1/queue's response — deleted/
// failed are both lists of item ids, not objects (see api/queue_handlers.go).
export interface BulkCancelResult {
  deleted: string[]
  failed: string[]
}

export interface QueueEvent {
  type: 'item_updated' | 'item_removed' | 'package_updated' | 'package_removed'
  item?: QueueItem
  package?: Package
}

// Settings is the runtime-editable subset of Hopper's configuration —
// see internal/settings.Settings on the backend. Persisted server-side
// in bbolt (overrides hopper.yaml at runtime); Notifications preference
// is separate and client-only (localStorage), not part of this.
export interface Settings {
  download_path: string
  parallel_files: number
  parallel_chunks: number
  bandwidth_limit: number // bytes/s, 0 = unlimited
  retry_max: number
  auto_start_imports: boolean
  default_priority: Priority
  log_level: 'debug' | 'info' | 'warn' | 'error'
  extract_enabled: boolean
  extract_delete_after: boolean
  extract_passwords: string[]
  // Plugin keys, most preferred first; [] = default order.
  account_priority: string[]
  // Daily check for a newer release on GitHub; off by default.
  update_check: boolean
}

// UpdateStatus mirrors internal/update.Status: the last update check's
// result, cached by hopperd.
export interface UpdateStatus {
  current: string
  latest?: string
  available: boolean
  release_url?: string
  checked_at?: string
  error?: string
}

// SettingsUpdate mirrors internal/settings.Update: every field is
// optional ("don't change this one"), matching POST /api/v1/settings'
// partial-update semantics.
export type SettingsUpdate = Partial<Settings>
