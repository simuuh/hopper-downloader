import assert from 'node:assert/strict'
import { test } from 'node:test'
import { packageProgress } from '../src/lib/progress.ts'
import type { QueueItem } from '../src/lib/types.ts'

const MB = 1_000_000

function item(state: QueueItem['state'], size: number, downloaded: number, speed = 0): QueueItem {
  return { state, size, downloaded, speed } as QueueItem
}

test('packageProgress: parts not started yet count with the average known size', () => {
  // A release of four 400 MB parts: one done, one half way, two not
  // started yet (size still unknown).
  const p = packageProgress([
    item('done', 400 * MB, 400 * MB),
    item('downloading', 400 * MB, 200 * MB, 2 * MB),
    item('queued', 0, 0),
    item('queued', 0, 0),
  ])
  assert.equal(p.totalBytes, 1600 * MB)
  assert.equal(p.doneBytes, 600 * MB)
  assert.equal(p.percent, 37.5)
  assert.equal(p.estimated, true)
  assert.equal(p.etaSeconds, 500) // 1000 MB left at 2 MB/s
})

test('packageProgress: all sizes known is exact', () => {
  const p = packageProgress([item('done', 100 * MB, 100 * MB), item('paused', 300 * MB, 0)])
  assert.equal(p.totalBytes, 400 * MB)
  assert.equal(p.percent, 25)
  assert.equal(p.estimated, false)
  assert.equal(p.etaSeconds, 0) // nothing downloading
})

test('packageProgress: no size known yet falls back to the share of finished files', () => {
  const p = packageProgress([item('done', 0, 0), item('queued', 0, 0), item('queued', 0, 0), item('queued', 0, 0)])
  assert.equal(p.totalBytes, 0)
  assert.equal(p.percent, 25)
})

test('packageProgress: never above 100%', () => {
  // A hoster that reported a smaller size than what arrived.
  const p = packageProgress([item('done', 90 * MB, 100 * MB)])
  assert.equal(p.percent, 100)
})
