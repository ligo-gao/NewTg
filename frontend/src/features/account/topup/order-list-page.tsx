import { useEffect, useState } from 'react'
import { useQuery } from '@tanstack/react-query'

import { apiClient } from '@/api/client'
import { requireData } from '@/api/result'
import { useI18n } from '@/shared/lib/i18n-context'
import { formatRelativeTime } from '@/shared/lib/datetime'
import { formatCredits } from '@/shared/lib/format'
import { Button } from '@/shared/ui/button'
import { useToastStore } from '@/shared/ui/toast'

import { formatYuan } from './money'
import { OrderDetailSheet } from './order-detail-sheet'
import { ORDER_STATUSES, OrderStatusBadge } from './order-status-badge'
import type { OrderList, OrderStatus } from './types'

const PAGE_SIZES = [20, 50] as const
type StatusFilter = 'all' | OrderStatus

/**
 * Order history (F-04-08/09, modules-04 §5). Offset pagination and the status
 * filter live in local state; the order number search is debounced by 300ms so
 * typing does not fire one cross-user query per keystroke.
 */
export function OrderListPage() {
  const { t, locale } = useI18n()
  const push = useToastStore((state) => state.push)

  const [searchInput, setSearchInput] = useState('')
  const [q, setQ] = useState('')
  const [status, setStatus] = useState<StatusFilter>('all')
  const [page, setPage] = useState(1)
  const [pageSize, setPageSize] = useState<number>(20)
  const [selectedId, setSelectedId] = useState<number | null>(null)

  useEffect(() => {
    const next = searchInput.trim()
    if (next === q) return
    const timer = setTimeout(() => {
      setQ(next)
      setPage(1)
    }, 300)
    return () => {
      clearTimeout(timer)
    }
  }, [searchInput, q])

  const listQuery = useQuery({
    queryKey: ['orders', 'list', { q, status, page, pageSize }],
    queryFn: async () => {
      const params: {
        p: number
        page_size: number
        q?: string
        status?: OrderStatus
      } = { p: page, page_size: pageSize }
      if (q !== '') params.q = q
      if (status !== 'all') params.status = status
      return requireData(await apiClient.GET('/api/orders', { params: { query: params } }))
    },
  })

  const data: OrderList | undefined = listQuery.data
  const items = data?.items ?? []
  const total = data?.total ?? 0
  const totalPages = Math.max(1, Math.ceil(total / pageSize))

  function copyOrderId(id: number): void {
    void navigator.clipboard?.writeText(String(id)).catch(() => undefined)
    push({ tone: 'success', message: t('account.checkout.copied') })
  }

  return (
    <section className="mx-auto flex max-w-[1520px] flex-col gap-4">
      <h1 className="text-page-title text-ink-1">{t('account.orders.title')}</h1>

      <div className="flex flex-wrap items-center gap-3">
        <input
          type="search"
          role="searchbox"
          aria-label={t('account.orders.search_label')}
          placeholder={t('account.orders.search_placeholder')}
          value={searchInput}
          onChange={(event) => {
            setSearchInput(event.target.value)
          }}
          className="h-10 w-[300px] rounded-lg border border-border-default bg-surface-card px-3 text-body text-ink-1 placeholder:text-ink-3 focus-visible:shadow-[var(--focus-ring)] focus-visible:outline-none"
        />
        <label className="flex items-center gap-2 text-body text-ink-2">
          {t('account.orders.status_label')}
          <select
            value={status}
            onChange={(event) => {
              setStatus(event.target.value as StatusFilter)
              setPage(1)
            }}
            className="h-10 rounded-lg border border-border-default bg-surface-card px-2 text-body text-ink-1"
          >
            <option value="all">{t('account.orders.status_all')}</option>
            {ORDER_STATUSES.map((value) => (
              <option key={value} value={value}>
                {t(`account.orders.status_${value}`)}
              </option>
            ))}
          </select>
        </label>
      </div>

      <div className="rounded-xl border border-border-hairline bg-surface-card">
        {listQuery.isPending ? (
          <p className="p-5 text-body text-ink-2">{t('account.orders.loading')}</p>
        ) : listQuery.isError ? (
          <div className="flex items-center gap-3 p-5 text-body text-ink-2">
            <span>{t('account.orders.error')}</span>
            <Button variant="secondary" onClick={() => void listQuery.refetch()}>
              {t('account.orders.retry')}
            </Button>
          </div>
        ) : items.length === 0 ? (
          <div className="flex flex-col items-center gap-2 p-8 text-body text-ink-2">
            <p>{q !== '' ? t('account.orders.empty_search') : t('account.orders.empty')}</p>
            {q !== '' ? (
              <Button
                variant="secondary"
                onClick={() => {
                  setSearchInput('')
                  setQ('')
                  setPage(1)
                }}
              >
                {t('account.orders.clear_search')}
              </Button>
            ) : (
              <a
                href="/wallet"
                className="text-meta font-medium text-brand hover:text-brand-hover"
              >
                {t('account.topup.title')}
              </a>
            )}
          </div>
        ) : (
          <table className="w-full text-body">
            <thead>
              <tr className="border-b border-border-hairline text-left text-meta text-ink-2">
                <th className="px-4 py-2 font-medium">{t('account.orders.column_id')}</th>
                <th className="px-4 py-2 font-medium">
                  {t('account.orders.column_provider')}
                </th>
                <th className="px-4 py-2 text-right font-medium">
                  {t('account.orders.column_amount')}
                </th>
                <th className="px-4 py-2 text-right font-medium">
                  {t('account.orders.column_credits')}
                </th>
                <th className="px-4 py-2 font-medium">
                  {t('account.orders.column_status')}
                </th>
              </tr>
            </thead>
            <tbody>
              {items.map((order) => (
                <tr
                  key={order.id}
                  tabIndex={0}
                  onClick={() => {
                    setSelectedId(order.id)
                  }}
                  onKeyDown={(event) => {
                    if (event.key === 'Enter' || event.key === ' ') {
                      event.preventDefault()
                      setSelectedId(order.id)
                    }
                  }}
                  className="h-14 cursor-pointer border-b border-border-hairline last:border-0 hover:bg-surface-hover focus-visible:bg-surface-hover focus-visible:outline-none"
                >
                  <td className="px-4 py-2">
                    <div className="flex items-center gap-2">
                      <span className="font-mono text-meta">#{order.id}</span>
                      <Button
                        variant="ghost"
                        aria-label={t('account.checkout.copy')}
                        onClick={(event) => {
                          event.stopPropagation()
                          copyOrderId(order.id)
                        }}
                      >
                        {t('common.action.copy')}
                      </Button>
                    </div>
                    <div
                      className="text-meta text-ink-3"
                      title={new Date(order.created_at).toISOString()}
                    >
                      {formatRelativeTime(order.created_at, locale)}
                    </div>
                  </td>
                  <td className="px-4 py-2">
                    {t(
                      order.provider === 'stripe'
                        ? 'account.orders.provider_stripe'
                        : 'account.orders.provider_alipay',
                    )}
                  </td>
                  <td className="px-4 py-2 text-right tabular-nums">
                    {formatYuan(order.amount)}
                  </td>
                  <td className="px-4 py-2 text-right tabular-nums">
                    {order.status === 'succeeded'
                      ? formatCredits(order.credited_credits)
                      : '—'}
                  </td>
                  <td className="px-4 py-2">
                    <OrderStatusBadge status={order.status} />
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        )}
      </div>

      <div className="flex items-center justify-end gap-3 text-body text-ink-2">
        <span>
          {t('account.orders.page', { page, pages: totalPages })}
        </span>
        <Button
          variant="secondary"
          aria-label={t('account.orders.prev')}
          disabled={page <= 1}
          onClick={() => {
            setPage((current) => Math.max(1, current - 1))
          }}
        >
          ‹
        </Button>
        <Button
          variant="secondary"
          aria-label={t('account.orders.next')}
          disabled={page >= totalPages}
          onClick={() => {
            setPage((current) => Math.min(totalPages, current + 1))
          }}
        >
          ›
        </Button>
        <label className="flex items-center gap-2">
          {t('account.orders.page_size')}
          <select
            value={pageSize}
            onChange={(event) => {
              setPageSize(Number(event.target.value))
              setPage(1)
            }}
            className="h-9 rounded-lg border border-border-default bg-surface-card px-2 text-body"
          >
            {PAGE_SIZES.map((size) => (
              <option key={size} value={size}>
                {size}
              </option>
            ))}
          </select>
        </label>
      </div>

      <OrderDetailSheet
        orderId={selectedId}
        onOpenChange={(open) => {
          if (!open) setSelectedId(null)
        }}
      />
    </section>
  )
}
