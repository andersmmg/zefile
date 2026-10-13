// What the storage meter shows, and to whom.
//
// `used` counts only the bytes this account *owns*
//
// An admin can see every file, including ones no account owns,
// so it shows the total as well.
import type { Space } from '@/api'

export type Gauge = {
  used: number
  total: number
  percent: number
  over: boolean
  /** admin only, just theirs as separate */
  own: { used: number; quota: number } | null
}

export function storageGauge(space: Space | null, isAdmin: boolean): Gauge | null {
  if (!space) return null

  const used = Math.max(0, space.used)
  const quota = Math.max(0, space.quota)
  const instanceUsed = Math.max(0, space.instance_used)
  const instanceTotal = Math.max(0, space.instance_total)

  const instanceKnown = instanceUsed > 0 || instanceTotal > 0
  const headline = isAdmin && instanceKnown

  const headUsed = headline ? instanceUsed : used
  const headTotal = headline ? instanceTotal : quota
  const own = headline ? { used, quota } : null

  const overHead = headTotal > 0 && headUsed > headTotal
  const ownOver = quota > 0 && used > quota

  return {
    used: headUsed,
    total: headTotal,
    percent: headTotal > 0 ? Math.min(100, (headUsed / headTotal) * 100) : 0,
    over: overHead || ownOver,
    own,
  }
}
