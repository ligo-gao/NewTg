import { getCsrfToken, toApiError } from '@/api/client'

/**
 * 周期末取消（billing-08 §3.5 P1）。
 *
 * 【为什么用原生 fetch 而不是生成的客户端】冻结契约（api/openapi、由此生成的
 * `schema.d.ts`）目前只有 `GET /api/subscriptions`，没有 `POST
 * /api/subscriptions/{id}/cancel`；`apiClient.POST` 在类型层面无法表达该路径。
 * 本任务禁止修改契约与生成物，因此这里按 §15 的端点定义直接调用，并完整复刻
 * `contractMiddleware` 的写请求约定：同源凭据、CSRF 头、Idempotency-Key。
 * 契约补齐后应改回 `apiClient`，届时本文件只剩一行调用（登记见交付报告）。
 *
 * `Idempotency-Key` 每次点击生成一个新值：取消是幂等置位，重复点击不会产生
 * 第二份副作用；但网络失败后的自动重试仍复用同一个键（调用方未自动重试，
 * 用户手动重试即视为新的意图）。
 */
export async function cancelSubscriptionAtPeriodEnd(
  id: number,
): Promise<void> {
  const origin = typeof location !== 'undefined' ? location.origin : ''
  const csrfToken = getCsrfToken()
  const response = await fetch(`${origin}/api/subscriptions/${id}/cancel`, {
    method: 'POST',
    credentials: 'same-origin',
    headers: {
      Accept: 'application/json',
      'Idempotency-Key': globalThis.crypto.randomUUID(),
      ...(csrfToken === undefined ? {} : { 'X-CSRF-Token': csrfToken }),
    },
  })
  if (!response.ok) throw await toApiError(response)
}
