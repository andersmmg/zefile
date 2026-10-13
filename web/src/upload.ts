// A tus core client, written here rather than pulled in.
//
// The protocol's core is three requests, and an off-the-shelf client would
// bring a plugin architecture and a bundle several times this file's size for
// extensions the server does not implement.

export type UploadProgress = {
  /** Stable identity for the transfer, so same-named files never collide. */
  id: string
  name: string
  sent: number
  total: number
  status: 'queued' | 'uploading' | 'done' | 'error' | 'cancelled'
  /** Bytes per second, smoothed. Present while a transfer is in flight. */
  speed?: number
  error?: string
}

/** chunkSize bounds a single request body.
 *
 * Small enough that a dropped connection costs little to redo, large enough
 * that per-request overhead stays negligible on a fast link. */
const chunkSize = 8 * 1024 * 1024

function encodeMetadata(entries: Record<string, string>): string {
  return Object.entries(entries)
    .filter(([, value]) => value !== '')
    .map(([key, value]) => `${key} ${btoa(String.fromCharCode(...new TextEncoder().encode(value)))}`)
    .join(',')
}

/**
 * uploadFile transfers one file, reporting progress and speed as it goes.
 *
 * The session creation answers which mode to speak: `tus` stages chunks
 * through the server, `direct` hands back a part size and expects every part
 * PUT straight to storage at presigned URLs. The chunk write goes through
 * XMLHttpRequest rather than fetch for one reason: fetch reports nothing
 * until a request finishes, so a whole chunk would land as a single jump.
 * XHR emits upload progress byte by byte, which is what lets the bar move
 * and a speed be measured.
 *
 * A failure part way is not fatal to the bytes already sent: the session keeps
 * them, and calling this again resumes. The signal lets the caller abandon a
 * transfer without abandoning what arrived.
 */
export async function uploadFile(
  file: File,
  targetPath: string,
  onProgress: (sent: number, speed: number) => void,
  signal?: AbortSignal,
  parallelism = defaultParallelism,
): Promise<void> {
  const created = await fetch('/api/v1/uploads', {
    method: 'POST',
    credentials: 'same-origin',
    headers: {
      'Tus-Resumable': '1.0.0',
      'Upload-Length': String(file.size),
      'Upload-Metadata': encodeMetadata({ path: targetPath }),
    },
    signal,
  })
  if (!created.ok) {
    throw new Error(await describeFailure(created))
  }

  const location = created.headers.get('Location')
  if (!location) throw new Error('the server did not say where to send the file')

  const session = (await created.json().catch(() => null)) as {
    mode?: string
    part_size?: number
  } | null
  if (session?.mode === 'direct' && Number(session.part_size) > 0) {
    // The browser hashes the file while it streams out
    const claim = hashFile(file, signal).catch(() => undefined)
    await uploadDirect(file, location, Number(session.part_size), onProgress, signal, parallelism, claim)
    return
  }

  const report = makeReporter(onProgress)

  let offset = 0
  while (offset < file.size) {
    if (signal?.aborted) throw new DOMException('aborted', 'AbortError')

    const end = Math.min(offset + chunkSize, file.size)
    const next = await patchChunk(location, offset, file.slice(offset, end), report, signal)

    // A conflict means the server is at a different offset than assumed — after
    // a retry, say. Asking where it got to is the documented recovery, and it is
    // why this is resumable rather than merely retryable.
    if (next === null) {
      offset = await currentOffset(location, signal)
      continue
    }
    offset = next
    report(offset, true)
  }
}

/** patchChunk sends one chunk and resolves the server's new offset, or null on a
 *  409 conflict. Progress is reported as the bytes leave. */
function patchChunk(
  location: string,
  offset: number,
  body: Blob,
  report: (sent: number, force?: boolean) => void,
  signal?: AbortSignal,
): Promise<number | null> {
  return new Promise((resolve, reject) => {
    const xhr = new XMLHttpRequest()
    xhr.open('PATCH', location)
    xhr.withCredentials = true
    xhr.setRequestHeader('Tus-Resumable', '1.0.0')
    xhr.setRequestHeader('Content-Type', 'application/offset+octet-stream')
    xhr.setRequestHeader('Upload-Offset', String(offset))

    const onAbort = () => xhr.abort()
    signal?.addEventListener('abort', onAbort, { once: true })
    const done = () => signal?.removeEventListener('abort', onAbort)

    xhr.upload.onprogress = (e) => {
      if (e.lengthComputable) report(offset + e.loaded)
    }
    xhr.onload = () => {
      done()
      if (xhr.status === 409) {
        resolve(null)
        return
      }
      if (xhr.status < 200 || xhr.status >= 300) {
        reject(new Error(describeXhr(xhr)))
        return
      }
      const reported = xhr.getResponseHeader('Upload-Offset')
      resolve(reported ? Number(reported) : offset + body.size)
    }
    xhr.onerror = () => {
      done()
      reject(new Error('the connection dropped during the upload'))
    }
    xhr.onabort = () => {
      done()
      reject(new DOMException('aborted', 'AbortError'))
    }

    xhr.send(body)
  })
}

/** makeReporter smooths a speed from the stream of offsets and throttles how
 *  often the caller is told, so a fast link does not trigger hundreds of state
 *  updates a second. */
function makeReporter(onProgress: (sent: number, speed: number) => void) {
  let lastSent = 0
  let lastTime = performance.now()
  let speed = 0
  let lastReport = 0

  return (sent: number, force = false) => {
    const now = performance.now()
    const dt = now - lastTime
    if (dt >= 250) {
      const instant = ((sent - lastSent) / dt) * 1000
      // Exponential moving average: enough smoothing that the number is
      // readable, little enough lag that it tracks a real slowdown.
      speed = speed === 0 ? instant : 0.4 * instant + 0.6 * speed
      lastSent = sent
      lastTime = now
    }
    if (force || now - lastReport >= 100) {
      lastReport = now
      onProgress(sent, Math.max(0, speed))
    }
  }
}

/** partBatch caps how many presigned URLs one round trip mints. The client
 *  maps its own chunking onto part numbers without asking, so batches only
 *  refill the window a few parts ahead. */
const partBatch = 8

/** hashFile computes a file's SHA-256 in a background worker while the upload
 *  streams. The hash is sent as a client claim; if hashing fails or is aborted,
 *  the upload still completes without it. */
function hashFile(file: Blob, signal?: AbortSignal): Promise<string> {
  return new Promise((resolve, reject) => {
    const worker = new Worker(new URL('./lib/hash-worker', import.meta.url), { type: 'module' })
    const finish = (settle: () => void) => {
      worker.terminate()
      signal?.removeEventListener('abort', onAbort)
      settle()
    }
    const onAbort = () => finish(() => reject(new DOMException('aborted', 'AbortError')))
    signal?.addEventListener('abort', onAbort, { once: true })
    worker.onmessage = (event: MessageEvent) => {
      const data = event.data as { hash?: string; error?: string }
      const hash = data.hash
      if (typeof hash === 'string') finish(() => resolve(hash))
      else finish(() => reject(new Error(data.error || 'hashing failed')))
    }
    worker.onerror = (event) => {
      finish(() => reject(new Error(event.message || 'the hash worker failed')))
    }
    worker.postMessage(file)
  })
}

/** defaultParallelism caps concurrent part PUTs. */
export const defaultParallelism = 4

export type DirectDeps = {
  partURLs: (session: string, parts: number[], signal?: AbortSignal) => Promise<Map<number, string>>
  putPart: (
    url: string,
    body: Blob,
    report: (loaded: number, force?: boolean) => void,
    signal?: AbortSignal,
  ) => Promise<void>
  currentOffset: (session: string, signal?: AbortSignal) => Promise<number>
  completeSession: (session: string, signal?: AbortSignal, claim?: string) => Promise<void>
}

/** runPool runs tasks with at most `limit` in flight, results in task order; Exported for tests. */
export async function runPool<T>(
  tasks: (() => Promise<T>)[],
  limit: number,
  signal?: AbortSignal,
): Promise<T[]> {
  const results = new Array<T>(tasks.length)
  let next = 0
  let failed = false
  let firstError: unknown = null
  const workers = Array.from({ length: Math.max(0, Math.min(limit, tasks.length)) }, async () => {
    for (;;) {
      if (signal?.aborted || failed) return
      const i = next++
      if (i >= tasks.length) return
      try {
        results[i] = await tasks[i]!()
      } catch (err) {
        if (!failed) {
          failed = true
          firstError = err
        }
        return
      }
    }
  })
  await Promise.all(workers)
  if (signal?.aborted) throw new DOMException('aborted', 'AbortError')
  if (failed) throw firstError
  return results
}

function sleepOrAbort(ms: number, signal?: AbortSignal): Promise<void> {
  if (signal?.aborted) return Promise.reject(new DOMException('aborted', 'AbortError'))
  return new Promise((resolve, reject) => {
    const timer = setTimeout(() => {
      cleanup()
      resolve()
    }, ms)
    const onAbort = () => {
      clearTimeout(timer)
      cleanup()
      reject(new DOMException('aborted', 'AbortError'))
    }
    const cleanup = () => signal?.removeEventListener('abort', onAbort)
    signal?.addEventListener('abort', onAbort, { once: true })
  })
}

/** retryWithBackoff retries a transient failure with capped exponential
 *  backoff. Aborts propagate at once and never retry. Exported for tests. */
export async function retryWithBackoff<T>(
  fn: () => Promise<T>,
  signal?: AbortSignal,
  tries = 4,
  baseDelayMs = 500,
): Promise<T> {
  let delay = baseDelayMs
  for (;;) {
    try {
      return await fn()
    } catch (err) {
      if (signal?.aborted || (err instanceof DOMException && err.name === 'AbortError')) throw err
      if (tries <= 1) throw err
      tries--
      await sleepOrAbort(delay, signal)
      delay = Math.min(delay * 2, 8000)
    }
  }
}

/**
 * uploadDirect sends every part of a file straight to storage.
 *
 * The server never sees a byte: it minted the session (a provider-side
 * multipart upload) and now only hands out short-lived PUT URLs and, at the
 * end, completes the upload once storage holds every declared byte. Parts
 * are committed atomically, so after a failure the session offset tells
 * exactly which parts to skip — resuming never re-sends a part that landed.
 * Parts within a URL batch fly concurrently (completion order does not
 * matter: parts are numbered), each with its own retry.
 */
async function uploadDirect(
  file: File,
  session: string,
  partSize: number,
  onProgress: (sent: number, speed: number) => void,
  signal?: AbortSignal,
  parallelism = defaultParallelism,
  claim?: Promise<string | undefined>,
): Promise<void> {
  await uploadDirectParts(
    file,
    session,
    partSize,
    onProgress,
    signal,
    parallelism,
    {
      partURLs,
      putPart,
      currentOffset,
      completeSession,
    },
    undefined,
    claim,
  )
}

/** uploadDirectParts is uploadDirect with injectable network, so tests can
 *  drive the concurrent path without a server. Exported for tests. */
export async function uploadDirectParts(
  file: Blob,
  session: string,
  partSize: number,
  onProgress: (sent: number, speed: number) => void,
  signal: AbortSignal | undefined,
  parallelism: number,
  deps: DirectDeps,
  retry?: { tries: number; baseMs: number },
  claim?: Promise<string | undefined>,
): Promise<void> {
  const report = makeReporter(onProgress)
  const totalParts = Math.ceil(file.size / partSize)

  // An empty file has no parts: completing publishes the empty object.
  if (totalParts === 0) {
    await deps.completeSession(session, signal, await claim)
    report(0, true)
    return
  }

  const parallel = Math.min(Math.max(1, parallelism), partBatch)
  const tries = retry?.tries ?? 4
  const baseMs = retry?.baseMs ?? 500
  let done = await deps.currentOffset(session, signal)
  while (done < file.size) {
    if (signal?.aborted) throw new DOMException('aborted', 'AbortError')

    // Parts before this one are committed; the window starts past them.
    const firstPart = Math.floor(done / partSize) + 1
    const want: number[] = []
    for (let n = firstPart; n <= totalParts && want.length < partBatch; n++) want.push(n)
    const urls = await deps.partURLs(session, want, signal)

    // Progress is the committed offset plus what the in-flight parts report.
    const inflight = new Map<number, number>()
    const aggregate = (force = false) => {
      let loaded = 0
      for (const v of inflight.values()) loaded += v
      report(Math.min(file.size, done + loaded), force)
    }
    const uploads = want.map((n) => async () => {
      const url = urls.get(n)
      if (!url) throw new Error(`the server did not sign part ${n}`)
      const start = (n - 1) * partSize
      const end = Math.min(start + partSize, file.size)
      await retryWithBackoff(
        () =>
          deps.putPart(
            url,
            file.slice(start, end),
            (loaded, force) => {
              inflight.set(n, loaded)
              aggregate(force)
            },
            signal,
          ),
        signal,
        tries,
        baseMs,
      )
      inflight.delete(n)
      aggregate()
    })
    await runPool(uploads, parallel, signal)

    // The offset is what storage acknowledges; re-read it so retries skip parts.
    done = Math.max(done, await deps.currentOffset(session, signal))
    report(done, true)
  }

  await deps.completeSession(session, signal, await claim)
  report(file.size, true)
}

/** partURLs mints one bearer PUT URL per part number. */
async function partURLs(session: string, parts: number[], signal?: AbortSignal): Promise<Map<number, string>> {
  const response = await fetch(`${session}/parts`, {
    method: 'POST',
    credentials: 'same-origin',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ parts }),
    signal,
  })
  if (!response.ok) throw new Error(await describeFailure(response))
  const body = (await response.json()) as { parts: { number: number; url: string }[] }
  return new Map(body.parts.map((p) => [p.number, p.url]))
}

/** completeSession publishes the upload once storage holds every byte. A
 *  claim rides along when the caller hashed the file: the server stores it
 *  labelled as the browser's own, since these bytes never passed through. */
async function completeSession(session: string, signal?: AbortSignal, claim?: string): Promise<void> {
  const response = await fetch(`${session}/complete`, {
    method: 'POST',
    credentials: 'same-origin',
    headers: claim ? { 'Content-Type': 'application/json' } : undefined,
    body: claim ? JSON.stringify({ checksum: { algorithm: 'sha256', hash: claim } }) : undefined,
    signal,
  })
  if (!response.ok) throw new Error(await describeFailure(response))
}

/** putPart sends one part straight to storage, reporting the part-relative
 *  bytes as they leave (the caller aggregates across concurrent parts).
 *
 * No credentials ride along: the URL itself is the bearer credential, and
 * cookies would only fail a wildcard CORS answer. */
function putPart(
  url: string,
  body: Blob,
  report: (loaded: number, force?: boolean) => void,
  signal?: AbortSignal,
): Promise<void> {
  return new Promise((resolve, reject) => {
    const xhr = new XMLHttpRequest()
    xhr.open('PUT', url)
    xhr.withCredentials = false

    const onAbort = () => xhr.abort()
    signal?.addEventListener('abort', onAbort, { once: true })
    const done = () => signal?.removeEventListener('abort', onAbort)

    xhr.upload.onprogress = (e) => {
      if (e.lengthComputable) report(e.loaded)
    }
    xhr.onload = () => {
      done()
      if (xhr.status < 200 || xhr.status >= 300) {
        reject(new Error(describeXhr(xhr)))
        return
      }
      report(body.size, true)
      resolve()
    }
    xhr.onerror = () => {
      done()
      reject(new Error('the connection dropped during the upload'))
    }
    xhr.onabort = () => {
      done()
      reject(new DOMException('aborted', 'AbortError'))
    }

    xhr.send(body)
  })
}

async function currentOffset(location: string, signal?: AbortSignal): Promise<number> {
  const response = await fetch(location, {
    method: 'HEAD',
    credentials: 'same-origin',
    headers: { 'Tus-Resumable': '1.0.0' },
    signal,
  })
  if (!response.ok) throw new Error(await describeFailure(response))
  return Number(response.headers.get('Upload-Offset') ?? '0')
}

function describeXhr(xhr: XMLHttpRequest): string {
  try {
    const problem = JSON.parse(xhr.responseText) as { detail?: string; title?: string }
    return problem.detail || problem.title || `request failed (${xhr.status})`
  } catch {
    return `request failed (${xhr.status})`
  }
}

async function describeFailure(response: Response): Promise<string> {
  try {
    const problem = (await response.json()) as { detail?: string; title?: string }
    return problem.detail || problem.title || `request failed (${response.status})`
  } catch {
    return `request failed (${response.status})`
  }
}
