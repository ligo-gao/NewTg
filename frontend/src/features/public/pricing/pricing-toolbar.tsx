/**
 * 定价工具栏（F-09-16 / F-09-17 / F-09-19）。
 *
 * 搜索 300ms 防抖后才写 URL；展示开关只写 URL 与本地格式化，
 * **不进入查询键**，因此切换 M/K、币种、充值价不会产生网络请求。
 */

import { useEffect, useRef, useState } from 'react'

import { SegmentedControl, SwitchControl } from './controls'
import { SEARCH_MAX_LENGTH, type PricingSearch, type PricingSort, type PricingView } from './params'
import { useI18n } from '@/shared/lib/i18n-context'

export interface PricingToolbarProps {
  value: PricingSearch
  onChange: (patch: Partial<PricingSearch>) => void
  /** 展示开关：不重置页码、不触发请求（F-09-19）。 */
  onDisplayChange: (patch: Partial<PricingSearch>) => void
  onViewChange: (view: PricingView) => void
  currencyAvailable: boolean
  /**
   * Operator publishes prices in CNY (`billing.display_unit=cny`), so the
   * currency estimate is mandatory and the switch is read-only. The URL
   * cannot override it (P1-3).
   */
  dualCurrencyForced?: boolean
  activeFilters: number
  onOpenFilters: () => void
}

function SearchInput({
  value,
  onCommit,
}: {
  value: string
  onCommit: (value: string) => void
}) {
  const { t } = useI18n()
  const [text, setText] = useState(value)
  const lastCommitted = useRef(value)
  const commitRef = useRef(onCommit)
  commitRef.current = onCommit

  // URL 被外部改变（清除筛选、浏览器后退）时同步输入框，但不能覆盖
  // 用户正在输入但尚未提交的文本。
  useEffect(() => {
    if (value !== lastCommitted.current) {
      lastCommitted.current = value
      setText(value)
    }
  }, [value])

  useEffect(() => {
    if (text === lastCommitted.current) return
    const timer = setTimeout(() => {
      lastCommitted.current = text
      commitRef.current(text)
    }, 300)
    return () => clearTimeout(timer)
  }, [text])

  return (
    <div className="relative w-full md:w-[320px]">
      <span
        aria-hidden="true"
        className="pointer-events-none absolute left-3 top-1/2 -translate-y-1/2 text-ink-3"
      >
        <svg viewBox="0 0 16 16" className="size-4" fill="none" stroke="currentColor" strokeWidth="1.5">
          <circle cx="7" cy="7" r="4.5" />
          <path d="m10.5 10.5 3 3" strokeLinecap="round" />
        </svg>
      </span>
      <input
        type="search"
        value={text}
        maxLength={SEARCH_MAX_LENGTH}
        onChange={(event) => setText(event.target.value.slice(0, SEARCH_MAX_LENGTH))}
        placeholder={t('public.pricing.search_placeholder')}
        aria-label={t('public.pricing.search_label')}
        className="h-10 w-full rounded-lg border border-border-default bg-surface-card pl-9 pr-9 text-sm text-ink-1 placeholder:text-ink-3 focus:border-brand focus-visible:shadow-[var(--focus-ring)] focus-visible:outline-none"
      />
      {text !== '' ? (
        <button
          type="button"
          aria-label={t('public.pricing.search_clear')}
          onClick={() => {
            setText('')
            lastCommitted.current = ''
            commitRef.current('')
          }}
          className="absolute right-2 top-1/2 inline-flex size-6 -translate-y-1/2 items-center justify-center rounded text-ink-3 hover:bg-surface-hover hover:text-ink-1 focus-visible:shadow-[var(--focus-ring)] focus-visible:outline-none"
        >
          <svg aria-hidden="true" viewBox="0 0 16 16" className="size-4" fill="none" stroke="currentColor" strokeWidth="1.5">
            <path d="m4 4 8 8M12 4l-8 8" strokeLinecap="round" />
          </svg>
        </button>
      ) : null}
    </div>
  )
}

export function PricingToolbar({
  value,
  onChange,
  onDisplayChange,
  onViewChange,
  currencyAvailable,
  dualCurrencyForced = false,
  activeFilters,
  onOpenFilters,
}: PricingToolbarProps) {
  const { t } = useI18n()

  const sortOptions: { value: PricingSort; label: string }[] = [
    { value: 'name', label: t('public.pricing.sort_name') },
    { value: 'price_asc', label: t('public.pricing.sort_price_asc') },
    { value: 'price_desc', label: t('public.pricing.sort_price_desc') },
  ]

  return (
    <div className="sticky top-0 z-20 flex flex-wrap items-center gap-2 border-b border-border-hairline bg-surface-card py-2">
      <SearchInput value={value.search} onCommit={(search) => onChange({ search })} />

      <label className="inline-flex items-center gap-1.5 text-[13px] text-ink-2">
        <span className="sr-only">{t('public.pricing.sort_label')}</span>
        <select
          value={value.sort}
          onChange={(event) => onChange({ sort: event.target.value as PricingSort })}
          aria-label={t('public.pricing.sort_label')}
          className="h-10 rounded-lg border border-border-default bg-surface-card px-2 text-[13px] text-ink-1 focus:border-brand focus-visible:shadow-[var(--focus-ring)] focus-visible:outline-none"
        >
          {sortOptions.map((option) => (
            <option key={option.value} value={option.value}>
              {option.label}
            </option>
          ))}
        </select>
      </label>

      <div className="ml-auto flex flex-wrap items-center gap-3">
        <button
          type="button"
          onClick={onOpenFilters}
          className="inline-flex h-9 items-center gap-1.5 rounded-lg border border-border-default bg-surface-card px-3 text-[13px] font-medium text-ink-1 hover:bg-surface-hover focus-visible:shadow-[var(--focus-ring)] focus-visible:outline-none xl:hidden"
        >
          <svg aria-hidden="true" viewBox="0 0 16 16" className="size-4" fill="none" stroke="currentColor" strokeWidth="1.5">
            <path d="M2 4h12M4 8h8M6 12h4" strokeLinecap="round" />
          </svg>
          {activeFilters > 0
            ? t('public.pricing.filter_button_count', { count: activeFilters })
            : t('public.pricing.filter_button')}
        </button>

        <SegmentedControl
          label={t('public.pricing.unit_label')}
          value={value.tokenUnit}
          options={[
            { value: 'M', label: t('public.pricing.unit_m') },
            { value: 'K', label: t('public.pricing.unit_k') },
          ]}
          onChange={(tokenUnit) => onDisplayChange({ tokenUnit })}
        />

        <SwitchControl
          label={t('public.pricing.dual_currency')}
          checked={value.dualCurrency}
          disabled={!currencyAvailable || dualCurrencyForced}
          disabledHint={
            dualCurrencyForced
              ? t('public.pricing.dual_currency_forced')
              : t('public.pricing.rate_unavailable')
          }
          onChange={(dualCurrency) => onDisplayChange({ dualCurrency })}
        >
          {t('public.pricing.dual_currency_short')}
        </SwitchControl>
        {dualCurrencyForced ? (
          // Visible explanation, not just a tooltip: the disabled switch
          // otherwise looks broken when a shared URL asked for it to be off.
          <span className="text-xs leading-5 text-ink-3">
            {t('public.pricing.dual_currency_forced')}
          </span>
        ) : null}

        <SwitchControl
          label={t('public.pricing.recharge_price')}
          checked={value.rechargePrice}
          disabled={!currencyAvailable}
          disabledHint={t('public.pricing.rate_unavailable')}
          onChange={(rechargePrice) => onDisplayChange({ rechargePrice })}
        >
          {t('public.pricing.recharge_price_short')}
        </SwitchControl>

        <SegmentedControl
          label={t('public.pricing.view_label')}
          value={value.view}
          options={[
            { value: 'card', label: t('public.pricing.view_card') },
            { value: 'table', label: t('public.pricing.view_table') },
          ]}
          onChange={onViewChange}
          className="hidden md:inline-flex"
        />
      </div>
    </div>
  )
}
