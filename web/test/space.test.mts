// storageGauge: members see owned bytes, admins see the instance total
import { describe, it } from 'node:test'
import assert from 'node:assert/strict'

import { storageGauge } from '../src/lib/space.ts'

const GiB = 1073741824

/** space builds an /fs/space report with the given fields overridden. */
function space(over: Record<string, number> = {}) {
  return {
    available: 0,
    total: 0,
    reserve: 0,
    read_only: false,
    used: 0,
    quota: 0,
    instance_used: 0,
    instance_total: 0,
    ...over,
  }
}

describe('storageGauge', () => {
  it('is null before a space report exists', () => {
    assert.equal(storageGauge(null, false), null)
    assert.equal(storageGauge(null, true), null)
  })

  it('a member is measured by their own owned bytes against their quota', () => {
    const g = storageGauge(space({ used: 33792, quota: GiB }), false)
    assert.ok(g)
    assert.equal(g.used, 33792)
    assert.equal(g.total, GiB)
    assert.equal(g.own, null, 'members have no second line')
    assert.equal(g.over, false)
    assert.ok(g.percent > 0 && g.percent < 1, `percent ${g.percent}`)
  })

  it('an unlimited member has no cap, so no bar position and no over state', () => {
    const g = storageGauge(space({ used: 33792, quota: 0 }), false)
    assert.ok(g)
    assert.equal(g.total, 0)
    assert.equal(g.percent, 0)
    assert.equal(g.over, false)
  })

  it('a member over their personal quota is marked over', () => {
    const g = storageGauge(space({ used: 4096, quota: 2048 }), false)
    assert.ok(g)
    assert.equal(g.over, true)
    assert.equal(g.percent, 100, 'the bar clamps at 100')
  })

  it('an admin is led by the instance total, which ignores ownership', () => {
    const g = storageGauge(
      space({ used: 33792, quota: 0, instance_used: 350 * 1024 * 1024, instance_total: GiB }),
      true,
    )
    assert.ok(g)
    assert.equal(g.used, 350 * 1024 * 1024, 'headline counts every file, not just owned ones')
    assert.equal(g.total, GiB)
    assert.deepEqual(g.own, { used: 33792, quota: 0 }, 'their own bytes ride along underneath')
    assert.equal(g.over, false)
    assert.ok(Math.abs(g.percent - 34.18) < 0.01, `percent ${g.percent}`)
  })

  it('an admin over the instance total is marked over', () => {
    const g = storageGauge(
      space({ instance_used: GiB + 1, instance_total: GiB }),
      true,
    )
    assert.ok(g)
    assert.equal(g.over, true)
    assert.equal(g.percent, 100)
  })

  it('an admin over their own quota is marked over even when the instance fits', () => {
    const g = storageGauge(
      space({ used: 4096, quota: 2048, instance_used: 1024, instance_total: GiB }),
      true,
    )
    assert.ok(g)
    assert.equal(g.over, true)
  })

  it('an admin with no instance data falls back to exactly the member view', () => {
    const g = storageGauge(space({ used: 33792, quota: GiB }), true)
    assert.ok(g)
    assert.equal(g.used, 33792)
    assert.equal(g.total, GiB)
    assert.equal(g.own, null)
  })

  it('an empty instance still counts as known for an admin', () => {
    const g = storageGauge(
      space({ used: 0, quota: GiB, instance_used: 0, instance_total: GiB }),
      true,
    )
    assert.ok(g)
    assert.equal(g.used, 0)
    assert.equal(g.total, GiB)
    assert.deepEqual(g.own, { used: 0, quota: GiB })
  })

  it('clamps negative figures instead of drawing a bar backwards', () => {
    const g = storageGauge(space({ used: -5, quota: -5, instance_used: -1, instance_total: -1 }), true)
    assert.ok(g)
    assert.equal(g.used, 0)
    assert.equal(g.total, 0)
    assert.equal(g.percent, 0)
    assert.equal(g.over, false)
  })
})
