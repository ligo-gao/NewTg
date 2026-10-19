import { useId, useState } from 'react'

import { useI18n } from '@/shared/lib/i18n-context'
import { formatCredits } from '@/shared/lib/format'
import { handleRadioGroupKeyDown } from '@/shared/lib/roving-focus'

import { formatYuan } from './money'

export interface BalanceCardProps {
  /** Integer credits, or `null` until a server response supplies a balance. */
  balance: number | null
  /**
   * Credits per yuan. Frozen at 100 by modules-04; exposed so the card stays a
   * pure projection if the pricing configuration ever changes.
   */
  quotaPerUnit?: number
}

type BalanceUnit = 'credits' | 'yuan'

const UNIT_OPTIONS: readonly BalanceUnit[] = ['credits', 'yuan']

/**
 * Wallet balance card with the credits/元 toggle (task brief: 100 credits = 1
 * yuan). The value is always an integer from the server; the yuan view is a
 * pure presentation conversion (`credits` is cents at 100 credits = 1 yuan).
 */
export function BalanceCard({ balance, quotaPerUnit = 100 }: BalanceCardProps) {
  const { t } = useI18n()
  const [unit, setUnit] = useState<BalanceUnit>('credits')
  const groupId = useId()

  const value = (() => {
    if (balance === null) return '—'
    if (unit === 'yuan') return formatYuan((balance * 100) / quotaPerUnit)
    return formatCredits(balance)
  })()

  return (
    <section className="rounded-xl border border-border-hairline bg-surface-card p-5">
      <div className="flex items-start justify-between gap-4">
        <h2 className="text-[13px] font-medium text-ink-2">
          {t('account.topup.balance_title')}
        </h2>
        <div
          role="radiogroup"
          aria-label={t('account.topup.balance_unit_label')}
          // UI-5：单位切换是单选组，键盘用户用方向键即可切换，且组内
          // 只有一个 tab stop（选中项 0、另一项 -1）。
          onKeyDown={handleRadioGroupKeyDown}
          className="flex rounded-lg border border-border-default p-0.5"
        >
          {UNIT_OPTIONS.map((option) => {
            const selected = unit === option
            return (
              <button
                key={option}
                type="button"
                role="radio"
                aria-checked={selected}
                tabIndex={selected ? 0 : -1}
                id={`${groupId}-${option}`}
                onClick={() => {
                  setUnit(option)
                }}
                className={[
                  'h-6 rounded-md px-2 text-xs font-medium transition-colors focus-visible:shadow-[var(--focus-ring)] focus-visible:outline-none',
                  selected
                    ? 'bg-brand-subtle text-brand'
                    : 'text-ink-2 hover:text-ink-1',
                ].join(' ')}
              >
                {t(
                  option === 'credits'
                    ? 'account.topup.balance_credits'
                    : 'account.topup.balance_yuan',
                )}
              </button>
            )
          })}
        </div>
      </div>
      <p
        data-testid="balance-value"
        className="mt-2 text-[28px] font-semibold leading-9 tracking-[-0.01em] tabular-nums"
      >
        {value}
      </p>
    </section>
  )
}
