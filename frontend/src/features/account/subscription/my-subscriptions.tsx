import { useState } from 'react'
import { useQueryClient } from '@tanstack/react-query'

import { useI18n } from '@/shared/lib/i18n-context'
import { formatCredits } from '@/shared/lib/format'
import { Button } from '@/shared/ui/button'
import { StatusBadge, type StatusBadgeVariant } from '@/shared/ui/status-badge'
import { useToastStore } from '@/shared/ui/toast'

import { ChangePlanDialog } from './change-plan-dialog'
import { SubscriptionEventsDialog } from './events-dialog'
import { formatSubscriptionTime } from './format'
import { RefundDialog } from './refund-dialog'
import { RenewDialog } from './renew-dialog'
import { refundIneligibilityKey } from './support'
import { cancelSubscriptionAtPeriodEnd, setSubscriptionAutoRenew } from './subscription-api'
import type {
  AvailableSubscriptionPlan,
  PaymentMethod,
  UserSubscription,
  UserSubscriptionList,
} from './types'

export const MY_SUBSCRIPTIONS_KEY = ['subscription', 'mine'] as const

export interface MySubscriptionsProps {
  data: UserSubscriptionList | undefined
  isPending: boolean
  isError: boolean
  onRetry: () => void
  /** Writes (period-end cancel / auto-renew) are disabled while offline. */
  offline: boolean
  /** 已保存的支付令牌（S4）：自动续费开关必须从中选择扣费方式。 */
  paymentMethods: PaymentMethod[]
  /** 可用套餐：续费金额预览与升降级目标列表用（服务端仍是定价真源）。 */
  plans: readonly AvailableSubscriptionPlan[]
}

const STATUS_VARIANT: Record<UserSubscription['status'], StatusBadgeVariant> = {
  active: 'success',
  // Terminal instances are inert, not errors: neutral per modules-00 §4.6.4.
  expired: 'neutral',
  cancelled: 'neutral-outline',
}

/**
 * 我的订阅（billing-08 §16.3 F-H-03）。
 *
 * Multiple active instances are legal (§3.1), so this is a list rather than a
 * single "current plan" card. `end_at` is the only expiry column; `updated_at`
 * is the cancellation instant (the API carries no `cancelled_at`).
 *
 * Quota: `amount_total = 0` means unlimited (§2.3). The server then omits
 * `amount_remain`, and the UI must not invent a denominator — "无限" text and
 * no progress bar.
 */
export function MySubscriptions({
  data,
  isPending,
  isError,
  onRetry,
  offline,
  paymentMethods,
  plans,
}: MySubscriptionsProps) {
  const { t, locale } = useI18n()
  const queryClient = useQueryClient()
  const push = useToastStore((state) => state.push)
  const [pendingId, setPendingId] = useState<number | null>(null)
  const [cancelErrorId, setCancelErrorId] = useState<number | null>(null)
  const [autoRenewPendingId, setAutoRenewPendingId] = useState<number | null>(null)
  const [autoRenewErrorId, setAutoRenewErrorId] = useState<number | null>(null)
  // 每个写入口一个「当前实例」：dialogs 关闭即为 null，天然串行化且不会把
  // 实例对象复制进全局状态。
  const [renewTarget, setRenewTarget] = useState<UserSubscription | null>(null)
  const [changeTarget, setChangeTarget] = useState<UserSubscription | null>(null)
  const [refundTarget, setRefundTarget] = useState<UserSubscription | null>(null)
  const [eventsTarget, setEventsTarget] = useState<UserSubscription | null>(null)

  // All four write dialogs (renew/change/refund) and the read-only event flow
  // refresh the same list; the server's projection is the only truth, so we
  // never patch rows locally.
  function refreshSubscriptions(): void {
    void queryClient.invalidateQueries({ queryKey: MY_SUBSCRIPTIONS_KEY })
  }

  async function handleCancel(item: UserSubscription): Promise<void> {
    if (pendingId !== null || offline) return
    setPendingId(item.id)
    setCancelErrorId(null)
    try {
      await cancelSubscriptionAtPeriodEnd(item.id)
      push({
        tone: 'success',
        message: t('account.subscription.toast.cancelled'),
      })
      // The switch reflects a server field (`cancel_at_period_end`); never set
      // it locally, so a failed write cannot leave the UI claiming success.
      await queryClient.invalidateQueries({ queryKey: MY_SUBSCRIPTIONS_KEY })
    } catch {
      setCancelErrorId(item.id)
      push({
        tone: 'error',
        message: t('account.subscription.mine.cancel_error'),
      })
    } finally {
      setPendingId(null)
    }
  }

  async function handleAutoRenew(
    item: UserSubscription,
    enabled: boolean,
    paymentMethodId: string | undefined,
  ): Promise<void> {
    if (autoRenewPendingId !== null || offline) return
    if (enabled && paymentMethodId === undefined) {
      // 启用但没有可用令牌：服务端会 422；本地先给可操作提示，避免无谓请求。
      push({ tone: 'error', message: t('account.subscription.mine.auto_renew_requires_method') })
      return
    }
    setAutoRenewPendingId(item.id)
    setAutoRenewErrorId(null)
    try {
      await setSubscriptionAutoRenew(item.id, enabled, paymentMethodId)
      push({
        tone: 'success',
        message: enabled
          ? t('account.subscription.mine.auto_renew_enabled')
          : t('account.subscription.mine.auto_renew_disabled'),
      })
      // 服务端是唯一真源（含失败计数清零）；绝不本地猜测开关状态。
      await queryClient.invalidateQueries({ queryKey: MY_SUBSCRIPTIONS_KEY })
    } catch {
      setAutoRenewErrorId(item.id)
      push({ tone: 'error', message: t('account.subscription.mine.auto_renew_error') })
    } finally {
      setAutoRenewPendingId(null)
    }
  }

  return (
    <>
      <section
        aria-labelledby="subscription-mine-title"
        className="rounded-xl border border-border-hairline bg-surface-card p-5"
      >
        <h2
          id="subscription-mine-title"
          className="text-[15px] font-semibold text-ink-1"
        >
          {t('account.subscription.mine.title')}
        </h2>

        {isPending ? (
          <div
            role="status"
            aria-label={t('account.subscription.mine.loading')}
            aria-busy="true"
            className="mt-4 flex flex-col gap-3"
          >
            <div aria-hidden="true" className="h-24 animate-pulse rounded-lg bg-surface-subtle" />
            <div aria-hidden="true" className="h-24 animate-pulse rounded-lg bg-surface-subtle" />
          </div>
        ) : isError ? (
          <div className="mt-4 flex items-center gap-3 text-sm text-ink-2">
            <span>{t('account.subscription.mine.error')}</span>
            <Button variant="secondary" onClick={onRetry}>
              {t('account.subscription.mine.retry')}
            </Button>
          </div>
        ) : data === undefined || data.items.length === 0 ? (
          <p className="mt-4 text-sm text-ink-2">
            {t('account.subscription.mine.empty')}
          </p>
        ) : (
          <ul className="mt-4 flex flex-col gap-3">
            {data.items.map((item) => (
              <li key={item.id}>
                <SubscriptionRow
                  item={item}
                  locale={locale}
                  offline={offline}
                  pending={pendingId === item.id}
                  anyPending={pendingId !== null}
                  cancelError={cancelErrorId === item.id}
                  onCancel={() => void handleCancel(item)}
                  paymentMethods={paymentMethods}
                  autoRenewPending={autoRenewPendingId === item.id}
                  autoRenewBusy={autoRenewPendingId !== null}
                  autoRenewError={autoRenewErrorId === item.id}
                  onAutoRenew={(enabled, methodId) =>
                    void handleAutoRenew(item, enabled, methodId)
                  }
                  onRenew={() => setRenewTarget(item)}
                  onChangePlan={() => setChangeTarget(item)}
                  onRefund={() => setRefundTarget(item)}
                  onShowEvents={() => setEventsTarget(item)}
                />
              </li>
            ))}
          </ul>
        )}
      </section>

      {/* 写操作各自成弹窗：列表只负责选择目标实例，金额/折算全部由服务端
          在弹窗提交时计算；成功后统一失效列表查询而不是本地改行。 */}
      <RenewDialog
        subscription={renewTarget}
        plans={plans}
        offline={offline}
        onOpenChange={(open) => {
          if (!open) setRenewTarget(null)
        }}
        onRenewed={refreshSubscriptions}
      />
      <ChangePlanDialog
        subscription={changeTarget}
        plans={plans}
        offline={offline}
        onOpenChange={(open) => {
          if (!open) setChangeTarget(null)
        }}
        onChanged={refreshSubscriptions}
      />
      <RefundDialog
        subscription={refundTarget}
        offline={offline}
        onOpenChange={(open) => {
          if (!open) setRefundTarget(null)
        }}
        onRequested={refreshSubscriptions}
      />
      <SubscriptionEventsDialog
        subscription={eventsTarget}
        plans={plans}
        onOpenChange={(open) => {
          if (!open) setEventsTarget(null)
        }}
      />
    </>
  )
}

function SubscriptionRow({
  item,
  locale,
  offline,
  pending,
  anyPending,
  cancelError,
  onCancel,
  paymentMethods,
  autoRenewPending,
  autoRenewBusy,
  autoRenewError,
  onAutoRenew,
  onRenew,
  onChangePlan,
  onRefund,
  onShowEvents,
}: {
  item: UserSubscription
  locale: string
  offline: boolean
  pending: boolean
  /** Another row's cancel is in flight; serialize all cancel switches. */
  anyPending: boolean
  cancelError: boolean
  onCancel: () => void
  paymentMethods: PaymentMethod[]
  autoRenewPending: boolean
  autoRenewBusy: boolean
  autoRenewError: boolean
  onAutoRenew: (enabled: boolean, methodId: string | undefined) => void
  onRenew: () => void
  onChangePlan: () => void
  onRefund: () => void
  onShowEvents: () => void
}) {
  const { t } = useI18n()
  // R-54：立即取消后实例仍是 active 但在宽限期内（end_at 已过）。
  // 此时重新开启自动续费会让 worker 立刻扣款并重置额度/延长周期，服务端
  // 会返回 422；前端在这里就禁用开关并说明「请重新购买」，同时不再把
  // 残留的旧卡作为默认选中项（避免误导用户「可以继续自动续费」）。
  const ended = new Date(item.end_at).getTime() <= Date.now()
  const [selectedMethodId, setSelectedMethodId] = useState<string | undefined>(
    ended ? undefined : (item.auto_renew_payment_method_id ?? undefined),
  )
  const chosenMethodId = ended
    ? undefined
    : (selectedMethodId ?? item.auto_renew_payment_method_id ?? paymentMethods[0]?.id)
  const autoRenewDisabled =
    offline || item.status !== 'active' || ended || autoRenewBusy || autoRenewPending
  const autoRenewLabel = `${t('account.subscription.mine.auto_renew')}: ${item.plan_title_snapshot}`
  // P-13/R-35：资格由本地先判定并解释原因；服务端 422 仍是最终裁决。
  const refundKey = refundIneligibilityKey(item)

  const unlimited = item.amount_total === 0
  const remain = unlimited
    ? null
    : (item.amount_remain ?? Math.max(0, item.amount_total - item.amount_used))
  // The server guarantees used <= total for finite quotas; clamping only
  // protects the bar from a transient inconsistent read during a reset.
  const percent = unlimited
    ? 0
    : Math.min(100, Math.round((item.amount_used / item.amount_total) * 100))

  const timeText =
    item.status === 'active'
      ? t('account.subscription.mine.until', {
          time: formatSubscriptionTime(item.end_at, locale),
        })
      : item.status === 'expired'
        ? t('account.subscription.mine.expired_at', {
            time: formatSubscriptionTime(item.end_at, locale),
          })
        : t('account.subscription.mine.cancelled_at', {
            time: formatSubscriptionTime(item.updated_at, locale),
          })

  const cancelDisabled =
    offline ||
    item.status !== 'active' ||
    item.cancel_at_period_end ||
    // One cancel at a time: the server serializes per instance, and two
    // in-flight switches would make the "which row is busy" notice ambiguous.
    anyPending

  return (
    <article
      className={[
        'rounded-lg border border-border-hairline bg-surface-page p-4',
        // Terminal instances are kept for history but visually inert.
        item.status === 'active' ? '' : 'opacity-70',
      ].join(' ')}
    >
      <div className="flex flex-wrap items-center justify-between gap-2">
        <div className="flex items-center gap-2">
          <h3 className="text-sm font-semibold text-ink-1">
            {item.plan_title_snapshot}
          </h3>
          <StatusBadge variant={STATUS_VARIANT[item.status]}>
            {t(`account.subscription.status.${item.status}`)}
          </StatusBadge>
        </div>
        <span className="text-[13px] text-ink-2">{timeText}</span>
      </div>

      <p className="mt-1 text-xs text-ink-3">
        {t('account.subscription.mine.period', { days: item.period_days })}
      </p>

      <div className="mt-3">
        <dl className="flex flex-wrap items-baseline gap-x-4 gap-y-1 text-[13px]">
          <div className="flex items-baseline gap-1.5">
            <dt className="text-ink-2">{t('account.subscription.mine.total')}</dt>
            <dd className="font-medium tabular-nums text-ink-1">
              {unlimited
                ? t('account.subscription.mine.unlimited')
                : formatCredits(item.amount_total)}
            </dd>
          </div>
          <div className="flex items-baseline gap-1.5">
            <dt className="text-ink-2">{t('account.subscription.mine.used')}</dt>
            <dd className="font-medium tabular-nums text-ink-1">
              {formatCredits(item.amount_used)}
            </dd>
          </div>
          {remain !== null ? (
            <div className="flex items-baseline gap-1.5">
              <dt className="text-ink-2">{t('account.subscription.mine.remain')}</dt>
              <dd className="font-medium tabular-nums text-ink-1">
                {formatCredits(remain)}
              </dd>
            </div>
          ) : null}
        </dl>

        {unlimited ? null : (
          <div
            role="progressbar"
            aria-label={t('account.subscription.mine.used')}
            aria-valuemin={0}
            aria-valuemax={item.amount_total}
            aria-valuenow={item.amount_used}
            // The bar is a visual ratio; assistive tech gets the exact numbers
            // instead of only a percentage implied by aria-valuenow/max.
            aria-valuetext={t('account.subscription.mine.progress', {
              used: formatCredits(item.amount_used),
              total: formatCredits(item.amount_total),
            })}
            className="mt-2 h-1.5 overflow-hidden rounded-full bg-surface-subtle"
          >
            <div
              className="h-full rounded-full bg-brand transition-[width]"
              style={{ width: `${percent}%` }}
            />
          </div>
        )}
      </div>

      <div className="mt-3 flex flex-wrap items-center justify-between gap-3">
        <div className="flex flex-col gap-1">
          {item.next_reset_at !== null && item.next_reset_at !== undefined ? (
            <span className="text-xs text-ink-2">
              {t('account.subscription.mine.next_reset')}:{' '}
              {formatSubscriptionTime(item.next_reset_at, locale)}
            </span>
          ) : null}
          {cancelError ? (
            <span role="alert" className="text-xs text-danger">
              {t('account.subscription.mine.cancel_error')}
            </span>
          ) : null}
          {pending ? (
            <span role="status" className="text-xs text-ink-2">
              {t('account.subscription.mine.cancelling')}
            </span>
          ) : null}
        </div>

        <div className="flex flex-wrap items-center gap-3">
          {item.status === 'active' ? (
            <div className="flex flex-col gap-1">
              <div className="flex items-center gap-2">
                <label className="text-xs text-ink-2" htmlFor={`auto-renew-method-${item.id}`}>
                  {t('account.subscription.mine.auto_renew')}
                </label>
                {item.auto_renew && item.auto_renew_failures > 0 ? (
                  <span className="text-xs text-danger">
                    {t('account.subscription.mine.auto_renew_failures', {
                      count: item.auto_renew_failures,
                    })}
                  </span>
                ) : null}
                {!item.auto_renew && item.auto_renew_failures >= 3 ? (
                  // P-16：第 3 次失败后服务端关闭开关。只显示禁用状态会让
                  // 用户以为是自己关的；这里必须说明原因与恢复方式。
                  <span className="text-xs text-danger">
                    {t('account.subscription.mine.auto_renew_failed_hint')}
                  </span>
                ) : null}
                {ended ? (
                  // R-54：合同期已结束（含立即取消后的宽限期）。
                  <span className="text-xs text-ink-2">
                    {t('account.subscription.mine.auto_renew_ended_hint')}
                  </span>
                ) : null}
              </div>
              {paymentMethods.length > 0 ? (
                <select
                  id={`auto-renew-method-${item.id}`}
                  aria-label={`${t('account.subscription.mine.auto_renew_method')}: ${item.plan_title_snapshot}`}
                  className="h-8 rounded-lg border border-border-default bg-surface-card px-2 text-xs text-ink-1 disabled:opacity-50"
                  value={chosenMethodId ?? ''}
                  disabled={autoRenewDisabled}
                  onChange={(event) => {
                    const next = event.target.value
                    setSelectedMethodId(next)
                    // 已开启时换卡立即生效：服务端会校验新令牌归属/可用性。
                    if (item.auto_renew) onAutoRenew(true, next)
                  }}
                >
                  {/* 已结束（R-54）时 chosenMethodId 为 undefined：给一个空
                      选项，避免浏览器把第一张卡当作默认值回显。 */}
                  {chosenMethodId === undefined ? <option value="" /> : null}
                  {paymentMethods.map((method) => (
                    <option key={method.id} value={method.id}>
                      {method.brand} ···· {method.last4}
                    </option>
                  ))}
                </select>
              ) : (
                <span className="text-xs text-ink-3">
                  {t('account.subscription.mine.auto_renew_requires_method')}
                </span>
              )}
              {autoRenewError ? (
                <span role="alert" className="text-xs text-danger">
                  {t('account.subscription.mine.auto_renew_error')}
                </span>
              ) : null}
              {autoRenewPending ? (
                <span role="status" className="text-xs text-ink-2">
                  {t('account.subscription.mine.auto_renew_saving')}
                </span>
              ) : null}
            </div>
          ) : null}

          {item.status === 'active' ? (
            <button
              type="button"
              role="switch"
              aria-checked={item.auto_renew}
              aria-label={autoRenewLabel}
              aria-busy={autoRenewPending}
              disabled={autoRenewDisabled || (paymentMethods.length === 0 && !item.auto_renew)}
              onClick={() => onAutoRenew(!item.auto_renew, chosenMethodId)}
              className={[
                'relative inline-flex h-5 w-9 shrink-0 items-center rounded-full border transition-colors focus-visible:shadow-[var(--focus-ring)] focus-visible:outline-none disabled:cursor-not-allowed disabled:opacity-50',
                item.auto_renew
                  ? 'border-brand bg-brand'
                  : 'border-border-default bg-surface-subtle',
              ].join(' ')}
            >
              <span
                aria-hidden="true"
                className={[
                  'inline-block size-3.5 rounded-full bg-white shadow transition-transform',
                  item.auto_renew ? 'translate-x-[18px]' : 'translate-x-[3px]',
                ].join(' ')}
              />
            </button>
          ) : null}

          <button
          type="button"
          role="switch"
          aria-checked={item.cancel_at_period_end}
          // The row title disambiguates multiple active instances: a bare
          // "Cancel at period end" gives a screen reader no way to tell which
          // subscription the switch belongs to.
          aria-label={`${t('account.subscription.mine.cancel_at_period_end')}: ${item.plan_title_snapshot}`}
          aria-busy={pending}
          disabled={cancelDisabled}
          onClick={onCancel}
          className={[
            'relative inline-flex h-5 w-9 shrink-0 items-center rounded-full border transition-colors focus-visible:shadow-[var(--focus-ring)] focus-visible:outline-none disabled:cursor-not-allowed disabled:opacity-50',
            item.cancel_at_period_end
              ? 'border-brand bg-brand'
              : 'border-border-default bg-surface-subtle',
          ].join(' ')}
        >
          <span
            aria-hidden="true"
            className={[
              'inline-block size-3.5 rounded-full bg-white shadow transition-transform',
              item.cancel_at_period_end ? 'translate-x-[18px]' : 'translate-x-[3px]',
            ].join(' ')}
          />
          </button>
        </div>
      </div>

      {/* 订阅生命周期入口：续费/变更/退款是写操作（离线禁用），事件流只读。
          每个按钮都带套餐快照名，避免多条实例时读屏无法区分目标。 */}
      <div className="mt-3 flex flex-wrap items-center gap-2 border-t border-border-hairline pt-3">
        {item.status !== 'cancelled' ? (
          <Button
            variant="secondary"
            className="h-8 px-2.5 text-xs"
            disabled={offline}
            aria-label={`${t('account.subscription.mine.renew')}: ${item.plan_title_snapshot}`}
            onClick={onRenew}
          >
            {t('account.subscription.mine.renew')}
          </Button>
        ) : null}
        {item.status === 'active' ? (
          <Button
            variant="secondary"
            className="h-8 px-2.5 text-xs"
            disabled={offline}
            aria-label={`${t('account.subscription.mine.change_plan')}: ${item.plan_title_snapshot}`}
            onClick={onChangePlan}
          >
            {t('account.subscription.mine.change_plan')}
          </Button>
        ) : null}
        <Button
          variant="ghost"
          className="h-8 px-2.5 text-xs"
          aria-label={`${t('account.subscription.mine.events')}: ${item.plan_title_snapshot}`}
          onClick={onShowEvents}
        >
          {t('account.subscription.mine.events')}
        </Button>
        {item.status === 'active' ? (
          <span className="flex flex-wrap items-center gap-2">
            <Button
              variant="ghost"
              className="h-8 px-2.5 text-xs"
              disabled={offline || refundKey !== null}
              aria-label={`${t('account.subscription.mine.refund')}: ${item.plan_title_snapshot}`}
              onClick={onRefund}
            >
              {t('account.subscription.mine.refund')}
            </Button>
            {refundKey !== null ? (
              <span className="text-xs text-ink-3">{t(refundKey)}</span>
            ) : null}
          </span>
        ) : null}
      </div>
    </article>
  )
}
