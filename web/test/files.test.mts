// character-limit truncation of displayed file names
import { describe, it } from 'node:test'
import assert from 'node:assert/strict'
import { NAME_LIMIT, shortName } from '../src/lib/files.ts'

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
