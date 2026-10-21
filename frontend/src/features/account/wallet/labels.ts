import type { TagVariant } from '@/shared/ui/tag'
import type { StatusBadgeVariant } from '@/shared/ui/status-badge'

import {
  WALLET_TRANSACTION_STATUSES,
  WALLET_TRANSACTION_TYPES,
} from './search'
import type {
  WalletTransactionStatus,
  WalletTransactionType,
} from './types'

/**
 * 流水类型的分类色（modules-03 UI §2.3）。Tag 表达「这是哪类变动」，状态
 * 由 StatusBadge 表达；两者不混用（modules-00 §4.6.4）。
 *
 * 设计只列了 7 类；余额购买订阅（P2b）与返利/签到/赠额沿用相邻语义：
 * 入账类带 success/info，购买带 brand，消费类的预扣用 warning（held 色系）。
 */
export const WALLET_TYPE_TAG_VARIANTS: Record<WalletTransactionType, TagVariant> = {
  topup: 'success',
  redemption: 'brand',
  consume_pre: 'warning',
  consume: 'neutral',
  release: 'success',
  reversal: 'reversal',
  admin_adjustment: 'admin',
  bonus: 'success',
  affiliate_transfer: 'info',
  checkin_bonus: 'success',
  subscription_purchase: 'brand',
}

/**
 * 状态徽章（modules-03 UI §2.5）：
 *   hold = 进行中（warning + 脉冲圆点由调用方加 motion-safe 动画）
 *   settled/released = 成功态；expired/confirmed = 中性。
 */
export const WALLET_STATUS_BADGE_VARIANTS: Record<
  WalletTransactionStatus,
  StatusBadgeVariant
> = {
  hold: 'warning',
  settled: 'success',
  released: 'success',
  expired: 'neutral',
  confirmed: 'neutral',
}

export function typeLabelKey(type: WalletTransactionType): string {
  return `account.wallet.type_${type}`
}

export function statusLabelKey(status: WalletTransactionStatus): string {
  return `account.wallet.status_${status}`
}

export function isWalletTransactionType(
  value: string,
): value is WalletTransactionType {
  return (WALLET_TRANSACTION_TYPES as readonly string[]).includes(value)
}

export function isWalletTransactionStatus(
  value: string,
): value is WalletTransactionStatus {
  return (WALLET_TRANSACTION_STATUSES as readonly string[]).includes(value)
}
