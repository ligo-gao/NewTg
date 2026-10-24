import { useId, useState, type ReactNode } from 'react'

import { useI18n } from '@/shared/lib/i18n-context'
import { formatCredits } from '@/shared/lib/format'
import { handleRadioGroupKeyDown } from '@/shared/lib/roving-focus'
import { StatusBadge, type StatusBadgeVariant } from '@/shared/ui/status-badge'

import { formatYuan } from './money'

export interface BalanceCardProps {
  /** Integer credits, or `null` until a server response supplies a balance. */
  balance: number | null
  /**
   * Credits per yuan. Frozen at 100 by modules-04; exposed so the card stays a
   * pure projection if the pricing configuration ever changes.
   */
  quotaPerUnit?: number
  /**
   * Overview variant: when set, the card becomes the wallet hero — health
   * badge, freeze/threshold context and caller-supplied actions. `empty` is a
   * presentation-only value the overview derives (zero balance, no ledger
   * rows); the server never returns it.
   */
  health?: 'healthy' | 'low' | 'depleted' | 'empty'
  heldCredits?: number
  threshold?: number
  actions?: ReactNode
}

type BalanceUnit = 'credits' | 'yuan'

const UNIT_OPTIONS: readonly BalanceUnit[] = ['credits', 'yuan']

const HEALTH_VARIANTS: Record<
  NonNullable<BalanceCardProps['health']>,
  StatusBadgeVariant
> = {
  healthy: 'success',
  low: 'warning',
  depleted: 'danger',
  // "No balance yet" is informational, not an alarm: a brand-new account has
  // simply not topped up, so it must not read as a red depleted state.
  empty: 'neutral',
}

/**
 * Wallet balance card with the credits/元 toggle (task brief: 100 credits = 1
 * yuan). The value is always an integer from the server; the yuan view is a
 * pure presentation conversion (`credits` is cents at 100 credits = 1 yuan).
 *
 * The optional health/held/threshold props turn it into the wallet overview's
 * hero without changing the top-up tab's plain card.
 */
export function BalanceCard({
  balance,
  quotaPerUnit = 100,
  health,
  heldCredits,
  threshold,
  actions,
}: BalanceCardProps) {
  const { t, locale } = useI18n()
  const [unit, setUnit] = useState<BalanceUnit>('credits')
  const groupId = useId()

  const value = (() => {
    if (balance === null) return '—'
    if (unit === 'yuan') return formatYuan((balance * 100) / quotaPerUnit, { locale })
    return formatCredits(balance, locale)
  })()

  const hero = health !== undefined

  return (
    <section
      className={[
        'rounded-xl p-5',
        hero
          ? 'border border-brand-line bg-[linear-gradient(135deg,var(--brand-subtle),var(--surface-card)_72%)]'
          : 'border border-border-hairline bg-surface-card',
      ].join(' ')}
    >
      <div className="flex items-start justify-between gap-4">
        <h2 className="text-meta font-medium text-ink-2">
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
                  'h-6 rounded-md px-2 text-badge transition-colors focus-visible:shadow-[var(--focus-ring)] focus-visible:outline-none',
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
        className={[
          'mt-2 tabular-nums',
          hero
            ? 'text-figure-lg leading-[42px] tracking-[-0.02em]'
            : 'text-figure-md leading-9 tracking-[-0.01em]',
        ].join(' ')}
      >
        {value}
      </p>

      {health !== undefined ? (
        <div className="mt-1.5 flex flex-wrap items-center gap-2 text-meta text-ink-2">
          <StatusBadge variant={HEALTH_VARIANTS[health]}>
            {t(`account.wallet.overview.health_${health}`)}
          </StatusBadge>
          <span>
            {health === 'empty'
              ? t('account.wallet.overview.health_empty_hint')
              : t('account.wallet.overview.held_hint', {
                  held: formatCredits(heldCredits ?? 0, locale),
                  threshold: formatCredits(threshold ?? 0, locale),
                })}
          </span>
        </div>
      ) : null}

      {actions !== undefined ? (
        <div className="mt-3.5 flex flex-wrap gap-2">{actions}</div>
      ) : null}
    </section>
  )
}
