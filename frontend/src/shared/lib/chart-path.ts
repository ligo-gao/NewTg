export interface ChartXY {
  x: number
  y: number
}

/**
 * Monotone cubic interpolation (Fritsch–Carlson). Unlike Catmull-Rom it never
 * overshoots the data, so a cost curve cannot dip below zero or spike above a
 * local maximum just because the smoothing looked nicer.
 */
export function smoothPath(points: ChartXY[]): string {
  const count = points.length
  if (count === 0) return ''
  const first = points[0]
  if (first === undefined) return ''
  if (count === 1) return `M ${first.x} ${first.y}`

  const dx: number[] = []
  const slope: number[] = []
  for (let index = 0; index < count - 1; index += 1) {
    const current = points[index]
    const next = points[index + 1]
    if (current === undefined || next === undefined) continue
    const deltaX = next.x - current.x
    dx.push(deltaX)
    slope.push(deltaX === 0 ? 0 : (next.y - current.y) / deltaX)
  }

  const tangents: number[] = new Array(count).fill(0)
  tangents[0] = slope[0] ?? 0
  tangents[count - 1] = slope[count - 2] ?? 0
  for (let index = 1; index < count - 1; index += 1) {
    const before = slope[index - 1] ?? 0
    const after = slope[index] ?? 0
    if (before * after <= 0) {
      tangents[index] = 0
      continue
    }
    const weightBefore = 2 * (dx[index] ?? 1) + (dx[index - 1] ?? 1)
    const weightAfter = (dx[index] ?? 1) + 2 * (dx[index - 1] ?? 1)
    tangents[index] =
      (weightBefore + weightAfter) /
      (weightBefore / before + weightAfter / after)
  }

  let path = `M ${first.x} ${first.y}`
  for (let index = 0; index < count - 1; index += 1) {
    const current = points[index]
    const next = points[index + 1]
    if (current === undefined || next === undefined) continue
    const span = dx[index] ?? 0
    const control1X = current.x + span / 3
    const control1Y = current.y + (tangents[index] ?? 0) * (span / 3)
    const control2X = next.x - span / 3
    const control2Y = next.y - (tangents[index + 1] ?? 0) * (span / 3)
    path += ` C ${control1X.toFixed(2)} ${control1Y.toFixed(2)}, ${control2X.toFixed(2)} ${control2Y.toFixed(2)}, ${next.x} ${next.y}`
  }
  return path
}

/** Closes a smooth line down to the baseline so it can be filled as an area. */
export function smoothAreaPath(points: ChartXY[], baseline: number): string {
  const first = points[0]
  const last = points[points.length - 1]
  const line = smoothPath(points)
  if (first === undefined || last === undefined || line === '') return ''
  return `${line} L ${last.x} ${baseline} L ${first.x} ${baseline} Z`
}

/** Rounds a maximum up to a 1/2/5 × 10^n step so axis ticks read cleanly. */
export function niceCeil(value: number): number {
  if (!Number.isFinite(value) || value <= 0) return 1
  const exponent = Math.floor(Math.log10(value))
  const magnitude = 10 ** exponent
  const fraction = value / magnitude
  const step = fraction <= 1 ? 1 : fraction <= 2 ? 2 : fraction <= 5 ? 5 : 10
  return step * magnitude
}
