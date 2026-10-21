import { apiClient } from '@/api/client'
import { requireData } from '@/api/result'

import type {
  CreateOrderResponse,
  SubscriptionChangeResponse,
  SubscriptionChannelProvider,
  SubscriptionEventList,
  SubscriptionRefundRequestView,
} from './types'

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

/**
 * 手动续费（P2a）。金额/周期/额度由服务端按实例当前套餐读取，客户端只发
 * provider —— 携带任何金额字段都会被 RBAC 字段层 403。
 *
 * `Idempotency-Key` 由调用方按「一次续费意图」铸造并跨重试复用：服务端以它
 * 区分「重放首次订单（200）」与「同周期第二笔（409）」。绝不能每次点击都换
 * 新键，否则网络失败后的重试会变成两笔订单。
 */
export async function renewSubscription(
  id: number,
  provider: SubscriptionChannelProvider,
  idempotencyKey: string,
): Promise<CreateOrderResponse> {
  return requireData(
    await apiClient.POST('/api/subscriptions/{id}/renew', {
      params: {
        path: { id },
        header: { 'Idempotency-Key': idempotencyKey },
      },
      body: { provider },
    }),
  )
}

/**
 * 升级/降级（S2）。`plan_id` 是目标套餐，差价由服务端按剩余价值折算；
 * 0 元路径（降级 / 低于渠道最小额免收）返回无支付载荷的 succeeded 订单。
 * 与续费同一幂等口径：同键重放返回首次结果，改口到另一套餐 → 409。
 */
export async function changeSubscriptionPlan(
  id: number,
  planId: number,
  provider: SubscriptionChannelProvider,
  idempotencyKey: string,
): Promise<SubscriptionChangeResponse> {
  return requireData(
    await apiClient.POST('/api/subscriptions/{id}/change', {
      params: {
        path: { id },
        header: { 'Idempotency-Key': idempotencyKey },
      },
      body: { plan_id: planId, provider },
    }),
  )
}

/**
 * 退款申请（S3）：只写订单的 `refund_requested_at` / `refund_reason`，
 * 不调用渠道、不动钱包；实际退款由管理员审批。重复申请幂等返回首次标记。
 */
export async function requestSubscriptionRefund(
  id: number,
  reason: string,
): Promise<SubscriptionRefundRequestView> {
  return requireData(
    await apiClient.POST('/api/subscriptions/{id}/refund-request', {
      params: { path: { id } },
      body: { reason },
    }),
  )
}

/**
 * 订阅事件流（S1/R-19）。按 `seq > after_seq` 升序返回，单次最多 500 条；
 * 调用方用本页最大 seq 作为下一次游标。只读端点，没有写入语义。
 */
export async function listSubscriptionEvents(
  id: number,
  afterSeq = 0,
): Promise<SubscriptionEventList> {
  return requireData(
    await apiClient.GET('/api/subscriptions/{id}/events', {
      params: { path: { id }, query: { after_seq: afterSeq } },
    }),
  )
}
