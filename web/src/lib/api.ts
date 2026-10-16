import type {
  Account,
  AddLinksResult,
  BulkCancelResult,
  Package,
  PackageAction,
  PluginInfo,
  Priority,
  QueueItem,
  Settings,
  SettingsUpdate,
  Stats,
} from './types'

const TOKEN_KEY = 'hopper_token'
const TOKEN_EXPIRES_KEY = 'hopper_token_expires_at'

export function getToken(): string | null {
  return localStorage.getItem(TOKEN_KEY)
}
// setToken stores expiresAt (an ISO timestamp, e.g. LoginResult.expires_at)
// alongside the token itself so tokenNearExpiry can decide whether a
// bulk action should proactively refresh first — see that function's
// doc comment for why this matters.
export function setToken(token: string, expiresAt?: string): void {
  localStorage.setItem(TOKEN_KEY, token)
  if (expiresAt) localStorage.setItem(TOKEN_EXPIRES_KEY, expiresAt)
  else localStorage.removeItem(TOKEN_EXPIRES_KEY)
}
export function clearToken(): void {
  localStorage.removeItem(TOKEN_KEY)
  localStorage.removeItem(TOKEN_EXPIRES_KEY)
}

function getTokenExpiresAt(): Date | null {
  const raw = localStorage.getItem(TOKEN_EXPIRES_KEY)
  if (!raw) return null
  const d = new Date(raw)
  return Number.isNaN(d.getTime()) ? null : d
}

// tokenNearExpiry reports whether the stored token expires within
// bufferMs (default 60s — comfortably longer than any bulk action
// should take to fire off its requests). An unknown expiry (e.g. an
// older stored token from before this field existed) reports false:
// there's nothing actionable to do without something to compare
// against, and this is a proactive optimization, not the only line of
// defense — a token that actually expires mid-batch is still handled
// correctly by the bulk-action 401 aggregation in QueuePage.
export function tokenNearExpiry(bufferMs = 60_000): boolean {
  const exp = getTokenExpiresAt()
  if (!exp) return false
  return exp.getTime() - Date.now() < bufferMs
}

export class ApiError extends Error {
  status: number
  constructor(status: number, message: string) {
    super(message)
    this.status = status
  }
}

async function request<T>(method: string, path: string, body?: unknown): Promise<T> {
  const headers: Record<string, string> = {}
  const token = getToken()
  if (token) headers['Authorization'] = `Bearer ${token}`
  if (body !== undefined) headers['Content-Type'] = 'application/json'

  const res = await fetch(path, {
    method,
    headers,
    body: body !== undefined ? JSON.stringify(body) : undefined,
  })

  // Deliberately not clearing the token here on a 401: a single request
  // failing doesn't mean the session is dead (see QueuePage's bulk
  // actions, where several parallel requests share one token and one
  // stray 401 must not silently wipe it out from under the others).
  // Callers decide what a 401 means in their context — see
  // App.tsx's handleAuthError for the "yes, actually log out" path.
  if (!res.ok) {
    let message = res.statusText
    try {
      const data = (await res.json()) as { error?: string }
      if (data?.error) message = data.error
    } catch {
      // response wasn't JSON; fall back to statusText
    }
    throw new ApiError(res.status, message)
  }
  if (res.status === 204) return undefined as T
  const text = await res.text()
  return text ? (JSON.parse(text) as T) : (undefined as T)
}

export interface LoginResult {
  token: string
  expires_at: string
}

export interface AddLinkInput {
  url: string
  priority?: Priority
  filename?: string
  password?: string
  scheduled_at?: string
}

export interface AddPackageInput {
  name: string
  urls: string[]
  priority?: Priority
  password?: string
}

export type ImportResult = AddLinksResult

export interface AddLinksInput {
  text: string
  name?: string
  password?: string
  priority?: Priority
  path?: string
}

export const api = {
  login: (username: string, password: string) =>
    request<LoginResult>('POST', '/api/v1/auth/login', { username, password }),
  refreshToken: () => request<LoginResult>('POST', '/api/v1/auth/refresh'),

  listQueue: () => request<QueueItem[]>('GET', '/api/v1/queue'),
  getItem: (id: string) => request<QueueItem>('GET', `/api/v1/queue/${id}`),
  addLink: (input: AddLinkInput) => request<QueueItem>('POST', '/api/v1/links', input),
  addLinks: (input: AddLinksInput) => request<AddLinksResult>('POST', '/api/v1/links/bulk', input),
  addPackage: (input: AddPackageInput) =>
    request<{ package: Package; items: QueueItem[] }>('POST', '/api/v1/packages', input),
  listPackages: () => request<Package[]>('GET', '/api/v1/packages'),
  pause: (id: string) => request<QueueItem>('POST', `/api/v1/queue/${id}/pause`),
  resume: (id: string) => request<QueueItem>('POST', `/api/v1/queue/${id}/resume`),
  retry: (id: string) => request<QueueItem>('POST', `/api/v1/queue/${id}/retry`),
  retryExtract: (id: string, password?: string) =>
    request<QueueItem>('POST', `/api/v1/queue/${id}/extract`, password ? { password } : undefined),
  solveCaptcha: (id: string, solution: string) =>
    request<QueueItem>('POST', `/api/v1/queue/${id}/captcha`, { solution }),
  cancel: (id: string) => request<void>('DELETE', `/api/v1/queue/${id}`),
  queueActions: (ids: string[], action: 'pause' | 'resume' | 'retry' | 'delete') =>
    request<{ changed: number }>('POST', '/api/v1/queue/actions', { ids, action }),
  bulkCancel: (ids: string[]) => request<BulkCancelResult>('DELETE', '/api/v1/queue', { ids }),
  reorder: (ids: string[]) => request<void>('PUT', '/api/v1/queue/order', { ids }),
  pauseAll: () => request<void>('POST', '/api/v1/queue/pause-all'),
  resumeAll: () => request<void>('POST', '/api/v1/queue/resume-all'),
  setPriority: (id: string, priority: Priority) =>
    request<QueueItem>('PATCH', `/api/v1/queue/${id}`, { priority }),
  editItem: (id: string, edit: { url?: string; filename?: string }) =>
    request<QueueItem>('PATCH', `/api/v1/queue/${id}`, edit),
  addToPackage: (id: string, text: string) =>
    request<AddLinksResult>('POST', `/api/v1/packages/${id}/links`, { text }),
  setPackagePath: (id: string, path: string) => request<Package>('PATCH', `/api/v1/packages/${id}`, { path }),
  renamePackage: (id: string, name: string) => request<Package>('PATCH', `/api/v1/packages/${id}`, { name }),
  packageAction: (id: string, action: PackageAction) =>
    request<{ changed: number }>('POST', `/api/v1/packages/${id}/${action}`),
  deletePackage: (id: string) => request<{ changed: number }>('DELETE', `/api/v1/packages/${id}`),

  getSettings: () => request<Settings>('GET', '/api/v1/settings'),
  updateSettings: (update: SettingsUpdate) => request<Settings>('POST', '/api/v1/settings', update),

  listPlugins: () => request<PluginInfo[]>('GET', '/api/v1/plugins'),

  listAccounts: () => request<Account[]>('GET', '/api/v1/accounts'),
  addAccount: (input: { hoster_key: string; username: string; password: string; extra?: Record<string, string> }) =>
    request<Account>('POST', '/api/v1/accounts', input),
  setAccountEnabled: (id: string, enabled: boolean) =>
    request<Account>('POST', `/api/v1/accounts/${id}/enabled`, { enabled }),
  deleteAccount: (id: string) => request<void>('DELETE', `/api/v1/accounts/${id}`),
  updateAccount: (id: string, input: { username?: string; password?: string }) =>
    request<Account>('PUT', `/api/v1/accounts/${id}`, input),
  validateAccount: (id: string) => request<Account>('POST', `/api/v1/accounts/${id}/validate`),

  getVersion: () => request<{ version: string }>('GET', '/api/v1/version'),
  getConfig: () => request<Record<string, unknown>>('GET', '/api/v1/config'),
  getStats: () => request<Stats>('GET', '/api/v1/stats'),
  getNotifyStatus: () => request<{ enabled: boolean }>('GET', '/api/v1/notify'),
  testNotify: () => request<void>('POST', '/api/v1/notify/test'),

  importFiles: async (files: File[]): Promise<ImportResult> => {
    const form = new FormData()
    for (const file of files) form.append('file', file)
    const token = getToken()
    const headers: Record<string, string> = {}
    if (token) headers['Authorization'] = `Bearer ${token}`
    const res = await fetch('/api/v1/import', { method: 'POST', headers, body: form })
    if (!res.ok) {
      let message = res.statusText
      try {
        const data = (await res.json()) as { error?: string }
        if (data?.error) message = data.error
      } catch {
        // ignore
      }
      throw new ApiError(res.status, message)
    }
    return res.json() as Promise<ImportResult>
  },
}
