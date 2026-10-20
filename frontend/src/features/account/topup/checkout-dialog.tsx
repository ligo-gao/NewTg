import { useCallback, useEffect, useRef, useState } from 'react'
import { CardElement, Elements, useElements, useStripe } from '@stripe/react-stripe-js'
import { loadStripe, type Stripe, type StripeCardElement } from '@stripe/stripe-js'
import { QRCodeSVG } from 'qrcode.react'

import { apiClient } from '@/api/client'
import { requireData } from '@/api/result'
import { useI18n } from '@/shared/lib/i18n-context'
import { formatCredits } from '@/shared/lib/format'
import { useCountdown } from '@/shared/lib/use-countdown'
import { Dialog } from '@/shared/ui/base/dialog'
import { Button } from '@/shared/ui/button'
import { useToastStore } from '@/shared/ui/toast'

import { formatCountdown, formatYuan } from './money'
import type { OrderView, TopupOrderView } from './types'

const DEFAULT_POLL_INTERVAL_MS = 2000
const DEFAULT_POLL_MAX_MS = 10_000
const DEFAULT_POLL_DURATION_MS = 5 * 60_000
const ORDER_LIFETIME_MS = 30 * 60_000

export interface CheckoutDialogProps {
  open: boolean
  /** Order as returned by `POST /api/orders` (top-up projection). */
  order: TopupOrderView
  /**
   * `subscription` reuses the same Elements/QR/polling flow but the purchase
   * grants a plan rather than wallet credits: the success view hides the
   * credits/balance rows (the order credits 0) and offers "Done" instead of
   * "Top up again".
   */
  variant?: 'topup' | 'subscription'
  /** Stripe PaymentIntent client secret (stripe only). */
  clientSecret?: string
  /** Alipay precreate QR payload (alipay only). */
  qrCode?: string
  /**
   * Full order row fetched for a payload-less idempotent replay. Its status is
   * authoritative: the replayed order may already be terminal.
   */
  replayOrder?: OrderView
  /** Stripe publishable key from `GET /api/topup/config`. */
  publishableKey?: string
  /** Server-known balance; the success view omits the row while unknown. */
  balance?: number | null
  onOpenChange: (open: boolean) => void
  onSucceeded: (order: SucceededOrder) => void
  /** Alipay only: mint a fresh order after the QR expired. */
  onRefresh?: () => void
  /** Terminal/failed orders: return to amount selection and start over. */
  onRetry?: () => void
  /** Test seams for the 2s → 10s → 5min polling contract. */
  pollIntervalMs?: number
  pollMaxMs?: number
  pollDurationMs?: number
}

export interface SucceededOrder {
  amount: number
  credited_credits: number
}

type View = 'checkout' | 'waiting' | 'success' | 'terminal'
type TerminalStatus = 'failed' | 'expired' | 'cancelled'

function isTerminal(status: string): status is TerminalStatus {
  return status === 'failed' || status === 'expired' || status === 'cancelled'
}

interface PollOptions {
  orderId: number
  active: boolean
  initialDelayMs: number
  maxDelayMs: number
  durationMs: number
  onFinal: (order: OrderView) => void
  onTimeout: () => void
}

/**
 * Polls `GET /api/orders/{id}` after payment: first check at 2s, doubling to a
 * 10s cap, stopping at the first terminal state or after 5 minutes (裁决 04-7).
 * A hidden tab pauses the request but keeps the schedule, so returning to the
 * page resumes without a burst of stale calls.
 */
function useOrderPolling(options: PollOptions): void {
  const { orderId, active, initialDelayMs, maxDelayMs, durationMs } = options
  const onFinalRef = useRef(options.onFinal)
  const onTimeoutRef = useRef(options.onTimeout)

  useEffect(() => {
    onFinalRef.current = options.onFinal
    onTimeoutRef.current = options.onTimeout
  }, [options.onFinal, options.onTimeout])

  useEffect(() => {
    if (!active) return
    let cancelled = false
    let timer: ReturnType<typeof setTimeout> | null = null
    let delay = initialDelayMs
    const startedAt = Date.now()

    const schedule = (): void => {
      timer = setTimeout(() => {
        void tick()
      }, delay)
    }

    const tick = async (): Promise<void> => {
      if (cancelled) return
      if (Date.now() - startedAt >= durationMs) {
        onTimeoutRef.current()
        return
      }
      if (typeof document !== 'undefined' && document.hidden) {
        // Paused, not failed: retry at the same cadence once visible again.
        schedule()
        return
      }

      let order: OrderView
      try {
        order = requireData(
          await apiClient.GET('/api/orders/{id}', {
            params: { path: { id: orderId } },
          }),
        )
      } catch {
        delay = Math.min(delay * 2, maxDelayMs)
        schedule()
        return
      }

      if (cancelled) return
      if (order.status !== 'pending') {
        onFinalRef.current(order)
        return
      }
      delay = Math.min(delay * 2, maxDelayMs)
      schedule()
    }

    schedule()

    return () => {
      cancelled = true
      if (timer !== null) clearTimeout(timer)
    }
  }, [orderId, active, initialDelayMs, maxDelayMs, durationMs])
}

/**
 * Checkout container (modules-04 §3/§4): Stripe Elements and the Alipay QR
 * share the same 480px dialog, polling and success view. The view switch is
 * driven only by server order state; the amount/credits shown on success come
 * from the polled order, never from a client-side accumulator.
 */
export function CheckoutDialog({
  open,
  order,
  variant = 'topup',
  clientSecret,
  qrCode,
  replayOrder,
  publishableKey,
  balance = null,
  onOpenChange,
  onSucceeded,
  onRefresh,
  onRetry,
  pollIntervalMs = DEFAULT_POLL_INTERVAL_MS,
  pollMaxMs = DEFAULT_POLL_MAX_MS,
  pollDurationMs = DEFAULT_POLL_DURATION_MS,
}: CheckoutDialogProps) {
  const { t } = useI18n()
  const push = useToastStore((state) => state.push)
  const [view, setView] = useState<View>('checkout')
  const [settled, setSettled] = useState<SucceededOrder | null>(null)
  const [terminalStatus, setTerminalStatus] = useState<TerminalStatus | null>(null)
  const [pollTimedOut, setPollTimedOut] = useState(false)
  const [stripe, setStripe] = useState<Stripe | null>(null)
  const [stripeError, setStripeError] = useState(false)
  const [stripeLoadAttempt, setStripeLoadAttempt] = useState(0)
  const [countdownReady, setCountdownReady] = useState(false)
  const countdown = useCountdown()
  // A single success may arrive both from the initial row and from a poll;
  // report it exactly once so the wallet summary is refreshed once.
  const reportedSucceeded = useRef(false)
  const onSucceededRef = useRef(onSucceeded)

  useEffect(() => {
    onSucceededRef.current = onSucceeded
  }, [onSucceeded])

  const isAlipay = order.provider === 'alipay'
  const isSubscription = variant === 'subscription'
  const hasPayload = isAlipay ? qrCode !== undefined : clientSecret !== undefined
  const createdMs = Date.parse(order.created_at)
  const expiresMs = Number.isNaN(createdMs)
    ? Date.now() + ORDER_LIFETIME_MS
    : createdMs + ORDER_LIFETIME_MS

  const handleFinal = useCallback((next: OrderView): void => {
    if (next.status === 'succeeded') {
      setSettled({
        amount: next.amount,
        credited_credits: next.credited_credits,
      })
      setView('success')
      if (!reportedSucceeded.current) {
        reportedSucceeded.current = true
        onSucceededRef.current({
          amount: next.amount,
          credited_credits: next.credited_credits,
        })
      }
      return
    }
    if (isTerminal(next.status)) {
      // Stop here: a terminal order can never change again, so polling must not
      // continue to the 5-minute ceiling with no UI feedback.
      setTerminalStatus(next.status)
      setView('terminal')
    }
  }, [])

  const handleTimeout = useCallback((): void => {
    setPollTimedOut(true)
  }, [])

  useOrderPolling({
    orderId: order.id,
    // Terminal views never poll; neither does a succeeded one. A payload-less
    // pending replay keeps polling so a late webhook still flips the view.
    active: open && (view === 'checkout' || view === 'waiting'),
    initialDelayMs: pollIntervalMs,
    maxDelayMs: pollMaxMs,
    durationMs: pollDurationMs,
    onFinal: handleFinal,
    onTimeout: handleTimeout,
  })

  // Reset per order; the dialog stays open through view changes. The status of
  // a replayed (payload-less) idempotent response is rendered immediately:
  // succeeded → success view, terminal → terminal view, pending → notice.
  useEffect(() => {
    if (!open) return
    reportedSucceeded.current = false
    setSettled(null)
    setTerminalStatus(null)
    setPollTimedOut(false)
    setStripeError(false)

    const status = replayOrder?.status ?? order.status
    if (status === 'succeeded') {
      const credits = replayOrder?.credited_credits ?? order.credited_credits
      const amount = replayOrder?.amount ?? order.amount
      setSettled({ amount, credited_credits: credits })
      setView('success')
      reportedSucceeded.current = true
      onSucceededRef.current({ amount, credited_credits: credits })
    } else if (isTerminal(status)) {
      setTerminalStatus(status)
      setView('terminal')
    } else {
      setView('checkout')
    }

    const remaining = Math.max(0, Math.round((expiresMs - Date.now()) / 1000))
    countdown.start(remaining)
    setCountdownReady(true)
    // `countdown.start` is stable; `expiresMs` derives from the order.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [open, order.id])

  // Stripe.js is only loaded for the card provider; the publishable key comes
  // from the public config (P-9/P-17) and never from a hardcoded value.
  useEffect(() => {
    if (!open || isAlipay) return
    if (publishableKey === undefined || publishableKey === '') {
      setStripeError(true)
      return
    }
    let cancelled = false
    loadStripe(publishableKey)
      .then((instance) => {
        if (!cancelled) setStripe(instance)
      })
      .catch(() => {
        if (!cancelled) setStripeError(true)
      })
    return () => {
      cancelled = true
    }
  }, [open, isAlipay, publishableKey, stripeLoadAttempt])

  const title = isSubscription
    ? t('account.subscription.checkout.title')
    : isAlipay
      ? t('account.checkout.alipay_title')
      : t('account.checkout.title')

  const footer = (() => {
    if (view === 'success') {
      // A subscription order is the whole intent: there is nothing to "top up
      // again", and the new instance is already listed on this page.
      if (isSubscription) {
        return (
          <Button
            onClick={() => {
              onOpenChange(false)
            }}
          >
            {t('account.subscription.action.done')}
          </Button>
        )
      }
      return (
        <>
          <a
            href="/orders"
            className="inline-flex h-9 items-center justify-center rounded-lg border border-border-default bg-surface-card px-3.5 text-sm font-medium text-ink-1 hover:bg-surface-hover"
          >
            {t('account.checkout.view_order')}
          </a>
          <Button
            onClick={() => {
              onOpenChange(false)
            }}
          >
            {t('account.checkout.top_up_again')}
          </Button>
        </>
      )
    }
    if (view === 'waiting') {
      return (
        <Button
          variant="secondary"
          onClick={() => {
            onOpenChange(false)
          }}
        >
          {t('account.checkout.view_order')}
        </Button>
      )
    }
    if (view === 'terminal') {
      return (
        <>
          <a
            href="/orders"
            className="inline-flex h-9 items-center justify-center rounded-lg border border-border-default bg-surface-card px-3.5 text-sm font-medium text-ink-1 hover:bg-surface-hover"
          >
            {t('account.checkout.view_order')}
          </a>
          <Button
            onClick={() => {
              // Retry = abandon this order and return to amount selection for a
              // fresh Idempotency-Key; the old order stays visible in history.
              if (onRetry !== undefined) {
                onRetry()
              } else if (onRefresh !== undefined) {
                onRefresh()
              } else {
                onOpenChange(false)
              }
            }}
          >
            {t('account.checkout.retry_payment')}
          </Button>
        </>
      )
    }
    return (
      <Button
        variant="ghost"
        onClick={() => {
          onOpenChange(false)
        }}
      >
        {t('account.checkout.cancel')}
      </Button>
    )
  })()

  return (
    <Dialog
      open={open}
      onOpenChange={onOpenChange}
      title={title}
      panelClassName="max-w-[480px]"
      footer={footer}
    >
      <OrderMetaRow
        orderId={order.id}
        onCopied={() => {
          push({ tone: 'success', message: t('account.checkout.copied') })
        }}
      />

      {view === 'success' && settled !== null ? (
        <div aria-live="polite" className="mt-4">
          <p className="flex items-center gap-2 text-[15px] font-semibold text-success">
            <span aria-hidden="true">✓</span>
            {isSubscription
              ? t('account.subscription.checkout.success')
              : t('account.checkout.success')}
          </p>
          <dl className="mt-3 flex flex-col gap-2">
            {isSubscription ? null : (
              <SummaryRow
                label={t('account.checkout.credits_received')}
                value={formatCredits(settled.credited_credits)}
                testId="checkout-credits"
              />
            )}
            <SummaryRow
              label={t('account.topup.you_pay')}
              value={formatYuan(settled.amount)}
            />
            {balance !== null && !isSubscription ? (
              <SummaryRow
                label={t('account.checkout.new_balance')}
                value={formatCredits(balance)}
              />
            ) : null}
          </dl>
        </div>
      ) : null}

      {view === 'waiting' ? (
        <div aria-live="polite" className="mt-4">
          <p className="text-[15px] font-semibold text-info">
            {t('account.checkout.processing')}
          </p>
          <p className="mt-1 text-sm text-ink-2">
            {t('account.checkout.processing_body')}
          </p>
          {pollTimedOut ? (
            <p className="mt-2 text-sm text-ink-2">{t('account.checkout.timeout')}</p>
          ) : null}
        </div>
      ) : null}

      {view === 'terminal' && terminalStatus !== null ? (
        <div aria-live="polite" className="mt-4">
          <p className="text-[15px] font-semibold text-danger">
            {t(`account.orders.status_${terminalStatus}`)}
          </p>
          <p className="mt-1 text-sm text-ink-2">
            {t(`account.checkout.${terminalStatus}_body`)}
          </p>
        </div>
      ) : null}

      {view === 'checkout' && !hasPayload ? (
        <div aria-live="polite" className="mt-4">
          <p className="text-[15px] font-semibold text-warning">
            {t('account.checkout.replay_title')}
          </p>
          <p className="mt-1 text-sm text-ink-2">
            {t('account.checkout.replay_pending')}
          </p>
        </div>
      ) : null}

      {view === 'checkout' && isAlipay && qrCode !== undefined ? (
        <div className="mt-4">
          <div className="relative mx-auto w-fit rounded-lg border border-border-default bg-surface-card p-4">
            <QRCodeSVG
              value={qrCode}
              size={256}
              role="img"
              aria-label={t('account.checkout.qr_alt', {
                id: order.id,
                amount: formatYuan(order.amount),
              })}
            />
            {countdownReady && countdown.seconds === 0 ? (
              <div className="absolute inset-0 flex flex-col items-center justify-center gap-2 rounded-lg bg-overlay">
                <p className="text-sm font-medium text-ink-1">
                  {t('account.checkout.qr_expired')}
                </p>
                {onRefresh !== undefined ? (
                  <Button variant="secondary" onClick={onRefresh}>
                    {t('account.checkout.refresh_qr')}
                  </Button>
                ) : null}
              </div>
            ) : null}
          </div>
          <p className="mt-3 text-center text-sm text-ink-2">
            {t('account.checkout.alipay_scan')}
          </p>
          <div className="mt-3 flex items-center justify-between text-sm">
            <span className="text-ink-2">{t('account.checkout.time_remaining')}</span>
            <span
              className={[
                'tabular-nums',
                countdown.seconds <= 300 ? 'font-medium text-warning' : 'text-ink-1',
              ].join(' ')}
            >
              {formatCountdown(countdown.seconds)}
            </span>
          </div>
        </div>
      ) : null}

      {view === 'checkout' && !isAlipay && clientSecret !== undefined ? (
        <div className="mt-4">
          <p className="text-xs text-ink-3">
            {t('account.checkout.powered_by')}
          </p>
          {stripeError ? (
            <div className="mt-3 rounded-lg border border-danger-line bg-danger-bg px-3 py-2 text-sm text-danger">
              {t('account.checkout.checkout_failed')}
              <Button
                variant="secondary"
                className="ml-3"
                onClick={() => {
                  setStripeError(false)
                  setStripeLoadAttempt((attempt) => attempt + 1)
                }}
              >
                {t('account.checkout.retry')}
              </Button>
            </div>
          ) : stripe === null ? (
            <div className="mt-3 flex flex-col gap-2" aria-hidden="true">
              <div className="h-11 rounded-lg bg-surface-subtle" />
              <div className="h-11 rounded-lg bg-surface-subtle" />
            </div>
          ) : clientSecret === undefined ? (
            <p className="mt-3 text-sm text-danger">
              {t('account.checkout.init_failed')}
            </p>
          ) : (
            <Elements stripe={stripe} options={{ clientSecret }}>
              <StripeCardForm
                clientSecret={clientSecret}
                amount={order.amount}
                onSubmitted={() => {
                  setView('waiting')
                }}
              />
            </Elements>
          )}
        </div>
      ) : null}
    </Dialog>
  )
}

function StripeCardForm({
  clientSecret,
  amount,
  onSubmitted,
}: {
  clientSecret: string
  amount: number
  onSubmitted: () => void
}) {
  const { t } = useI18n()
  const stripe = useStripe()
  const elements = useElements()
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<string | null>(null)

  async function pay(): Promise<void> {
    if (stripe === null || elements === null || busy) return
    const card = elements.getElement(CardElement) as StripeCardElement | null
    if (card === null) return
    setBusy(true)
    setError(null)
    try {
      const result = await stripe.confirmCardPayment(clientSecret, {
        payment_method: { card },
      })
      if (result.error !== undefined) {
        // Declines stay inside Elements; the order remains pending for a retry.
        setError(result.error.message ?? null)
        return
      }
      onSubmitted()
    } finally {
      setBusy(false)
    }
  }

  return (
    <div className="mt-3 flex flex-col gap-3">
      <div className="rounded-lg border border-border-default px-3 py-3">
        <CardElement options={{ hidePostalCode: true }} />
      </div>
      {error !== null ? (
        <p role="alert" className="text-sm text-danger">
          {t('account.checkout.card_error', { message: error })}
        </p>
      ) : null}
      <Button disabled={busy || stripe === null} onClick={() => void pay()}>
        {busy ? t('account.checkout.processing') : t('account.checkout.pay', {
          amount: formatYuan(amount),
        })}
      </Button>
    </div>
  )
}

function OrderMetaRow({
  orderId,
  onCopied,
}: {
  orderId: number
  onCopied: () => void
}) {
  const { t } = useI18n()
  return (
    <div className="flex items-center justify-between gap-3 text-sm">
      <span className="text-ink-2">{t('account.checkout.order_id')}</span>
      <span className="font-mono text-[13px] text-ink-1">#{orderId}</span>
      <Button
        variant="ghost"
        aria-label={t('account.checkout.copy')}
        onClick={() => {
          void navigator.clipboard?.writeText(String(orderId)).catch(() => undefined)
          onCopied()
        }}
      >
        {t('common.action.copy')}
      </Button>
    </div>
  )
}

function SummaryRow({
  label,
  value,
  testId,
}: {
  label: string
  value: string
  testId?: string
}) {
  return (
    <div className="flex items-end gap-2 text-sm">
      <dt className="text-ink-2">{label}</dt>
      <span
        aria-hidden="true"
        className="mb-1 flex-1 border-b border-dotted border-border-default"
      />
      <dd className="font-medium tabular-nums text-ink-1" data-testid={testId}>
        {value}
      </dd>
    </div>
  )
}
