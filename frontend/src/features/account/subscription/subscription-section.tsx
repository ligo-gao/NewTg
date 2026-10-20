import { useState } from 'react'
import { useQuery, useQueryClient } from '@tanstack/react-query'

import { apiClient } from '@/api/client'
import { requireData } from '@/api/result'
import { useI18n } from '@/shared/lib/i18n-context'
import { Button } from '@/shared/ui/button'

import { BillingPreferenceCard, BILLING_PREFERENCE_KEY } from './billing-preference'
import { MY_SUBSCRIPTIONS_KEY, MySubscriptions } from './my-subscriptions'
import { PlanCard } from './plan-card'
import { SubscriptionPurchaseDialog } from './purchase-dialog'
import { useOnlineStatus } from './use-online-status'
import type { AvailableSubscriptionPlan } from './types'

export const PLANS_KEY = ['subscription', 'plans'] as const

/**
 * /wallet 的订阅区（billing-08 §16.3 F-H-03/04）。
 *
 * Each block owns its loading/error/empty state so a failing preference read
 * cannot blank the plan grid. The six-state contract maps as:
 *   - loading: skeleton rows (`aria-busy`);
 *   - empty: plans [] and subscriptions [] have their own copy (§16.5);
 *   - error: inline message + Retry;
 *   - 429: rendered inside the purchase dialog next to the failing action;
 *   - offline: `navigator.onLine` disables every write (subscribe, cancel,
 *     preference) and shows a notice; reads stay available;
 *   - no-permission: not applicable on this surface — all four endpoints are
 *     scoped to `Principal.UserID` and require only a signed-in user, so a 403
 *     is surfaced as an ordinary load error rather than a fake auth state.
 *
 * The page remains usable with zero enabled plans: the section renders the
 * documented empty state instead of disappearing (产品要求保留空态，便于用户
 * 确认订阅功能存在但当前无在售套餐).
 */
export function SubscriptionSection() {
  const { t } = useI18n()
  const queryClient = useQueryClient()
  // The hook reports online; every write here is gated on its negation.
  const offline = !useOnlineStatus()
  const [purchasePlan, setPurchasePlan] =
    useState<AvailableSubscriptionPlan | null>(null)

  const plansQuery = useQuery({
    queryKey: PLANS_KEY,
    queryFn: async () =>
      requireData(await apiClient.GET('/api/subscription/plans')),
  })

  const mineQuery = useQuery({
    queryKey: MY_SUBSCRIPTIONS_KEY,
    queryFn: async () =>
      requireData(
        await apiClient.GET('/api/subscriptions', {
          // The user-side list is unpaginated in the UI: show the newest 100
          // instances (the contract's maximum page size) rather than silently
          // hiding older active instances behind page 2.
          params: { query: { page_size: 100 } },
        }),
      ),
  })

  const preferenceQuery = useQuery({
    queryKey: BILLING_PREFERENCE_KEY,
    queryFn: async () =>
      requireData(await apiClient.GET('/api/billing-preference')),
  })

  return (
    <div className="mx-auto flex w-full max-w-[1080px] flex-col gap-4">
      {offline ? (
        <p
          role="status"
          className="rounded-lg border border-warning-line bg-warning-bg px-3 py-2 text-sm text-warning"
        >
          {t('account.subscription.offline_notice')}
        </p>
      ) : null}

      <section
        aria-labelledby="subscription-plans-title"
        className="rounded-xl border border-border-hairline bg-surface-card p-5"
      >
        <h2
          id="subscription-plans-title"
          className="text-[15px] font-semibold text-ink-1"
        >
          {t('account.subscription.plans.title')}
        </h2>
        <p className="mt-1 text-[13px] text-ink-2">
          {t('account.subscription.plans.subtitle')}
        </p>

        {plansQuery.isPending ? (
          <div
            role="status"
            aria-label={t('account.subscription.plans.loading')}
            aria-busy="true"
            className="mt-4 grid gap-3 md:grid-cols-2 xl:grid-cols-3"
          >
            <div aria-hidden="true" className="h-40 animate-pulse rounded-xl bg-surface-subtle" />
            <div aria-hidden="true" className="h-40 animate-pulse rounded-xl bg-surface-subtle" />
            <div aria-hidden="true" className="h-40 animate-pulse rounded-xl bg-surface-subtle" />
          </div>
        ) : plansQuery.isError ? (
          <div className="mt-4 flex items-center gap-3 text-sm text-ink-2">
            <span>{t('account.subscription.plans.error')}</span>
            <Button variant="secondary" onClick={() => void plansQuery.refetch()}>
              {t('account.subscription.plans.retry')}
            </Button>
          </div>
        ) : (plansQuery.data?.items.length ?? 0) === 0 ? (
          <p className="mt-4 text-sm text-ink-2">
            {t('account.subscription.plans.empty')}
          </p>
        ) : (
          <div className="mt-4 grid gap-3 md:grid-cols-2 xl:grid-cols-3">
            {plansQuery.data?.items.map((plan) => (
              <PlanCard
                key={plan.id}
                plan={plan}
                disabled={offline}
                onSubscribe={setPurchasePlan}
              />
            ))}
          </div>
        )}
      </section>

      <MySubscriptions
        data={mineQuery.data}
        isPending={mineQuery.isPending}
        isError={mineQuery.isError}
        onRetry={() => void mineQuery.refetch()}
        offline={offline}
      />

      <BillingPreferenceCard query={preferenceQuery} offline={offline} />

      <SubscriptionPurchaseDialog
        plan={purchasePlan}
        onOpenChange={(open) => {
          if (!open) setPurchasePlan(null)
        }}
        onPurchased={() => {
          // The server created an instance; re-read instead of inserting it
          // locally so the quota/status shown are the stored truth.
          void queryClient.invalidateQueries({ queryKey: MY_SUBSCRIPTIONS_KEY })
        }}
      />
    </div>
  )
}
