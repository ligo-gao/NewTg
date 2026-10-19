import { useEffect, useState } from 'react'
import { useQuery, useQueryClient } from '@tanstack/react-query'

import { apiClient } from '@/api/client'
import { ApiError } from '@/api/errors'
import { requireData } from '@/api/result'
import { useI18n } from '@/shared/lib/i18n-context'
import { formatCredits } from '@/shared/lib/format'
import { formatRelativeTime } from '@/shared/lib/datetime'
import { formatCountdown, formatYuan } from './money'
import { useCountdown } from '@/shared/lib/use-countdown'
import { Sheet } from '@/shared/ui/base/sheet'
import { Button } from '@/shared/ui/button'
import { Dialog } from '@/shared/ui/base/dialog'
import { useToastStore } from '@/shared/ui/toast'

import { OrderStatusBadge } from './order-status-badge'
import type { OrderView } from './types'

export interface OrderDetailSheetProps {
  orderId: number | null
  onOpenChange: (open: boolean) => void
}

type VerifyResult = 'not_found' | 'failed' | null

/**
 * Order detail (F-04-09/F-04-12). Field pairs replace the checkout ledger: this
 * is a read-only record, not a payment decision surface. Only `pending` orders
 * expose self-verification and cancellation; every terminal state hides them.
 */
export function OrderDetailSheet({ orderId, onOpenChange }: OrderDetailSheetProps) {
  const { t, locale } = useI18n()
  const queryClient = useQueryClient()
  const push = useToastStore((state) => state.push)
  const [verifying, setVerifying] = useState(false)
  const [verifyResult, setVerifyResult] = useState<VerifyResult>(null)
  const [cancelOpen, setCancelOpen] = useState(false)
  const [cancelling, setCancelling] = useState(false)
  const countdown = useCountdown()
  const rateLimitCountdown = useCountdown()

  const detailQuery = useQuery({
    queryKey: ['orders', 'detail', orderId],
    enabled: orderId !== null,
    queryFn: async () =>
      requireData(
        await apiClient.GET('/api/orders/{id}', {
          params: { path: { id: orderId as number } },
        }),
      ),
  })

  const order: OrderView | undefined = detailQuery.data

  useEffect(() => {
    setVerifyResult(null)
    if (order?.status === 'pending') {
      const remaining = Math.max(
        0,
        Math.round((Date.parse(order.expires_at) - Date.now()) / 1000),
      )
      countdown.start(remaining)
    } else {
      countdown.stop()
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [order?.id, order?.status, order?.expires_at])

  async function verify(): Promise<void> {
    if (order === undefined || verifying) return
    setVerifying(true)
    setVerifyResult(null)
    try {
      const result = requireData(
        await apiClient.POST('/api/orders/{id}/verify', {
          params: { path: { id: order.id } },
        }),
      )
      if (result.paid) {
        push({
          tone: 'success',
          message: t('account.orders.verify_success', {
            credits: formatCredits(result.order.credited_credits),
          }),
        })
        await queryClient.invalidateQueries({ queryKey: ['orders'] })
        await detailQuery.refetch()
      } else {
        setVerifyResult('not_found')
      }
    } catch (caught) {
      if (caught instanceof ApiError && caught.kind === 'rate_limited') {
        // Server-driven countdown (Retry-After), never a hardcoded duration.
        rateLimitCountdown.start(caught.retry_after ?? 60)
      } else {
        setVerifyResult('failed')
      }
    } finally {
      setVerifying(false)
    }
  }

  async function cancel(): Promise<void> {
    if (order === undefined || cancelling) return
    setCancelling(true)
    try {
      requireData(
        await apiClient.POST('/api/orders/{id}/cancel', {
          params: { path: { id: order.id } },
        }),
      )
      push({ tone: 'success', message: t('account.orders.cancelled') })
      setCancelOpen(false)
      await queryClient.invalidateQueries({ queryKey: ['orders'] })
      await detailQuery.refetch()
    } catch {
      push({ tone: 'error', message: t('account.orders.cancel_failed') })
    } finally {
      setCancelling(false)
    }
  }

  const rateLimitSeconds = rateLimitCountdown.seconds

  return (
    <>
      <Sheet
        open={orderId !== null}
        onOpenChange={onOpenChange}
        title={t('account.orders.detail_title')}
      >
        {detailQuery.isPending ? (
          <p className="text-sm text-ink-2">{t('account.orders.loading')}</p>
        ) : detailQuery.isError || order === undefined ? (
          <div className="flex items-center gap-3 text-sm text-ink-2">
            <span>{t('account.orders.error')}</span>
            <Button variant="secondary" onClick={() => void detailQuery.refetch()}>
              {t('account.orders.retry')}
            </Button>
          </div>
        ) : (
          <div className="flex flex-col gap-4">
            <div className="flex items-center gap-2">
              <OrderStatusBadge status={order.status} />
              <span className="text-sm text-ink-2">{t('account.orders.detail_type')}</span>
            </div>

            <dl className="flex flex-col">
              <FieldRow
                label={t('account.orders.field_id')}
                value={`#${order.id}`}
                mono
              />
              <FieldRow
                label={t('account.orders.field_provider')}
                value={t(
                  order.provider === 'stripe'
                    ? 'account.orders.provider_stripe'
                    : 'account.orders.provider_alipay',
                )}
              />
              <FieldRow
                label={t('account.orders.field_amount')}
                value={formatYuan(order.amount)}
              />
              <FieldRow
                label={t('account.orders.field_credits')}
                value={
                  order.status === 'succeeded'
                    ? formatCredits(order.credited_credits)
                    : '—'
                }
              />
              <FieldRow
                label={t('account.orders.field_created')}
                value={formatRelativeTime(order.created_at, locale)}
              />
              <FieldRow
                label={t('account.orders.field_paid_at')}
                value={
                  order.paid_at == null
                    ? t('account.orders.unpaid')
                    : formatRelativeTime(order.paid_at, locale)
                }
              />
              {order.status === 'pending' ? (
                <FieldRow
                  label={t('account.orders.field_expires')}
                  value={formatCountdown(countdown.seconds)}
                />
              ) : null}
            </dl>

            {order.status === 'pending' ? (
              <div className="flex flex-col gap-2 border-t border-border-hairline pt-4">
                {verifyResult === 'not_found' ? (
                  <p role="status" className="text-[13px] text-info">
                    {t('account.orders.verify_not_found')}
                  </p>
                ) : null}
                {verifyResult === 'failed' ? (
                  <p role="alert" className="text-[13px] text-danger">
                    {t('account.orders.verify_failed')}
                  </p>
                ) : null}
                {rateLimitSeconds > 0 ? (
                  <p role="alert" className="text-[13px] text-danger">
                    {t('account.orders.verify_rate_limited', {
                      seconds: rateLimitSeconds,
                    })}
                  </p>
                ) : null}
                <Button
                  variant="secondary"
                  disabled={verifying || rateLimitSeconds > 0}
                  onClick={() => void verify()}
                >
                  {verifying ? t('account.orders.verifying') : t('account.orders.verify')}
                </Button>
                <Button
                  variant="ghost"
                  disabled={verifying}
                  onClick={() => {
                    setCancelOpen(true)
                  }}
                >
                  {t('account.orders.cancel')}
                </Button>
              </div>
            ) : null}
          </div>
        )}
      </Sheet>

      <Dialog
        open={cancelOpen}
        onOpenChange={setCancelOpen}
        title={t('account.orders.cancel_confirm_title')}
        description={t('account.orders.cancel_confirm_body')}
        dismissible={!cancelling}
        footer={
          <>
            <Button
              variant="ghost"
              disabled={cancelling}
              onClick={() => {
                setCancelOpen(false)
              }}
            >
              {t('account.orders.cancel_keep')}
            </Button>
            <Button
              variant="danger"
              disabled={cancelling}
              onClick={() => void cancel()}
            >
              {t('account.orders.cancel_confirm')}
            </Button>
          </>
        }
      />
    </>
  )
}

function FieldRow({
  label,
  value,
  mono = false,
}: {
  label: string
  value: string
  mono?: boolean
}) {
  return (
    <div className="flex h-8 items-center justify-between gap-4 text-sm">
      <dt className="text-ink-2">{label}</dt>
      <dd
        className={[
          'text-right text-ink-1',
          mono ? 'font-mono text-[13px]' : 'tabular-nums',
        ].join(' ')}
      >
        {value}
      </dd>
    </div>
  )
}
