import assert from 'node:assert/strict'
import { test } from 'node:test'
import { splitName } from '../src/lib/filename.ts'

// splitName keeps the part that tells files apart (the tail) out of the
// ellipsis: head + tail is always the full name.
const cases: [string, string, string][] = [
  ['Example.Release.2026.2160p.part3.rar', 'Example.Release.2026.2160p', '.part3.rar'],
  ['Example.Release.2026.2160p.part12.rar', 'Example.Release.2026.2160p', '.part12.rar'],
  ['Example.Release.2026.r07', 'Example.Release.2026', '.r07'],
  ['Example.Show.S01.Complete.tar.gz', 'Example.Show.S01.Complete', '.tar.gz'],
  ['debian-13.1.0-amd64-netinst.iso', 'debian-13.1.0-amd64-netinst', '.iso'],
  ['Example.Release.2026.1080p.mkv', 'Example.Release.2026.1080p', '.mkv'],
]

test('splitName: tail is the archive part or extension', () => {
  for (const [name, head, tail] of cases) {
    assert.deepEqual(splitName(name), { head, tail }, name)
  }
})

test('splitName: short names and names without extension stay whole', () => {
  for (const name of ['file.zip', 'a.rar', 'README', 'no-extension-but-quite-a-long-name']) {
    assert.deepEqual(splitName(name), { head: name, tail: '' }, name)
  }
})

test('splitName: never loses characters', () => {
  for (const name of ['.hidden-file-with-a-long-name.txt', 'x.part1.rar', 'weird..double..dots..here.part2.rar', '']) {
    const { head, tail } = splitName(name)
    assert.equal(head + tail, name, name)
  }
})
