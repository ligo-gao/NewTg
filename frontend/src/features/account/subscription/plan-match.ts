import type { AvailableSubscriptionPlan, UserSubscription } from './types'

/**
 * 用户侧订阅投影不含 plan_id/价格（billing-08 §15.4 的脱敏口径），只有
 * `plan_title_snapshot` 与 `period_days`。续费金额预览与升降级「当前套餐」
 * 判定只能按这两个快照字段回查可用套餐。
 *
 * 这是**展示用**的尽力匹配：套餐改名/停用后查不到时调用方必须退回
 * 「金额以收银台为准」，绝不能凭猜测显示一个价格。服务端始终按实例当前
 * 套餐行重新定价，客户端预览永远不是金额来源。
 */
export function findMatchingPlan(
  subscription: UserSubscription,
  plans: readonly AvailableSubscriptionPlan[],
): AvailableSubscriptionPlan | undefined {
  return plans.find(
    (plan) =>
      plan.title === subscription.plan_title_snapshot &&
      plan.period_days === subscription.period_days,
  )
}
