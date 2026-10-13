// tested against node crypto and verified, smaller than getting another library

const K = new Uint32Array([
  0x428a2f98, 0x71374491, 0xb5c0fbcf, 0xe9b5dba5, 0x3956c25b, 0x59f111f1, 0x923f82a4, 0xab1c5ed5,
  0xd807aa98, 0x12835b01, 0x243185be, 0x550c7dc3, 0x72be5d74, 0x80deb1fe, 0x9bdc06a7, 0xc19bf174,
  0xe49b69c1, 0xefbe4786, 0x0fc19dc6, 0x240ca1cc, 0x2de92c6f, 0x4a7484aa, 0x5cb0a9dc, 0x76f988da,
  0x983e5152, 0xa831c66d, 0xb00327c8, 0xbf597fc7, 0xc6e00bf3, 0xd5a79147, 0x06ca6351, 0x14292967,
  0x27b70a85, 0x2e1b2138, 0x4d2c6dfc, 0x53380d13, 0x650a7354, 0x766a0abb, 0x81c2c92e, 0x92722c85,
  0xa2bfe8a1, 0xa81a664b, 0xc24b8b70, 0xc76c51a3, 0xd192e819, 0xd6990624, 0xf40e3585, 0x106aa070,
  0x19a4c116, 0x1e376c08, 0x2748774c, 0x34b0bcb5, 0x391c0cb3, 0x4ed8aa4a, 0x5b9cca4f, 0x682e6ff3,
  0x748f82ee, 0x78a5636f, 0x84c87814, 0x8cc70208, 0x90befffa, 0xa4506ceb, 0xbef9a3f7, 0xc67178f2,
])

export type Hasher = {
  update(data: Uint8Array): void
  digestHex(): string
}

export function createHasher(): Hasher {
  const h = new Uint32Array([
    0x6a09e667, 0xbb67ae85, 0x3c6ef372, 0xa54ff53a, 0x510e527f, 0x9b05688c, 0x1f83d9ab, 0x5be0cd19,
  ])
  const block = new Uint8Array(64)
  const w = new Uint32Array(64)
  let blockLen = 0
  let totalBytes = 0

  const compress = (data: Uint8Array, off: number) => {
    for (let i = 0; i < 16; i++) {
      w[i] =
        ((data[off + i * 4]! << 24) |
          (data[off + i * 4 + 1]! << 16) |
          (data[off + i * 4 + 2]! << 8) |
          data[off + i * 4 + 3]!) >>>
        0
    }
    for (let i = 16; i < 64; i++) {
      const x = w[i - 15]!
      const y = w[i - 2]!
      const s0 = ((x >>> 7) | (x << 25)) ^ ((x >>> 18) | (x << 14)) ^ (x >>> 3)
      const s1 = ((y >>> 17) | (y << 15)) ^ ((y >>> 19) | (y << 13)) ^ (y >>> 10)
      w[i] = (w[i - 16]! + s0 + w[i - 7]! + s1) >>> 0
    }
    let a = h[0]!
    let b = h[1]!
    let c = h[2]!
    let d = h[3]!
    let e = h[4]!
    let f = h[5]!
    let g = h[6]!
    let hh = h[7]!
    for (let i = 0; i < 64; i++) {
      const s1 = ((e >>> 6) | (e << 26)) ^ ((e >>> 11) | (e << 21)) ^ ((e >>> 25) | (e << 7))
      const ch = (e & f) ^ (~e & g)
      const t1 = (hh + s1 + ch + K[i]! + w[i]!) >>> 0
      const s0 = ((a >>> 2) | (a << 30)) ^ ((a >>> 13) | (a << 19)) ^ ((a >>> 22) | (a << 10))
      const maj = (a & b) ^ (a & c) ^ (b & c)
      const t2 = (s0 + maj) >>> 0
      hh = g
      g = f
      f = e
      e = (d + t1) >>> 0
      d = c
      c = b
      b = a
      a = (t1 + t2) >>> 0
    }
    h[0] = (h[0]! + a) >>> 0
    h[1] = (h[1]! + b) >>> 0
    h[2] = (h[2]! + c) >>> 0
    h[3] = (h[3]! + d) >>> 0
    h[4] = (h[4]! + e) >>> 0
    h[5] = (h[5]! + f) >>> 0
    h[6] = (h[6]! + g) >>> 0
    h[7] = (h[7]! + hh) >>> 0
  }

  const update = (data: Uint8Array) => {
    totalBytes += data.length
    let i = 0
    if (blockLen > 0) {
      const take = Math.min(64 - blockLen, data.length)
      block.set(data.subarray(0, take), blockLen)
      blockLen += take
      i = take
      if (blockLen === 64) {
        compress(block, 0)
        blockLen = 0
      }
    }
    for (; i + 64 <= data.length; i += 64) compress(data, i)
    if (i < data.length) {
      block.set(data.subarray(i), 0)
      blockLen = data.length - i
    }
  }

  const digestHex = () => {
    const bitsLo = (totalBytes * 8) >>> 0
    const bitsHi = Math.floor((totalBytes * 8) / 0x100000000)
    const padLen = blockLen < 56 ? 64 - blockLen : 128 - blockLen
    const pad = new Uint8Array(padLen)
    pad[0] = 0x80
    const view = new DataView(pad.buffer)
    view.setUint32(padLen - 8, bitsHi)
    view.setUint32(padLen - 4, bitsLo)
    update(pad)
    let hex = ''
    for (let i = 0; i < 8; i++) hex += h[i]!.toString(16).padStart(8, '0')
    return hex
  }

  return { update, digestHex }
}
