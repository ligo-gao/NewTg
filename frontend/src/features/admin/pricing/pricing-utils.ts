import type { components } from '@/api/schema'

export type PricingMode = components['schemas']['PricingMode']
export type PricingStatus = components['schemas']['PricingStatus']

/**
 * 列表信封缺少 quota_per_unit 时的展示兜底系数。后端默认 100
 * （billing-02 / R-01）；列表接口会返回实时值，因此这里只用于第一页
 * 数据尚未到达（例如从过期缓存打开抽屉）的场景。
 */
export const DEFAULT_QUOTA_PER_UNIT = 100

/**
 * 后端拒绝超过 2^53−1（maxPricingPrice）的价格，保证每个存储值经过 JS
 * number 的 JSON 往返后仍然精确。客户端同步该上限，在发请求前就失败，
 * 而不是等 422。
 */
export const MAX_PRICING_PRICE = Number.MAX_SAFE_INTEGER

/**
 * 与后端 model 名校验同步（`^[A-Za-z0-9._:/-]{1,128}$`）。`/` 是公开模型
 * 名的厂商前缀分隔符（如 `openai/gpt-5`），不能在前端本地拒绝；字符集
 * 变化必须同时改后端正则与这里，否则会出现「前端放行、服务端 422」或
 * 反之的割裂。
 */
export const MODEL_NAME_PATTERN = /^[A-Za-z0-9._:/-]{1,128}$/

/** 模型名在提交前按 trim 后的值校验，和服务端一致。 */
export function isValidModelName(name: string): boolean {
  return MODEL_NAME_PATTERN.test(name.trim())
}

export type PriceUnit = 'credits' | 'yuan'

export type PriceFieldKey = 'prompt' | 'completion' | 'cacheRead' | 'cacheWrite'

export interface PriceFieldValues {
  prompt: string
  completion: string
  cacheRead: string
  cacheWrite: string
}

export interface ParsedPrice {
  /** null = 留空（后端存 NULL = 未设置，不是 0）。 */
  value: number | null
  /** 原始文本非空但不是合法价格。 */
  invalid: boolean
}

const CREDIT_PATTERN = /^\d+$/
const CREDIT_CENTS_PATTERN = /^\d+(\.\d{1,2})?$/

/**
 * 解析积分金额。空输入表示三态里的「未设置」（null），不是 0：
 * R9 区分 NULL（默认价兜底）与显式 0（免费）。仅空白字符也按空处理。
 */
export function parseCreditInput(raw: string): ParsedPrice {
  const value = raw.trim()
  if (value === '') return { value: null, invalid: false }
  if (!CREDIT_PATTERN.test(value)) return { value: null, invalid: true }
  const parsed = Number(value)
  if (!Number.isSafeInteger(parsed) || parsed > MAX_PRICING_PRICE) {
    return { value: null, invalid: true }
  }
  return { value: parsed, invalid: false }
}

/**
 * 解析元金额并按实时 quota_per_unit（冻结配置下为 100）换算为整数积分。
 * 换算必须十进制精确：100 积分/单位时 1.5 元必须提交 150，不能是
 * 149.999。
 *
 * 输入最多两位小数（本 UI 宣传的最小单位是分）；在实时系数下不能落到
 * 整数积分的值（如 100 单位时的 0.001 元）直接拒绝，而不是静默四舍五入
 * ——价格编辑必须精确。
 */
export function parseYuanInput(raw: string, quotaPerUnit: number): ParsedPrice {
  const value = raw.trim()
  if (value === '') return { value: null, invalid: false }
  if (!CREDIT_CENTS_PATTERN.test(value)) return { value: null, invalid: true }
  const yuan = Number(value)
  if (!Number.isFinite(yuan) || yuan < 0) return { value: null, invalid: true }
  const credits = yuan * quotaPerUnit
  if (!Number.isFinite(credits) || credits > MAX_PRICING_PRICE) {
    return { value: null, invalid: true }
  }
  // 浮点只用于输入校验，提交的是取整后的整数。不匹配说明文本精度
  // 超过系数可表示的范围，必须让操作者看见，不能悄悄改价。
  if (Math.abs(credits - Math.round(credits)) > 1e-6) {
    return { value: null, invalid: true }
  }
  return { value: Math.round(credits), invalid: false }
}

export function parsePriceInput(
  raw: string,
  unit: PriceUnit,
  quotaPerUnit: number,
): ParsedPrice {
  return unit === 'credits'
    ? parseCreditInput(raw)
    : parseYuanInput(raw, quotaPerUnit)
}

/** 把已存积分渲染为元编辑器文本，不制造虚假精度。 */
export function creditsToYuanInput(credits: number, quotaPerUnit: number): string {
  const factor = quotaPerUnit > 0 ? quotaPerUnit : DEFAULT_QUOTA_PER_UNIT
  const yuan = credits / factor
  // 6 位小数足以在「分」的粒度上往返，同时让非 100 的 quota_per_unit
  // （可能除不尽）保持可读。
  return String(Number(yuan.toFixed(6)))
}

/**
 * 在积分与元之间换算单个价格输入，不改变金额。非法文本原样返回让操作者
 * 修正；若在切换单位时静默改写，会掩盖校验错误。
 */
export function convertPriceInput(
  raw: string,
  from: PriceUnit,
  to: PriceUnit,
  quotaPerUnit: number,
): string {
  if (from === to || raw.trim() === '') return raw
  const parsed = parsePriceInput(raw, from, quotaPerUnit)
  if (parsed.invalid || parsed.value === null) return raw
  return to === 'credits'
    ? String(parsed.value)
    : creditsToYuanInput(parsed.value, quotaPerUnit)
}

/** 预览/表格提示用的固定两位元展示。 */
export function formatYuan(credits: number, quotaPerUnit: number): string {
  const factor = quotaPerUnit > 0 ? quotaPerUnit : DEFAULT_QUOTA_PER_UNIT
  return (credits / factor).toFixed(2)
}

/** 分组倍率按整数百分比存储（120 → 1.20x）。 */
export function formatGroupRatio(ratio: number): string {
  return `${(ratio / 100).toFixed(2)}x`
}

/**
 * 把线格式模式映射为 i18n 标签。参数刻意用 string 而非生成联合类型：
 * GroupPricing.mode 在源 model_pricing 行缺失（source_status = 0）时约定为
 * 空串，且契约可能把联合类型扩到 ""。未知/空值必须渲染中性标签，不能崩溃
 * 或谎称为按 token 计价。
 */
export function pricingModeKey(mode: string): string {
  switch (mode) {
    case 'per_token':
      return 'admin.pricing.mode_per_token'
    case 'per_request':
      return 'admin.pricing.mode_per_request'
    case 'tiered_expr':
      return 'admin.pricing.mode_tiered_expr'
    default:
      return 'admin.pricing.mode_unknown'
  }
}

export function isPerRequest(mode: string): boolean {
  return mode === 'per_request'
}

/**
 * 把任意线格式模式收敛为编辑器模式。源缺失（""）回退到按 token 字段集，
 * 这是覆盖价行仍能有意义编辑的唯一字段集。
 */
export function normalizePricingMode(mode: string | null | undefined): PricingMode {
  if (mode === 'per_request' || mode === 'tiered_expr') return mode
  return 'per_token'
}

/** 把存储的可空价格转成编辑器显示文本。 */
export function priceToInput(
  price: number | null | undefined,
  unit: PriceUnit,
  quotaPerUnit: number,
): string {
  if (price === null || price === undefined) return ''
  return unit === 'credits'
    ? String(price)
    : creditsToYuanInput(price, quotaPerUnit)
}

export interface PriceParseResult {
  values: Record<PriceFieldKey, number | null>
  errors: Partial<Record<PriceFieldKey, boolean>>
  valid: boolean
}

/**
 * 按模式解析四个可见价格字段。只解析该模式实际计费的字段；隐藏字段在
 * 表单状态里保留旧值（UI §8.5「切换模式不清值」），但抽屉不会把它们
 * 发进请求。
 */
export function parsePriceFields(
  mode: PricingMode,
  values: PriceFieldValues,
  unit: PriceUnit,
  quotaPerUnit: number,
): PriceParseResult {
  const result: PriceParseResult = {
    values: { prompt: null, completion: null, cacheRead: null, cacheWrite: null },
    errors: {},
    valid: true,
  }
  if (mode === 'tiered_expr') return result

  const keys: PriceFieldKey[] =
    mode === 'per_request' ? ['prompt'] : ['prompt', 'completion', 'cacheRead', 'cacheWrite']
  for (const key of keys) {
    const parsed = parsePriceInput(values[key], unit, quotaPerUnit)
    result.values[key] = parsed.value
    if (parsed.invalid) {
      result.errors[key] = true
      result.valid = false
    }
  }
  return result
}
