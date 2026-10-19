import type { components } from '@/api/schema'

export type UsageLogItem = components['schemas']['UsageLogItem']
export type UsageLogDetail = components['schemas']['UsageLogDetail']
export type UsageLogStatus = NonNullable<UsageLogItem['status']>
export type UsageLogRange = '24h' | '7d' | '14d' | '30d'
export type UsageLogRangeFilter = UsageLogRange | 'custom'
/** `logs` stays absent from the URL so the default view keeps a clean link. */
export type UsageLogTab = 'logs' | 'stats'

const RANGE_VALUES: readonly UsageLogRange[] = ['24h', '7d', '14d', '30d']
const STATUS_VALUES: readonly UsageLogStatus[] = [
  'success',
  'error',
  'refunded',
  'refused',
]
const PAGE_SIZES: readonly number[] = [10, 20, 50, 100]

/** Maximum span accepted by the backend (`MAX_RANGE_DAYS=90`, sub-feature §3.2). */
export const MAX_RANGE_DAYS = 90
const MAX_RANGE_MS = MAX_RANGE_DAYS * 24 * 60 * 60 * 1000

/**
 * Applied filter state. This is the shape held in the URL by the route's
 * `validateSearch`: every list/page query is a pure function of it, so a
 * refresh or a shared link recreates exactly the same view.
 */
export interface UsageLogSearch {
  range: UsageLogRangeFilter
  /** Raw `datetime-local` values, only for `range=custom`. */
  start?: string
  end?: string
  model?: string
  group?: string
  token_id?: number
  request_id?: string
  status?: UsageLogStatus
  username?: string
  /**
   * F-05-03 in-page view switch. Only `stats` is serialised: an explicit
   * `?tab=logs` is normalised away so the default view never grows a
   * redundant parameter. Any other value falls back to the list view.
   */
  tab?: UsageLogTab
  p: number
  page_size: number
}

export const DEFAULT_USAGE_LOG_SEARCH: UsageLogSearch = {
  range: '24h',
  p: 1,
  page_size: 20,
}

function asTrimmedString(value: unknown): string | undefined {
  if (typeof value !== 'string') return undefined
  const trimmed = value.trim()
  return trimmed === '' ? undefined : trimmed
}

function asPositiveInteger(value: unknown): number | undefined {
  const parsed =
    typeof value === 'number'
      ? value
      : typeof value === 'string' && value.trim() !== ''
        ? Number(value)
        : Number.NaN
  if (!Number.isInteger(parsed) || parsed < 1) return undefined
  return parsed
}

/**
 * UUID shape gate. The backend returns 422 for a malformed `request_id`
 * (sub-feature §3.2); rejecting it here keeps the invalid value out of the URL
 * and out of the query entirely.
 */
export function isUuid(value: string): boolean {
  return /^[0-9a-f]{8}-[0-9a-f]{4}-[1-5][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/i.test(
    value,
  )
}

/** Normalizes route search into the frozen filter shape; unknown keys are dropped. */
export function validateUsageLogSearch(
  search: Record<string, unknown>,
): UsageLogSearch {
  const result: UsageLogSearch = {
    range: DEFAULT_USAGE_LOG_SEARCH.range,
    p: 1,
    page_size: DEFAULT_USAGE_LOG_SEARCH.page_size,
  }

  const range = search.range
  if (range === 'custom') {
    result.range = 'custom'
    const start = asTrimmedString(search.start)
    const end = asTrimmedString(search.end)
    if (start !== undefined) result.start = start
    if (end !== undefined) result.end = end
  } else if (
    typeof range === 'string' &&
    (RANGE_VALUES as readonly string[]).includes(range)
  ) {
    result.range = range as UsageLogRange
  }

  const model = asTrimmedString(search.model)
  if (model !== undefined) result.model = model
  const group = asTrimmedString(search.group)
  if (group !== undefined) result.group = group
  const username = asTrimmedString(search.username)
  if (username !== undefined) result.username = username

  const tokenId = asPositiveInteger(search.token_id)
  if (tokenId !== undefined) result.token_id = tokenId

  // The raw value is kept so a shared link with a malformed `request_id` can be
  // reported to the user instead of silently rendering an unfiltered list.
  // Callers must gate `isUuid` before putting it into a query (the backend
  // answers 422 for malformed values, sub-feature §3.2).
  const requestId = asTrimmedString(search.request_id)
  if (requestId !== undefined) result.request_id = requestId

  const status = search.status
  if (
    typeof status === 'string' &&
    (STATUS_VALUES as readonly string[]).includes(status)
  ) {
    result.status = status as UsageLogStatus
  }

  // `logs` is the default: keep it out of the canonical shape so switching
  // back does not leave `?tab=logs` behind in the address bar.
  if (search.tab === 'stats') result.tab = 'stats'

  const page = asPositiveInteger(search.p)
  if (page !== undefined) result.p = page

  const pageSize = asPositiveInteger(search.page_size)
  if (pageSize !== undefined && PAGE_SIZES.includes(pageSize)) {
    result.page_size = pageSize
  }

  return result
}

/** ISO timestamps for a custom range; both ends are required to apply it. */
function customRange(search: UsageLogSearch): { start: string; end: string } | undefined {
  if (search.range !== 'custom') return undefined
  const start = toIso(search.start)
  const end = toIso(search.end)
  if (start === undefined || end === undefined) return undefined
  return { start, end }
}

function toIso(value: string | undefined): string | undefined {
  if (value === undefined || value === '') return undefined
  const timestamp = Date.parse(value)
  if (Number.isNaN(timestamp)) return undefined
  return new Date(timestamp).toISOString()
}

/**
 * The range parameters shared by the list and every `/api/usage-stats/*`
 * endpoint. Keeping this in one place is what guarantees the cards, the
 * charts and the table describe the same window (F-05-03 §5.7).
 */
export function usageRangeParams(
  search: UsageLogSearch,
): { range: UsageLogRange } | { start: string; end: string } {
  const custom = customRange(search)
  if (search.range === 'custom') {
    // An incomplete/invalid custom range would otherwise silently widen to the
    // 24h default; falling back to the preset keeps the request valid.
    return custom ?? { range: '24h' }
  }
  return { range: search.range }
}

/**
 * Granularity is automatic (§5.1): 24h and short custom windows are hourly,
 * everything wider is daily. A short custom window stays hourly so a
 * sub-day range does not collapse into one bucket.
 */
export function usageStatsGranularity(
  search: UsageLogSearch,
): 'hour' | 'day' {
  const custom = customRange(search)
  if (custom !== undefined) {
    const span = Date.parse(custom.end) - Date.parse(custom.start)
    return span <= 48 * 60 * 60 * 1000 ? 'hour' : 'day'
  }
  // `custom` without both ends falls back to the 24h preset in
  // `usageRangeParams`, so the granularity must follow that same fallback.
  return search.range === 'custom' || search.range === '24h' ? 'hour' : 'day'
}

export type CustomRangeError = 'required' | 'invalid' | 'order' | 'too_long' | null

/**
 * Validates a custom range entirely client-side (Review Focus 2): a reversed
 * or >90-day span never reaches the network. The backend's 422 stays as the
 * authoritative backstop for hand-crafted requests.
 */
export function validateCustomRange(
  start: string,
  end: string,
): CustomRangeError {
  if (start === '' || end === '') return 'required'
  const startMs = Date.parse(start)
  const endMs = Date.parse(end)
  if (Number.isNaN(startMs) || Number.isNaN(endMs)) return 'invalid'
  if (startMs >= endMs) return 'order'
  if (endMs - startMs > MAX_RANGE_MS) return 'too_long'
  return null
}
