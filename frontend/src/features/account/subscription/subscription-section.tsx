import { useState } from 'react'
import { useQuery, useQueryClient } from '@tanstack/react-query'

import { apiClient } from '@/api/client'
import { requireData } from '@/api/result'
import { useI18n } from '@/shared/lib/i18n-context'
import { Button } from '@/shared/ui/button'

import { BillingPreferenceCard, BILLING_PREFERENCE_KEY } from './billing-preference'
import { MY_SUBSCRIPTIONS_KEY, MySubscriptions } from './my-subscriptions'
import { PAYMENT_METHODS_KEY, PaymentMethodsSection } from './payment-methods'
import { PlanCard } from './plan-card'
import { SubscriptionPurchaseDialog } from './purchase-dialog'
import { useOnlineStatus } from './use-online-status'
import type { AvailableSubscriptionPlan } from './types'

export const PLANS_KEY = ['subscription', 'plans'] as const

/**
 * /wallet 的订阅区（billing-08 §16.3 F-H-03/04）。
 *
 * Each block owns its loading/error state so a failing preference read cannot
 * blank the plan grid. The six-state contract maps as:
 *   - loading: skeleton rows (`aria-busy`);
 *   - empty: the *whole* subscription area is omitted when no plan is enabled
 *     (billing-08 §16.3 / modules-04:623, 裁决 A3a-M2), not rendered as an
 *     empty state — there is nothing a user could act on;
 *   - error: inline message + Retry;
 *   - 429: rendered inside the purchase dialog next to the failing action;
 *   - offline: `navigator.onLine` disables every write (subscribe, cancel,
 *     preference) and shows a notice; reads stay available;
 *   - no-permission: not applicable on this surface — all four endpoints are
 *     scoped to `Principal.UserID` and require only a signed-in user, so a 403
 *     is surfaced as an ordinary load error rather than a fake auth state.
 *
 * Plan-list errors still render the section: "we could not read the catalog"
 * is actionable (Retry), unlike "the catalog is empty". My Subscriptions and
 * the billing preference live inside the same area, so they disappear too when
 * the area is omitted; the no-plans rule is about the subscription product as
 * a whole, not just the plan grid.
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

  // S4：自动续费开关需要已保存的令牌列表；与令牌区共用同一份查询缓存，
  // 删除/新增后只需失效 PAYMENT_METHODS_KEY。
  const methodsQuery = useQuery({
    queryKey: PAYMENT_METHODS_KEY,
    queryFn: async () =>
      requireData(await apiClient.GET('/api/payment-methods')),
  })

  const preferenceQuery = useQuery({
    queryKey: BILLING_PREFERENCE_KEY,
    queryFn: async () =>
      requireData(await apiClient.GET('/api/billing-preference')),
  })

  // 裁决 A3a-M2: no enabled plan means the subscription product is not offered.
  // Render nothing at all — the empty-state copy in the dictionaries is kept
  // for parity but intentionally unused here. Only a *successful* empty read
  // hides the area; a failed read keeps the error + Retry visible.
  if (plansQuery.isSuccess && (plansQuery.data?.items.length ?? 0) === 0) {
    return null
  }

  return (
    <div className="mx-auto flex w-full max-w-[1520px] flex-col gap-4">
      {offline ? (
        <p
          role="status"
          className="rounded-lg border border-warning-line bg-warning-bg px-3 py-2 text-body text-warning"
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
          className="text-card-title text-ink-1"
        >
          {t('account.subscription.plans.title')}
        </h2>
        <p className="mt-1 text-meta text-ink-2">
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
          <div className="mt-4 flex items-center gap-3 text-body text-ink-2">
            <span>{t('account.subscription.plans.error')}</span>
            <Button variant="secondary" onClick={() => void plansQuery.refetch()}>
              {t('account.subscription.plans.retry')}
            </Button>
          </div>
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

      <PaymentMethodsSection offline={offline} />

      <MySubscriptions
        data={mineQuery.data}
        isPending={mineQuery.isPending}
        isError={mineQuery.isError}
        onRetry={() => void mineQuery.refetch()}
        offline={offline}
        paymentMethods={methodsQuery.data?.items ?? []}
        plans={plansQuery.data?.items ?? []}
      />

      <BillingPreferenceCard query={preferenceQuery} offline={offline} />

      <SubscriptionPurchaseDialog
        plan={purchasePlan}
        offline={offline}
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
