// Run: npm test (node --test, TypeScript via Node's type stripping).
import assert from 'node:assert/strict'
import { test } from 'node:test'
import { formatVersion } from '../src/lib/format.ts'

test('formatVersion: one leading v, non-versions as-is', () => {
  assert.equal(formatVersion('v0.9.0'), 'v0.9.0')
  assert.equal(formatVersion('0.9.0'), 'v0.9.0')
  assert.equal(formatVersion('dev'), 'dev')
})
