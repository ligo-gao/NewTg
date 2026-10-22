import { ApiError } from '@/api/errors'

/**
 * 订阅写操作的统一内联错误描述（key + 占位符）。
 *
 * 映射的是后端 `handlers_subscriptions.go` 的冻结错误口径：
 *   - 续费：404（他人/不存在）、400/422（字段非法）、409（同周期在途 /
 *     不可续费 / 幂等键复用）、502/503（渠道）、429（限流）；
 *   - 升降级：额外区分 422 `subscription_cycle_mismatch`（跨币种/跨周期）；
 *   - 退款申请：422 `subscription_in_use`（已消耗/过期/非 active）与
 *     `refund_requires_manual_review`（续费/升降级订单只能人工退）。
 *
 * 这里只读 `kind`/`status`/`title`/`retry_after`，不解析 `detail` 文本——
 * 服务端的英文 detail 是诊断信息，不是给用户看的文案（i18n 必须走词典）。
 */
export interface SubscriptionInlineError {
  key: string
  values?: Record<string, string | number>
}

/** 429 的 Retry-After 缺失时回落到文档冻结的 60s，而不是展示 0s。 */
function rateLimited(caught: ApiError): SubscriptionInlineError {
  return {
    key: 'account.subscription.common.rate_limited',
    values: { seconds: caught.retry_after ?? 60 },
  }
}

function networkOrServer(
  caught: unknown,
): SubscriptionInlineError | null {
  if (!(caught instanceof ApiError)) return null
  if (caught.kind === 'offline') {
    return { key: 'account.subscription.common.offline' }
  }
  if (caught.kind === 'rate_limited') return rateLimited(caught)
  // 502（渠道出站失败）与 503（未配置/限流器不可用）按契约都是可重试的
  // 暂时性错误，共用一句「稍后再试」；不要把它们说成「请求非法」。
  if (caught.status === 502 || caught.status === 503) {
    return { key: 'account.subscription.common.unavailable' }
  }
  return null
}

export function renewalError(caught: unknown): SubscriptionInlineError {
  const generic = networkOrServer(caught)
  if (generic !== null) return generic
  if (caught instanceof ApiError) {
    // 续费不走钱包（契约 provider ∈ stripe/alipay/epay）。402 在这里只表示
    // 「必须在收银台完成支付」，不能误写成钱包余额不足。
    if (caught.kind === 'quota') {
      return { key: 'account.subscription.renew.error_payment_required' }
    }
    if (caught.kind === 'not_found') return { key: 'account.subscription.common.not_found' }
    if (caught.kind === 'conflict') return { key: 'account.subscription.renew.error_conflict' }
    if (caught.kind === 'validation') return { key: 'account.subscription.renew.error_invalid' }
  }
  return { key: 'account.subscription.renew.error_failed' }
}

export function changeError(caught: unknown): SubscriptionInlineError {
  const generic = networkOrServer(caught)
  if (generic !== null) return generic
  if (caught instanceof ApiError) {
    if (caught.kind === 'not_found') return { key: 'account.subscription.common.not_found' }
    if (caught.kind === 'conflict') return { key: 'account.subscription.change.error_conflict' }
    if (caught.kind === 'validation') {
      // 后端把跨周期/跨币种单独映射为 422 subscription_cycle_mismatch
      // （writeChangeError）；这是唯一能给出可操作提示的 422 分支。
      if (caught.title === 'subscription_cycle_mismatch') {
        return { key: 'account.subscription.change.error_cycle_mismatch' }
      }
      return { key: 'account.subscription.change.error_invalid' }
    }
  }
  return { key: 'account.subscription.change.error_failed' }
}

export function refundError(caught: unknown): SubscriptionInlineError {
  const generic = networkOrServer(caught)
  if (generic !== null) return generic
  if (caught instanceof ApiError) {
    if (caught.kind === 'not_found') return { key: 'account.subscription.common.not_found' }
    if (caught.kind === 'conflict') return { key: 'account.subscription.refund.error_conflict' }
    if (caught.kind === 'validation') {
      // 两个 422 的 title 是冻结的机器码（handlers_subscriptions.go），
      // 直接按 title 分支比匹配英文 detail 稳定。
      if (caught.title === 'subscription_in_use') {
        return { key: 'account.subscription.refund.error_in_use' }
      }
      if (caught.title === 'refund_requires_manual_review') {
        return { key: 'account.subscription.refund.error_manual' }
      }
      return { key: 'account.subscription.refund.error_invalid_reason' }
    }
  }
  return { key: 'account.subscription.refund.error_failed' }
}
