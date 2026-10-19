import type { components } from '@/api/schema'

export type AuditLogItem = components['schemas']['AuditLogItem']
export type AuditCategory = NonNullable<AuditLogItem['category']>
export type AuditResult = NonNullable<AuditLogItem['result']>
export type AuditRange = '24h' | '7d' | '14d' | '30d'

const RANGE_VALUES: readonly AuditRange[] = ['24h', '7d', '14d', '30d']
const CATEGORY_VALUES: readonly AuditCategory[] = [
  'login',
  'security',
  'operation',
  'access_token',
]
const RESULT_VALUES: readonly AuditResult[] = ['success', 'failed']
const PAGE_SIZES: readonly number[] = [10, 20, 50, 100]

/** Applied audit filters, held in the URL by the route (single source of truth). */
export interface AuditLogSearch {
  range: AuditRange
  category?: AuditCategory
  result?: AuditResult
  username?: string
  p: number
  page_size: number
}

export const DEFAULT_AUDIT_LOG_SEARCH: AuditLogSearch = {
  range: '30d',
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

/** Normalizes audit route search; unknown keys and enum misses are dropped. */
export function validateAuditLogSearch(
  search: Record<string, unknown>,
): AuditLogSearch {
  const result: AuditLogSearch = {
    range: DEFAULT_AUDIT_LOG_SEARCH.range,
    p: 1,
    page_size: DEFAULT_AUDIT_LOG_SEARCH.page_size,
  }

  const range = search.range
  if (
    typeof range === 'string' &&
    (RANGE_VALUES as readonly string[]).includes(range)
  ) {
    result.range = range as AuditRange
  }

  const category = search.category
  if (
    typeof category === 'string' &&
    (CATEGORY_VALUES as readonly string[]).includes(category)
  ) {
    result.category = category as AuditCategory
  }

  const auditResult = search.result
  if (
    typeof auditResult === 'string' &&
    (RESULT_VALUES as readonly string[]).includes(auditResult)
  ) {
    result.result = auditResult as AuditResult
  }

  const username = asTrimmedString(search.username)
  if (username !== undefined) result.username = username

  const page = asPositiveInteger(search.p)
  if (page !== undefined) result.p = page

  const pageSize = asPositiveInteger(search.page_size)
  if (pageSize !== undefined && PAGE_SIZES.includes(pageSize)) {
    result.page_size = pageSize
  }

  return result
}
