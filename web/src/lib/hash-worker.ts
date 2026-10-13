// hash worker separated to not freeze the tab during uploads

import { createHasher } from './sha256'

const worker = self as unknown as {
  postMessage(message: unknown): void
  onmessage: ((event: MessageEvent) => void) | null
}

worker.onmessage = async (event: MessageEvent) => {
  try {
    const file = event.data as Blob
    const hasher = createHasher()
    const reader = file.stream().getReader()
    for (;;) {
      const { done, value } = await reader.read()
      if (done) break
      if (value) hasher.update(value)
    }
    worker.postMessage({ hash: hasher.digestHex() })
  } catch (err) {
    worker.postMessage({ error: String(err) })
  }
}
