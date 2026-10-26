/**
 * 模型库的读数格式化。
 *
 * 性能字段缺失表示「没有样本」，不是 0ms / 0 tokens/s：`null` 一律渲染
 * `—`。样本数为 0 则是真实的零，必须渲染 `0`。
 */

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
