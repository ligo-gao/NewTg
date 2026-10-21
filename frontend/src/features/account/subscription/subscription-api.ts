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

/** 自动续费开关（S4）。服务端校验令牌归属与可用性；失败不改本地状态。 */
export async function setSubscriptionAutoRenew(
  id: number,
  enabled: boolean,
  paymentMethodId?: string,
) {
  return requireData(
    await apiClient.PUT('/api/subscriptions/{id}/auto-renew', {
      params: { path: { id } },
      body: {
        enabled,
        // 关闭时不需要令牌；显式省略而不是发空串，避免把空 uuid 当选择。
        ...(paymentMethodId !== undefined
          ? { payment_method_id: paymentMethodId }
          : {}),
      },
    }),
  )
}

/** 支付令牌列表（零回显：只有 brand/last4/exp）。 */
export async function listPaymentMethods() {
  return requireData(await apiClient.GET('/api/payment-methods'))
}

/** 保存 Stripe PaymentMethod id（由 Stripe.js 在浏览器端创建）。 */
export async function createPaymentMethod(token: string) {
  return requireData(
    await apiClient.POST('/api/payment-methods', {
      body: { provider: 'stripe', token },
    }),
  )
}

/** 删除令牌：服务端在同一事务里 fail-closed 关闭关联自动续费（R-42）。 */
export async function deletePaymentMethod(id: string): Promise<void> {
  await apiClient.DELETE('/api/payment-methods/{id}', {
    params: { path: { id } },
  })
}
