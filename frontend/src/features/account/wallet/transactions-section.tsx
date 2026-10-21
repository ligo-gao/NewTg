import { useEffect, useRef, useState } from 'react'
import { keepPreviousData, useQuery, useQueryClient } from '@tanstack/react-query'

import { apiClient } from '@/api/client'
import { ApiError } from '@/api/errors'
import { requireData } from '@/api/result'
import { PUBLIC_STATUS_QUERY_KEY, type PublicStatus } from '@/lib/public-status'
import { formatRelativeTime } from '@/shared/lib/datetime'
import { formatCredits } from '@/shared/lib/format'
import { useI18n } from '@/shared/lib/i18n-context'
import { useCountdown } from '@/shared/lib/use-countdown'
import { Button } from '@/shared/ui/button'
import { StatusBadge } from '@/shared/ui/status-badge'
import { Tag } from '@/shared/ui/tag'

import { useOnlineStatus } from '../subscription/use-online-status'
import {
  WALLET_STATUS_BADGE_VARIANTS,
  WALLET_TYPE_TAG_VARIANTS,
  statusLabelKey,
  typeLabelKey,
} from './labels'
import {
  WALLET_PAGE_SIZES,
  WALLET_TRANSACTION_STATUSES,
  WALLET_TRANSACTION_TYPES,
  buildWalletTransactionsQuery,
  clearWalletFilters,
  hasWalletFilters,
  validateWalletAmountRange,
  validateWalletRange,
  type WalletTransactionsSearch,
} from './search'
import { TransactionDetailSheet } from './transaction-detail-sheet'
import type {
  WalletTransaction,
  WalletTransactionPage,
  WalletTransactionStatus,
  WalletTransactionType,
} from './types'

/**
 * 列表只在当前页存在 `status='hold'` 行时轮询（R-M3-14 / F-03-04）。
 * 30s 是设计冻结值；连续 2 次失败后停止，避免故障时无限重试。
 */
export const WALLET_HOLD_POLL_INTERVAL_MS = 30_000
export const WALLET_HOLD_POLL_MAX_FAILURES = 2

/** F-03-04：hold 行消失前，状态徽章带脉冲点（reduced-motion 下静止）。 */
const HOLD_PULSE_CLASS = 'animate-pulse motion-reduce:animate-none'

export interface TransactionsSectionProps {
  search: WalletTransactionsSearch
  onSearchChange: (next: WalletTransactionsSearch) => void
}

function pageHasHold(page: WalletTransactionPage | undefined): boolean {
  return page?.items.some((item) => item.status === 'hold') ?? false
}

/**
 * 钱包流水区（F-03-02）：筛选栏 + 工具栏 + 7 列表格 + offset 分页 + 详情抽屉。
 *
 * 所有筛选与分页状态来自 URL（search），组件只负责把变更交回路由；
 * 刷新/分享链接会重建完全相同的查询。
 */
export function TransactionsSection({
  search,
  onSearchChange,
}: TransactionsSectionProps) {
  const { t, locale } = useI18n()
  const online = useOnlineStatus()
  const queryClient = useQueryClient()
  const [selectedId, setSelectedId] = useState<number | null>(null)

  // R-M3-13：单位符号优先读 /api/status 的缓存（根门禁已取过）；未取到
  // 时回退为纯数字，不发额外请求、不硬编码 C/¥。
  const publicStatus = queryClient.getQueryData<PublicStatus>(PUBLIC_STATUS_QUERY_KEY)
  const unit = publicStatus?.credit_display_symbol?.trim() ?? ''

  // 文本/数字/日期输入的草稿：失焦或回车才提交，避免每击键改写 URL
  // （设计：类型/方向即时，金额区间与关联号失焦触发）。
  const [minDraft, setMinDraft] = useState(search.min?.toString() ?? '')
  const [maxDraft, setMaxDraft] = useState(search.max?.toString() ?? '')
  const [refDraft, setRefDraft] = useState(search.ref ?? '')
  const [fromDraft, setFromDraft] = useState(search.from ?? '')
  const [toDraft, setToDraft] = useState(search.to ?? '')

  useEffect(() => {
    setMinDraft(search.min?.toString() ?? '')
  }, [search.min])
  useEffect(() => {
    setMaxDraft(search.max?.toString() ?? '')
  }, [search.max])
  useEffect(() => {
    setRefDraft(search.ref ?? '')
  }, [search.ref])
  useEffect(() => {
    setFromDraft(search.from ?? '')
  }, [search.from])
  useEffect(() => {
    setToDraft(search.to ?? '')
  }, [search.to])

  // 草稿校验只影响「能否提交」与行内报错；查询是否可发只看已提交的 search。
  // 否则用户草稿里输错一个字符就会把已提交列表的查询禁用（切筛选后表格
  // 变骨架）。已提交值仍可能来自手工 URL（>90 天 / min>max），后端 422 是
  // 兜底，但前端在这里先拦住不发请求。
  const draftRangeError = validateWalletRange(fromDraft, toDraft)
  const draftAmountError = validateWalletAmountRange(minDraft, maxDraft)
  const committedRangeError = validateWalletRange(search.from, search.to)
  const committedAmountError =
    search.min !== undefined &&
    search.max !== undefined &&
    search.max < search.min

  const query = buildWalletTransactionsQuery(search)
  // query-core 在每次 fetch 开始时把 fetchFailureCount 归零（fetchState），
  // 跨轮询周期无法累计；用 errorUpdateCount 的增量自己数「连续失败」。
  const pollFailures = useRef(0)
  const lastErrorUpdateCount = useRef(0)
  const listQuery = useQuery({
    queryKey: ['wallet', 'transactions', query],
    queryFn: async () =>
      requireData(
        await apiClient.GET('/api/wallet/transactions', {
          params: { query },
        }),
      ),
    // 切换筛选时保留上一页数据（P3）：新键加载期间不闪骨架。
    placeholderData: keepPreviousData,
    enabled: committedRangeError === null && !committedAmountError,
    // R-M3-14：仅当本次结果含 hold 行时 30s 轮询；连续 2 次失败停止。
    // 成功一次即清零，因此"失败两次后用户手动刷新成功"会重新开启轮询。
    refetchInterval: (queryState) => {
      const state = queryState.state
      if (state.errorUpdateCount !== lastErrorUpdateCount.current) {
        lastErrorUpdateCount.current = state.errorUpdateCount
        pollFailures.current += 1
      }
      if (state.status === 'success') pollFailures.current = 0
      if (!online) return false
      if (pollFailures.current >= WALLET_HOLD_POLL_MAX_FAILURES) return false
      return pageHasHold(state.data) ? WALLET_HOLD_POLL_INTERVAL_MS : false
    },
  })

  const data: WalletTransactionPage | undefined = listQuery.data
  const items = data?.items ?? []
  const total = data?.total ?? 0
  const totalPages = Math.max(1, Math.ceil(total / search.page_size))
  // 服务端会把越界页夹到最后一页（设计验收），展示以响应里的 page 为准。
  const currentPage = data?.page ?? search.p

  const rateLimitError =
    listQuery.error instanceof ApiError &&
    listQuery.error.kind === 'rate_limited'
      ? listQuery.error
      : null
  const { seconds: cooldown, start: startCooldown } = useCountdown()
  const retryAfter = rateLimitError?.retry_after
  useEffect(() => {
    if (retryAfter === undefined) return
    // Retry-After 由服务端给出，最多等 60s；不硬编码窗口长度。
    startCooldown(Math.min(60, Math.max(1, Math.ceil(retryAfter))))
  }, [retryAfter, startCooldown])
  const controlsDisabled = cooldown > 0

  const offlineError = !online || (listQuery.error instanceof ApiError &&
    listQuery.error.kind === 'offline')
  const activeFilters = hasWalletFilters(search)

  function apply(patch: Partial<WalletTransactionsSearch>): void {
    onSearchChange({ ...search, ...patch, p: 1 })
  }

  function commitAmounts(): void {
    if (draftAmountError !== null) return
    apply({
      min: minDraft.trim() === '' ? undefined : Number(minDraft.trim()),
      max: maxDraft.trim() === '' ? undefined : Number(maxDraft.trim()),
    })
  }

  function commitRange(): void {
    if (draftRangeError !== null) return
    apply({
      from: fromDraft === '' ? undefined : fromDraft,
      to: toDraft === '' ? undefined : toDraft,
    })
  }

  function commitRef(): void {
    const trimmed = refDraft.trim()
    if (trimmed.length > 64) return
    apply({ ref: trimmed === '' ? undefined : trimmed })
  }

  function amountText(transaction: WalletTransaction): string {
    const sign = transaction.amount < 0 ? '−' : '+'
    const value = formatCredits(Math.abs(transaction.amount), locale)
    return unit === '' ? `${sign}${value}` : `${sign}${value} ${unit}`
  }

  return (
    <section className="flex flex-col gap-4" aria-label={t('account.wallet.tab_transactions')}>
      <div className="flex flex-wrap items-end gap-3 text-sm">
        <label className="flex flex-col gap-1 text-xs text-ink-2">
          {t('account.wallet.filter_type')}
          <select
            aria-label={t('account.wallet.filter_type')}
            value={search.type ?? ''}
            disabled={controlsDisabled}
            onChange={(event) => {
              const value = event.target.value
              apply({
                type: value === '' ? undefined : (value as WalletTransactionType),
              })
            }}
            className="h-10 rounded-lg border border-border-default bg-surface-card px-2 text-sm text-ink-1"
          >
            <option value="">{t('account.wallet.filter_all')}</option>
            {WALLET_TRANSACTION_TYPES.map((type) => (
              <option key={type} value={type}>
                {t(typeLabelKey(type))}
              </option>
            ))}
          </select>
        </label>

        <label className="flex flex-col gap-1 text-xs text-ink-2">
          {t('account.wallet.filter_status')}
          <select
            aria-label={t('account.wallet.filter_status')}
            value={search.status ?? ''}
            disabled={controlsDisabled}
            onChange={(event) => {
              const value = event.target.value
              apply({
                status:
                  value === '' ? undefined : (value as WalletTransactionStatus),
              })
            }}
            className="h-10 rounded-lg border border-border-default bg-surface-card px-2 text-sm text-ink-1"
          >
            <option value="">{t('account.wallet.filter_all')}</option>
            {WALLET_TRANSACTION_STATUSES.map((status) => (
              <option key={status} value={status}>
                {t(statusLabelKey(status))}
              </option>
            ))}
          </select>
        </label>

        <label className="flex flex-col gap-1 text-xs text-ink-2">
          {t('account.wallet.filter_dir')}
          <select
            aria-label={t('account.wallet.filter_dir')}
            value={search.dir ?? ''}
            disabled={controlsDisabled}
            onChange={(event) => {
              const value = event.target.value
              apply({ dir: value === '' ? undefined : (value as 'in' | 'out') })
            }}
            className="h-10 rounded-lg border border-border-default bg-surface-card px-2 text-sm text-ink-1"
          >
            <option value="">{t('account.wallet.filter_all')}</option>
            <option value="in">{t('account.wallet.dir_in')}</option>
            <option value="out">{t('account.wallet.dir_out')}</option>
          </select>
        </label>

        <label className="flex flex-col gap-1 text-xs text-ink-2">
          {t('account.wallet.filter_from')}
          <input
            type="datetime-local"
            aria-label={t('account.wallet.filter_from')}
            value={fromDraft}
            disabled={controlsDisabled}
            onChange={(event) => setFromDraft(event.target.value)}
            onBlur={commitRange}
            className="h-10 rounded-lg border border-border-default bg-surface-card px-2 text-sm text-ink-1"
          />
        </label>

        <label className="flex flex-col gap-1 text-xs text-ink-2">
          {t('account.wallet.filter_to')}
          <input
            type="datetime-local"
            aria-label={t('account.wallet.filter_to')}
            value={toDraft}
            disabled={controlsDisabled}
            onChange={(event) => setToDraft(event.target.value)}
            onBlur={commitRange}
            className="h-10 rounded-lg border border-border-default bg-surface-card px-2 text-sm text-ink-1"
          />
        </label>

        <label className="flex flex-col gap-1 text-xs text-ink-2">
          {t('account.wallet.filter_min')}
          <input
            type="number"
            min={0}
            inputMode="numeric"
            aria-label={t('account.wallet.filter_min')}
            value={minDraft}
            disabled={controlsDisabled}
            onChange={(event) => setMinDraft(event.target.value)}
            onBlur={commitAmounts}
            className="h-10 w-28 rounded-lg border border-border-default bg-surface-card px-2 text-sm text-ink-1"
          />
        </label>

        <label className="flex flex-col gap-1 text-xs text-ink-2">
          {t('account.wallet.filter_max')}
          <input
            type="number"
            min={0}
            inputMode="numeric"
            aria-label={t('account.wallet.filter_max')}
            value={maxDraft}
            disabled={controlsDisabled}
            onChange={(event) => setMaxDraft(event.target.value)}
            onBlur={commitAmounts}
            className="h-10 w-28 rounded-lg border border-border-default bg-surface-card px-2 text-sm text-ink-1"
          />
        </label>

        <label className="flex flex-col gap-1 text-xs text-ink-2">
          {t('account.wallet.filter_ref')}
          <input
            type="search"
            maxLength={64}
            placeholder={t('account.wallet.filter_ref_placeholder')}
            aria-label={t('account.wallet.filter_ref')}
            value={refDraft}
            disabled={controlsDisabled}
            onChange={(event) => setRefDraft(event.target.value)}
            onBlur={commitRef}
            onKeyDown={(event) => {
              if (event.key === 'Enter') commitRef()
            }}
            className="h-10 w-52 rounded-lg border border-border-default bg-surface-card px-3 text-sm text-ink-1 placeholder:text-ink-3"
          />
        </label>

        <Button
          variant="secondary"
          disabled={controlsDisabled}
          onClick={() => {
            onSearchChange(clearWalletFilters(search))
            setMinDraft('')
            setMaxDraft('')
            setRefDraft('')
            setFromDraft('')
            setToDraft('')
          }}
        >
          {t('account.wallet.filter_reset')}
        </Button>
      </div>

      {draftRangeError !== null ? (
        <p role="alert" className="text-xs text-danger">
          {draftRangeError === 'too_long'
            ? t('account.wallet.range_error')
            : t('account.wallet.range_order')}
        </p>
      ) : null}
      {draftAmountError !== null ? (
        <p role="alert" className="text-xs text-danger">
          {draftAmountError === 'order'
            ? t('account.wallet.amount_error')
            : t('account.wallet.amount_invalid')}
        </p>
      ) : null}

      {rateLimitError !== null ? (
        <p role="alert" className="rounded-lg border border-warning-line bg-warning-bg px-3 py-2 text-sm text-warning">
          {t('account.wallet.rate_limited', { seconds: cooldown })}
        </p>
      ) : null}
      {offlineError ? (
        <p role="status" className="rounded-lg border border-neutral-line bg-neutral-bg px-3 py-2 text-sm text-ink-2">
          {t('account.wallet.offline')}
        </p>
      ) : null}

      <div className="flex items-center justify-between text-sm text-ink-2">
        <span>{t('account.wallet.total', { count: total })}</span>
        <Button
          variant="secondary"
          disabled={controlsDisabled || listQuery.isFetching || !online}
          onClick={() => void listQuery.refetch()}
        >
          {t('account.wallet.refresh')}
        </Button>
      </div>

      <div className="rounded-xl border border-border-hairline bg-surface-card">
        {listQuery.isPending ? (
          <div className="flex flex-col gap-2 p-4" aria-busy="true">
            {Array.from({ length: 5 }, (_, index) => (
              <div key={index} className="h-8 animate-pulse rounded bg-surface-subtle motion-reduce:animate-none" />
            ))}
          </div>
        ) : listQuery.isError && rateLimitError === null && !offlineError ? (
          <div className="flex items-center gap-3 p-5 text-sm text-ink-2">
            <span role="alert">{t('account.wallet.error')}</span>
            <Button variant="secondary" onClick={() => void listQuery.refetch()}>
              {t('account.wallet.retry')}
            </Button>
          </div>
        ) : items.length === 0 && rateLimitError === null && !offlineError ? (
          <div className="flex flex-col items-center gap-2 p-8 text-sm text-ink-2">
            <p>{activeFilters ? t('account.wallet.empty_filtered') : t('account.wallet.empty')}</p>
            {activeFilters ? (
              <Button
                variant="secondary"
                onClick={() => {
                  onSearchChange(clearWalletFilters(search))
                  setMinDraft('')
                  setMaxDraft('')
                  setRefDraft('')
                  setFromDraft('')
                  setToDraft('')
                }}
              >
                {t('account.wallet.clear_filters')}
              </Button>
            ) : (
              <button
                type="button"
                className="text-[13px] font-medium text-brand hover:text-brand-hover"
                onClick={() => {
                  onSearchChange({ ...search, tab: undefined, p: 1 })
                }}
              >
                {t('account.wallet.go_topup')}
              </button>
            )}
          </div>
        ) : rateLimitError !== null ? null : (
          <table className="w-full text-sm">
            <thead>
              <tr className="border-b border-border-hairline text-left text-xs text-ink-2">
                <th scope="col" className="px-4 py-2 font-medium">{t('account.wallet.column_time')}</th>
                <th scope="col" className="px-4 py-2 font-medium">{t('account.wallet.column_type')}</th>
                <th scope="col" className="px-4 py-2 font-medium">{t('account.wallet.column_dir')}</th>
                <th scope="col" className="px-4 py-2 text-right font-medium">{t('account.wallet.column_amount')}</th>
                <th scope="col" className="px-4 py-2 text-right font-medium">{t('account.wallet.column_balance')}</th>
                <th scope="col" className="px-4 py-2 font-medium">{t('account.wallet.column_status')}</th>
                <th scope="col" className="px-4 py-2 font-medium">{t('account.wallet.column_actions')}</th>
              </tr>
            </thead>
            <tbody>
              {items.map((transaction) => (
                <tr
                  key={transaction.id}
                  tabIndex={0}
                  onClick={() => {
                    setSelectedId(transaction.id)
                  }}
                  onKeyDown={(event) => {
                    if (event.key === 'Enter' || event.key === ' ') {
                      event.preventDefault()
                      setSelectedId(transaction.id)
                    }
                  }}
                  className="h-12 cursor-pointer border-b border-border-hairline last:border-0 hover:bg-surface-hover focus-visible:bg-surface-hover focus-visible:outline-none"
                >
                  <td className="px-4 py-2">
                    <time
                      dateTime={transaction.created_at}
                      title={new Date(transaction.created_at).toISOString()}
                      className="text-ink-1"
                    >
                      {formatRelativeTime(transaction.created_at, locale)}
                    </time>
                  </td>
                  <td className="px-4 py-2">
                    <Tag variant={WALLET_TYPE_TAG_VARIANTS[transaction.type]}>
                      {t(typeLabelKey(transaction.type))}
                    </Tag>
                  </td>
                  <td className="px-4 py-2 text-ink-2">
                    {transaction.amount < 0
                      ? t('account.wallet.dir_out')
                      : t('account.wallet.dir_in')}
                  </td>
                  <td
                    className={`px-4 py-2 text-right tabular-nums ${
                      transaction.amount < 0 ? 'text-amount-out' : 'text-amount-in'
                    }`}
                  >
                    {amountText(transaction)}
                  </td>
                  <td className="px-4 py-2 text-right tabular-nums text-ink-2">
                    {formatCredits(transaction.balance_after, locale)}
                  </td>
                  <td className="px-4 py-2">
                    <StatusBadge
                      variant={WALLET_STATUS_BADGE_VARIANTS[transaction.status]}
                      className={transaction.status === 'hold' ? HOLD_PULSE_CLASS : undefined}
                    >
                      {t(statusLabelKey(transaction.status))}
                    </StatusBadge>
                  </td>
                  <td className="px-4 py-2">
                    <Button
                      variant="ghost"
                      onClick={(event) => {
                        event.stopPropagation()
                        setSelectedId(transaction.id)
                      }}
                    >
                      {t('account.wallet.action_details')}
                    </Button>
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        )}
      </div>

      <div className="flex items-center justify-end gap-3 text-sm text-ink-2">
        <span>{t('account.wallet.page', { page: currentPage, pages: totalPages })}</span>
        <Button
          variant="secondary"
          aria-label={t('account.wallet.prev')}
          disabled={controlsDisabled || currentPage <= 1}
          onClick={() => {
            onSearchChange({ ...search, p: Math.max(1, currentPage - 1) })
          }}
        >
          ‹
        </Button>
        <Button
          variant="secondary"
          aria-label={t('account.wallet.next')}
          disabled={controlsDisabled || currentPage >= totalPages}
          onClick={() => {
            onSearchChange({ ...search, p: Math.min(totalPages, currentPage + 1) })
          }}
        >
          ›
        </Button>
        <label className="flex items-center gap-2">
          {t('account.wallet.page_size')}
          <select
            aria-label={t('account.wallet.page_size')}
            value={search.page_size}
            disabled={controlsDisabled}
            onChange={(event) => {
              onSearchChange({
                ...search,
                p: 1,
                page_size: Number(event.target.value),
              })
            }}
            className="h-9 rounded-lg border border-border-default bg-surface-card px-2 text-sm"
          >
            {WALLET_PAGE_SIZES.map((size) => (
              <option key={size} value={size}>
                {size}
              </option>
            ))}
          </select>
        </label>
      </div>

      <TransactionDetailSheet
        transactionId={selectedId}
        onOpenChange={(open) => {
          if (!open) setSelectedId(null)
        }}
        onOpenCorrects={setSelectedId}
      />
    </section>
  )
}
