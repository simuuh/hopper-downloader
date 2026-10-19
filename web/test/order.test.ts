import assert from 'node:assert/strict'
import { test } from 'node:test'
import { finishedLast, groupCount } from '../src/lib/order.ts'
import type { QueueItem } from '../src/lib/types.ts'

function item(id: string, pkg: string, state: QueueItem['state']): QueueItem {
  return { id, package_id: pkg, state } as QueueItem
}

test('finishedLast: finished packages move below the others, order otherwise kept', () => {
  const items = [
    item('a1', 'A', 'done'), // A finished
    item('b1', 'B', 'done'), // B still downloading
    item('b2', 'B', 'downloading'),
    item('c1', 'C', 'done'), // C finished
    item('d1', 'D', 'paused'),
  ]
  const finished = new Set(['A', 'C'])
  assert.deepEqual(
    finishedLast(items, finished).map((i) => i.id),
    ['b1', 'b2', 'd1', 'a1', 'c1'],
  )
})

test('finishedLast: package-less items use their own key', () => {
  const items = [item('x', '', 'done'), item('y', '', 'queued')]
  assert.deepEqual(
    finishedLast(items, new Set(['item:x'])).map((i) => i.id),
    ['y', 'x'],
  )
})

test('finishedLast: nothing finished leaves the list as it is', () => {
  const items = [item('a', 'A', 'queued'), item('b', 'B', 'error')]
  assert.deepEqual(finishedLast(items, new Set()), items)
})

test('groupCount: counts packages, package-less items each on their own', () => {
  const items = [
    item('a1', 'A', 'done'),
    item('a2', 'A', 'queued'),
    item('b1', 'B', 'error'),
    item('x', '', 'queued'),
    item('y', '', 'done'),
  ]
  assert.equal(groupCount(items), 4)
  assert.equal(groupCount([]), 0)
})
