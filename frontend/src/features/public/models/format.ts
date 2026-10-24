/**
 * 模型库的读数格式化。
 *
 * 性能字段缺失表示「没有样本」，不是 0ms / 0 tokens/s：`null` 一律渲染
 * `—`。样本数为 0 则是真实的零，必须渲染 `0`。
 */

/** 时延条固定刻度：0–4000ms，超出部分贴边而不是扩张画布。 */
const LATENCY_SCALE_MS = 4_000

function clamp(value: number, min: number, max: number): number {
  if (value < min) return min
  if (value > max) return max
  return value
}

/** 349.6 → `350 ms`；≥10,000ms 用秒并保留一位小数（12_400 → `12.4 s`）。 */
export function formatMs(value: number | null): string {
  if (value === null || !Number.isFinite(value) || value < 0) return '—'
  if (value >= 10_000) return `${(value / 1_000).toFixed(1)} s`
  const rounded = new Intl.NumberFormat('en-US', {
    maximumFractionDigits: 0,
  }).format(Math.round(value))
  return `${rounded} ms`
}

/** 低于 10 保留一位小数，10 以上取整；缺失渲染 `—`。 */
export function formatTPS(value: number | null): string {
  if (value === null || !Number.isFinite(value) || value < 0) return '—'
  const text =
    value < 10
      ? new Intl.NumberFormat('en-US', {
          minimumFractionDigits: 1,
          maximumFractionDigits: 1,
        }).format(value)
      : new Intl.NumberFormat('en-US', {
          maximumFractionDigits: 0,
        }).format(Math.round(value))
  return `${text} t/s`
}

/** 0.998 → `99.8%`（一位小数）；缺失渲染 `—`。 */
export function formatPercent(value: number | null): string {
  if (value === null || !Number.isFinite(value)) return '—'
  return `${(value * 100).toFixed(1)}%`
}

/** 样本量：0 渲染 `0`；≥10,000 用 compact（10K / 1.2M）。 */
export function formatSamples(value: number, locale: string): string {
  if (value >= 10_000) {
    return new Intl.NumberFormat(locale, {
      notation: 'compact',
      maximumFractionDigits: 1,
    }).format(value)
  }
  return new Intl.NumberFormat(locale).format(value)
}

export interface LatencyStripGeometry {
  leftPct: number
  widthPct: number
  markerOnly: boolean
}

/**
 * P50–P95 带宽的百分比几何。
 *
 * `markerOnly`（没有 P95，或整个 P50 都缺失）时只画 P50 刻度点、宽度 0；
 * 有 P95 时右端按 `p95 - p50` 画带，且左右都不越出 0–100。
 */
export function latencyStripGeom(
  p50: number | null,
  p95: number | null,
): LatencyStripGeometry {
  if (p50 === null || !Number.isFinite(p50)) {
    return { leftPct: 0, widthPct: 0, markerOnly: true }
  }
  const leftPct = clamp((p50 / LATENCY_SCALE_MS) * 100, 0, 100)
  if (p95 === null || !Number.isFinite(p95)) {
    return { leftPct, widthPct: 0, markerOnly: true }
  }
  const widthPct = clamp(
    ((p95 - p50) / LATENCY_SCALE_MS) * 100,
    0,
    100 - leftPct,
  )
  return { leftPct, widthPct, markerOnly: false }
}

/**
 * 四个固定延迟桶：0–250 / 250–500 / 500–1000 / ≥1000 ms。
 * 桶边界取左闭右开（250 落入第二桶）；非法值不参与计数。
 */
export function latencyHistogram(
  values: readonly number[],
): [number, number, number, number] {
  const buckets: [number, number, number, number] = [0, 0, 0, 0]
  for (const value of values) {
    if (!Number.isFinite(value) || value < 0) continue
    if (value < 250) buckets[0] += 1
    else if (value < 500) buckets[1] += 1
    else if (value < 1_000) buckets[2] += 1
    else buckets[3] += 1
  }
  return buckets
}

/** 中位数：空数组为 null；偶数取中间两值均值。不修改入参。 */
export function median(values: readonly number[]): number | null {
  if (values.length === 0) return null
  const sorted = [...values].sort((a, b) => a - b)
  const middle = Math.floor(sorted.length / 2)
  if (sorted.length % 2 === 1) return sorted[middle] ?? null
  const lower = sorted[middle - 1]
  const upper = sorted[middle]
  if (lower === undefined || upper === undefined) return null
  return (lower + upper) / 2
}
