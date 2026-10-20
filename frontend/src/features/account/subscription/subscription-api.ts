import { apiClient } from '@/api/client'
import { requireData } from '@/api/result'

/**
 * 周期末取消（billing-08 §3.5 P1）。
 *
 * The frozen contract carries `POST /api/subscriptions/{id}/cancel`, so the
 * generated client is the only write path: it supplies the shared write
 * conventions (same-origin cookie, CSRF header, `Idempotency-Key`) and
 * normalizes every non-2xx response into `ApiError`. The endpoint is a
 * conditional UPDATE and is idempotent — a repeated call still answers 200 —
 * so a per-click key is correct and the UI never auto-retries a cancel.
 */
export async function cancelSubscriptionAtPeriodEnd(id: number): Promise<void> {
  requireData(
    await apiClient.POST('/api/subscriptions/{id}/cancel', {
      params: { path: { id } },
    }),
  )
}
