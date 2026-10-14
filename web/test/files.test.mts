// character-limit truncation of displayed file names
import { describe, it } from 'node:test'
import assert from 'node:assert/strict'
import { childLabel, displayName, entryPerms, formatRelativeTime, NAME_LIMIT, shortName } from '../src/lib/files.ts'
import type { Entry, PermSet } from '../src/api.ts'

describe('formatRelativeTime', () => {
  it('renders a real timestamp relatively', () => {
    assert.equal(formatRelativeTime(new Date(Date.now() - 2 * 3600_000).toISOString()), '2 hours ago')
    assert.equal(formatRelativeTime(new Date().toISOString()), 'just now')
  })

  it('an unknown timestamp — object storage\'s zero time — has no label', () => {
    assert.equal(formatRelativeTime('0001-01-01T00:00:00Z'), '')
    assert.equal(formatRelativeTime('not a date'), '')
  })
})

describe('childLabel', () => {
  it('names both kinds, folders first, in the singular when there is one', () => {
    assert.equal(childLabel({ files: 3, dirs: 2 }), '2 folders · 3 files')
    assert.equal(childLabel({ files: 1, dirs: 1 }), '1 folder · 1 file')
    assert.equal(childLabel({ files: 1, dirs: 0 }), '1 file')
    assert.equal(childLabel({ files: 0, dirs: 4 }), '4 folders')
  })

  it('an empty folder says so', () => {
    assert.equal(childLabel({ files: 0, dirs: 0 }), 'Empty')
  })

  it('a count that has not arrived — or could not be fetched — shows nothing', () => {
    assert.equal(childLabel(undefined), '')
    assert.equal(childLabel(null), '')
  })
})

describe('shortName', () => {
  it('leaves names within the budget untouched', () => {
    assert.equal(shortName('notes.txt'), 'notes.txt')
    assert.equal(shortName('a'.repeat(NAME_LIMIT)), 'a'.repeat(NAME_LIMIT))
    assert.equal(shortName(''), '')
  })

  it('middle-truncates a long name to exactly the budget, keeping the extension', () => {
    // Longer than whatever the budget is, with a fixed head and extension.
    const name = `245343${'x'.repeat(NAME_LIMIT + 10)}.jpg`
    assert.ok(name.length > NAME_LIMIT)
    const short = shortName(name)
    assert.equal(short.length, NAME_LIMIT)
    assert.ok(short.includes('...'), `${short} should carry the ellipsis`)
    assert.ok(short.startsWith('245343'), `${short} should keep the head`)
    assert.ok(short.endsWith('.jpg'), `${short} should keep the extension whole`)
  })

  it('cuts the first character past the budget, and no further', () => {
    assert.equal(shortName('a'.repeat(NAME_LIMIT + 1)).length, NAME_LIMIT)
    assert.equal(shortName('a'.repeat(NAME_LIMIT + 40)).length, NAME_LIMIT)
  })

  it('treats an absurd extension as part of the name', () => {
    const short = shortName(`file.${'e'.repeat(40)}`)
    assert.equal(short.length, NAME_LIMIT)
    assert.ok(short.includes('...'))
  })

  it('handles extensionless and hidden names without mangling them', () => {
    assert.equal(shortName('README').length, 'README'.length)
    const hidden = shortName(`.config${'x'.repeat(NAME_LIMIT)}`)
    assert.equal(hidden.length, NAME_LIMIT)
    assert.ok(hidden.startsWith('.conf'))
  })

  it('honours a custom limit, giving the extension the tail', () => {
    assert.equal(shortName('abcdefghij.jpg', 8).length, 8)
    assert.equal(shortName('abcdefghij.jpg', 8), 'a....jpg')
  })
})

const ALL: PermSet = { read: true, write: true, delete: true, share: true, manage: true }

// entryPerms only reads the path, so a path is enough to stand in for an entry.
const at = (path: string): Entry =>
  ({ name: path.slice(path.lastIndexOf('/') + 1) || '/', path, is_dir: true }) as Entry

describe('entryPerms', () => {
  it('passes an ordinary entry the caller\'s permissions untouched', () => {
    assert.equal(entryPerms(at('/photos'), ALL), ALL)
  })

  it('strips delete from the managed members structure, keeping the rest', () => {
    // Callers express rename as write && delete and move as delete, so
    // dropping delete is what forbids renaming, moving and deleting these.
    for (const path of ['/members', '/members/alice']) {
      assert.deepEqual(entryPerms(at(path), ALL), { ...ALL, delete: false })
    }
  })

  it('still passes the caller\'s own limits through on members children', () => {
    const readOnly: PermSet = { ...ALL, write: false, delete: false }
    assert.deepEqual(entryPerms(at('/members/alice'), readOnly), readOnly)
  })
})

describe('displayName', () => {
  it('capitalizes the managed members folder', () => {
    assert.equal(displayName(at('/members')), 'Members')
  })

  it('keeps every other name exactly as stored', () => {
    assert.equal(displayName(at('/members/alice')), 'alice')
    assert.equal(displayName(at('/photos/holiday.png')), 'holiday.png')
  })
})
