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

/* ------------------------------------------------------------------ *
 * 时段策略（model_pricing.strategy v1）
 *
 * 存储格式是十进制倍数（0.5 = 半价），编辑器用百分比输入（50 = 半价）。
 * 换算只发生在这里，组件之间传的都是草稿结构，避免前后端各乘一次 100。
 * ------------------------------------------------------------------ */

export type PricingStrategy = components['schemas']['PricingStrategy']
export type PricingTimeWindow = components['schemas']['PricingTimeWindow']
export type HolidayPolicy = NonNullable<
  components['schemas']['PricingTimeConfig']['holiday_policy']
>

/** 默认时区与站点缺省一致（spec §6.2）；保存时仍显式写入策略。 */
export const DEFAULT_STRATEGY_TIMEZONE = 'Asia/Shanghai'

/** 演示与常见上游使用的 IANA 时区；值本身由服务端再次校验。 */
export const TIMEZONE_OPTIONS = [
  'Asia/Shanghai',
  'UTC',
  'America/Los_Angeles',
  'America/New_York',
  'Europe/London',
  'Asia/Tokyo',
  'Asia/Singapore',
] as const

/** 编辑器里的窗口；multiplier 是百分比的原始文本（可暂时非法）。 */
export interface TimeWindowDraft {
  name: string
  /** 1=周一 … 7=周日；跨夜窗口指窗口开始的那一天。 */
  days: number[]
  /** HH:MM；end <= start 表示跨夜。 */
  start: string
  end: string
  /** 百分比文本，例如 "100"、"50"、"125"。 */
  multiplier: string
}

export interface TimeConfigDraft {
  enabled: boolean
  timezone: string
  holidayPolicy: HolidayPolicy
  /** 未命中窗口时的百分比文本。 */
  defaultMultiplier: string
  windows: TimeWindowDraft[]
}

export interface ParsedMultiplier {
  /** 十进制倍数；null = 留空。 */
  value: number | null
  invalid: boolean
}

export const MIN_MULTIPLIER = 0.01
export const MAX_MULTIPLIER = 100

export function createDefaultTimeConfig(): TimeConfigDraft {
  return {
    enabled: false,
    timezone: DEFAULT_STRATEGY_TIMEZONE,
    holidayPolicy: 'none',
    defaultMultiplier: '100',
    windows: [],
  }
}

/** 百分比文本 → 十进制倍数。范围 0.01–100（即 1%–10000%）。 */
export function parseMultiplierPercent(raw: string): ParsedMultiplier {
  const text = raw.trim()
  if (text === '') return { value: null, invalid: false }
  if (!/^\d+(\.\d+)?$/.test(text)) return { value: null, invalid: true }
  const percent = Number(text)
  if (!Number.isFinite(percent)) return { value: null, invalid: true }
  const multiplier = percent / 100
  if (multiplier < MIN_MULTIPLIER || multiplier > MAX_MULTIPLIER) {
    return { value: null, invalid: true }
  }
  return { value: multiplier, invalid: false }
}

/** 十进制倍数 → 百分比文本；去掉无意义的尾零。 */
export function formatMultiplierPercent(multiplier: number | null | undefined): string {
  if (multiplier === null || multiplier === undefined) return ''
  if (!Number.isFinite(multiplier)) return ''
  return String(Number((multiplier * 100).toFixed(4)))
}

export function isValidTimezone(timezone: string): boolean {
  if (timezone.trim() === '') return false
  try {
    new Intl.DateTimeFormat('en-US', { timeZone: timezone })
    return true
  } catch {
    return false
  }
}

const HHMM_PATTERN = /^([01]\d|2[0-3]):([0-5]\d)$/

/** "HH:MM" → 从零点起的分钟数；非法返回 null。 */
export function parseHhMm(value: string): number | null {
  const match = HHMM_PATTERN.exec(value.trim())
  if (match === null) return null
  return Number(match[1]) * 60 + Number(match[2])
}

/** 策略 JSON / 草稿 → 编辑器草稿。null 返回默认（未启用）配置。 */
export function timeConfigFromStrategy(
  strategy: PricingStrategy | null | undefined,
): TimeConfigDraft {
  if (strategy === null || strategy === undefined) return createDefaultTimeConfig()
  const time = strategy.time
  if (time === undefined) return createDefaultTimeConfig()
  return {
    enabled: time.enabled ?? false,
    timezone: time.timezone ?? DEFAULT_STRATEGY_TIMEZONE,
    holidayPolicy: time.holiday_policy ?? 'none',
    defaultMultiplier: formatMultiplierPercent(time.default_multiplier ?? 1),
    windows: (time.windows ?? []).map((window) => ({
      name: window.name,
      days: [...window.days],
      start: window.start,
      end: window.end,
      multiplier: formatMultiplierPercent(window.multiplier),
    })),
  }
}

export interface StrategyBuildResult {
  /** 未启用时段时返回 null（表示不配置策略）。 */
  strategy: PricingStrategy | null
  /** 字段路径（与草稿一致，如 `windows.0.start`）→ 是否非法。 */
  errors: Record<string, boolean>
}

/**
 * 编辑器草稿 → 提交用策略。启用时段时才返回对象；未启用返回 null，
 * 让调用方决定是省略字段（创建）还是显式 null（清除旧策略）。
 */
export function strategyFromTimeConfig(config: TimeConfigDraft): StrategyBuildResult {
  const errors: Record<string, boolean> = {}
  if (!config.enabled) return { strategy: null, errors }

  if (!isValidTimezone(config.timezone)) errors.timezone = true

  const defaultMultiplier = parseMultiplierPercent(config.defaultMultiplier)
  if (defaultMultiplier.value === null) errors.defaultMultiplier = true

  const windows: PricingTimeWindow[] = []
  config.windows.forEach((window, index) => {
    const start = parseHhMm(window.start)
    const end = parseHhMm(window.end)
    if (start === null) errors[`windows.${index}.start`] = true
    if (end === null) errors[`windows.${index}.end`] = true
    const multiplier = parseMultiplierPercent(window.multiplier)
    if (multiplier.value === null) errors[`windows.${index}.multiplier`] = true
    windows.push({
      name: window.name.trim(),
      days: [...window.days].sort((a, b) => a - b),
      start: window.start,
      end: window.end,
      multiplier: multiplier.value ?? 1,
    })
  })

  if (Object.keys(errors).length > 0) return { strategy: null, errors }
  if (defaultMultiplier.value === null) return { strategy: null, errors }

  return {
    strategy: {
      version: 1,
      time: {
        enabled: true,
        timezone: config.timezone.trim(),
        holiday_policy: config.holidayPolicy,
        default_multiplier: defaultMultiplier.value,
        windows,
      },
    },
    errors,
  }
}

export interface TimeMatch {
  /** 命中的窗口名；未命中（使用默认倍率）时为空串。 */
  window: string
  /** 十进制倍数。 */
  multiplier: number
}

/** 判定某个窗口在给定星期/分钟是否生效（跨夜窗口按开始日归属）。 */
export function windowMatches(
  window: Pick<TimeWindowDraft, 'days' | 'start' | 'end'>,
  day: number,
  minutes: number,
): boolean {
  const start = parseHhMm(window.start)
  const end = parseHhMm(window.end)
  if (start === null || end === null) return false
  const days = new Set(window.days)
  if (start < end) {
    return days.has(day) && minutes >= start && minutes < end
  }
  // end <= start：跨夜。今天从 start 开始，或昨天从 start 开始且还没到 end。
  const previousDay = day === 1 ? 7 : day - 1
  return (
    (days.has(day) && minutes >= start) || (days.has(previousDay) && minutes < end)
  )
}

/**
 * 本地（不含节假日）时段匹配。节假日只有服务端持有日历，因此本地试算
 * 按星期/窗口规则计算；保存前的 preview-strategy 调用补上节假日事实。
 * 同一时刻命中多个窗口时，数组顺序最后一条生效（与后端一致）。
 */
export function matchLocalTimeRule(
  config: TimeConfigDraft,
  day: number,
  minutes: number,
): TimeMatch {
  if (!config.enabled) return { window: '', multiplier: 1 }
  const fallback = parseMultiplierPercent(config.defaultMultiplier)
  let match: TimeMatch = {
    window: '',
    multiplier: fallback.invalid ? 1 : (fallback.value ?? 1),
  }
  for (const window of config.windows) {
    if (!windowMatches(window, day, minutes)) continue
    const parsed = parseMultiplierPercent(window.multiplier)
    match = {
      window: window.name.trim(),
      multiplier: parsed.invalid ? 1 : (parsed.value ?? 1),
    }
  }
  return match
}

export interface WallClockParts {
  /** YYYY-MM-DD */
  date: string
  /** 1=周一 … 7=周日 */
  day: number
  minutes: number
  /** 原始 HH:MM（已归一为两位）。 */
  time: string
  /** `YYYY-MM-DDTHH:MM`，可直接作为 datetime-local 的值。 */
  value: string
}

/** 解析 `datetime-local` 的值（不涉及时区换算，按字面墙钟时间处理）。 */
export function wallClockParts(value: string): WallClockParts | null {
  const match = /^(\d{4})-(\d{2})-(\d{2})T(\d{2}):(\d{2})/.exec(value)
  if (match === null) return null
  const year = Number(match[1])
  const month = Number(match[2])
  const date = Number(match[3])
  const hour = Number(match[4])
  const minute = Number(match[5])
  if (hour > 23 || minute > 59) return null
  const utc = new Date(Date.UTC(year, month - 1, date))
  if (Number.isNaN(utc.getTime())) return null
  const weekday = utc.getUTCDay()
  const dateText = `${match[1]}-${match[2]}-${match[3]}`
  const timeText = `${match[4]}:${match[5]}`
  return {
    date: dateText,
    day: weekday === 0 ? 7 : weekday,
    minutes: hour * 60 + minute,
    time: timeText,
    value: `${dateText}T${timeText}`,
  }
}

const WEEKDAY_SHORT_INDEX: Record<string, number> = {
  Mon: 1,
  Tue: 2,
  Wed: 3,
  Thu: 4,
  Fri: 5,
  Sat: 6,
  Sun: 7,
}

const zonedFormatters = new Map<string, Intl.DateTimeFormat>()

function zonedFormatter(timezone: string): Intl.DateTimeFormat {
  const cached = zonedFormatters.get(timezone)
  if (cached !== undefined) return cached
  const formatter = new Intl.DateTimeFormat('en-GB', {
    timeZone: timezone,
    year: 'numeric',
    month: '2-digit',
    day: '2-digit',
    hour: '2-digit',
    minute: '2-digit',
    hourCycle: 'h23',
    weekday: 'short',
  })
  zonedFormatters.set(timezone, formatter)
  return formatter
}

/** 当前时刻在指定 IANA 时区的墙钟表示（用于试算默认时间）。 */
export function zonedNow(timezone: string): WallClockParts {
  const zone = isValidTimezone(timezone) ? timezone : 'UTC'
  const parts = zonedFormatter(zone).formatToParts(new Date())
  const get = (type: Intl.DateTimeFormatPartTypes): string =>
    parts.find((part) => part.type === type)?.value ?? ''
  const year = get('year')
  const month = get('month')
  const day = get('day')
  const hour = get('hour')
  const minute = get('minute')
  const weekday = get('weekday')
  const dateText = `${year}-${month}-${day}`
  const timeText = `${hour.padStart(2, '0')}:${minute.padStart(2, '0')}`
  return {
    date: dateText,
    day: WEEKDAY_SHORT_INDEX[weekday] ?? 1,
    minutes: Number(hour) * 60 + Number(minute),
    time: timeText,
    value: `${dateText}T${timeText}`,
  }
}

/** 取与给定墙钟时间同一自然周（周一为第一天）的目标星期。 */
export function wallClockForWeekday(selected: string, day: number, hour: number): string {
  const parts = wallClockParts(selected)
  const base = parts ?? zonedNow(DEFAULT_STRATEGY_TIMEZONE)
  const dateMatch = /^(\d{4})-(\d{2})-(\d{2})$/.exec(base.date)
  const year = Number(dateMatch?.[1] ?? 1970)
  const month = Number(dateMatch?.[2] ?? 1)
  const date = Number(dateMatch?.[3] ?? 1)
  const delta = day - base.day
  const target = new Date(Date.UTC(year, month - 1, date + delta))
  const yyyy = String(target.getUTCFullYear()).padStart(4, '0')
  const mm = String(target.getUTCMonth() + 1).padStart(2, '0')
  const dd = String(target.getUTCDate()).padStart(2, '0')
  const hh = String(hour).padStart(2, '0')
  return `${yyyy}-${mm}-${dd}T${hh}:00`
}

function zoneOffsetAt(instant: Date, timezone: string): string {
  if (!isValidTimezone(timezone)) return '+00:00'
  const parts = new Intl.DateTimeFormat('en-US', {
    timeZone: timezone,
    timeZoneName: 'longOffset',
  }).formatToParts(instant)
  const name = parts.find((part) => part.type === 'timeZoneName')?.value ?? 'GMT'
  if (name === 'GMT' || name === 'UTC') return '+00:00'
  const match = /GMT([+-]\d{2}:\d{2})/.exec(name)
  return match?.[1] ?? '+00:00'
}

/**
 * `datetime-local` 的墙钟时间 + IANA 时区 → RFC 3339。服务端试算需要
 * 带偏移的瞬时值；夏令时边界按两次估算后的偏移取值。
 */
export function wallClockToRfc3339(value: string, timezone: string): string | null {
  const parts = wallClockParts(value)
  if (parts === null) return null
  const dateMatch = /^(\d{4})-(\d{2})-(\d{2})$/.exec(parts.date)
  if (dateMatch === null) return null
  const year = Number(dateMatch[1])
  const month = Number(dateMatch[2])
  const date = Number(dateMatch[3])
  const timeMatch = /^(\d{2}):(\d{2})$/.exec(parts.time)
  if (timeMatch === null) return null
  const hour = Number(timeMatch[1])
  const minute = Number(timeMatch[2])
  const guess = new Date(Date.UTC(year, month - 1, date, hour, minute))
  const firstOffset = zoneOffsetAt(guess, timezone)
  const firstInstant = new Date(guess.getTime() - offsetToMs(firstOffset))
  const offset = zoneOffsetAt(firstInstant, timezone)
  return `${parts.date}T${parts.time}:00${offset}`
}

function offsetToMs(offset: string): number {
  const match = /^([+-])(\d{2}):(\d{2})$/.exec(offset)
  if (match === null) return 0
  const sign = match[1] === '-' ? -1 : 1
  return sign * (Number(match[2]) * 60 + Number(match[3])) * 60_000
}

/** 星期 1–7 → i18n 短名键。 */
export function weekdayKey(day: number): string {
  const keys = [
    'admin.pricing.day_mon',
    'admin.pricing.day_tue',
    'admin.pricing.day_wed',
    'admin.pricing.day_thu',
    'admin.pricing.day_fri',
    'admin.pricing.day_sat',
    'admin.pricing.day_sun',
  ]
  return keys[day - 1] ?? 'admin.pricing.day_mon'
}

/** 星期 1–7 → i18n 全名键（用于可访问名称与跨夜说明）。 */
export function weekdayLongKey(day: number): string {
  const keys = [
    'admin.pricing.day_full_mon',
    'admin.pricing.day_full_tue',
    'admin.pricing.day_full_wed',
    'admin.pricing.day_full_thu',
    'admin.pricing.day_full_fri',
    'admin.pricing.day_full_sat',
    'admin.pricing.day_full_sun',
  ]
  return keys[day - 1] ?? 'admin.pricing.day_full_mon'
}

/** 跨夜窗口的结束星期（end <= start 时为开始日的下一天）。 */
export function overnightEndDay(day: number): number {
  return day === 7 ? 1 : day + 1
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

/**
 * 模型抽屉的缓存读兜底：留空 = 未命中价（后端 DefaultCacheRatio = 1.0）。
 * 显式 0 仍是 0，不能被当成「未设置」。
 */
export function effectiveCacheReadPrice(
  prompt: number | null,
  cacheRead: number | null,
): number | null {
  if (cacheRead !== null) return cacheRead
  return prompt
}

/** 模型抽屉的缓存写兜底：留空 = 未命中价 × 1.25（后端 DefaultCacheCreationRatio）。 */
export function effectiveCacheWritePrice(
  prompt: number | null,
  cacheWrite: number | null,
): number | null {
  if (cacheWrite !== null) return cacheWrite
  return prompt === null ? null : prompt * 1.25
}

/**
 * 通用峰谷预设：工作日 09:00–12:00、14:00–18:00 为高峰（100%），
 * 其余时段（含周末与中国法定节假日）按 50%。不绑定任何厂商或模型——
 * 任何模型都能用这个时段表，也都能改窗口与倍率。
 */
export function weekdayPeakPreset(t: (key: string) => string): TimeConfigDraft {
  return {
    enabled: true,
    timezone: DEFAULT_STRATEGY_TIMEZONE,
    holidayPolicy: 'cn_offpeak',
    defaultMultiplier: '50',
    windows: [
      {
        name: t('admin.pricing.time_preset_window_morning'),
        days: [1, 2, 3, 4, 5],
        start: '09:00',
        end: '12:00',
        multiplier: '100',
      },
      {
        name: t('admin.pricing.time_preset_window_afternoon'),
        days: [1, 2, 3, 4, 5],
        start: '14:00',
        end: '18:00',
        multiplier: '100',
      },
    ],
  }
}
