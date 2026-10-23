/**
 * 公开定价页的数字格式化（modules-00 §4.7 / modules-09 子模块 B §0.3）。
 *
 * 三条纪律，代码必须逐条守住：
 * 1. 积分恒为整数。服务端把 int64 以 JSON 字符串下发，正是为了避免
 *    `Number` 在 2^53 之后静默丢精度；因此这里全程用 `BigInt`，
 *    不提供「先转 number 再格式化」的捷径。
 * 2. token 量（上下文/最大输出）是计数而不是账本，可缩写为 K/M。
 * 3. 币种换算是展示层参考，保留 2 位小数并恒带 `≈`；换算只读
 *    `quota_per_unit` 与 `usd_exchange_rate`，前端不硬编码汇率。
 */

import { formatTokenAmount } from '@/shared/lib/format'

export { formatTokenAmount }

/**
 * 上下文/最大输出的 token 量（modules-09 UI §0.3 的三层规则）：千位以上
 * 缩写为 `K`/`M`，最多 1 位小数。共享 `formatTokenAmount` 只在 1,000,000
 * 处缩写，公开定价页的规格行还需要 `128K`/`32K`，故在此补一层。
 */
export function formatContextTokens(value: number, locale: string): string {
  if (!Number.isFinite(value) || value < 0) return '—'
  const options: Intl.NumberFormatOptions = {
    minimumFractionDigits: 0,
    maximumFractionDigits: 1,
  }
  if (value >= 1_000_000) {
    return `${new Intl.NumberFormat(locale, options).format(value / 1_000_000)}M`
  }
  if (value >= 1_000) {
    return `${new Intl.NumberFormat(locale, options).format(value / 1_000)}K`
  }
  return new Intl.NumberFormat(locale, {
    minimumFractionDigits: 0,
    maximumFractionDigits: 0,
  }).format(value)
}

/** 匹配服务端下发的非负 int64 十进制字符串。 */
const INT64_PATTERN = /^(?:0|[1-9]\d*)$/

/**
 * 解析服务端金额。负值/小数/非数字一律返回 `null`，由调用方渲染 `—`：
 * 展示负数价会误导用户，而「跳过并上报」是 F-09-18 的字段级错误口径。
 */
export function parseCredit(raw: string | null | undefined): bigint | null {
  if (raw === null || raw === undefined) return null
  const value = raw.trim()
  if (!INT64_PATTERN.test(value)) return null
  return BigInt(value)
}

/** 千分位整数积分（无小数）。`null` 表示无数据，渲染为 `—`。 */
export function formatCredit(
  raw: string | null | undefined,
  locale: string,
): string {
  const value = parseCredit(raw)
  return value === null ? '—' : new Intl.NumberFormat(locale).format(value)
}

/** 直接格式化已解析的 BigInt（分组价表等内部值）。 */
export function formatBigInt(value: bigint, locale: string): string {
  return new Intl.NumberFormat(locale).format(value)
}

function decimalSeparator(locale: string): string {
  return (
    new Intl.NumberFormat(locale)
      .formatToParts(1.5)
      .find((part) => part.type === 'decimal')?.value ?? '.'
  )
}

function formatScaled(
  scaled: bigint,
  scale: number,
  locale: string,
  maxFractionDigits = scale,
  fixed = false,
): string {
  const divisor = 10n ** BigInt(scale)
  const integer = scaled / divisor
  const remainder = scaled % divisor
  const text = formatBigInt(integer, locale)
  if (maxFractionDigits <= 0) return text
  if (fixed) {
    // 币种金额必须恒为 2 位小数：15.10 不能缩成 15.1。整数「分」直接
    // 补足两位，不做尾随 0 裁剪。
    const fraction = remainder.toString().padStart(scale, '0').slice(0, maxFractionDigits)
    return `${text}${decimalSeparator(locale)}${fraction}`
  }
  if (remainder === 0n) return text
  const padded = remainder.toString().padStart(scale, '0')
  const trimmed = padded.replace(/0+$/, '').slice(0, maxFractionDigits)
  return trimmed === '' ? text : `${text}${decimalSeparator(locale)}${trimmed}`
}

/**
 * 「每 K token」换算：积分 ÷ 1000，最多 4 位小数（F-09-19）。
 * 四舍五入在 BigInt 上完成，避免 500 → 0.5 这类边界出现 0.4999。
 */
export function convertCreditsPerK(
  raw: string | null | undefined,
  locale: string,
): string | null {
  const value = parseCredit(raw)
  if (value === null) return null
  // value / 1000 在 4 位小数空间里恰好是 value × 10，不需要任何舍入；
  // 全程 BigInt，保证大额积分不会被 Number 截断。
  return formatScaled(value * 10n, 4, locale)
}

function cnyCentsOf(
  value: bigint,
  quotaPerUnit: number,
): bigint | null {
  if (!Number.isFinite(quotaPerUnit) || quotaPerUnit <= 0) return null
  const quota = BigInt(Math.round(quotaPerUnit))
  if (quota <= 0n) return null
  return (value * 100n + quota / 2n) / quota
}

/** 人民币「分」→ 美元「分」，汇率为「1 美元 = N 人民币」。 */
function usdCentsOf(cents: bigint, usdExchangeRate: number): bigint | null {
  if (!Number.isFinite(usdExchangeRate) || usdExchangeRate <= 0) return null
  // 用万分比把浮点汇率变成整数比率，避免把账本金额送进 Number。
  const rateBps = BigInt(Math.round(usdExchangeRate * 10_000))
  if (rateBps <= 0n) return null
  return (cents * 10_000n + rateBps / 2n) / rateBps
}

/** 把积分换算成人民币（2 位小数，half-up），返回不带 `≈` 的数值。 */
export function creditsToCny(
  raw: string | null | undefined,
  quotaPerUnit: number,
  locale: string,
): string | null {
  const value = parseCredit(raw)
  if (value === null) return null
  const cents = cnyCentsOf(value, quotaPerUnit)
  return cents === null ? null : formatScaled(cents, 2, locale, 2, true)
}

/** 分组倍率：120 → `1.20x`。ratio 是 int32，Number 安全。 */
export function formatRatio(ratio: number | null | undefined): string | null {
  if (ratio === null || ratio === undefined || !Number.isInteger(ratio)) return null
  return `${(ratio / 100).toFixed(2)}x`
}

/** 总数的 int64 JSON 字符串；不经过 Number。 */
export function formatTotal(
  raw: string | null | undefined,
  locale: string,
): string {
  if (raw === null || raw === undefined) return '—'
  try {
    return formatBigInt(BigInt(raw), locale)
  } catch {
    return '—'
  }
}

export interface MoneyMeta {
  quota_per_unit: number
  usd_exchange_rate: number
}

export interface CurrencyReference {
  cny: string
  usd: string | null
}

/** 双币种参考值；汇率缺失时返回 `null`，开关应保持禁用。 */
export function currencyReference(
  raw: string | null | undefined,
  meta: MoneyMeta | undefined,
  locale: string,
): CurrencyReference | null {
  if (meta === undefined) return null
  const value = parseCredit(raw)
  if (value === null) return null
  const cents = cnyCentsOf(value, meta.quota_per_unit)
  if (cents === null) return null
  const usdCents = usdCentsOf(cents, meta.usd_exchange_rate)
  return {
    cny: `¥${formatScaled(cents, 2, locale, 2, true)}`,
    usd: usdCents === null ? null : `$${formatScaled(usdCents, 2, locale, 2, true)}`,
  }
}

/** 充值价参考同样走积分→币种，只是文案标注来源不同。 */
export function rechargeReference(
  raw: string | null | undefined,
  meta: MoneyMeta | undefined,
  locale: string,
): string | null {
  if (meta === undefined) return null
  const cny = creditsToCny(raw, meta.quota_per_unit, locale)
  return cny === null ? null : `¥${cny}`
}

/**
 * 性能区（F-09-25）的展示格式化。`null`/非有限值一律渲染 `—`：
 * 性能字段缺失表示「没有样本」，不是 0ms / 0 tokens/s。
 */
export function formatPerfMilliseconds(
  value: number | null | undefined,
  locale: string,
): string {
  if (value === null || value === undefined || !Number.isFinite(value) || value < 0) {
    return '—'
  }
  return `${new Intl.NumberFormat(locale, { maximumFractionDigits: 0 }).format(value)} ms`
}

export function formatPerfTPS(
  value: number | null | undefined,
  locale: string,
): string {
  if (value === null || value === undefined || !Number.isFinite(value) || value < 0) {
    return '—'
  }
  return new Intl.NumberFormat(locale, { maximumFractionDigits: 1 }).format(value)
}

export function formatSuccessRate(
  value: number | null | undefined,
  locale: string,
): string {
  if (value === null || value === undefined || !Number.isFinite(value)) {
    return '—'
  }
  return new Intl.NumberFormat(locale, {
    style: 'percent',
    maximumFractionDigits: 1,
  }).format(value)
}
