import type { ExtractState, QueueItem } from './types'

// An archive in one of these states still has extraction ahead of it (or
// a failed one to deal with): its download is done, the item isn't.
const EXTRACT_OPEN: ExtractState[] = ['extract_pending', 'extract_queued', 'extracting', 'extract_failed']

// isFinished: downloaded and, for an archive auto-extract handles,
// successfully extracted. The Finished view, "Clear done", the done
// counter and the done notifications all go by this, not by state alone.
export function isFinished(item: QueueItem): boolean {
  return item.state === 'done' && !(item.extract_state && EXTRACT_OPEN.includes(item.extract_state))
}

// needsAttention: a failed download or a failed extraction — what the
// Errors view lists.
export function needsAttention(item: QueueItem): boolean {
  return item.state === 'error' || item.extract_state === 'extract_failed'
}
