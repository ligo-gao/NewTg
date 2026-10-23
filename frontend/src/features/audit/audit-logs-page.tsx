import { useCallback, useEffect, useRef, useState } from 'react'
import { useQuery } from '@tanstack/react-query'

import { apiClient } from '@/api/client'
import { ApiError } from '@/api/errors'
import { requireData } from '@/api/result'
import type { operations } from '@/api/schema'
import { useLogsCapabilities } from '@/features/logs/capabilities'
import {
  EmptyState,
  InlineError,
  PaginationFooter,
  SkeletonRows,
} from '@/features/logs/table-ui'
import { formatRelativeTime } from '@/shared/lib/datetime'
import { useCountdown } from '@/shared/lib/use-countdown'
import { useI18n } from '@/shared/lib/i18n-context'
import { handleRadioGroupKeyDown } from '@/shared/lib/roving-focus'
import { TextField } from '@/shared/ui/form'
import { StatusBadge } from '@/shared/ui/status-badge'
import { Tag } from '@/shared/ui/tag'
import { useToastStore } from '@/shared/ui/toast'

import { AuditDetailDialog } from './audit-detail-dialog'
import { auditActionLabel } from './actions'
import {
  DEFAULT_AUDIT_LOG_SEARCH,
  type AuditCategory,
  type AuditLogItem,
  type AuditLogSearch,
  type AuditRange,
  type AuditResult,
} from './search'

type AuditLogsQuery = NonNullable<
  operations['listAuditLogs']['parameters']['query']
>

const RANGES: readonly { value: AuditRange; key: string }[] = [
  { value: '24h', key: 'range_24h' },
  { value: '7d', key: 'range_7d' },
  { value: '14d', key: 'range_14d' },
  { value: '30d', key: 'range_30d' },
]

const CATEGORIES: readonly AuditCategory[] = [
  'login',
  'security',
  'operation',
  'access_token',
]

const RESULTS: readonly AuditResult[] = ['success', 'failed']

function asRecord(value: unknown): Record<string, unknown> | undefined {
  return typeof value === 'object' && value !== null
    ? (value as Record<string, unknown>)
    : undefined
}

function readString(
  source: Record<string, unknown> | undefined,
  keys: readonly string[],
): string | undefined {
  for (const key of keys) {
    const value = source?.[key]
    if (typeof value === 'string' && value !== '') return value
  }
  return undefined
}

/** Short `Chrome · macOS` client summary (modules-05 §5.4). */
function summarizeUserAgent(userAgent: string | null | undefined): string {
  if (userAgent === null || userAgent === undefined || userAgent === '') {
    return '—'
  }
  const browser = /Edg\//.test(userAgent)
    ? 'Edge'
    : /Chrome\//.test(userAgent)
      ? 'Chrome'
      : /Firefox\//.test(userAgent)
        ? 'Firefox'
        : /Safari\//.test(userAgent)
          ? 'Safari'
          : undefined
  const os = /Windows/.test(userAgent)
    ? 'Windows'
    : /Mac OS X|Macintosh/.test(userAgent)
      ? 'macOS'
      : /Android/.test(userAgent)
        ? 'Android'
        : /iPhone|iPad/.test(userAgent)
          ? 'iOS'
          : /Linux/.test(userAgent)
            ? 'Linux'
            : undefined
  return [browser, os].filter(Boolean).join(' · ') || userAgent.slice(0, 40)
}

export interface AuditLogsPageProps {
  search: AuditLogSearch
  onSearchChange: (next: AuditLogSearch) => void
}

/**
 * F-05-05 audit log page (modules-05 §5).
 *
 * A user only ever sees their own events: the `username` filter is not
 * rendered nor sent, and the user column is hidden. Those gates are UX only;
 * `/api/audit-logs` narrows the data scope server-side regardless.
 */
export function AuditLogsPage({ search, onSearchChange }: AuditLogsPageProps) {
  const { t, locale } = useI18n()
  const capabilities = useLogsCapabilities()
  const push = useToastStore((state) => state.push)
  const {
    seconds: retrySeconds,
    start: startCountdown,
    stop: stopCountdown,
  } = useCountdown()
  const [selected, setSelected] = useState<AuditLogItem | null>(null)
  const [usernameDraft, setUsernameDraft] = useState(search.username ?? '')
  const [retryRemaining, setRetryRemaining] = useState(0)

  const searchRef = useRef(search)
  const onSearchChangeRef = useRef(onSearchChange)
  useEffect(() => {
    searchRef.current = search
    onSearchChangeRef.current = onSearchChange
  }, [search, onSearchChange])

  const update = useCallback((patch: Partial<AuditLogSearch>) => {
    const next = { ...searchRef.current, ...patch }
    searchRef.current = next
    onSearchChangeRef.current(next)
  }, [])

  // Re-seed the username draft only when the *applied* username changed, so an
  // in-flight value survives a category/result/range change inside the
  // debounce window.
  const lastSearchRef = useRef(search)
  useEffect(() => {
    const previous = lastSearchRef.current
    if (previous === search) return
    if ((search.username ?? '') !== (previous.username ?? '')) {
      setUsernameDraft(search.username ?? '')
    }
    lastSearchRef.current = search
  }, [search])

  useEffect(() => {
    if (!capabilities.canFilterByUsername) return
    const next = usernameDraft.trim()
    if (next === (search.username ?? '')) return
    if (next !== '' && next.length < 2) return
    const timer = setTimeout(() => {
      update({ username: next === '' ? undefined : next, p: 1 })
    }, 300)
    return () => {
      clearTimeout(timer)
    }
  }, [usernameDraft, search.username, capabilities.canFilterByUsername, update])

  const query: AuditLogsQuery = { p: search.p, page_size: search.page_size }
  query.range = search.range
  if (search.category !== undefined) query.category = search.category
  if (search.result !== undefined) query.result = search.result
  if (capabilities.canFilterByUsername && search.username !== undefined) {
    query.username = search.username
  }

  const listQuery = useQuery({
    queryKey: ['audit-logs', 'list', query, capabilities.canFilterByUsername],
    enabled: !capabilities.loading,
    queryFn: async () =>
      requireData(await apiClient.GET('/api/audit-logs', { params: { query } })),
  })

  const data = listQuery.data
  const total = data?.total ?? 0
  const items = data?.items ?? []

  const listError = listQuery.error
  const isRateLimited =
    listError instanceof ApiError && listError.kind === 'rate_limited'
  const countdownErrorRef = useRef<unknown>(null)

  useEffect(() => {
    if (!isRateLimited) {
      countdownErrorRef.current = null
      stopCountdown()
      setRetryRemaining(0)
      return
    }
    if (countdownErrorRef.current === listError) return
    countdownErrorRef.current = listError
    const seconds = (listError as ApiError).retry_after ?? 60
    setRetryRemaining(seconds)
    startCountdown(seconds)
  }, [isRateLimited, listError, startCountdown, stopCountdown])

  useEffect(() => {
    // Keep 0 at expiry instead of snapping back to the original Retry-After.
    if (countdownErrorRef.current !== null) setRetryRemaining(retrySeconds)
  }, [retrySeconds])

  useEffect(() => {
    if (!listQuery.isSuccess || data === undefined) return
    const lastPage = Math.max(
      1,
      Math.ceil((data.total ?? 0) / search.page_size),
    )
    if (search.p <= lastPage) return
    push({ tone: 'info', message: t('logs.usage.list_updated') })
    update({ p: lastPage })
  }, [
    listQuery.isSuccess,
    data,
    search.p,
    search.page_size,
    push,
    t,
    update,
  ])

  const hasActiveFilters =
    search.range !== DEFAULT_AUDIT_LOG_SEARCH.range ||
    search.category !== undefined ||
    search.result !== undefined ||
    search.username !== undefined

  function clearFilters(): void {
    setUsernameDraft('')
    const next: AuditLogSearch = {
      ...DEFAULT_AUDIT_LOG_SEARCH,
      page_size: searchRef.current.page_size,
    }
    searchRef.current = next
    onSearchChangeRef.current(next)
  }

  let errorMessage: string | null = null
  if (listError instanceof ApiError) {
    if (listError.kind === 'rate_limited') {
      errorMessage = t('logs.audit.error.too_many', {
        seconds: retryRemaining,
      })
    } else if (listError.kind === 'offline') {
      errorMessage = t('logs.audit.error.offline')
    } else {
      errorMessage = t('logs.audit.error.load')
    }
  } else if (listError !== null) {
    errorMessage = t('logs.audit.error.load')
  }

  const columnCount = 6 + (capabilities.canFilterByUsername ? 1 : 0)

  return (
    <section className="mx-auto flex max-w-[1520px] flex-col gap-4">
      <div className="flex flex-wrap items-baseline justify-between gap-3">
        <h1 className="text-xl font-semibold text-ink-1">
          {t('logs.audit.title')}
        </h1>
        <button
          type="button"
          onClick={clearFilters}
          className="text-[13px] font-medium text-brand focus-visible:shadow-[var(--focus-ring)] focus-visible:outline-none"
          hidden={!hasActiveFilters}
        >
          {t('logs.audit.filters.clear')}
        </button>
      </div>

      <div className="rounded-xl border border-border-hairline bg-surface-card p-4">
        <div className="flex flex-wrap items-end gap-3">
          <div
            role="radiogroup"
            aria-label={t('logs.audit.filters.range')}
            // UI-5：与用量日志页一致的单选组键盘行为（方向键移动+选中）。
            onKeyDown={handleRadioGroupKeyDown}
            className="inline-flex rounded-lg border border-border-default bg-surface-card p-0.5"
          >
            {RANGES.map((option) => {
              const active = search.range === option.value
              return (
                <button
                  key={option.value}
                  type="button"
                  role="radio"
                  aria-checked={active}
                  tabIndex={active ? 0 : -1}
                  onClick={() => {
                    update({ range: option.value, p: 1 })
                  }}
                  className={[
                    'rounded-md px-2.5 py-1 text-[13px] focus-visible:shadow-[var(--focus-ring)] focus-visible:outline-none',
                    active
                      ? 'bg-surface-hover font-medium text-ink-1'
                      : 'text-ink-2 hover:text-ink-1',
                  ].join(' ')}
                >
                  {t(`logs.audit.filters.${option.key}`)}
                </button>
              )
            })}
          </div>

          <label className="flex flex-col gap-1.5 text-[13px] font-medium text-ink-1">
            {t('logs.audit.filters.category')}
            <select
              aria-label={t('logs.audit.filters.category')}
              value={search.category ?? 'all'}
              onChange={(event) => {
                const value = event.target.value
                update({
                  category:
                    value === 'all' ? undefined : (value as AuditCategory),
                  p: 1,
                })
              }}
              className="h-9 rounded-lg border border-border-default bg-surface-card px-2 text-sm text-ink-1"
            >
              <option value="all">{t('logs.audit.filters.category_all')}</option>
              {CATEGORIES.map((category) => (
                <option key={category} value={category}>
                  {t(`logs.audit.category.${category}`)}
                </option>
              ))}
            </select>
          </label>

          <label className="flex flex-col gap-1.5 text-[13px] font-medium text-ink-1">
            {t('logs.audit.filters.result')}
            <select
              aria-label={t('logs.audit.filters.result')}
              value={search.result ?? 'all'}
              onChange={(event) => {
                const value = event.target.value
                update({
                  result: value === 'all' ? undefined : (value as AuditResult),
                  p: 1,
                })
              }}
              className="h-9 rounded-lg border border-border-default bg-surface-card px-2 text-sm text-ink-1"
            >
              <option value="all">{t('logs.audit.filters.result_all')}</option>
              {RESULTS.map((result) => (
                <option key={result} value={result}>
                  {t(`logs.audit.result.${result}`)}
                </option>
              ))}
            </select>
          </label>

          {capabilities.canFilterByUsername ? (
            <TextField
              id="audit-username"
              label={t('logs.audit.filters.username')}
              placeholder={t('logs.audit.filters.username_placeholder')}
              value={usernameDraft}
              onChange={(event) => {
                setUsernameDraft(event.target.value)
              }}
              className="h-9 w-40"
            />
          ) : null}
        </div>
      </div>

      <div className="flex flex-wrap items-center justify-between gap-3 text-sm text-ink-2">
        {errorMessage === null ? (
          <span data-testid="audit-total">
            {t('logs.audit.total', {
              total: new Intl.NumberFormat(locale).format(total),
            })}
          </span>
        ) : null}
      </div>

      <div className="overflow-x-auto rounded-xl border border-border-hairline bg-surface-card">
        {errorMessage !== null ? (
          <InlineError
            message={errorMessage}
            onRetry={() => void listQuery.refetch()}
            retryLabel={t('logs.audit.error.retry')}
          />
        ) : listQuery.isPending ? (
          <SkeletonRows columns={columnCount} />
        ) : items.length === 0 ? (
          <EmptyState
            title={
              hasActiveFilters
                ? t('logs.audit.empty.filtered')
                : t('logs.audit.empty.none')
            }
            hint={
              hasActiveFilters ? undefined : t('logs.audit.empty.hint')
            }
          />
        ) : (
          <table className="w-full text-sm">
            <thead>
              <tr className="border-b border-border-hairline text-left text-xs text-ink-2">
                <th scope="col" className="px-4 py-2 font-medium">
                  {t('logs.audit.columns.time')}
                </th>
                {capabilities.canFilterByUsername ? (
                  <th scope="col" className="px-4 py-2 font-medium">
                    {t('logs.audit.columns.username')}
                  </th>
                ) : null}
                <th scope="col" className="px-4 py-2 font-medium">
                  {t('logs.audit.columns.event')}
                </th>
                <th scope="col" className="px-4 py-2 font-medium">
                  {t('logs.audit.columns.ip_client')}
                </th>
                <th scope="col" className="px-4 py-2 font-medium">
                  {t('logs.audit.columns.method_route')}
                </th>
                <th scope="col" className="px-4 py-2 font-medium">
                  {t('logs.audit.columns.status')}
                </th>
                <th scope="col" className="px-4 py-2 font-medium">
                  {t('logs.audit.columns.details')}
                </th>
              </tr>
            </thead>
            <tbody>
              {items.map((row) => {
                const detail = asRecord(row.detail)
                // 优先用 writer 注入的 detail.action（冻结枚举），再兼容旧字段；
                // 未登记动作由 auditActionLabel 做可读回退，绝不直接抛错。
                const action = auditActionLabel(
                  t,
                  readString(detail, [
                    'action',
                    'action_label',
                    'event',
                    'description',
                  ]),
                )
                return (
                  <tr
                    key={row.id}
                    className="h-11 border-b border-border-hairline last:border-0 hover:bg-surface-hover"
                  >
                    <td
                      className="px-4 py-2 text-ink-2"
                      title={new Date(row.created_at).toISOString()}
                    >
                      {formatRelativeTime(row.created_at, locale)}
                    </td>
                    {capabilities.canFilterByUsername ? (
                      <td className="px-4 py-2 text-ink-2">
                        {row.username ?? '—'}
                      </td>
                    ) : null}
                    <td className="px-4 py-2">
                      <div className="flex flex-wrap items-center gap-2">
                        <Tag variant="neutral">
                          {t(`logs.audit.category.${row.category}`)}
                        </Tag>
                        <span className="text-ink-1">{action}</span>
                      </div>
                    </td>
                    <td className="px-4 py-2">
                      <div className="font-mono text-[13px] text-ink-1">
                        {row.ip ?? '—'}
                      </div>
                      <div className="text-[12px] text-ink-2">
                        {summarizeUserAgent(row.user_agent)}
                      </div>
                    </td>
                    <td className="px-4 py-2">
                      <span className="font-mono text-[12px] font-semibold text-ink-1">
                        {row.method ?? '—'}
                      </span>
                      <span className="ml-2 font-mono text-[12px] text-ink-2">
                        {row.route ?? '—'}
                      </span>
                    </td>
                    <td className="px-4 py-2">
                      <div className="font-mono text-[13px]">
                        {row.status_code ?? '—'}
                      </div>
                      <StatusBadge
                        variant={
                          row.result === 'success' ? 'success' : 'danger'
                        }
                      >
                        {t(`logs.audit.result.${row.result}`)}
                      </StatusBadge>
                    </td>
                    <td className="px-4 py-2">
                      <button
                        type="button"
                        onClick={() => {
                          setSelected(row)
                        }}
                        className="text-[13px] font-medium text-brand focus-visible:shadow-[var(--focus-ring)] focus-visible:outline-none"
                      >
                        {t('logs.audit.view')}
                      </button>
                    </td>
                  </tr>
                )
              })}
            </tbody>
          </table>
        )}
      </div>

      {errorMessage === null &&
      !listQuery.isPending &&
      items.length > 0 ? (
        <PaginationFooter
          namespace="audit"
          page={search.p}
          pageSize={search.page_size}
          total={total}
          onPageChange={(page) => {
            update({ p: page })
          }}
          onPageSizeChange={(pageSize) => {
            update({ page_size: pageSize, p: 1 })
          }}
        />
      ) : null}

      <AuditDetailDialog
        item={selected}
        onOpenChange={(open) => {
          if (!open) setSelected(null)
        }}
        showUsername={capabilities.canFilterByUsername}
      />
    </section>
  )
}
