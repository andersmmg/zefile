import { describe, it } from 'node:test'
import assert from 'node:assert/strict'
import { createHash, randomBytes } from 'node:crypto'
import { createHasher } from '../src/lib/sha256.ts'

const nodeHex = (data: Uint8Array) => createHash('sha256').update(data).digest('hex')

describe('sha256', () => {
  it('matches the published vectors', () => {
    const empty = createHasher()
    assert.equal(
      empty.digestHex(),
      'e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855',
    )

    const abc = createHasher()
    abc.update(new TextEncoder().encode('abc'))
    assert.equal(
      abc.digestHex(),
      'ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad',
    )

    const twoBlocks = createHasher()
    twoBlocks.update(new TextEncoder().encode('abcdbcdecdefdefgefghfghighijhijkijkljklmklmnlmnomnopnopq'))
    assert.equal(
      twoBlocks.digestHex(),
      '248d6a61d20638b8e5c026930c3e6039a33ce45964ff2167f6ecedd419db06c1',
    )
  })

  it('agrees with node crypto for every awkward length, however chunked', () => {
    const sizes = [1, 54, 55, 56, 57, 63, 64, 65, 119, 120, 127, 128, 129, 1000, 65536, 100003]
    for (const size of sizes) {
      const data = randomBytes(size)

      const whole = createHasher()
      whole.update(data)
      assert.equal(whole.digestHex(), nodeHex(data), `one-shot at ${size} bytes`)

      const chunked = createHasher()
      let off = 0
      let step = 1
      while (off < size) {
        const take = Math.min(step, size - off)
        chunked.update(data.subarray(off, off + take))
        off += take
        step = (step % 73) + 1
      }
      assert.equal(chunked.digestHex(), nodeHex(data), `chunked at ${size} bytes`)
    }
  })
})
