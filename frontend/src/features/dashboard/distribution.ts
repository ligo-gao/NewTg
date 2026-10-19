import type { UsageStatsByModelItem } from './api'

/**
 * Model-distribution normalisation (modules-05 §3.4).
 *
 * The server already collapses beyond the top 8, but the client repeats the
 * fold defensively: a contract-compliant response with nine plain rows must
 * still render eight named slices plus Other, because the ring has exactly
 * eight categorical colour slots. Percentages are recomputed here instead of
 * trusting the wire values so the fold cannot leave the arcs summing to more
 * than 100 (or a single zero-cost row drawing a full ring).
 */

export interface DonutSlice {
  /** Stable DOM key: the model name, or `other`. */
  key: string
  model: string
  cost: number
  /** 0…100, recomputed over the folded set; the values sum to exactly 100. */
  percent: number
  color: string
  isOther: boolean
}

const MAX_NAMED_SLICES = 8
const OTHER_KEY = 'other'

export function isOtherModel(model: string): boolean {
  return model.trim().toLowerCase() === OTHER_KEY
}

/**
 * Deterministic colour slot for a model entity. The spec requires a model to
 * keep the same colour when rankings or filters change — assigning slots in
 * rank order would recolour every survivor whenever the leader changes. The
 * API exposes no entity→slot map, so a stable hash is the only client-side
 * way to honour that rule; collisions simply reuse a slot. `other` never
 * consumes a categorical slot (it uses the neutral token).
 */
export function modelColor(model: string): string {
  let hash = 0x811c9dc5
  for (let index = 0; index < model.length; index += 1) {
    hash ^= model.charCodeAt(index)
    hash = Math.imul(hash, 0x01000193) >>> 0
  }
  const slot = (hash % MAX_NAMED_SLICES) + 1
  return `var(--chart-cat-${slot})`
}

interface Candidate {
  model: string
  cost: number
  isOther: boolean
}

export function normalizeModelDistribution(
  items: UsageStatsByModelItem[],
): DonutSlice[] {
  const named = items
    .filter((item) => Number.isFinite(item.cost) && !isOtherModel(item.model))
    .sort((a, b) => b.cost - a.cost || a.model.localeCompare(b.model))

  let otherCost = items
    .filter((item) => Number.isFinite(item.cost) && isOtherModel(item.model))
    .reduce((sum, item) => sum + item.cost, 0)

  if (named.length > MAX_NAMED_SLICES) {
    for (const folded of named.slice(MAX_NAMED_SLICES)) {
      otherCost += folded.cost
    }
  }

  const candidates: Candidate[] = named
    .slice(0, MAX_NAMED_SLICES)
    .map((item) => ({ model: item.model, cost: item.cost, isOther: false }))
  if (otherCost > 0) {
    candidates.push({ model: OTHER_KEY, cost: otherCost, isOther: true })
  }

  const total = candidates.reduce((sum, item) => sum + item.cost, 0)
  if (total <= 0) return []

  // Integer basis points + residual on the largest slice mirrors the backend
  // allocator: percentages sum to exactly 100.00 without float drift.
  const basisPoints = candidates.map((item) =>
    Math.round((item.cost * 10_000) / total),
  )
  const residual = 10_000 - basisPoints.reduce((sum, value) => sum + value, 0)
  if (residual !== 0 && basisPoints.length > 0) {
    basisPoints[0] = (basisPoints[0] ?? 0) + residual
  }

  return candidates.map((item, index) => ({
    key: item.isOther ? OTHER_KEY : item.model,
    model: item.model,
    cost: item.cost,
    percent: (basisPoints[index] ?? 0) / 100,
    color: item.isOther ? 'var(--neutral-fg)' : modelColor(item.model),
    isOther: item.isOther,
  }))
}
