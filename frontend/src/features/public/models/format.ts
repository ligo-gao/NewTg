/**
 * 模型库的读数格式化。
 *
 * 性能字段缺失表示「没有样本」，不是 0ms / 0 tokens/s：`null` 一律渲染
 * `—`。样本数为 0 则是真实的零，必须渲染 `0`。
 */

/**
 * 349.6 → `350 ms`；≥10,000ms 用秒并保留一位小数（12_400 → `12.4 s`）。
 *
 * 分组符号跟随 `locale`（fr/ru/vi 读作 `2 340 ms`）：表格格、卡片、页头与
 * 展开行的详情必须传同一个 locale，否则同一个数字会出现两种写法。
 */
export function formatMs(value: number | null, locale = 'en-US'): string {
  if (value === null || !Number.isFinite(value) || value < 0) return '—'
  if (value >= 10_000) return `${(value / 1_000).toFixed(1)} s`
  const rounded = new Intl.NumberFormat(locale, {
    maximumFractionDigits: 0,
  }).format(Math.round(value))
  return `${rounded} ms`
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
