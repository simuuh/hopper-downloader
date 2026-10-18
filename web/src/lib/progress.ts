import type { QueueItem } from './types'

export interface PackageProgress {
  totalBytes: number
  doneBytes: number
  percent: number
  speed: number
  etaSeconds: number
  // estimated is true when totalBytes includes guessed sizes of parts that
  // haven't reported theirs yet.
  estimated: boolean
}

// packageProgress sums a package's progress. A part that hasn't started
// yet has no size (0) until its hoster reports one; counting it as 0 made
// the package look nearly done while only the running parts were counted.
// Such parts are estimated with the average known size of the package —
// a release's parts are practically always the same size. With no size
// known at all, progress is the share of finished files.
export function packageProgress(items: QueueItem[]): PackageProgress {
  const known = items.filter((i) => i.size > 0)
  const unknown = items.filter((i) => i.size <= 0 && i.state !== 'done')
  const average = known.length > 0 ? known.reduce((s, i) => s + i.size, 0) / known.length : 0
  const estimated = average > 0 && unknown.length > 0

  const totalBytes = known.reduce((s, i) => s + i.size, 0) + (estimated ? average * unknown.length : 0)
  const doneBytes = items.reduce((s, i) => s + (i.state === 'done' ? Math.max(i.size, i.downloaded) : i.downloaded), 0)
  const speed = items.reduce((s, i) => s + (i.state === 'downloading' ? i.speed : 0), 0)

  let percent: number
  if (totalBytes > 0) percent = Math.min(100, (doneBytes / totalBytes) * 100)
  else percent = items.length > 0 ? (items.filter((i) => i.state === 'done').length / items.length) * 100 : 0

  const etaSeconds = speed > 0 && totalBytes > doneBytes ? Math.round((totalBytes - doneBytes) / speed) : 0
  return { totalBytes, doneBytes, percent, speed, etaSeconds, estimated }
}
