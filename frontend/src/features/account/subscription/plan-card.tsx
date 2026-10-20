import { useI18n } from '@/shared/lib/i18n-context'
import { formatCredits } from '@/shared/lib/format'
import { Button } from '@/shared/ui/button'

import { formatYuan } from '../topup/money'
import type { AvailableSubscriptionPlan } from './types'

export interface PlanCardProps {
  plan: AvailableSubscriptionPlan
  /** Writes are disabled while offline (web-00 §8.5). */
  disabled: boolean
  onSubscribe: (plan: AvailableSubscriptionPlan) => void
}

/**
 * 用户侧套餐卡（billing-08 §16.3 F-H-03）。
 *
 * `grant_credits = 0` means unlimited (§2.3 / §5.1): rendering "0" would read
 * as "no quota", so it is translated to the same word the subscription list
 * uses. `model_scope` is an admission filter only — it never changes price
 * (§2.2), hence the neutral "Models" summary rather than a pricing claim.
 */
export function PlanCard({ plan, disabled, onSubscribe }: PlanCardProps) {
  const { t } = useI18n()

  const unlimited = plan.grant_credits === 0
  const scope = plan.model_scope ?? []
  const visibleScope = scope.slice(0, 2).join(', ')
  const hiddenScopeCount = Math.max(0, scope.length - 2)

  return (
    <article
      data-testid="subscription-plan-card"
      className="flex flex-col rounded-xl border border-border-hairline bg-surface-card p-4"
    >
      <h3 className="text-[15px] font-semibold text-ink-1">{plan.title}</h3>
      {plan.description !== null && plan.description !== undefined ? (
        <p className="mt-1 line-clamp-2 text-[13px] text-ink-2">
          {plan.description}
        </p>
      ) : null}

      <p className="mt-3 text-2xl font-semibold tabular-nums text-ink-1">
        {plan.price_amount === 0
          ? t('account.subscription.plan.free')
          : formatYuan(plan.price_amount)}
        <span className="ml-1 text-[13px] font-normal text-ink-2">
          / {t('account.subscription.plan.period', { days: plan.period_days })}
        </span>
      </p>

      <dl className="mt-3 flex flex-col gap-1.5 text-[13px]">
        <div className="flex items-baseline justify-between gap-3">
          <dt className="text-ink-2">
            {t('account.subscription.plan.grant_credits')}
          </dt>
          <dd className="font-medium tabular-nums text-ink-1">
            {unlimited
              ? t('account.subscription.plan.unlimited')
              : formatCredits(plan.grant_credits)}
          </dd>
        </div>
        <div className="flex items-baseline justify-between gap-3">
          <dt className="text-ink-2">{t('account.subscription.plan.model_scope')}</dt>
          <dd
            className="text-right font-medium text-ink-1"
            title={scope.length > 0 ? scope.join(', ') : undefined}
          >
            {scope.length === 0
              ? t('account.subscription.plan.all_models')
              : hiddenScopeCount > 0
                ? `${visibleScope} ${t('account.subscription.plan.models_more', {
                    count: hiddenScopeCount,
                  })}`
                : visibleScope}
          </dd>
        </div>
      </dl>

      <Button
        className="mt-4"
        disabled={disabled}
        aria-label={`${t('account.subscription.action.subscribe')} ${plan.title}`}
        onClick={() => {
          onSubscribe(plan)
        }}
      >
        {t('account.subscription.action.subscribe')}
      </Button>
    </article>
  )
}
