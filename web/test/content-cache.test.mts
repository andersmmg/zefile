// browser-side caching of previewed file content
import { afterEach, beforeEach, describe, it } from 'node:test'
import assert from 'node:assert/strict'

import { createContentCache, type TextPayload } from '../src/lib/content-cache.ts'
import type { Entry } from '../src/api.ts'

const realFetch = globalThis.fetch
const realCreateObjectURL = URL.createObjectURL
const realRevokeObjectURL = URL.revokeObjectURL
const realLocation = (globalThis as { location?: unknown }).location

let loads = 0
let mints = 0
let contentFetches = 0
let blobBytes = 11
let revoked: string[] = []

beforeEach(() => {
  loads = 0
  mints = 0
  contentFetches = 0
  blobBytes = 11
  revoked = []
  ;(globalThis as { location?: unknown }).location = {
    origin: 'https://cdn.test',
    href: 'https://cdn.test/',
  }
  let seq = 0
  URL.createObjectURL = () => `blob:cache/${++seq}`
  URL.revokeObjectURL = (url: string) => {
    revoked.push(url)
  }
  globalThis.fetch = (async () => {
    contentFetches++
    return new Response(new Blob([new Uint8Array(blobBytes)]))
  }) as typeof fetch
})

afterEach(() => {
  globalThis.fetch = realFetch
  URL.createObjectURL = realCreateObjectURL
  URL.revokeObjectURL = realRevokeObjectURL
  ;(globalThis as { location?: unknown }).location = realLocation
})

function entry(patch: Partial<Entry> = {}): Entry {
  return {
    path: '/docs/a.txt',
    name: 'a.txt',
    size: 10,
    mod_time: '2026-01-01T00:00:00Z',
    is_dir: false,
    ...patch,
  }
}

function textLoad(content = 'hello') {
  return async (): Promise<TextPayload> => {
    loads++
    return { content, truncated: false }
  }
}

function minter(expiresAt = new Date(Date.now() + 5 * 60_000).toISOString()) {
  return async () => {
    mints++
    return { url: `https://cdn.test/signed/${mints}`, expires_at: expiresAt }
  }
}

const image = () => entry({ path: '/pic.png', name: 'pic.png', size: 11 })
const video = () => entry({ path: '/clip.mp4', name: 'clip.mp4', size: 100 })

describe('cachedText', () => {
  it('serves a repeat read from cache without fetching again', async () => {
    const cache = createContentCache({ budgetBytes: 10_000 })
    const load = textLoad()
    const e = entry()
    assert.deepEqual(await cache.cachedText(e, load), { content: 'hello', truncated: false })
    assert.deepEqual(await cache.cachedText(e, load), { content: 'hello', truncated: false })
    assert.equal(loads, 1)
  })

  it('refetches when the file changed (new modification time)', async () => {
    const cache = createContentCache({ budgetBytes: 10_000 })
    const load = textLoad()
    await cache.cachedText(entry(), load)
    await cache.cachedText(entry({ mod_time: '2026-02-01T00:00:00Z' }), load)
    assert.equal(loads, 2)
  })

  it('shares one fetch between concurrent readers of the same file', async () => {
    const cache = createContentCache({ budgetBytes: 10_000 })
    let release!: (value: TextPayload) => void
    const load = () => {
      loads++
      return new Promise<TextPayload>((r) => {
        release = r
      })
    }
    const e = entry()
    const a = cache.cachedText(e, load)
    const b = cache.cachedText(e, load)
    release({ content: 'shared', truncated: false })
    await Promise.all([a, b])
    assert.equal(loads, 1)
  })

  it('evicts the least recently used entry once the budget is exceeded', async () => {
    // 400 UTF-16 units ≈ 800 bytes each, so two entries overflow a 1000-byte
    // budget and the first in loses its slot
    const cache = createContentCache({ budgetBytes: 1000 })
    const load = textLoad('x'.repeat(400))
    await cache.cachedText(entry({ path: '/a' }), load)
    await cache.cachedText(entry({ path: '/b' }), load)
    assert.equal(loads, 2)
    // /b is still resident…
    await cache.cachedText(entry({ path: '/b' }), load)
    assert.equal(loads, 2)
    // …while /a was evicted and must be fetched again.
    await cache.cachedText(entry({ path: '/a' }), load)
    assert.equal(loads, 3)
  })

  it('keeps a single oversized entry rather than evicting what was just read', async () => {
    const cache = createContentCache({ budgetBytes: 1000 })
    const load = textLoad('x'.repeat(2000))
    await cache.cachedText(entry(), load)
    assert.deepEqual(await cache.cachedText(entry(), load), { content: 'x'.repeat(2000), truncated: false })
    assert.equal(loads, 1)
  })
})

describe('cachedMediaUrl', () => {
  it('buffers a small file into a blob and paints revisits with no request', async () => {
    const cache = createContentCache({ budgetBytes: 10_000 })
    const first = await cache.cachedMediaUrl(image(), minter())
    const second = await cache.cachedMediaUrl(image(), minter())
    assert.equal(first, 'blob:cache/1')
    assert.equal(second, 'blob:cache/1')
    assert.equal(mints, 1)
    assert.equal(contentFetches, 1)
  })

  it('streams from the signed link when the caller forbids buffering (video)', async () => {
    const cache = createContentCache({ budgetBytes: 10_000 })
    const mint = minter()
    const url = await cache.cachedMediaUrl(video(), mint, false)
    assert.equal(url, 'https://cdn.test/signed/1')
    assert.equal(contentFetches, 0)
    // The remembered link is reused, so the mint is not repeated
    assert.equal(await cache.cachedMediaUrl(video(), mint, false), url)
    assert.equal(mints, 1)
  })

  it('leaves files over the blob limit on their signed link', async () => {
    const cache = createContentCache({ budgetBytes: 10_000, blobLimitBytes: 5 })
    const big = entry({ path: '/big.jpg', name: 'big.jpg', size: 6 })
    const url = await cache.cachedMediaUrl(big, minter())
    assert.equal(url, 'https://cdn.test/signed/1')
    assert.equal(contentFetches, 0)
  })

  it('never tries to read bytes back from another origin', async () => {
    const cache = createContentCache({ budgetBytes: 10_000 })
    // A content host on its own origin (or a redirect to the object store):
    // fetching would transfer the whole file and then discard it unreadable.
    const mint = async () => {
      mints++
      return { url: `https://content.other/signed/${mints}`, expires_at: new Date(Date.now() + 300_000).toISOString() }
    }
    const url = await cache.cachedMediaUrl(image(), mint)
    assert.equal(url, 'https://content.other/signed/1')
    assert.equal(contentFetches, 0)
    // The link is still remembered, so even the mint is not repeated.
    assert.equal(await cache.cachedMediaUrl(image(), mint), url)
    assert.equal(mints, 1)
  })

  it('re-mints a link that is about to expire', async () => {
    const cache = createContentCache({ budgetBytes: 10_000 })
    // Expiring inside the renewal margin, so the remembered link is stale.
    const mint = minter(new Date(Date.now() + 10_000).toISOString())
    await cache.cachedMediaUrl(video(), mint, false)
    assert.equal(await cache.cachedMediaUrl(video(), mint, false), 'https://cdn.test/signed/2')
    assert.equal(mints, 2)
  })

  it('falls back to the signed link when the bytes cannot be read back', async () => {
    const cache = createContentCache({ budgetBytes: 10_000 })
    globalThis.fetch = (async () => {
      contentFetches++
      throw new TypeError('Failed to fetch')
    }) as typeof fetch
    const mint = minter()
    const url = await cache.cachedMediaUrl(image(), mint)
    assert.equal(url, 'https://cdn.test/signed/1')
    // The fallback is remembered too: the link is reused, not re-minted.
    assert.equal(await cache.cachedMediaUrl(image(), mint), 'https://cdn.test/signed/1')
    assert.equal(mints, 1)
  })

  it('refetches when the file changed (new modification time)', async () => {
    const cache = createContentCache({ budgetBytes: 10_000 })
    const mint = minter()
    await cache.cachedMediaUrl(image(), mint)
    await cache.cachedMediaUrl(image(), mint)
    assert.equal(mints, 1)
    await cache.cachedMediaUrl(image(), mint)
    await cache.cachedMediaUrl(entry({ path: '/pic.png', name: 'pic.png', size: 11, mod_time: '2026-02-01T00:00:00Z' }), mint)
    assert.equal(mints, 2)
  })

  it('revokes the evicted file\'s object URL', async () => {
    const cache = createContentCache({ budgetBytes: 1000 })
    blobBytes = 600
    const a = entry({ path: '/a.png', name: 'a.png', size: 600 })
    const b = entry({ path: '/b.png', name: 'b.png', size: 600 })
    const first = await cache.cachedMediaUrl(a, minter())
    assert.equal(first, 'blob:cache/1')
    await cache.cachedMediaUrl(b, minter())
    assert.deepEqual(revoked, ['blob:cache/1'])
  })
})

describe('clear', () => {
  it('empties the cache, revoking blobs, so everything is fetched again', async () => {
    const cache = createContentCache({ budgetBytes: 10_000 })
    const url = await cache.cachedMediaUrl(image(), minter())
    await cache.cachedText(entry(), textLoad())
    cache.clear()
    assert.deepEqual(revoked, [url])
    // text and media are both fetched
    assert.equal(await cache.cachedMediaUrl(image(), minter()), 'blob:cache/2')
    await cache.cachedText(entry(), textLoad())
    assert.equal(loads, 2)
    assert.equal(mints, 2)
  })

  it('discards the result of a load that was still in flight when it ran', async () => {
    const cache = createContentCache({ budgetBytes: 10_000 })
    let release!: (value: TextPayload) => void
    const load = () => {
      loads++
      return new Promise<TextPayload>((r) => {
        release = r
      })
    }
    const pending = cache.cachedText(entry(), load)
    cache.clear()
    release({ content: 'stale session', truncated: false })
    assert.equal((await pending).content, 'stale session')
    // Stored only after the clear, so the next reader still fetches.
    await cache.cachedText(entry(), textLoad())
    assert.equal(loads, 2)
  })
})
