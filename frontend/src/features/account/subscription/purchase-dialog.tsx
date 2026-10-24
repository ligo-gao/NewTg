import { useEffect, useMemo, useRef, useState } from 'react'
import { useQuery } from '@tanstack/react-query'

import { apiClient } from '@/api/client'
import { ApiError } from '@/api/errors'
import { requireData } from '@/api/result'
import { useI18n } from '@/shared/lib/i18n-context'
import { formatCredits } from '@/shared/lib/format'
import { Dialog } from '@/shared/ui/base/dialog'
import { Button } from '@/shared/ui/button'
import { useToastStore } from '@/shared/ui/toast'

import { CheckoutDialog } from '../topup/checkout-dialog'
import { formatYuan } from '../topup/money'
import type { CreateOrderResponse } from '../topup/types'
import { isUiSupportedProvider } from '../topup/types'
import type { OrderView } from '../topup/types'
import {
  EMPTY_PROVIDERS,
  TOPUP_CONFIG_KEY,
  WALLET_SUMMARY_KEY,
  newIdempotencyKey,
} from './constants'
import type { AvailableSubscriptionPlan, SubscriptionProvider } from './types'

function providerLabelKey(provider: SubscriptionProvider): string {
  if (provider === 'wallet') return 'account.subscription.purchase.wallet'
  if (provider === 'alipay') return 'account.orders.provider_alipay'
  return 'account.orders.provider_stripe'
}

export interface SubscriptionPurchaseDialogProps {
  /** `null` closes the purchase intent. */
  plan: AvailableSubscriptionPlan | null
  /**
   * Browser offline signal. A write started while offline would only surface
   * as an `offline` error after the round trip, so confirmation is disabled
   * up front (same rule as the plan cards and cancel switches).
   */
  offline: boolean
  onOpenChange: (open: boolean) => void
  /** Called whenever an instance was created (free, replay-paid or webhook). */
  onPurchased: () => void
}

interface CheckoutState {
  response: CreateOrderResponse
  /** Replayed orders carry no payment payload; status comes from this row. */
  replayOrder?: OrderView
}

interface InlineError {
  key: string
  values?: Record<string, string | number>
}

/**
 * 订阅收银台（billing-08 §16.3 F-H-03 的 `subscription-purchase-dialog`）。
 *
 * Flow: plan → provider → `POST /api/subscription/purchase` → either
 *   - Stripe `client_secret` / Alipay `qr_code` → the existing CheckoutDialog
 *     (same Elements, QR, polling and 2s→10s→5min schedule as top-up), or
 *   - a 0-price plan, which the server activates inside the same transaction;
 *     the response has no payment payload and is shown as "activated".
 *
 * Idempotency: one key per purchase intent, minted when the dialog opens and
 * kept across retries (a failed network call must replay the first order).
 * Closing discards it so the next open is a fresh intent.
 *
 * Providers come from `GET /api/topup/config.enabled_providers` — the server
 * rejects a provider it has not configured with 503, so offering a disabled
 * one would only produce a dead end. A free plan still needs a provider in the
 * request body (the contract requires the field); the first enabled provider
 * is sent, or `stripe` when none is configured, because the server never calls
 * the channel for a zero amount.
 */
export function SubscriptionPurchaseDialog({
  plan,
  offline,
  onOpenChange,
  onPurchased,
}: SubscriptionPurchaseDialogProps) {
  const { t } = useI18n()
  const push = useToastStore((state) => state.push)
  const planId = plan?.id ?? null
  const keyRef = useRef<string | null>(null)
  const [provider, setProvider] = useState<SubscriptionProvider | null>(null)
  const [submitting, setSubmitting] = useState(false)
  const [error, setError] = useState<InlineError | null>(null)
  const [activated, setActivated] = useState(false)
  const [checkout, setCheckout] = useState<CheckoutState | null>(null)

  const configQuery = useQuery({
    queryKey: TOPUP_CONFIG_KEY,
    queryFn: async () =>
      requireData(await apiClient.GET('/api/topup/config')),
    enabled: plan !== null,
  })

  // Wallet funding (P2b). The option is offered only when the wallet summary
  // is readable: a deployment without the wallet surface (or a transient read
  // failure) hides it instead of sending the user into a 404/503 dead end.
  const walletQuery = useQuery({
    queryKey: WALLET_SUMMARY_KEY,
    queryFn: async () =>
      requireData(await apiClient.GET('/api/wallet/summary')),
    enabled: plan !== null,
  })

  // epay is advertised by the server but its redirect-form checkout is not
  // wired into the console yet (G-DOM-72 ④); hide it rather than open a dead end.
  // Memoized because the list seeds a useEffect; a fresh array each render
  // would re-run that effect on every keystroke-like update.
  const providers: readonly SubscriptionProvider[] = useMemo(() => {
    const channels = (
      configQuery.data?.enabled_providers ?? EMPTY_PROVIDERS
    ).filter(isUiSupportedProvider)
    // Wallet is a purchase-only funding source (P2b); append it after the
    // channels so the default selection stays a real payment provider. Hide it
    // entirely when the wallet summary cannot be read.
    return walletQuery.isSuccess ? [...channels, 'wallet'] : channels
  }, [configQuery.data, walletQuery.isSuccess])
  const walletBalance = walletQuery.data?.balance
  const isFree = plan !== null && plan.price_amount === 0

  // Fresh intent per open: new key, cleared state. Keyed on the plan id (not
  // the object) because a plans refetch produces a new object for the same
  // plan. Closing (planId → null) clears as well, so reopening the *same* plan
  // starts a new intent instead of resuming the previous attempt's state/key.
  useEffect(() => {
    keyRef.current = planId === null ? null : newIdempotencyKey()
    setProvider(null)
    setSubmitting(false)
    setError(null)
    setActivated(false)
    setCheckout(null)
  }, [planId])

  // Seed the selected provider once the public config arrives.
  useEffect(() => {
    if (provider !== null || providers.length === 0) return
    setProvider(providers[0] ?? null)
  }, [provider, providers])

  if (plan === null) return null

  function errorFor(caught: unknown): InlineError {
    if (caught instanceof ApiError) {
      if (caught.kind === 'rate_limited') {
        return {
          key: 'account.subscription.purchase.rate_limited',
          // Retry-After is the server's own window; a missing header falls back
          // to the frozen 5/minute documentation default rather than "0s".
          values: { seconds: caught.retry_after ?? 60 },
        }
      }
      if (caught.kind === 'conflict') {
        return { key: 'account.subscription.purchase.conflict' }
      }
      if (caught.kind === 'validation') {
        return { key: 'account.subscription.purchase.invalid' }
      }
      if (caught.kind === 'offline') {
        return { key: 'account.subscription.purchase.offline' }
      }
      if (caught.kind === 'quota') {
        // 402 is the wallet-funding branch (P2b): the transaction rolled back,
        // leaving no order/instance/ledger row behind.
        return { key: 'account.subscription.purchase.insufficient_wallet' }
      }
      // 502 (provider outbound) and 503 (provider/limiter unavailable) share
      // the "try again later" copy; both are transient by contract.
      if (caught.status === 502 || caught.status === 503) {
        return { key: 'account.subscription.purchase.unavailable' }
      }
    }
    return { key: 'account.subscription.purchase.failed' }
  }

  function announceActivated(): void {
    setActivated(true)
    onPurchased()
    push({
      tone: 'success',
      message:
        plan !== null && plan.grant_credits === 0
          ? t('account.subscription.toast.activated_unlimited')
          : t('account.subscription.toast.activated'),
    })
  }

  async function submit(): Promise<void> {
    if (submitting || offline || plan === null) return
    const effectiveProvider: SubscriptionProvider | null = isFree
      ? (providers[0] ?? 'stripe')
      : provider
    if (effectiveProvider === null) return

    const key = keyRef.current ?? (keyRef.current = newIdempotencyKey())
    setSubmitting(true)
    setError(null)
    try {
      const response = requireData(
        await apiClient.POST('/api/subscription/purchase', {
          params: { header: { 'Idempotency-Key': key } },
          body: { plan_id: plan.id, provider: effectiveProvider },
        }),
      )
      const hasPayload =
        response.client_secret !== undefined || response.qr_code !== undefined
      if (hasPayload) {
        setCheckout({ response })
        return
      }
      // No payment payload: either the 0-price instant activation or an
      // idempotent replay. The order status is the only discriminator.
      if (response.order.status === 'succeeded') {
        announceActivated()
        return
      }
      // Pending/terminal replay: read the authoritative row so the checkout can
      // render a real state instead of dead-ending on a missing QR/Elements
      // payload (same recovery as PaymentConfirmDialog).
      const replayOrder = requireData(
        await apiClient.GET('/api/orders/{id}', {
          params: { path: { id: response.order.id } },
        }),
      )
      if (replayOrder.status === 'succeeded') {
        announceActivated()
        return
      }
      setCheckout({ response, replayOrder })
    } catch (caught) {
      setError(errorFor(caught))
      // Keep the key: an automatic or manual retry of the same intent must
      // replay the first order rather than create a second one.
    } finally {
      setSubmitting(false)
    }
  }

  const providerSection = isFree ? (
    <p className="mt-4 rounded-lg border border-info-line bg-info-bg px-3 py-2 text-body text-info">
      {t('account.subscription.purchase.free_hint')}
    </p>
  ) : (
    <fieldset className="mt-4">
      <legend className="mb-2 text-meta font-medium text-ink-1">
        {t('account.subscription.purchase.payment_method')}
      </legend>
      {providers.length === 0 ? (
        <p className="text-body text-ink-2">
          {configQuery.isPending || walletQuery.isPending
            ? t('account.subscription.purchase.loading_providers')
            : t('account.subscription.purchase.no_providers')}
        </p>
      ) : (
        <div className="flex flex-col gap-2">
          {providers.map((option) => (
            <label
              key={option}
              className="flex cursor-pointer items-center justify-between gap-2 rounded-lg border border-border-default bg-surface-page px-3 py-2 text-body text-ink-1"
            >
              <span className="flex items-center gap-2">
                <input
                  type="radio"
                  name="subscription-provider"
                  value={option}
                  checked={provider === option}
                  disabled={submitting}
                  onChange={() => {
                    setProvider(option)
                  }}
                  className="accent-brand"
                />
                {t(providerLabelKey(option))}
              </span>
              {option === 'wallet' && walletBalance !== undefined ? (
                <span className="text-meta tabular-nums text-ink-2">
                  {t('account.subscription.purchase.wallet_balance', {
                    balance: formatCredits(walletBalance),
                  })}
                </span>
              ) : null}
            </label>
          ))}
        </div>
      )}
    </fieldset>
  )

  const footer = activated ? (
    <Button
      onClick={() => {
        onOpenChange(false)
      }}
    >
      {t('account.subscription.action.done')}
    </Button>
  ) : (
    <>
      <Button
        variant="ghost"
        disabled={submitting}
        onClick={() => {
          onOpenChange(false)
        }}
      >
        {t('account.subscription.action.cancel')}
      </Button>
      <Button
        disabled={
          offline ||
          submitting ||
          (!isFree && (provider === null || providers.length === 0))
        }
        onClick={() => void submit()}
      >
        {submitting
          ? t('account.subscription.action.creating')
          : t('account.subscription.action.confirm')}
      </Button>
    </>
  )

  return (
    <>
      <Dialog
        open={checkout === null}
        onOpenChange={(next) => {
          if (!submitting) onOpenChange(next)
        }}
        title={t('account.subscription.action.confirm_title')}
        dismissible={!submitting}
        panelClassName="max-w-[480px]"
        footer={footer}
      >
        <dl className="flex flex-col gap-2 text-body">
          <div className="flex items-center justify-between gap-4">
            <dt className="text-ink-2">{t('account.subscription.plan.label')}</dt>
            <dd className="font-medium text-ink-1">{plan.title}</dd>
          </div>
          <div className="flex items-center justify-between gap-4">
            <dt className="text-ink-2">{t('account.subscription.plan.period_label')}</dt>
            <dd className="font-medium text-ink-1">
              {t('account.subscription.mine.period', { days: plan.period_days })}
            </dd>
          </div>
          <div className="flex items-center justify-between gap-4">
            <dt className="text-ink-2">{t('account.subscription.plan.grant_credits')}</dt>
            <dd className="font-medium tabular-nums text-ink-1">
              {plan.grant_credits === 0
                ? t('account.subscription.plan.unlimited')
                : formatCredits(plan.grant_credits)}
            </dd>
          </div>
          <div className="flex items-center justify-between gap-4">
            <dt className="text-ink-2">{t('account.subscription.action.amount_due')}</dt>
            <dd className="font-semibold tabular-nums text-ink-1">
              {isFree
                ? t('account.subscription.plan.free')
                : formatYuan(plan.price_amount)}
            </dd>
          </div>
        </dl>

        {activated ? (
          <div aria-live="polite" className="mt-4">
            <p className="text-card-title text-success">
              {t('account.subscription.purchase.activated_title')}
            </p>
            <p className="mt-1 text-body text-ink-2">
              {t('account.subscription.purchase.activated_body')}
            </p>
          </div>
        ) : (
          providerSection
        )}

        {error !== null ? (
          <p role="alert" className="mt-3 text-body text-danger">
            {t(error.key, error.values)}
          </p>
        ) : null}
      </Dialog>

      {checkout !== null ? (
        <CheckoutDialog
          open
          variant="subscription"
          order={checkout.response.order}
          clientSecret={checkout.response.client_secret}
          qrCode={checkout.response.qr_code}
          replayOrder={checkout.replayOrder}
          publishableKey={configQuery.data?.providers.stripe?.publishable_key}
          onOpenChange={(next) => {
            if (!next) {
              setCheckout(null)
              onOpenChange(false)
            }
          }}
          onSucceeded={() => {
            onPurchased()
            push({
              tone: 'success',
              message:
                plan.grant_credits === 0
                  ? t('account.subscription.toast.activated_unlimited')
                  : t('account.subscription.toast.activated'),
            })
          }}
          onRetry={() => {
            // Paid replay that is already terminal: the idempotency key still
            // points at that order, so a fresh intent requires a new key.
            setCheckout(null)
            keyRef.current = newIdempotencyKey()
          }}
          onRefresh={() => {
            // Alipay QR expired: same rule — a new order needs a new intent.
            setCheckout(null)
            keyRef.current = newIdempotencyKey()
          }}
        />
      ) : null}
    </>
  )
}
