import { useI18n } from '@/shared/lib/i18n-context'

import { describeDiscount } from './format'

export interface DiscountLabelProps {
  /** Applied discount ratio 0..1 (`null` = no discount, `0` = full waiver). */
  rate: number | null | undefined
}

/**
 * The bill's 折扣 cell: the charged percentage (`90%`, `0%`, `100%`) or an em
 * dash when the log carries no discount ratio. Percentages are the only form
 * the product shows, so this component is locale-agnostic apart from `t`.
 */
export function DiscountLabel({ rate }: DiscountLabelProps) {
  const { t } = useI18n()
  const display = describeDiscount(rate)
  if (display.kind === 'none') return <span className="text-ink-3">—</span>
  return (
    <span>{t('logs.usage.discount.percent', { value: display.value })}</span>
  )
}
