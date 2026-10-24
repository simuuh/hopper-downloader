import assert from 'node:assert/strict'
import { test } from 'node:test'
import { isFinished, needsAttention } from '../src/lib/finished.ts'
import type { QueueItem } from '../src/lib/types.ts'

function item(state: QueueItem['state'], extract_state?: QueueItem['extract_state']): QueueItem {
  return { state, extract_state } as QueueItem
}

test('isFinished: a plain download is finished once done', () => {
  assert.equal(isFinished(item('done')), true)
  assert.equal(isFinished(item('downloading')), false)
})

test('isFinished: a downloaded archive is finished only once extracted', () => {
  assert.equal(isFinished(item('done', 'extract_pending')), false)
  assert.equal(isFinished(item('done', 'extract_queued')), false)
  assert.equal(isFinished(item('done', 'extracting')), false)
  assert.equal(isFinished(item('done', 'extract_failed')), false)
  assert.equal(isFinished(item('done', 'extracted')), true)
})

test('needsAttention: failed downloads and failed extractions', () => {
  assert.equal(needsAttention(item('error')), true)
  assert.equal(needsAttention(item('done', 'extract_failed')), true)
  assert.equal(needsAttention(item('done', 'extracting')), false)
})
