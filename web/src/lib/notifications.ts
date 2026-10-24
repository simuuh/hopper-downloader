import { itemName } from './format'
import type { QueueItem } from './types'

const STORAGE_KEY = 'hopper_notifications_enabled'

export function notificationsEnabled(): boolean {
  return localStorage.getItem(STORAGE_KEY) === '1'
}

export function setNotificationsEnabled(enabled: boolean): void {
  localStorage.setItem(STORAGE_KEY, enabled ? '1' : '0')
}

// notificationsSupported reports whether the Web Notification API exists
// at all — Notification is undefined on some embedded/older WebViews.
export function notificationsSupported(): boolean {
  return typeof Notification !== 'undefined'
}

// requestPermission prompts the browser if permission hasn't been
// decided yet. Safe to call even if already granted/denied — the
// browser just returns the existing decision without prompting again.
export async function requestPermission(): Promise<NotificationPermission> {
  if (!notificationsSupported()) return 'denied'
  if (Notification.permission !== 'default') return Notification.permission
  return Notification.requestPermission()
}

function notify(title: string, body: string): void {
  if (!notificationsEnabled() || !notificationsSupported()) return
  if (Notification.permission !== 'granted') return
  new Notification(title, { body })
}

function truncate(s: string, max: number): string {
  return s.length > max ? s.slice(0, max - 1) + '…' : s
}

export function notifyItemDone(item: QueueItem): void {
  notify(itemName(item), 'done')
}

export function notifyItemError(item: QueueItem): void {
  notify(itemName(item), truncate(item.error || 'error', 120))
}

export function notifyExtractFailed(item: QueueItem): void {
  notify(itemName(item), truncate(`extract failed: ${item.extract_error || 'unknown error'}`, 120))
}

export function notifyPackageComplete(packageName: string, fileCount: number): void {
  notify(packageName, `package complete — ${fileCount} file${fileCount === 1 ? '' : 's'}`)
}
