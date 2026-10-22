import type { UserSubscription } from './types'

/**
 * Refund eligibility (P-13 / R-35).
 *
 * The server is the authority (422 `subscription_in_use`), but the UI must not
 * offer a request that is guaranteed to fail: only an active instance with no
 * quota consumed and an unexpired `end_at` qualifies. Widening any one of
 * these three conditions would let users file doomed requests.
 *
 * `now` is injectable so tests can freeze the clock.
 */
export function isRefundEligible(
  subscription: UserSubscription,
  now: number = Date.now(),
): boolean {
  const endAt = Date.parse(subscription.end_at)
  return (
    subscription.status === 'active' &&
    subscription.amount_used === 0 &&
    !Number.isNaN(endAt) &&
    endAt > now
  )
}

/**
 * Why the refund action is unavailable, or `null` when it is available.
 *
 * Callers only render the action for `status === 'active'` instances, so this
 * helper describes the two reachable refusals (used / ended) and deliberately
 * has no "not active" branch — a cancelled row never offers the button.
 */
export function refundIneligibilityKey(
  subscription: UserSubscription,
  now: number = Date.now(),
): string | null {
  if (isRefundEligible(subscription, now)) return null
  if (subscription.amount_used > 0) {
    return 'account.subscription.mine.refund_ineligible_used'
  }
  return 'account.subscription.mine.refund_ineligible_expired'
}
