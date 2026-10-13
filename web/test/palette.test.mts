import { describe, it } from 'node:test'
import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import { fileURLToPath } from 'node:url'

import { DEFAULT_PALETTE, PALETTES, paletteById } from '../src/lib/palette.ts'

/** What the server accepts for a theme id (internal/api/profile_handlers.go). */
const THEME_ID = /^[a-z][a-z0-9-]{0,31}$/

/** converts #rrggbb to [hue°, saturation%, lightness%], left from custom
 * color tests I removed for now until I feel like making a color picker
  */
function hexToHsl(hex: string): [number, number, number] {
  const r = parseInt(hex.slice(1, 3), 16) / 255
  const g = parseInt(hex.slice(3, 5), 16) / 255
  const b = parseInt(hex.slice(5, 7), 16) / 255
  const max = Math.max(r, g, b)
  const min = Math.min(r, g, b)
  const l = (max + min) / 2
  const d = max - min
  const s = d === 0 ? 0 : d / (1 - Math.abs(2 * l - 1))
  let h = 0
  if (d !== 0) {
    if (max === r) h = 60 * (((g - b) / d) % 6)
    else if (max === g) h = 60 * ((b - r) / d + 2)
    else h = 60 * ((r - g) / d + 4)
  }
  return [(h + 360) % 360, s * 100, l * 100]
}

describe('palette presets', () => {
  it('ids are unique and in the shape the server accepts', () => {
    const ids = PALETTES.map((p) => p.id)
    assert.equal(new Set(ids).size, ids.length, 'preset ids must be unique')
    for (const id of ids) assert.match(id, THEME_ID, `${id} would be rejected by PUT /auth/theme`)
  })

  it('every seed is a readable #rrggbb hex', () => {
    for (const p of PALETTES) assert.match(p.seed, /^#[0-9a-f]{6}$/, `${p.id} seed: ${p.seed}`)
  })

  it('the default preset exists and unknown ids fall back to it', () => {
    assert.ok(PALETTES.some((p) => p.id === DEFAULT_PALETTE))
    assert.equal(paletteById('no-such-theme').id, DEFAULT_PALETTE)
    assert.equal(paletteById('').id, DEFAULT_PALETTE)
    assert.equal(paletteById(undefined).id, DEFAULT_PALETTE)
    assert.equal(paletteById(DEFAULT_PALETTE).id, DEFAULT_PALETTE)
  })
})

describe('index.css tokens', () => {
  const css = readFileSync(
    fileURLToPath(new URL('../src/index.css', import.meta.url)),
    'utf8',
  )

  const exempt = new Set(['seed', 'radius', 'destructive', 'destructive-foreground'])

  function tokensIn(block: RegExp): Map<string, string> {
    const body = css.match(block)?.[1]
    assert.ok(body, `no ${block} block in index.css`)
    const tokens = new Map<string, string>()
    for (const line of body.split('\n')) {
      const m = line.match(/^\s*(--[a-z-]+):\s*(.+);$/)
      if (m) tokens.set(m[1], m[2])
    }
    return tokens
  }

  function derivesFrom(value: string, tokens: Map<string, string>): boolean {
    if (value.includes('var(--seed)')) return true
    for (const ref of value.matchAll(/var\((--[a-z-]+)\)/g)) {
      const target = ref[1].replace(/^--/, '')
      if (exempt.has(target)) continue
      const resolved = tokens.get(`--${target}`)
      if (resolved && derivesFrom(resolved, tokens)) return true
    }
    return false
  }

  for (const [mode, block] of [
    ['light', /:root \{([\s\S]*?)\}/],
    ['dark', /\.dark \{([\s\S]*?)\}/],
  ] as const) {
    it(`${mode} mode: every colour token is calculated from --seed`, () => {
      const tokens = tokensIn(block)
      if (mode === 'light') assert.ok(tokens.has('--seed'), ':root must define --seed')
      else assert.ok(!tokens.has('--seed'), '.dark must inherit --seed, not redefine it')
      for (const [name, value] of tokens) {
        if (exempt.has(name.replace(/^--/, ''))) continue
        assert.ok(
          derivesFrom(value, tokens),
          `${name}: ${value} does not derive from --seed; themes must stay one colour`,
        )
      }
    })

    it(`${mode} mode: no hand-picked hexes outside the exempt list`, () => {
      const tokens = tokensIn(block)
      for (const [name, value] of tokens) {
        if (exempt.has(name.replace(/^--/, ''))) continue
        assert.doesNotMatch(value, /#[0-9a-f]{3,8}\b/, `${name}: ${value}`)
      }
    })
  }
})
