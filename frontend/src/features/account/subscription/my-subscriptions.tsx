import { useState } from 'react'
import { useQueryClient } from '@tanstack/react-query'

import { useI18n } from '@/shared/lib/i18n-context'
import { formatCredits } from '@/shared/lib/format'
import { Button } from '@/shared/ui/button'
import { StatusBadge, type StatusBadgeVariant } from '@/shared/ui/status-badge'
import { useToastStore } from '@/shared/ui/toast'

import { formatSubscriptionTime } from './format'
import { cancelSubscriptionAtPeriodEnd } from './subscription-api'
import type { UserSubscription, UserSubscriptionList } from './types'

export const MY_SUBSCRIPTIONS_KEY = ['subscription', 'mine'] as const

export interface MySubscriptionsProps {
  data: UserSubscriptionList | undefined
  isPending: boolean
  isError: boolean
  onRetry: () => void
  /** Writes (period-end cancel) are disabled while offline. */
  offline: boolean
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
}: MySubscriptionsProps) {
  const { t, locale } = useI18n()
  const queryClient = useQueryClient()
  const push = useToastStore((state) => state.push)
  const [pendingId, setPendingId] = useState<number | null>(null)
  const [cancelErrorId, setCancelErrorId] = useState<number | null>(null)

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

  return (
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
              />
            </li>
          ))}
        </ul>
      )}
    </section>
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
}: {
  item: UserSubscription
  locale: string
  offline: boolean
  pending: boolean
  /** Another row's cancel is in flight; serialize all cancel switches. */
  anyPending: boolean
  cancelError: boolean
  onCancel: () => void
}) {
  const { t } = useI18n()

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
    </article>
  )
}
