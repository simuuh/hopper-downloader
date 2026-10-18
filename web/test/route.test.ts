// Run: npm test (node --test, TypeScript via Node's type stripping).
import assert from 'node:assert/strict'
import { test } from 'node:test'
import { hashForTab, tabFromHash } from '../src/lib/route.ts'

test('tabFromHash: known tabs', () => {
  assert.equal(tabFromHash('#/accounts'), 'accounts')
  assert.equal(tabFromHash('#/stats'), 'stats')
  assert.equal(tabFromHash('#/settings'), 'settings')
  assert.equal(tabFromHash('#/queue'), 'queue')
})

test('tabFromHash: anything else is the queue', () => {
  for (const hash of ['', '#', '#/', '#/nope', '#share_file=abc', '#/settings/extra']) {
    assert.equal(tabFromHash(hash), 'queue', hash)
  }
})

test('hashForTab round-trips through tabFromHash', () => {
  for (const tab of ['queue', 'accounts', 'stats', 'settings'] as const) {
    assert.equal(tabFromHash(hashForTab(tab)), tab)
  }
})
