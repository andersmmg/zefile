import { describe, it } from 'node:test'
import assert from 'node:assert/strict'
import { COLORS, colorHex, colorLabel, frameClasses, panelClasses } from '../src/lib/colors.ts'

const SERVER_PALETTE = ['red', 'orange', 'yellow', 'green', 'teal', 'cyan', 'blue', 'purple', 'pink']

describe('color palette', () => {
  it('offers exactly the ids the server accepts', () => {
    assert.deepEqual(Object.keys(COLORS), SERVER_PALETTE)
  })

  it('has a unique label and a plain hex circle per swatch', () => {
    const labels = new Set<string>()
    for (const [id, c] of Object.entries(COLORS)) {
      assert.match(c.hex, /^#[0-9a-f]{6}$/, `${id} is not a plain hex value`)
      labels.add(c.label)
    }
    assert.equal(labels.size, Object.keys(COLORS).length)
  })

  it('resolves a stored id, and reads anything unknown as no colour', () => {
    assert.equal(colorHex('teal'), '#14b8a6')
    assert.equal(colorHex('chartreuse'), undefined)
    assert.equal(colorHex(''), undefined)
    assert.equal(colorHex(undefined), undefined)
    assert.equal(colorLabel('pink'), 'Pink')
    assert.equal(colorLabel('chartreuse'), 'Colour')
  })

  it('paints nothing for an uncoloured entry', () => {
    assert.equal(panelClasses(undefined), undefined)
    assert.equal(frameClasses(null), undefined)
    assert.equal(panelClasses('chartreuse'), undefined)
  })

  it('gives a tinted tile its classes, and frames a preview', () => {
    assert.equal(
      panelClasses('teal'),
      'bg-teal-100 text-teal-600 dark:bg-teal-950 dark:text-teal-400',
    )
    assert.deepEqual(
      frameClasses('red'),
      'outline-solid outline-red-100 dark:outline-red-950',
    )
  })
})
