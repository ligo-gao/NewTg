/**
 * 模型库的价格换算与人民币格式化（纯函数）。
 *
 * 公开目录里的 `prompt_price` / `completion_price` / `cache_read_price` 是
 * int64 的 JSON 字符串，语义取决于 `mode`：
 * - `per_token`：积分 / 1M tokens（定价页直接按每 M 展示），「元 / 百万
 *   token」= `raw / quota_per_unit`，**没有 ×1000 的系数**；
 * - `per_request`：`prompt_price` 是积分 / 次，「元 / 次」用同一个除数换算；
 * - 其它模式不认，三个价格一律 `null`。
 *
 * `quota_per_unit` 来自 `GET /api/pricing` 响应的 `meta`。缺失价格、非法
 * 积分串或不可用的 `quota_per_unit` 一律得到 `null`，由展示层渲染 `—`：
 * 既不能出现 `¥NaN` / `¥Infinity`，也不能把未知价格伪装成免费。
 */

import { parseCredit } from '@/features/public/pricing/format'
import type { PublicModelCard } from '@/features/public/pricing/api'

/**
 * 一个模型的价格，单位：元。`null` 表示无可用价格。
 *
 * 按 token 计价时三个字段是「元 / 百万 token」；`perCall === true`（服务端
 * 的 `mode` 为 `per_request`）时只有 `input` 有意义，它是「元 / 次调用」，
 * 其余字段恒为 `null`。既不是 `per_token` 也不是 `per_request` 的模式一律
 * 三个 `null`，展示层渲染 `—`，绝不套用每百万 token 的换算。
 */
export interface ModelPrice {
  input: number | null
  output: number | null
  cacheRead: number | null
  /** 按次计价：`input` 是每次调用的元金额，不是每百万 token。 */
  perCall?: boolean
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

/**
 * 把目录卡片的价格字段换算成元。
 *
 * `per_token`：三个字段都是「积分 / 1M tokens」，除以 `quota_per_unit` 得到
 * 元 / 百万 token。`per_request`：只有 `prompt_price` 有意义，它是「积分 /
 * 次」，同一个除数得到元 / 次（`perCall` 标记由展示层选择单位，不再回读
 * `card.mode`）。其它模式一律 `null`，不把未知计价方式伪装成按 token。
 */
export function priceOf(
  card: PublicModelCard,
  quotaPerUnit: number | undefined,
): ModelPrice {
  if (card.mode === 'per_request') {
    return {
      input: pricePerMillion(card.prompt_price, quotaPerUnit),
      output: null,
      cacheRead: null,
      perCall: true,
    }
  }
  if (card.mode !== 'per_token') {
    return { input: null, output: null, cacheRead: null }
  }
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
 * 价格区间：两端都有值才渲染，格式化后相同时退化为单值。
 * 任一端缺失返回 `null`——半个区间会让人误以为另一端是 0。
 *
 * 比较的是**格式化后的字符串**：两个不同的原始值（如 ¥0.0002 与 ¥0.0018）
 * 都可能四舍五入成 `¥0.00`，此时再写 `¥0.00–¥0.00` 只会骗人。
 */
export function formatRmbRange(
  lo: number | null,
  hi: number | null,
  locale: string,
): string | null {
  const low = formatRmb(lo, locale)
  const high = formatRmb(hi, locale)
  if (low === null || high === null) return null
  return low === high ? low : `${low}–${high}`
}
