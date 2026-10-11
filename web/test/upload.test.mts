// Concurrency tests for the direct-upload path. They run on Node's built-in
// runner (`npm test`) with type stripping — no test framework dependency —
// and live outside src/ so the app's tsconfig and bundle ignore them.
import { describe, it } from 'node:test'
import assert from 'node:assert/strict'
import { retryWithBackoff, runPool, uploadDirectParts, type DirectDeps } from '../src/upload.ts'

const tick = () => new Promise<void>((r) => setTimeout(r, 0))

function deferred<T>() {
  let resolve!: (v: T) => void
  let reject!: (e: unknown) => void
  const promise = new Promise<T>((res, rej) => {
    resolve = res
    reject = rej
  })
  return { promise, resolve, reject }
}

describe('runPool', () => {
  it('caps in-flight tasks and keeps result order', async () => {
    let inflight = 0
    let maxInflight = 0
    const tasks = Array.from({ length: 6 }, (_, i) => async () => {
      inflight++
      maxInflight = Math.max(maxInflight, inflight)
      await tick()
      inflight--
      return i * 10
    })
    const results = await runPool(tasks, 3)
    assert.deepEqual(results, [0, 10, 20, 30, 40, 50])
    assert.equal(maxInflight, 3)
  })

  it('runs sequentially with limit 1', async () => {
    const order: number[] = []
    const tasks = [1, 2, 3].map((i) => async () => {
      await tick()
      order.push(i)
      return i
    })
    assert.deepEqual(await runPool(tasks, 1), [1, 2, 3])
    assert.deepEqual(order, [1, 2, 3])
  })

  it('reports the first error once the in-flight tasks settle', async () => {
    const gate = deferred<void>()
    let settled = 0
    const tasks = [
      async () => {
        await gate.promise
        settled++
        return 'slow-ok'
      },
      async () => {
        settled++
        throw new Error('boom-part-2')
      },
      async () => {
        settled++
        return 'never-started'
      },
    ]
    const pending = runPool(tasks, 2)
    gate.resolve()
    await assert.rejects(pending, /boom-part-2/)
    // The in-flight task finished; the third never started.
    assert.equal(settled, 2)
  })

  it('aborts without starting new tasks', async () => {
    const controller = new AbortController()
    const started: number[] = []
    const tasks = [1, 2, 3].map((i) => async () => {
      started.push(i)
      await tick()
      return i
    })
    const pending = runPool(tasks, 1, controller.signal)
    controller.abort()
    await assert.rejects(pending, (e: unknown) => e instanceof DOMException && e.name === 'AbortError')
    assert.ok(started.length <= 1, `started ${started}`)
  })
})

describe('retryWithBackoff', () => {
  it('succeeds after transient failures', async () => {
    let attempts = 0
    const result = await retryWithBackoff(
      () => {
        attempts++
        if (attempts < 3) throw new Error('flaky')
        return Promise.resolve('landed')
      },
      undefined,
      4,
      1,
    )
    assert.equal(result, 'landed')
    assert.equal(attempts, 3)
  })

  it('gives up after the budgeted tries', async () => {
    let attempts = 0
    await assert.rejects(
      retryWithBackoff(
        () => {
          attempts++
          return Promise.reject(new Error('down'))
        },
        undefined,
        2,
        1,
      ),
      /down/,
    )
    assert.equal(attempts, 2)
  })

  it('never retries an abort', async () => {
    const controller = new AbortController()
    controller.abort()
    let attempts = 0
    await assert.rejects(
      retryWithBackoff(
        () => {
          attempts++
          return Promise.reject(new DOMException('aborted', 'AbortError'))
        },
        controller.signal,
        4,
        1,
      ),
      (e: unknown) => e instanceof DOMException && e.name === 'AbortError',
    )
    assert.equal(attempts, 1)
  })
})

describe('uploadDirectParts', () => {
  const partSize = 10
  const file = new Blob([new Uint8Array(50)]) // 5 parts

  /** Fake deps with gateable PUTs: every part resolves only when released,
   *  so completion order is fully controlled. */
  function fakeDeps(release: (n: number) => Promise<void>) {
    const puts: number[] = []
    let inflight = 0
    let maxInflight = 0
    let completed = false
    const landed = new Set<number>()
    const deps: DirectDeps = {
      partURLs: async (_session, parts) => new Map(parts.map((n) => [n, `https://put/part-${n}`])),
      putPart: async (_url, _body, report, signal) => {
        const n = puts.length + 1
        puts.push(n)
        inflight++
        maxInflight = Math.max(maxInflight, inflight)
        try {
          signal?.throwIfAborted()
          await release(n)
          report(10, true)
          landed.add(n)
        } finally {
          inflight--
        }
      },
      currentOffset: async () => {
        // Committed bytes: the leading consecutive run of landed parts.
        let done = 0
        for (let n = 1; done < 50; n++) {
          if (!landed.has(n)) break
          done += 10
        }
        return done
      },
      completeSession: async () => {
        completed = true
      },
    }
    return { deps, puts, maxInflight: () => maxInflight, completed: () => completed }
  }

  it('uploads parts concurrently and completes in any order', async () => {
    // Parts resolve latest-first, forcing out-of-order completion.
    const order: number[] = []
    const f = fakeDeps(async (n) => {
      order.push(n)
      await new Promise<void>((r) => setTimeout(r, (6 - n) * 5))
    })
    const progress: number[] = []
    await uploadDirectParts(
      file,
      'session',
      partSize,
      (sent) => progress.push(sent),
      undefined,
      3,
      f.deps,
      { tries: 2, baseMs: 1 },
    )
    assert.deepEqual([...f.puts].sort((a, b) => a - b), [1, 2, 3, 4, 5])
    assert.ok(f.maxInflight() > 1, `parts flew one at a time (max ${f.maxInflight()})`)
    assert.ok(f.maxInflight() <= 3, `over the cap (max ${f.maxInflight()})`)
    assert.ok(f.completed(), 'completeSession was not called')
    assert.ok(progress.length > 0, 'no progress reported')
    assert.ok(progress.every((s) => s <= 50), `progress exceeded total: ${progress}`)
    assert.equal(progress[progress.length - 1], 50)
  })

  it('retries a failed part and still completes', async () => {
    let part2Attempts = 0
    const urls = new Map([1, 2, 3].map((n) => [n, `https://put/part-${n}`]))
    const landed = new Set<number>()
    let completed = false
    const threeParts = new Blob([new Uint8Array(30)])
    const deps: DirectDeps = {
      partURLs: async () => urls,
      putPart: async (url, _body, report) => {
        const n = Number(url.split('-')[1])
        if (n === 2 && ++part2Attempts === 1) throw new Error('transient')
        await tick()
        report(10, true)
        landed.add(n)
      },
      currentOffset: async () => (landed.has(1) && landed.has(2) && landed.has(3) ? 30 : 0),
      completeSession: async () => {
        completed = true
      },
    }
    await uploadDirectParts(threeParts, 'session', partSize, () => {}, undefined, 3, deps, {
      tries: 3,
      baseMs: 1,
    })
    assert.equal(part2Attempts, 2)
    assert.ok(completed, 'completeSession was not called')
  })

  it('aborts mid-flight without completing', async () => {
    const controller = new AbortController()
    const f = fakeDeps(async () => {
      await new Promise<void>((_r, reject) => {
        controller.signal.addEventListener('abort', () =>
          reject(new DOMException('aborted', 'AbortError')),
        )
      })
    })
    const pending = uploadDirectParts(file, 'session', partSize, () => {}, controller.signal, 3, f.deps, {
      tries: 2,
      baseMs: 1,
    })
    await tick()
    controller.abort()
    await assert.rejects(
      pending,
      (e: unknown) => e instanceof DOMException && e.name === 'AbortError',
    )
    assert.ok(!f.completed(), 'completeSession must not run after an abort')
  })

  it('publishes an empty file with no parts', async () => {
    let puts = 0
    let completed = false
    const deps: DirectDeps = {
      partURLs: async () => new Map(),
      putPart: async () => {
        puts++
      },
      currentOffset: async () => 0,
      completeSession: async () => {
        completed = true
      },
    }
    await uploadDirectParts(new Blob([]), 'session', partSize, () => {}, undefined, 3, deps)
    assert.equal(puts, 0)
    assert.ok(completed, 'completeSession was not called')
  })

  it('fails clearly when the server will not sign a part', async () => {
    const deps: DirectDeps = {
      partURLs: async () => new Map(),
      putPart: async () => {},
      currentOffset: async () => 0,
      completeSession: async () => {},
    }
    await assert.rejects(
      uploadDirectParts(file, 'session', partSize, () => {}, undefined, 3, deps, { tries: 1, baseMs: 1 }),
      /did not sign part 1/,
    )
  })
})
