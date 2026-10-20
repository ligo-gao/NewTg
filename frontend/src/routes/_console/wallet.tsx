import { createFileRoute } from '@tanstack/react-router'

import { loadRouteDictionary } from '@/app/providers/i18n'
import { SubscriptionSection } from '@/features/account/subscription/subscription-section'
import { TopupPage } from '@/features/account/topup/topup-page'

export const Route = createFileRoute('/_console/wallet')({
  loader: () => loadRouteDictionary('account'),
  component: WalletPage,
})

/**
 * `/wallet` = recharge stack + subscriptions (billing-08 §16.3).
 *
 * The subscription block lives beside `TopupPage` rather than inside it so the
 * recharge page (and its tests) keep their existing data contract: rendering
 * three extra queries there would make every top-up mount depend on the
 * subscription endpoints.
 */
function WalletPage() {
  return (
    <div className="flex flex-col gap-4">
      <TopupPage />
      <SubscriptionSection />
    </div>
  )
}
