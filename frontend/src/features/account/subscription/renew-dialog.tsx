import { useEffect, useRef, useState } from 'react'

import { apiClient } from '@/api/client'
import { requireData } from '@/api/result'
import { useI18n } from '@/shared/lib/i18n-context'
import { Dialog } from '@/shared/ui/base/dialog'
import { Button } from '@/shared/ui/button'
import { useToastStore } from '@/shared/ui/toast'

import { CheckoutDialog } from '../topup/checkout-dialog'
import { formatYuan } from '../topup/money'
import type { OrderView } from '../topup/types'
import { newIdempotencyKey } from './constants'
import { renewalError, type SubscriptionInlineError } from './errors'
import { findMatchingPlan } from './plan-match'
import { ChannelProviderPicker, useEnabledChannelProviders } from './provider-picker'
import { renewSubscription } from './subscription-api'
import type {
  AvailableSubscriptionPlan,
  CreateOrderResponse,
  SubscriptionChannelProvider,
  UserSubscription,
} from './types'

interface CheckoutState {
  response: CreateOrderResponse
  /** Replayed orders carry no payment payload; status comes from this row. */
  replayOrder?: OrderView
}

export interface RenewDialogProps {
  /** `null` closes the dialog. */
  subscription: UserSubscription | null
  plans: readonly AvailableSubscriptionPlan[]
  offline: boolean
  onOpenChange: (open: boolean) => void
  onRenewed: () => void
}

/**
 * 手动续费弹窗（P2a；`POST /api/subscriptions/{id}/renew`）。
 *
 * 续费复用同一实例推进 `end_at`，所以入口只对 active/expired 开放（cancelled
 * 是终态，服务端 409）。金额由服务端按实例当前套餐行读取；本弹窗的价格只是
 * 用快照 title/period 回查可用套餐得到的**预览**，查不到时明确说「以收银台
 * 为准」，绝不编造数字。
 *
 * Idempotency：一次续费意图一个键（打开时铸造、失败重试复用、关闭丢弃）。
 * 服务端同键重放返回首次订单（200）；换键重试同一周期会得到 409，这正是
 * 「网络失败后不要重复下单」的契约保证。
 */
export function RenewDialog({
  subscription,
  plans,
  offline,
  onOpenChange,
  onRenewed,
}: RenewDialogProps) {
  const { t } = useI18n()
  const push = useToastStore((state) => state.push)
  const { providers, publishableKey } = useEnabledChannelProviders(
    subscription !== null,
  )
  const keyRef = useRef<string | null>(null)
  const [provider, setProvider] = useState<SubscriptionChannelProvider | null>(null)
  const [submitting, setSubmitting] = useState(false)
  const [error, setError] = useState<SubscriptionInlineError | null>(null)
  const [activated, setActivated] = useState(false)
  const [checkout, setCheckout] = useState<CheckoutState | null>(null)

  const subscriptionId = subscription?.id ?? null
  const plan = subscription === null ? undefined : findMatchingPlan(subscription, plans)
  const isFree = plan !== undefined && plan.price_amount === 0

  // 新意图：换实例（或关闭）时换键并清空全部瞬时状态。依赖 id 而不是对象，
  // 因为列表刷新会为同一条记录产生新对象。
  useEffect(() => {
    keyRef.current = subscriptionId === null ? null : newIdempotencyKey()
    setSubmitting(false)
    setError(null)
    setActivated(false)
    setCheckout(null)
    setProvider(null)
  }, [subscriptionId])

  // 渠道配置到达后选中第一个；用户主动切换后不再覆盖。
  useEffect(() => {
    if (provider !== null || providers.length === 0) return
    setProvider(providers[0] ?? null)
  }, [provider, providers])

  if (subscription === null) return null

  function announceRenewed(): void {
    setActivated(true)
    onRenewed()
    push({ tone: 'success', message: t('account.subscription.renew.success') })
  }

  async function submit(): Promise<void> {
    if (submitting || offline || subscription === null) return
    // 0 元套餐服务端仍要求 provider 字段，但不会调用渠道；与购买同口径，
    // 发送第一个已启用渠道，未配置时用 stripe（零金额不触达渠道）。
    const effectiveProvider: SubscriptionChannelProvider | undefined = isFree
      ? (providers[0] ?? 'stripe')
      : (provider ?? providers[0])
    if (effectiveProvider === undefined) return

    const key = keyRef.current ?? (keyRef.current = newIdempotencyKey())
    setSubmitting(true)
    setError(null)
    try {
      const response = await renewSubscription(
        subscription.id,
        effectiveProvider,
        key,
      )
      const hasPayload =
        response.client_secret !== undefined || response.qr_code !== undefined
      if (hasPayload) {
        setCheckout({ response })
        return
      }
      if (response.order.status === 'succeeded') {
        announceRenewed()
        return
      }
      // 无支付载荷的 pending/终态只可能是幂等重放：读权威行再决定呈现。
      const replayOrder = requireData(
        await apiClient.GET('/api/orders/{id}', {
          params: { path: { id: response.order.id } },
        }),
      )
      if (replayOrder.status === 'succeeded') {
        announceRenewed()
        return
      }
      setCheckout({ response, replayOrder })
    } catch (caught) {
      // 保留 key：同一意图的重试必须重放首单。
      setError(renewalError(caught))
    } finally {
      setSubmitting(false)
    }
  }

  const footer = activated ? (
    <Button onClick={() => onOpenChange(false)}>
      {t('account.subscription.action.done')}
    </Button>
  ) : (
    <>
      <Button variant="ghost" disabled={submitting} onClick={() => onOpenChange(false)}>
        {t('account.subscription.action.cancel')}
      </Button>
      <Button
        disabled={
          offline ||
          submitting ||
          (!isFree && (providers.length === 0 || provider === null))
        }
        onClick={() => void submit()}
      >
        {submitting
          ? t('account.subscription.renew.renewing')
          : t('account.subscription.renew.confirm')}
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
        title={t('account.subscription.renew.title')}
        description={t('account.subscription.renew.description', {
          plan: subscription.plan_title_snapshot,
          days: subscription.period_days,
        })}
        dismissible={!submitting}
        panelClassName="max-w-[480px]"
        footer={footer}
      >
        <dl className="flex flex-col gap-2 text-sm">
          <div className="flex items-center justify-between gap-4">
            <dt className="text-ink-2">{t('account.subscription.plan.label')}</dt>
            <dd className="font-medium text-ink-1">
              {subscription.plan_title_snapshot}
            </dd>
          </div>
          <div className="flex items-center justify-between gap-4">
            <dt className="text-ink-2">{t('account.subscription.plan.period_label')}</dt>
            <dd className="font-medium text-ink-1">
              {t('account.subscription.mine.period', {
                days: subscription.period_days,
              })}
            </dd>
          </div>
          <div className="flex items-center justify-between gap-4">
            <dt className="text-ink-2">{t('account.subscription.renew.amount')}</dt>
            <dd className="font-semibold tabular-nums text-ink-1">
              {plan === undefined
                ? t('account.subscription.renew.amount_unknown')
                : plan.price_amount === 0
                  ? t('account.subscription.plan.free')
                  : formatYuan(plan.price_amount)}
            </dd>
          </div>
        </dl>

        {activated ? (
          <div aria-live="polite" className="mt-4">
            <p className="text-[15px] font-semibold text-success">
              {t('account.subscription.renew.success')}
            </p>
            <p className="mt-1 text-sm text-ink-2">
              {t('account.subscription.renew.success_body', {
                days: subscription.period_days,
              })}
            </p>
          </div>
        ) : isFree ? (
          <p className="mt-4 rounded-lg border border-info-line bg-info-bg px-3 py-2 text-sm text-info">
            {t('account.subscription.renew.free_hint')}
          </p>
        ) : (
          <div className="mt-4">
            <ChannelProviderPicker
              name="renew-provider"
              value={provider}
              onChange={setProvider}
              disabled={submitting}
            />
          </div>
        )}

        {error !== null ? (
          <p role="alert" className="mt-3 text-sm text-danger">
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
          publishableKey={publishableKey}
          onOpenChange={(next) => {
            if (!next) {
              setCheckout(null)
              onOpenChange(false)
            }
          }}
          onSucceeded={() => {
            announceRenewed()
          }}
          onRetry={() => {
            // 终态重放仍指向同一订单：换新意图必须换键。
            setCheckout(null)
            keyRef.current = newIdempotencyKey()
          }}
          onRefresh={() => {
            setCheckout(null)
            keyRef.current = newIdempotencyKey()
          }}
        />
      ) : null}
    </>
  )
}
