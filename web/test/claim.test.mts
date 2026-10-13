import { describe, it } from 'node:test'
import assert from 'node:assert/strict'
import { uploadDirectParts, type DirectDeps } from '../src/upload.ts'

describe('checksum claim', () => {
  const partSize = 10
  const file = new Blob([new Uint8Array(25)])

  function deps(capture: { claim?: string; completes: number }): DirectDeps {
    let sent = 0
    return {
      partURLs: async (_session, parts) => new Map(parts.map((n) => [n, `https://put/part-${n}`])),
      putPart: async (_url, body) => {
        sent += body.size
      },
      currentOffset: async () => sent,
      completeSession: async (_session, _signal, claim) => {
        capture.claim = claim
        capture.completes++
      },
    }
  }

  it('passes the digest to the complete call', async () => {
    const capture = { completes: 0 }
    const hash = 'a'.repeat(64)
    await uploadDirectParts(
      file,
      'session',
      partSize,
      () => {},
      undefined,
      2,
      deps(capture),
      undefined,
      Promise.resolve(hash),
    )
    assert.equal(capture.completes, 1)
    assert.equal(capture.claim, hash)
  })

  it('completes with no claim when the hash failed', async () => {
    const capture = { completes: 0 }
    await uploadDirectParts(
      file,
      'session',
      partSize,
      () => {},
      undefined,
      2,
      deps(capture),
      undefined,
      Promise.resolve(undefined),
    )
    assert.equal(capture.completes, 1)
    assert.equal(capture.claim, undefined)
  })
})
