import { useCallback, useEffect, useRef, useState, type ReactNode } from 'react'
import { useQuery } from '@tanstack/react-query'

import { apiClient } from '@/api/client'
import { ApiError } from '@/api/errors'
import { requireData } from '@/api/result'
import type { operations } from '@/api/schema'
import { formatRelativeTime } from '@/shared/lib/datetime'
import { formatCredits, formatTokenAmount } from '@/shared/lib/format'
import { useCountdown } from '@/shared/lib/use-countdown'
import { useI18n } from '@/shared/lib/i18n-context'
import { handleRadioGroupKeyDown } from '@/shared/lib/roving-focus'
import { TextField } from '@/shared/ui/form'
import { Dialog } from '@/shared/ui/base/dialog'
import { useToastStore } from '@/shared/ui/toast'

import { useLogsCapabilities } from './capabilities'
import { ExportDialog } from './export-dialog'
import { formatSeconds } from './format'
import { LogDetailDrawer } from './log-detail-drawer'
import { UsageStatusBadge, USAGE_STATUSES } from './log-status-badge'
import {
  DEFAULT_USAGE_LOG_SEARCH,
  isUuid,
  validateCustomRange,
  type CustomRangeError,
  type UsageLogRangeFilter,
  type UsageLogSearch,
  type UsageLogStatus,
} from './search'
import {
  EmptyState,
  InlineError,
  PaginationFooter,
  SkeletonRows,
} from './table-ui'
import { PAGE_SIZE_OPTIONS } from './ui-constants'

type UsageLogsQuery = NonNullable<
  operations['listUsageLogs']['parameters']['query']
> & {
  /**
   * F-05-06 会话内敏感开关。OpenAPI 冻结稿尚未收录该参数，但后端已实现；
   * 缺省即打码，只有显式 1 才会请求原文（服务端仍重校验 log.prompt.view）。
   */
  reveal_sensitive?: 0 | 1
}

const RANGE_OPTIONS: readonly { value: UsageLogRangeFilter; key: string }[] = [
  { value: '24h', key: 'range_24h' },
  { value: '7d', key: 'range_7d' },
  { value: '14d', key: 'range_14d' },
  { value: '30d', key: 'range_30d' },
  { value: 'custom', key: 'range_custom' },
]

export interface UsageLogsPageProps {
  /** Applied filters, held in the URL by the route (single source of truth). */
  search: UsageLogSearch
  onSearchChange: (next: UsageLogSearch) => void
}

function toIso(value: string | undefined): string | undefined {
  if (value === undefined || value === '') return undefined
  const timestamp = Date.parse(value)
  if (Number.isNaN(timestamp)) return undefined
  return new Date(timestamp).toISOString()
}

function buildQuery(
  search: UsageLogSearch,
  canFilterByUsername: boolean,
  revealSensitive: boolean,
): UsageLogsQuery {
  const params: UsageLogsQuery = {
    p: search.p,
    page_size: search.page_size,
  }

  if (search.range === 'custom') {
    const start = toIso(search.start)
    const end = toIso(search.end)
    if (start !== undefined && end !== undefined) {
      params.start = start
      params.end = end
    }
  } else {
    params.range = search.range
  }

  if (search.model !== undefined) params.model = search.model
  if (search.group !== undefined) params.group = search.group
  if (search.token_id !== undefined) params.token_id = search.token_id
  if (search.request_id !== undefined) params.request_id = search.request_id
  if (search.status !== undefined) params.status = search.status
  // Admin-only parameter (sub-feature §3.8). A user must never send it even if
  // a hand-crafted URL carries one; the server would answer 403.
  if (canFilterByUsername && search.username !== undefined) {
    params.username = search.username
  }

  // Only send the flag when the user asked for it: the default request stays
  // byte-for-byte the masked query the list has always made.
  if (revealSensitive) params.reveal_sensitive = 1

  return params
}

/**
 * F-05-01 usage-log list (modules-05 §1, R-M6-13).
 *
 * The applied filter set lives in the route search string; typing edits local
 * drafts that only reach the URL after their debounce elapses (300ms, 500ms
 * for the exact request ID). Every query is a pure function of `search`, so a
 * refresh or a shared link reproduces the same page. Statistics stay on
 * `/dashboard` — this page is list + detail only.
 */
export function UsageLogsPage({ search, onSearchChange }: UsageLogsPageProps) {
  const { t, locale } = useI18n()
  const capabilities = useLogsCapabilities()
  const push = useToastStore((state) => state.push)
  const {
    seconds: retrySeconds,
    start: startCountdown,
    stop: stopCountdown,
  } = useCountdown()

  const [selectedId, setSelectedId] = useState<number | null>(null)
  const [modelDraft, setModelDraft] = useState(search.model ?? '')
  const [groupDraft, setGroupDraft] = useState(search.group ?? '')
  const [tokenIdDraft, setTokenIdDraft] = useState(
    search.token_id === undefined ? '' : String(search.token_id),
  )
  const [requestIdDraft, setRequestIdDraft] = useState(search.request_id ?? '')
  const [requestIdError, setRequestIdError] = useState(false)
  const [retryRemaining, setRetryRemaining] = useState(0)
  const [usernameDraft, setUsernameDraft] = useState(search.username ?? '')
  const [customStart, setCustomStart] = useState(search.start ?? '')
  const [customEnd, setCustomEnd] = useState(search.end ?? '')
  const [customError, setCustomError] = useState<CustomRangeError>(null)
  // F-05-06：敏感开关只存在于本次会话（不落 URL、不落库），关闭页面即恢复
  // 默认隐藏。它不进入路由，避免把「显示原文」变成可分享链接。
  const [revealSensitive, setRevealSensitive] = useState(false)
  const [sensitiveConfirmOpen, setSensitiveConfirmOpen] = useState(false)
  const [exportOpen, setExportOpen] = useState(false)

  // `update` stays stable so debounce effects are not restarted by unrelated
  // parent renders; the latest applied search is read through a ref.
  const searchRef = useRef(search)
  const onSearchChangeRef = useRef(onSearchChange)
  useEffect(() => {
    searchRef.current = search
    onSearchChangeRef.current = onSearchChange
  }, [search, onSearchChange])

  const update = useCallback((patch: Partial<UsageLogSearch>) => {
    const next = { ...searchRef.current, ...patch }
    searchRef.current = next
    onSearchChangeRef.current(next)
  }, [])

  // External navigation (back/forward, shared link) re-seeds only the fields
  // whose applied value actually changed. Re-seeding every draft on any search
  // change would wipe text the user is still typing when another filter is
  // changed inside the debounce window.
  const lastSearchRef = useRef(search)
  useEffect(() => {
    const previous = lastSearchRef.current
    if (previous === search) return
    if ((search.model ?? '') !== (previous.model ?? '')) {
      setModelDraft(search.model ?? '')
    }
    if ((search.group ?? '') !== (previous.group ?? '')) {
      setGroupDraft(search.group ?? '')
    }
    if (search.token_id !== previous.token_id) {
      setTokenIdDraft(
        search.token_id === undefined ? '' : String(search.token_id),
      )
    }
    if ((search.request_id ?? '') !== (previous.request_id ?? '')) {
      setRequestIdDraft(search.request_id ?? '')
      setRequestIdError(false)
    }
    if ((search.username ?? '') !== (previous.username ?? '')) {
      setUsernameDraft(search.username ?? '')
    }
    if (search.start !== previous.start) setCustomStart(search.start ?? '')
    if (search.end !== previous.end) setCustomEnd(search.end ?? '')
    lastSearchRef.current = search
  }, [search])

  useEffect(() => {
    const next = modelDraft.trim()
    if (next === (search.model ?? '')) return
    const timer = setTimeout(() => {
      update({ model: next === '' ? undefined : next, p: 1 })
    }, 300)
    return () => {
      clearTimeout(timer)
    }
  }, [modelDraft, search.model, update])

  useEffect(() => {
    const next = groupDraft.trim()
    if (next === (search.group ?? '')) return
    const timer = setTimeout(() => {
      update({ group: next === '' ? undefined : next, p: 1 })
    }, 300)
    return () => {
      clearTimeout(timer)
    }
  }, [groupDraft, search.group, update])

  useEffect(() => {
    const next = tokenIdDraft.trim()
    if (next === '') {
      if (search.token_id === undefined) return
      const timer = setTimeout(() => {
        update({ token_id: undefined, p: 1 })
      }, 300)
      return () => {
        clearTimeout(timer)
      }
    }
    const parsed = Number(next)
    // Invalid token IDs stay out of the query; the field keeps the raw text so
    // the user can correct it.
    if (!Number.isInteger(parsed) || parsed < 1) return
    if (parsed === search.token_id) return
    const timer = setTimeout(() => {
      update({ token_id: parsed, p: 1 })
    }, 300)
    return () => {
      clearTimeout(timer)
    }
  }, [tokenIdDraft, search.token_id, update])

  useEffect(() => {
    const next = requestIdDraft.trim()
    if (next === (search.request_id ?? '')) {
      setRequestIdError(false)
      return
    }
    if (next !== '' && !isUuid(next)) {
      // Invalid UUID never enters the query (sub-feature §3.2).
      setRequestIdError(true)
      return
    }
    setRequestIdError(false)
    const timer = setTimeout(() => {
      update({ request_id: next === '' ? undefined : next, p: 1 })
    }, 500)
    return () => {
      clearTimeout(timer)
    }
  }, [requestIdDraft, search.request_id, update])

  useEffect(() => {
    if (!capabilities.canFilterByUsername) return
    const next = usernameDraft.trim()
    if (next === (search.username ?? '')) return
    // Two characters minimum before an admin username filter is applied.
    if (next !== '' && next.length < 2) return
    const timer = setTimeout(() => {
      update({ username: next === '' ? undefined : next, p: 1 })
    }, 300)
    return () => {
      clearTimeout(timer)
    }
  }, [usernameDraft, search.username, capabilities.canFilterByUsername, update])

  useEffect(() => {
    if (search.range !== 'custom') return
    if (customStart === '' && customEnd === '') {
      setCustomError(null)
      return
    }
    if (
      customStart === (search.start ?? '') &&
      customEnd === (search.end ?? '')
    ) {
      setCustomError(null)
      return
    }
    const error = validateCustomRange(customStart, customEnd)
    setCustomError(error)
    if (error !== null) return
    const timer = setTimeout(() => {
      update({ start: customStart, end: customEnd, p: 1 })
    }, 300)
    return () => {
      clearTimeout(timer)
    }
  }, [customStart, customEnd, search.range, search.start, search.end, update])

  const query = buildQuery(
    search,
    capabilities.canFilterByUsername,
    revealSensitive,
  )
  const listQuery = useQuery({
    queryKey: [
      'usage-logs',
      'list',
      query,
      capabilities.canFilterByUsername,
      revealSensitive,
    ],
    enabled: !capabilities.loading,
    queryFn: async () =>
      requireData(
        await apiClient.GET('/api/usage-logs', { params: { query } }),
      ),
  })

  const data = listQuery.data
  const total = data?.total ?? 0
  const items = data?.items ?? []

  // 429 countdown: the duration is server-driven (`Retry-After`), never a
  // hardcoded constant. The value is mirrored once the countdown reaches zero
  // so the message shows `0` instead of snapping back to the full duration;
  // Retry stays enabled throughout.
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
    if (countdownErrorRef.current !== null) setRetryRemaining(retrySeconds)
  }, [retrySeconds])

  // 权限中途收回（例如管理员撤销 log.prompt.view）时立即回到打码态并告知，
  // 而不是让界面继续假装开关有效。已下发内容保留，下一次请求重新打码。
  useEffect(() => {
    if (capabilities.loading || capabilities.canViewSensitive) return
    if (!revealSensitive) return
    setRevealSensitive(false)
    push({ tone: 'info', message: t('logs.usage.sensitive.permission_changed') })
  }, [
    capabilities.loading,
    capabilities.canViewSensitive,
    revealSensitive,
    push,
    t,
  ])

  // Page overflow fallback (sub-feature §3.2): a shrunken dataset can make the
  // requested offset page empty while `total` is still exact. Return to the
  // last valid page and tell the user the list moved under them.
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
    search.range !== DEFAULT_USAGE_LOG_SEARCH.range ||
    search.model !== undefined ||
    search.group !== undefined ||
    search.token_id !== undefined ||
    search.request_id !== undefined ||
    search.status !== undefined ||
    search.username !== undefined

  function clearFilters(): void {
    setModelDraft('')
    setGroupDraft('')
    setTokenIdDraft('')
    setRequestIdDraft('')
    setUsernameDraft('')
    setCustomStart('')
    setCustomEnd('')
    setCustomError(null)
    setRequestIdError(false)
    // Replace, do not merge: a merge would keep `status`/`model`/… from the
    // previous applied search because the defaults do not spell them out.
    const next: UsageLogSearch = {
      ...DEFAULT_USAGE_LOG_SEARCH,
      page_size: searchRef.current.page_size,
    }
    searchRef.current = next
    onSearchChangeRef.current(next)
  }

  function changeRange(next: UsageLogRangeFilter): void {
    if (next === 'custom') {
      update({ range: 'custom', start: undefined, end: undefined, p: 1 })
      return
    }
    setCustomStart('')
    setCustomEnd('')
    setCustomError(null)
    update({ range: next, start: undefined, end: undefined, p: 1 })
  }

  function copy(value: string): void {
    void navigator.clipboard?.writeText(value).catch(() => undefined)
    push({ tone: 'success', message: t('common.action.copied') })
  }

  // Keep the filter toolbar mounted on every failure: a 422/429 must leave the
  // user able to correct the range or clear the filters. Only the table region
  // is replaced by the inline error.
  let errorMessage: string | null = null
  if (listError instanceof ApiError) {
    if (listError.kind === 'rate_limited') {
      errorMessage = t('logs.usage.error.too_many', {
        seconds: retryRemaining,
      })
    } else if (listError.kind === 'validation') {
      errorMessage = t('logs.usage.error.invalid_range')
    } else if (listError.kind === 'offline') {
      errorMessage = t('logs.usage.error.offline')
    } else {
      errorMessage = t('logs.usage.error.load')
    }
  } else if (listError !== null) {
    errorMessage = t('logs.usage.error.load')
  }

  const columnCount =
    7 + (capabilities.isAdmin ? 1 : 0) + (capabilities.canFilterByUsername ? 1 : 0)

  return (
    <section className="mx-auto flex max-w-[1440px] flex-col gap-4">
      <UsageHeader
        onClear={clearFilters}
        clearLabel={t('logs.usage.filters.reset')}
        showClear={hasActiveFilters}
      />

      <div className="rounded-xl border border-border-hairline bg-surface-card p-4">
        <div className="flex flex-wrap items-end gap-3">
          <div
            role="radiogroup"
            aria-label={t('logs.usage.filters.range')}
            // UI-5：范围组是单选组；键盘方向键移动焦点并直接切换范围，
            // 组内只保留一个 tab stop（选中项）。
            onKeyDown={handleRadioGroupKeyDown}
            className="inline-flex rounded-lg border border-border-default bg-surface-card p-0.5"
          >
            {RANGE_OPTIONS.map((option) => {
              const active = search.range === option.value
              return (
                <button
                  key={option.value}
                  type="button"
                  role="radio"
                  aria-checked={active}
                  tabIndex={active ? 0 : -1}
                  onClick={() => {
                    changeRange(option.value)
                  }}
                  className={[
                    'rounded-md px-2.5 py-1 text-[13px] focus-visible:shadow-[var(--focus-ring)] focus-visible:outline-none',
                    active
                      ? 'bg-surface-hover font-medium text-ink-1'
                      : 'text-ink-2 hover:text-ink-1',
                  ].join(' ')}
                >
                  {t(`logs.usage.filters.${option.key}`)}
                </button>
              )
            })}
          </div>

          <TextField
            id="logs-model"
            label={t('logs.usage.filters.model')}
            placeholder={t('logs.usage.filters.model_placeholder')}
            value={modelDraft}
            onChange={(event) => {
              setModelDraft(event.target.value)
            }}
            className="h-9 w-44"
          />

          <TextField
            id="logs-group"
            label={t('logs.usage.filters.group')}
            placeholder={t('logs.usage.filters.group_placeholder')}
            value={groupDraft}
            onChange={(event) => {
              setGroupDraft(event.target.value)
            }}
            className="h-9 w-36"
          />

          <TextField
            id="logs-token"
            label={t('logs.usage.filters.token')}
            placeholder={t('logs.usage.filters.token_placeholder')}
            inputMode="numeric"
            value={tokenIdDraft}
            onChange={(event) => {
              setTokenIdDraft(event.target.value)
            }}
            className="h-9 w-32"
          />

          <TextField
            id="logs-request-id"
            label={t('logs.usage.filters.request_id')}
            placeholder={t('logs.usage.filters.request_id_placeholder')}
            value={requestIdDraft}
            error={
              requestIdError ? t('logs.usage.invalid_uuid') : undefined
            }
            onChange={(event) => {
              setRequestIdDraft(event.target.value)
            }}
            className="h-9 w-64"
          />

          <label className="flex flex-col gap-1.5 text-[13px] font-medium text-ink-1">
            {t('logs.usage.filters.status')}
            <select
              aria-label={t('logs.usage.filters.status')}
              value={search.status ?? 'all'}
              onChange={(event) => {
                const value = event.target.value
                update({
                  status:
                    value === 'all' ? undefined : (value as UsageLogStatus),
                  p: 1,
                })
              }}
              className="h-9 rounded-lg border border-border-default bg-surface-card px-2 text-sm text-ink-1"
            >
              <option value="all">{t('logs.usage.filters.status_all')}</option>
              {USAGE_STATUSES.map((status) => (
                <option key={status} value={status}>
                  {t(`logs.usage.status.${status}`)}
                </option>
              ))}
            </select>
          </label>

          {capabilities.canFilterByUsername ? (
            <TextField
              id="logs-username"
              label={t('logs.usage.filters.username')}
              placeholder={t('logs.usage.filters.username_placeholder')}
              value={usernameDraft}
              onChange={(event) => {
                setUsernameDraft(event.target.value)
              }}
              className="h-9 w-40"
            />
          ) : null}

          <SensitiveSwitch
            checked={revealSensitive}
            disabled={capabilities.loading || !capabilities.canViewSensitive}
            onChange={(next) => {
              if (next) {
                // 开启前确认：共享屏幕时把 Prompt 原文暴露给旁观者的风险
                // 由用户显式承担（modules-05 §6.4）。
                setSensitiveConfirmOpen(true)
                return
              }
              setRevealSensitive(false)
            }}
          />
        </div>

        {search.range === 'custom' ? (
          <div className="mt-3 flex flex-wrap items-end gap-3">
            <TextField
              id="logs-start"
              type="datetime-local"
              label={t('logs.usage.filters.start')}
              value={customStart}
              onChange={(event) => {
                setCustomStart(event.target.value)
              }}
              className="h-9 w-52"
            />
            <TextField
              id="logs-end"
              type="datetime-local"
              label={t('logs.usage.filters.end')}
              value={customEnd}
              onChange={(event) => {
                setCustomEnd(event.target.value)
              }}
              className="h-9 w-52"
            />
            {customError !== null ? (
              <p role="alert" className="pb-2 text-xs text-danger">
                {customErrorMessage(customError, t)}
              </p>
            ) : null}
          </div>
        ) : null}

        {hasActiveFilters ? (
          <div
            aria-label={t('logs.usage.filters.clear')}
            className="mt-3 flex flex-wrap items-center gap-2"
          >
            {search.model !== undefined ? (
              <FilterChip
                label={`${t('logs.usage.filters.model')}: ${search.model}`}
                onRemove={() => {
                  setModelDraft('')
                  update({ model: undefined, p: 1 })
                }}
              />
            ) : null}
            {search.group !== undefined ? (
              <FilterChip
                label={`${t('logs.usage.filters.group')}: ${search.group}`}
                onRemove={() => {
                  update({ group: undefined, p: 1 })
                }}
              />
            ) : null}
            {search.status !== undefined ? (
              <FilterChip
                label={`${t('logs.usage.filters.status')}: ${t(`logs.usage.status.${search.status}`)}`}
                onRemove={() => {
                  update({ status: undefined, p: 1 })
                }}
              />
            ) : null}
            {search.username !== undefined ? (
              <FilterChip
                label={`${t('logs.usage.filters.username')}: ${search.username}`}
                onRemove={() => {
                  setUsernameDraft('')
                  update({ username: undefined, p: 1 })
                }}
              />
            ) : null}
            <button
              type="button"
              onClick={clearFilters}
              className="text-[13px] font-medium text-brand focus-visible:shadow-[var(--focus-ring)] focus-visible:outline-none"
            >
              {t('logs.usage.filters.reset')}
            </button>
          </div>
        ) : null}
      </div>

      <div className="flex flex-wrap items-center justify-between gap-3 text-sm text-ink-2">
        {errorMessage === null ? (
          <span data-testid="logs-total">
            {total > 10_000
              ? t('logs.usage.total_capped')
              : t('logs.usage.total', {
                  total: new Intl.NumberFormat(locale).format(total),
                })}
          </span>
        ) : (
          <span />
        )}
        <button
          type="button"
          data-testid="logs-export-open"
          // 空列表也允许打开：用户可能想导出「全部」，不能用当前页 0 条
          // 推断整个可见范围为空（服务端 204 会给出准确答复）。
          onClick={() => {
            setExportOpen(true)
          }}
          className="inline-flex h-9 items-center gap-1.5 rounded-lg border border-border-default bg-surface-card px-3 text-[13px] font-medium text-ink-1 hover:bg-surface-hover focus-visible:shadow-[var(--focus-ring)] focus-visible:outline-none"
        >
          <span aria-hidden="true">↓</span>
          {t('logs.usage.export.button')}
        </button>
      </div>

      <div className="overflow-x-auto rounded-xl border border-border-hairline bg-surface-card">
        {errorMessage !== null ? (
          <InlineError
            message={errorMessage}
            onRetry={() => void listQuery.refetch()}
            retryLabel={t('logs.usage.error.retry')}
          />
        ) : listQuery.isPending ? (
          <SkeletonRows columns={columnCount} />
        ) : items.length === 0 ? (
          <EmptyState
            title={
              hasActiveFilters
                ? t('logs.usage.empty.filtered')
                : t('logs.usage.empty.none')
            }
            hint={
              hasActiveFilters
                ? undefined
                : t('logs.usage.empty.description')
            }
            action={
              hasActiveFilters ? (
                <button
                  type="button"
                  onClick={clearFilters}
                  className="text-[13px] font-medium text-brand focus-visible:shadow-[var(--focus-ring)] focus-visible:outline-none"
                >
                  {t('logs.usage.empty.clear')}
                </button>
              ) : null
            }
          />
        ) : (
          <table className="w-full text-sm">
            <thead>
              <tr className="border-b border-border-hairline text-left text-xs text-ink-2">
                <th scope="col" className="px-4 py-2 font-medium">
                  {t('logs.usage.columns.time')}
                </th>
                <th scope="col" className="px-4 py-2 font-medium">
                  {t('logs.usage.columns.status')}
                </th>
                <th scope="col" className="px-4 py-2 font-medium">
                  {t('logs.usage.columns.model')}
                </th>
                <th scope="col" className="px-4 py-2 font-medium">
                  {t('logs.usage.columns.token')}
                </th>
                <th scope="col" className="px-4 py-2 text-right font-medium">
                  {t('logs.usage.columns.tokens')}
                </th>
                <th scope="col" className="px-4 py-2 text-right font-medium">
                  {t('logs.usage.columns.cost')}
                </th>
                <th scope="col" className="px-4 py-2 font-medium">
                  {t('logs.usage.columns.timing')}
                </th>
                {capabilities.isAdmin ? (
                  <th scope="col" className="px-4 py-2 font-medium">
                    {t('logs.usage.columns.channel')}
                  </th>
                ) : null}
                {capabilities.canFilterByUsername ? (
                  <th scope="col" className="px-4 py-2 font-medium">
                    {t('logs.usage.columns.user')}
                  </th>
                ) : null}
              </tr>
            </thead>
            <tbody>
              {items.map((row) => {
                const retries = Math.max(
                  0,
                  (row.retry_chain?.length ?? 0) - 1,
                )
                return (
                  <tr
                    key={row.id}
                    tabIndex={0}
                    onClick={() => {
                      setSelectedId(row.id)
                    }}
                    onKeyDown={(event) => {
                      if (event.key === 'Enter' || event.key === ' ') {
                        event.preventDefault()
                        setSelectedId(row.id)
                      }
                    }}
                    className="h-11 cursor-pointer border-b border-border-hairline last:border-0 hover:bg-surface-hover focus-visible:bg-surface-hover focus-visible:outline-none"
                  >
                    <td
                      className="px-4 py-2 text-ink-2"
                      title={new Date(row.created_at).toISOString()}
                    >
                      {formatRelativeTime(row.created_at, locale)}
                    </td>
                    <td className="px-4 py-2">
                      <UsageStatusBadge status={row.status} />
                    </td>
                    <td className="px-4 py-2">
                      <span className="font-mono text-[13px]">{row.model}</span>
                      <button
                        type="button"
                        aria-label={t('logs.usage.copy.model')}
                        onClick={(event) => {
                          event.stopPropagation()
                          copy(row.model)
                        }}
                        className="ml-2 rounded px-1 text-ink-3 hover:text-ink-1 focus-visible:shadow-[var(--focus-ring)] focus-visible:outline-none"
                      >
                        ⧉
                      </button>
                    </td>
                    <td className="px-4 py-2 text-ink-2">{row.token_name ?? '—'}</td>
                    <td className="px-4 py-2 text-right font-mono text-[13px] tabular-nums">
                      <span>{formatTokenAmount(row.prompt_tokens, locale)}</span>
                      {' / '}
                      <span>{formatTokenAmount(row.completion_tokens, locale)}</span>
                      {row.cache_read_tokens > 0 ? (
                        <span
                          className="ml-2 text-[12px] text-ink-2"
                          title={t('logs.usage.cache_read_full')}
                        >
                          {t('logs.usage.cache_read', {
                            value: formatTokenAmount(row.cache_read_tokens, locale),
                          })}
                        </span>
                      ) : null}
                    </td>
                    <td className="px-4 py-2 text-right tabular-nums">
                      <span className="font-mono text-[13px]">
                        {formatCredits(row.cost, locale)}
                      </span>
                      <span className="ml-1 text-[12px] text-ink-3">
                        {t('logs.usage.credits')}
                      </span>
                      {row.refunded > 0 ? (
                        <div className="text-[12px] text-warning">
                          {t('logs.usage.refunded', {
                            value: formatCredits(row.refunded, locale),
                          })}
                        </div>
                      ) : null}
                    </td>
                    <td className="px-4 py-2 font-mono text-[13px] tabular-nums">
                      {formatSeconds(row.latency_ms)}
                      {row.stream ? (
                        <div className="text-[12px] text-ink-2">
                          {t('logs.usage.ttft', {
                            value: formatSeconds(row.ttft),
                          })}
                        </div>
                      ) : null}
                      {retries > 0 ? (
                        <div className="text-[12px] text-ink-3">
                          {t('logs.usage.retries', { count: retries })}
                        </div>
                      ) : null}
                    </td>
                    {capabilities.isAdmin ? (
                      <td className="px-4 py-2">
                        <span className="text-[13px] text-ink-2">
                          {row.upstream ?? '—'}
                        </span>
                        {row.upstream !== null && row.upstream !== undefined ? (
                          <button
                            type="button"
                            aria-label={t('logs.usage.copy.channel')}
                            onClick={(event) => {
                              event.stopPropagation()
                              copy(row.upstream as string)
                            }}
                            className="ml-2 rounded px-1 text-ink-3 hover:text-ink-1 focus-visible:shadow-[var(--focus-ring)] focus-visible:outline-none"
                          >
                            ⧉
                          </button>
                        ) : null}
                      </td>
                    ) : null}
                    {capabilities.canFilterByUsername ? (
                      <td className="px-4 py-2 text-ink-2">
                        {row.username ?? '—'}
                      </td>
                    ) : null}
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
          namespace="usage"
          page={search.p}
          pageSize={search.page_size}
          total={total}
          onPageChange={(page) => {
            update({ p: page })
          }}
          onPageSizeChange={(pageSize) => {
            if (!PAGE_SIZE_OPTIONS.includes(pageSize as 10 | 20 | 50)) return
            update({ page_size: pageSize, p: 1 })
          }}
        />
      ) : null}

      <LogDetailDrawer
        logId={selectedId}
        onOpenChange={(open) => {
          if (!open) setSelectedId(null)
        }}
        capabilities={capabilities}
        revealSensitive={revealSensitive}
      />

      <Dialog
        open={sensitiveConfirmOpen}
        onOpenChange={setSensitiveConfirmOpen}
        title={t('logs.usage.sensitive.toggle')}
        description={t('logs.usage.sensitive.confirm')}
        footer={
          <>
            <button
              type="button"
              onClick={() => {
                setSensitiveConfirmOpen(false)
              }}
              className="rounded-lg border border-border-default px-3 py-1.5 text-sm font-medium text-ink-1 hover:bg-surface-hover focus-visible:shadow-[var(--focus-ring)] focus-visible:outline-none"
            >
              {t('common.action.cancel')}
            </button>
            <button
              type="button"
              data-testid="sensitive-confirm"
              onClick={() => {
                setRevealSensitive(true)
                setSensitiveConfirmOpen(false)
              }}
              // 隐私边界用 warning 而非 brand/danger：不是破坏性操作，但
              // 需要用户停下来确认（UI §6.7）。
              className="rounded-lg bg-warning px-3 py-1.5 text-sm font-medium text-surface-card focus-visible:shadow-[var(--focus-ring)] focus-visible:outline-none"
            >
              {t('logs.usage.sensitive.confirm_action')}
            </button>
          </>
        }
      />

      <ExportDialog
        open={exportOpen}
        onOpenChange={setExportOpen}
        search={search}
        filteredTotal={total}
        revealSensitive={revealSensitive}
        canRevealSensitive={capabilities.canViewSensitive}
        canFilterByUsername={capabilities.canFilterByUsername}
        onExported={() => {
          push({ tone: 'success', message: t('logs.usage.export.downloaded') })
        }}
      />
    </section>
  )
}

function UsageHeader({
  onClear,
  clearLabel,
  showClear,
}: {
  onClear: () => void
  clearLabel: string
  showClear: boolean
}) {
  const { t } = useI18n()
  return (
    <div className="flex flex-wrap items-baseline justify-between gap-3">
      <div>
        <h1 className="text-xl font-semibold text-ink-1">
          {t('logs.usage.title')}
        </h1>
        <p className="mt-1 text-[13px] text-ink-3">
          {t('logs.usage.retention')}
        </p>
      </div>
      {showClear ? (
        <button
          type="button"
          onClick={onClear}
          className="text-[13px] font-medium text-brand focus-visible:shadow-[var(--focus-ring)] focus-visible:outline-none"
        >
          {clearLabel}
        </button>
      ) : null}
    </div>
  )
}

function customErrorMessage(
  error: Exclude<CustomRangeError, null>,
  t: (key: string) => string,
): string {
  if (error === 'too_long') return t('logs.usage.invalid_range_long')
  if (error === 'order') return t('logs.usage.invalid_range_order')
  return t('logs.usage.range_required')
}

function FilterChip({
  label,
  onRemove,
}: {
  label: string
  onRemove: () => void
}): ReactNode {
  return (
    <span className="inline-flex h-6 items-center gap-1 rounded-full bg-surface-subtle px-2 text-xs text-ink-2">
      {label}
      <button
        type="button"
        aria-label={`${label} ×`}
        onClick={onRemove}
        className="rounded px-0.5 text-ink-3 hover:text-ink-1 focus-visible:shadow-[var(--focus-ring)] focus-visible:outline-none"
      >
        ×
      </button>
    </span>
  )
}

/**
 * F-05-06 敏感内容开关（modules-05 §6.4 / §6.7 / §6.8）。
 *
 * 关闭是默认且安全的；开启需要确认。无 `log.prompt.view` 时保持可见但
 * 禁用，并给出「没有查看权限」的原因——用户不会误以为开关坏了，也不会
 * 在无权限时发出一份声称已解除打码的请求。
 */
function SensitiveSwitch({
  checked,
  disabled,
  onChange,
}: {
  checked: boolean
  disabled: boolean
  onChange: (next: boolean) => void
}) {
  const { t } = useI18n()
  const label = t('logs.usage.sensitive.toggle')
  const hintId = 'logs-sensitive-hint'

  return (
    <div className="flex flex-col gap-1">
      <button
        type="button"
        role="switch"
        aria-checked={checked}
        aria-label={label}
        aria-describedby={disabled ? hintId : undefined}
        disabled={disabled}
        data-testid="sensitive-toggle"
        onClick={() => {
          onChange(!checked)
        }}
        className="flex h-9 items-center gap-2 rounded-lg border border-border-default bg-surface-card px-2.5 text-[13px] font-medium text-ink-1 hover:bg-surface-hover focus-visible:shadow-[var(--focus-ring)] focus-visible:outline-none disabled:cursor-not-allowed disabled:opacity-50"
      >
        <span>{label}</span>
        <span
          aria-hidden="true"
          className={[
            'relative inline-flex h-5 w-9 shrink-0 items-center rounded-full transition-colors motion-reduce:transition-none',
            checked ? 'bg-warning' : 'bg-surface-subtle',
          ].join(' ')}
        >
          <span
            aria-hidden="true"
            className={[
              'absolute size-4 rounded-full bg-surface-card shadow-sm transition-transform motion-reduce:transition-none',
              checked ? 'translate-x-[18px]' : 'translate-x-0.5',
            ].join(' ')}
          />
        </span>
      </button>
      {disabled ? (
        <span id={hintId} className="text-xs text-ink-3">
          {t('logs.usage.sensitive.no_permission')}
        </span>
      ) : null}
    </div>
  )
}
