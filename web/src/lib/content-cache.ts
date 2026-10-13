// A session-lifetime, in-memory LRU cache of previewed file content keyed by
// path and modification time.
import type { Entry } from '@/api'

/** Default combined bytes of cached text and buffered media before
 *  least-recently-used entries start being evicted. */
const DEFAULT_BUDGET_BYTES = 64 * 1024 * 1024

/** Default per-file cap on blob buffering. Larger files stream from their
 *  signed link insteade. */
const DEFAULT_BLOB_LIMIT_BYTES = 24 * 1024 * 1024

/** Signed links are renewed this long before they expire */
const RENEW_MARGIN_MS = 30_000

export type TextPayload = { content: string; truncated: boolean }

type TextRecord = { kind: 'text'; value: TextPayload; bytes: number }

type MediaRecord = {
  kind: 'media'
  /** the blob's object URL or the signed link itself */
  url: string
  /** infinity for an object URL */
  expiresAt: number
  /** buffered bytes; 0 when the file streams from its signed link */
  bytes: number
  /** whether url is an object URL that eviction should revoke */
  revoke: boolean
}

type CacheRecord = TextRecord | MediaRecord

/** builds an independent cache. The app holds one shared
 *  instance; tests build their own with small budgets. */
export function createContentCache(opts: { budgetBytes?: number; blobLimitBytes?: number } = {}) {
  const budgetBytes = opts.budgetBytes ?? DEFAULT_BUDGET_BYTES
  const blobLimitBytes = opts.blobLimitBytes ?? DEFAULT_BLOB_LIMIT_BYTES

  const store = new Map<string, CacheRecord>()
  const inFlight = new Map<string, Promise<unknown>>()
  let generation = 0
  let bytesUsed = 0

  const keyOf = (entry: Entry) => `${entry.path}|${entry.mod_time}`

  function canRead(url: string): boolean {
    if (typeof location === 'undefined') return false
    try {
      return new URL(url, location.href).origin === location.origin
    } catch {
      return false
    }
  }

  function touch(key: string) {
    const record = store.get(key)
    if (!record) return
    store.delete(key)
    store.set(key, record)
  }

  function drop(key: string) {
    const record = store.get(key)
    if (!record) return
    store.delete(key)
    bytesUsed -= record.bytes
    if (record.kind === 'media' && record.revoke) URL.revokeObjectURL(record.url)
  }

  function remember(key: string, record: CacheRecord) {
    drop(key)
    store.set(key, record)
    bytesUsed += record.bytes
    while (bytesUsed > budgetBytes && store.size > 1) {
      const oldest = store.keys().next()
      if (oldest.done) break
      drop(oldest.value)
    }
  }

  function dedupe<T>(key: string, load: () => Promise<T>): Promise<T> {
    const pending = inFlight.get(key) as Promise<T> | undefined
    if (pending) return pending
    const shared = load().finally(() => inFlight.delete(key))
    inFlight.set(key, shared)
    return shared
  }

  async function cachedText(entry: Entry, load: () => Promise<TextPayload>): Promise<TextPayload> {
    const key = keyOf(entry)
    const gen = generation
    const hit = store.get(key)
    if (hit?.kind === 'text') {
      touch(key)
      return hit.value
    }
    return dedupe(key, async () => {
      const value = await load()
      if (gen === generation) remember(key, { kind: 'text', value, bytes: value.content.length * 2 })
      return value
    })
  }

  async function cachedMediaUrl(
    entry: Entry,
    mint: () => Promise<{ url: string; expires_at: string }>,
    buffer = true,
  ): Promise<string> {
    const key = keyOf(entry)
    const gen = generation
    const hit = store.get(key)
    const fresh = hit?.kind === 'media' && (hit.revoke || Date.now() < hit.expiresAt - RENEW_MARGIN_MS)
    if (hit && fresh) {
      touch(key)
      return hit.url
    }
    return dedupe(key, async () => {
      const { url, expires_at } = await mint()
      const expiresAt = Date.parse(expires_at)
      const keepLink = () => {
        if (gen === generation) {
          remember(key, { kind: 'media', url, expiresAt, bytes: 0, revoke: false })
        }
        return url
      }
      if (!buffer || !canRead(url) || entry.size > blobLimitBytes) return keepLink()
      try {
        const res = await fetch(url)
        if (!res.ok) throw new Error(`content responded ${res.status}`)
        const blob = await res.blob()
        const objectUrl = URL.createObjectURL(blob)
        if (gen === generation) {
          remember(key, { kind: 'media', url: objectUrl, expiresAt: Infinity, bytes: blob.size, revoke: true })
        }
        return objectUrl
      } catch {
        return keepLink()
      }
    })
  }

  function clear() {
    generation++
    for (const key of [...store.keys()]) drop(key)
    inFlight.clear()
  }

  return { cachedText, cachedMediaUrl, clear }
}

export const contentCache = createContentCache()
