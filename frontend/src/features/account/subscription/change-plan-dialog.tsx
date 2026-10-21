import { useEffect, useRef, useState } from 'react'

import { apiClient } from '@/api/client'
import { requireData } from '@/api/result'
import { useI18n } from '@/shared/lib/i18n-context'
import { formatCredits } from '@/shared/lib/format'
import { Dialog } from '@/shared/ui/base/dialog'
import { Button } from '@/shared/ui/button'
import { useToastStore } from '@/shared/ui/toast'

import { CheckoutDialog } from '../topup/checkout-dialog'
import { formatYuan } from '../topup/money'
import type { OrderView } from '../topup/types'
import { changeError, type SubscriptionInlineError } from './errors'
import { formatSubscriptionTime } from './format'
import { findMatchingPlan } from './plan-match'
import { ChannelProviderPicker, useEnabledChannelProviders } from './provider-picker'
import { changeSubscriptionPlan } from './subscription-api'
import type {
  AvailableSubscriptionPlan,
  SubscriptionChangeResponse,
  SubscriptionChannelProvider,
  UserSubscription,
} from './types'

interface CheckoutState {
  response: SubscriptionChangeResponse
  replayOrder?: OrderView
}

export interface ChangePlanDialogProps {
  /** `null` closes the dialog. */
  subscription: UserSubscription | null
  plans: readonly AvailableSubscriptionPlan[]
  offline: boolean
  onOpenChange: (open: boolean) => void
  onChanged: () => void
}

function newKey(): string {
  return globalThis.crypto.randomUUID()
}

/** 负额度差按模块规范用 U+2212，且 formatCredits 拒绝负数输入。 */
function formatCreditsDelta(delta: number): string {
  if (delta < 0) return `−${formatCredits(-delta)}`
  return `+${formatCredits(delta)}`
}

/**
 * 升级/降级弹窗（S2；`POST /api/subscriptions/{id}/change`）。
 *
 * 两段式确认：先选目标套餐，再进入确认步骤说明「折算与生效时点」，最后才
 * 提交。折算公式（新套餐价 − 剩余价值）是服务端唯一真源，客户端只做可判定
 * 的部分：
 *   - 目标价 ≤ 当前套餐价 ⇒ 应补必为 0（剩余价值永远小于已付价），走
 *     0 元即时生效/延长时长路径；
 *   - 目标价 > 当前价 ⇒ 差额取决于剩余时间，展示「以收银台为准」并在提交后
 *     用响应里的 `price_delta` 回显实付。
 *
 * 契约只允许同币种、同 `period_days` 的目标套餐（P-10），所以候选列表先按
 * 周期过滤；跨周期请求即使绕过 UI 也只会得到 422 subscription_cycle_mismatch。
 * Idempotency 与续费同口径：一次变更意图一个键，失败重试复用、关闭丢弃。
 */
export function ChangePlanDialog({
  subscription,
  plans,
  offline,
  onOpenChange,
  onChanged,
}: ChangePlanDialogProps) {
  const { t, locale } = useI18n()
  const push = useToastStore((state) => state.push)
  const { providers, publishableKey } = useEnabledChannelProviders()
  const keyRef = useRef<string | null>(null)
  const [targetPlanId, setTargetPlanId] = useState<number | null>(null)
  const [step, setStep] = useState<'select' | 'confirm'>('select')
  const [provider, setProvider] = useState<SubscriptionChannelProvider | null>(null)
  const [submitting, setSubmitting] = useState(false)
  const [error, setError] = useState<SubscriptionInlineError | null>(null)
  const [result, setResult] = useState<SubscriptionChangeResponse | null>(null)
  const [checkout, setCheckout] = useState<CheckoutState | null>(null)

  const subscriptionId = subscription?.id ?? null
  const currentPlan =
    subscription === null ? undefined : findMatchingPlan(subscription, plans)
  const candidates =
    subscription === null
      ? []
      : plans.filter(
          (plan) =>
            plan.period_days === subscription.period_days &&
            (currentPlan === undefined
              ? plan.title !== subscription.plan_title_snapshot
              : plan.id !== currentPlan.id),
        )
  const target = candidates.find((plan) => plan.id === targetPlanId)
  // 目标价不高于当前价 ⇒ 剩余价值必然覆盖差价，应补为 0（降级/同价）。
  const zeroDelta =
    target !== undefined &&
    currentPlan !== undefined &&
    target.price_amount <= currentPlan.price_amount
  const requiresProvider = !zeroDelta

  useEffect(() => {
    keyRef.current = subscriptionId === null ? null : newKey()
    setTargetPlanId(null)
    setStep('select')
    setProvider(null)
    setSubmitting(false)
    setError(null)
    setResult(null)
    setCheckout(null)
  }, [subscriptionId])

  useEffect(() => {
    if (provider !== null || providers.length === 0) return
    setProvider(providers[0] ?? null)
  }, [provider, providers])

  if (subscription === null) return null

  function announceChanged(response: SubscriptionChangeResponse): void {
    setResult(response)
    onChanged()
    push({
      tone: 'success',
      message:
        response.event_kind === 'upgraded'
          ? t('account.subscription.change.success_upgraded')
          : t('account.subscription.change.success_downgraded'),
    })
  }

  async function submit(): Promise<void> {
    if (submitting || offline || subscription === null || target === undefined) return
    const effectiveProvider: SubscriptionChannelProvider | undefined = zeroDelta
      ? (providers[0] ?? 'stripe')
      : (provider ?? providers[0])
    if (effectiveProvider === undefined) return

    const key = keyRef.current ?? (keyRef.current = newKey())
    setSubmitting(true)
    setError(null)
    try {
      const response = await changeSubscriptionPlan(
        subscription.id,
        target.id,
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
        announceChanged(response)
        return
      }
      // 无支付载荷的 pending：幂等重放。读权威订单决定呈现。
      const replayOrder = requireData(
        await apiClient.GET('/api/orders/{id}', {
          params: { path: { id: response.order.id } },
        }),
      )
      if (replayOrder.status === 'succeeded') {
        announceChanged(response)
        return
      }
      setCheckout({ response, replayOrder })
    } catch (caught) {
      setError(changeError(caught))
    } finally {
      setSubmitting(false)
    }
  }

  const footer = result !== null ? (
    <Button onClick={() => onOpenChange(false)}>
      {t('account.subscription.action.done')}
    </Button>
  ) : step === 'select' ? (
    <>
      <Button variant="ghost" onClick={() => onOpenChange(false)}>
        {t('account.subscription.action.cancel')}
      </Button>
      <Button
        disabled={target === undefined}
        onClick={() => setStep('confirm')}
      >
        {t('account.subscription.change.next')}
      </Button>
    </>
  ) : (
    <>
      <Button
        variant="ghost"
        disabled={submitting}
        onClick={() => setStep('select')}
      >
        {t('account.subscription.change.back')}
      </Button>
      <Button
        disabled={
          offline ||
          submitting ||
          (requiresProvider && (providers.length === 0 || provider === null))
        }
        onClick={() => void submit()}
      >
        {submitting
          ? t('account.subscription.change.changing')
          : t('account.subscription.change.confirm')}
      </Button>
    </>
  )

  const timingHint = zeroDelta
    ? t('account.subscription.change.zero_hint')
    : t('account.subscription.change.paid_hint')

  return (
    <>
      <Dialog
        open={checkout === null}
        onOpenChange={(next) => {
          if (!submitting) onOpenChange(next)
        }}
        title={
          step === 'confirm'
            ? t('account.subscription.change.confirm_title')
            : t('account.subscription.change.title')
        }
        description={
          step === 'confirm'
            ? t('account.subscription.change.confirm_body', {
                plan: subscription.plan_title_snapshot,
                target: target?.title ?? '',
              })
            : t('account.subscription.change.description', {
                days: subscription.period_days,
              })
        }
        dismissible={!submitting}
        panelClassName="max-w-[480px]"
        footer={footer}
      >
        {result !== null ? (
          <div aria-live="polite" className="flex flex-col gap-2 text-sm">
            <p className="text-[15px] font-semibold text-success">
              {result.event_kind === 'upgraded'
                ? t('account.subscription.change.success_upgraded')
                : t('account.subscription.change.success_downgraded')}
            </p>
            <dl className="flex flex-col gap-2">
              <div className="flex items-center justify-between gap-4">
                <dt className="text-ink-2">
                  {t('account.subscription.change.result_delta')}
                </dt>
                <dd className="font-medium tabular-nums text-ink-1">
                  {result.price_delta === 0
                    ? t('account.subscription.plan.free')
                    : formatYuan(result.price_delta)}
                </dd>
              </div>
              {result.new_end_at !== undefined ? (
                <div className="flex items-center justify-between gap-4">
                  <dt className="text-ink-2">
                    {t('account.subscription.change.result_end')}
                  </dt>
                  <dd className="font-medium tabular-nums text-ink-1">
                    {formatSubscriptionTime(result.new_end_at, locale)}
                  </dd>
                </div>
              ) : null}
              <div className="flex items-center justify-between gap-4">
                <dt className="text-ink-2">
                  {t('account.subscription.change.result_credits')}
                </dt>
                <dd className="font-medium tabular-nums text-ink-1">
                  {formatCreditsDelta(result.credits_delta)}
                </dd>
              </div>
            </dl>
          </div>
        ) : step === 'select' ? (
          candidates.length === 0 ? (
            <p className="text-sm text-ink-2">
              {t('account.subscription.change.empty')}
            </p>
          ) : (
            <fieldset>
              <legend className="mb-2 text-[13px] font-medium text-ink-1">
                {t('account.subscription.change.target')}
              </legend>
              <div className="flex flex-col gap-2">
                {candidates.map((plan) => (
                  <label
                    key={plan.id}
                    className="flex cursor-pointer items-center justify-between gap-3 rounded-lg border border-border-default bg-surface-page px-3 py-2 text-sm text-ink-1"
                  >
                    <span className="flex items-center gap-2">
                      <input
                        type="radio"
                        name="change-plan-target"
                        value={plan.id}
                        checked={targetPlanId === plan.id}
                        onChange={() => setTargetPlanId(plan.id)}
                        className="accent-brand"
                      />
                      <span className="font-medium">{plan.title}</span>
                    </span>
                    <span className="tabular-nums text-ink-2">
                      {plan.price_amount === 0
                        ? t('account.subscription.plan.free')
                        : formatYuan(plan.price_amount)}
                    </span>
                  </label>
                ))}
              </div>
            </fieldset>
          )
        ) : (
          <div className="flex flex-col gap-3">
            <dl className="flex flex-col gap-2 text-sm">
              <div className="flex items-center justify-between gap-4">
                <dt className="text-ink-2">
                  {t('account.subscription.change.current')}
                </dt>
                <dd className="font-medium text-ink-1">
                  {subscription.plan_title_snapshot}
                </dd>
              </div>
              <div className="flex items-center justify-between gap-4">
                <dt className="text-ink-2">
                  {t('account.subscription.change.target')}
                </dt>
                <dd className="font-medium text-ink-1">{target?.title}</dd>
              </div>
              <div className="flex items-center justify-between gap-4">
                <dt className="text-ink-2">
                  {t('account.subscription.change.price')}
                </dt>
                <dd className="font-semibold tabular-nums text-ink-1">
                  {target !== undefined && target.price_amount === 0
                    ? t('account.subscription.plan.free')
                    : target !== undefined
                      ? formatYuan(target.price_amount)
                      : ''}
                </dd>
              </div>
            </dl>
            <p className="rounded-lg border border-info-line bg-info-bg px-3 py-2 text-sm text-info">
              {timingHint}
            </p>
            {requiresProvider ? (
              <ChannelProviderPicker
                name="change-provider"
                value={provider}
                onChange={setProvider}
                disabled={submitting}
              />
            ) : null}
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
            announceChanged(checkout.response)
          }}
          onRetry={() => {
            setCheckout(null)
            keyRef.current = newKey()
          }}
          onRefresh={() => {
            setCheckout(null)
            keyRef.current = newKey()
          }}
        />
      ) : null}
    </>
  )
}
