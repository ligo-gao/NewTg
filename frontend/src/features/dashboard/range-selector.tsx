import { useI18n } from '@/shared/lib/i18n-context'
import { handleRadioGroupKeyDown } from '@/shared/lib/roving-focus'

import type { UsageRange } from './api'

const RANGES: readonly UsageRange[] = ['24h', '7d', '14d', '30d']

export interface RangeSelectorProps {
  value: UsageRange
  onChange: (range: UsageRange) => void
}

/**
 * Shared time-range control (R-M6-12). A radiogroup makes the current choice
 * visible to assistive technology without introducing a select's popup.
 * Range alone drives granularity: 24h → hourly, everything wider → daily.
 */
export function RangeSelector({ value, onChange }: RangeSelectorProps) {
  const { t } = useI18n()

  return (
    <div
      role="radiogroup"
      aria-label={t('common.dashboard.range.label')}
      // UI-5：组内仅一个 tab stop（选中项 0，其余 -1），方向键/Home/End
      // 由共享 roving-focus 处理器移动焦点并提交选择。
      onKeyDown={handleRadioGroupKeyDown}
      className="inline-flex rounded-lg border border-border-default bg-surface-card p-0.5"
    >
      {RANGES.map((range) => {
        const active = range === value
        return (
          <button
            key={range}
            type="button"
            role="radio"
            aria-checked={active}
            tabIndex={active ? 0 : -1}
            data-testid={`range-${range}`}
            onClick={() => onChange(range)}
            className={[
              'rounded-md px-2.5 py-1 text-[13px] focus-visible:shadow-[var(--focus-ring)] focus-visible:outline-none',
              active
                ? 'bg-surface-hover font-medium text-ink-1'
                : 'text-ink-2 hover:text-ink-1',
            ].join(' ')}
          >
            {t(`common.dashboard.range.${range}`)}
          </button>
        )
      })}
    </div>
  )
}
