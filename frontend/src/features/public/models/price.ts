/**
 * 模型库的价格换算与人民币格式化（纯函数）。
 *
 * 公开目录里的 `prompt_price` / `completion_price` / `cache_read_price` 是
 * int64 的 JSON 字符串，语义已经是「积分 / 1M tokens」（定价页直接按每 M
 * 展示）。因此「元 / 百万 token」= `raw / quota_per_unit`，**没有 ×1000
 * 的系数**；`quota_per_unit` 来自 `GET /api/pricing` 响应的 `meta`。
 *
 * 缺失价格、非法积分串或不可用的 `quota_per_unit` 一律得到 `null`，由展示层
 * 渲染 `—`：既不能出现 `¥NaN` / `¥Infinity`，也不能把未知价格伪装成免费。
 */

import { parseCredit } from '@/features/public/pricing/format'
import type { PublicModelCard } from '@/features/public/pricing/api'

/** 一个模型的每百万 token 价格，单位：元。`null` 表示无可用价格。 */
export interface ModelPrice {
  input: number | null
  output: number | null
  cacheRead: number | null
}

/**
 * 积分字符串 → 每百万 token 的元金额。
 *
 * `parseCredit` 已经挡掉负数、小数与非数字；这里再要求 `quota_per_unit`
 * 有限且为正，并确认换算结果有限，避免脏 meta 把 `NaN`/`Infinity` 带进 UI。
 */
export function pricePerMillion(
  raw: string | null | undefined,
  quotaPerUnit: number | undefined,
): number | null {
  if (
    quotaPerUnit === undefined ||
    !Number.isFinite(quotaPerUnit) ||
    quotaPerUnit <= 0
  ) {
    return null
  }
  const value = parseCredit(raw)
  if (value === null) return null
  const price = Number(value) / quotaPerUnit
  return Number.isFinite(price) ? price : null
}

/** 把目录卡片的三个价格字段换算成元；缺失字段保持 `null`。 */
export function priceOf(
  card: PublicModelCard,
  quotaPerUnit: number | undefined,
): ModelPrice {
  return {
    input: pricePerMillion(card.prompt_price, quotaPerUnit),
    output: pricePerMillion(card.completion_price, quotaPerUnit),
    cacheRead: pricePerMillion(card.cache_read_price, quotaPerUnit),
  }
}

/** 元金额 → `¥12.34`（恒两位小数）。`null`/非有限值返回 `null`。 */
export function formatRmb(value: number | null, locale: string): string | null {
  if (value === null || !Number.isFinite(value)) return null
  const formatted = new Intl.NumberFormat(locale, {
    minimumFractionDigits: 2,
    maximumFractionDigits: 2,
  }).format(value)
  return `¥${formatted}`
}

/**
 * 价格区间：两端都有值才渲染，`lo === hi` 时退化为单值。
 * 任一端缺失返回 `null`——半个区间会让人误以为另一端是 0。
 */
export function formatRmbRange(
  lo: number | null,
  hi: number | null,
  locale: string,
): string | null {
  const low = formatRmb(lo, locale)
  const high = formatRmb(hi, locale)
  if (low === null || high === null) return null
  return lo === hi ? low : `${low}–${high}`
}
