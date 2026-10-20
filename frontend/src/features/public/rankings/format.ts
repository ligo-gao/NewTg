/**
 * 排行榜的数值/时间格式化。所有可见数字都走 `Intl`，不手写千分位，避免
 * 7 语环境下出现英文分隔符；精确值与缩写读数的分工：
 * - 表格/读数类：精确千分位（`formatExact`）；
 * - 条尾/图例：紧凑缩写（`formatCompact`，1.2M / 98.6K）。
 */

import type { RankingsGranularity, RankingsPeriod } from './api'

export function formatExact(value: number, locale: string): string {
  return new Intl.NumberFormat(locale, { maximumFractionDigits: 0 }).format(value)
}

export function formatCompact(value: number, locale: string): string {
  return new Intl.NumberFormat(locale, {
    notation: 'compact',
    maximumFractionDigits: 1,
  }).format(value)
}

export function formatPercent(
  value: number,
  locale: string,
  digits = 1,
): string {
  // 7 语环境的十进制分隔符不同（fr/ru 用逗号），不能用 toFixed 拼字符串。
  return new Intl.NumberFormat(locale, {
    style: 'percent',
    minimumFractionDigits: digits,
    maximumFractionDigits: digits,
  }).format(value / 100)
}

/**
 * 时间桶标签由后端按站点时区序列化（含偏移，如 `2026-10-20T00:00:00+08:00`）。
 * 【不要用 Date 再格式化】`new Date()` 会丢掉原始偏移并套用浏览器时区，
 * 东八区访客在西五区打开页面时 「10-20」会变成 「10-19」。直接切字符串
 * 能保留站点时区的日历日/小时。
 */
export function formatBucketLabel(
  iso: string,
  granularity: RankingsGranularity,
): string {
  const match = /^(\d{4})-(\d{2})-(\d{2})T(\d{2}):/.exec(iso)
  if (match === null) return iso
  const [, year, month, day, hour] = match
  if (granularity === 'hour') return `${hour}:00`
  if (granularity === 'week') return `${year}-${month}-${day}`
  return `${month}-${day}`
}

/** 时间桶的完整站内时间（tooltip/表格 title）。 */
export function formatBucketFull(iso: string): string {
  return iso.replace('T', ' ').replace(/([+-]\d{2}:\d{2}|Z)$/, ' $1')
}

// 排行榜格式化模块不提供 formatClock：live_feed 的时间列使用
// `home/live-feed-data.ts` 的同名实现，避免第二份重复代码（L9）。

export const PERIOD_LABEL_KEYS: Record<RankingsPeriod, string> = {
  today: 'public.rankings.period.today',
  week: 'public.rankings.period.week',
  month: 'public.rankings.period.month',
  year: 'public.rankings.period.year',
}

/** 空态里的「切换到更宽周期」：year 已是最宽，返回 null。 */
export function widerPeriod(period: RankingsPeriod): RankingsPeriod | null {
  switch (period) {
    case 'today':
      return 'week'
    case 'week':
      return 'month'
    case 'month':
      return 'year'
    case 'year':
      return null
  }
}
