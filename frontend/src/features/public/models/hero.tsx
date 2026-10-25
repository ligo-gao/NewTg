/**
 * 模型库页头：标题、介绍与五项目录统计，外加能力条码。
 *
 * 统计必须来自**完整目录**（`entries`），不是当前筛选结果；能力值是目录里
 * 出现过的全部能力去重数，不能用 `slots` 相加——条码只有前 12 个槽位，
 * 拿它计数会悄悄漏掉长尾能力。最快 P50 只在有读数的模型里取最小，没有读数
 * 时如实显示 `—`，副标题给出该模型的展示名。
 */

import { useI18n } from '@/shared/lib/i18n-context'

import { formatMs } from './format'
import type { ModelEntry } from './performance'
import type { CapabilitySlot } from './taxonomy'

export interface ModelHeroProps {
  entries: readonly ModelEntry[]
  slots: readonly CapabilitySlot[]
  activeCapability: string
  onPickCapability: (value: string) => void
}

interface Stat {
  label: string
  value: string
  /** 最快 P50 的模型名；其它统计没有副标题。 */
  note?: string
}

export function ModelHero({
  entries,
  slots,
  activeCapability,
  onPickCapability,
}: ModelHeroProps) {
  const { t, locale } = useI18n()
  const numberFormat = new Intl.NumberFormat(locale)

  const vendors = new Set<string>()
  const capabilities = new Set<string>()
  let measured = 0
  let fastestEntry: ModelEntry | null = null
  let fastestP50: number | null = null

  for (const entry of entries) {
    const vendor = entry.card.vendor.trim().toLowerCase()
    if (vendor !== '') vendors.add(vendor)

    if (entry.perf.hasData) measured += 1

    for (const raw of entry.card.capabilities ?? []) {
      if (typeof raw !== 'string') continue
      const value = raw.trim().toLowerCase()
      if (value !== '') capabilities.add(value)
    }

    const p50 = entry.perf.p50
    if (
      p50 !== null &&
      Number.isFinite(p50) &&
      p50 >= 0 &&
      (fastestP50 === null || p50 < fastestP50)
    ) {
      fastestP50 = p50
      fastestEntry = entry
    }
  }

  const stats: Stat[] = [
    {
      label: t('public.models.stat.models'),
      value: numberFormat.format(entries.length),
    },
    {
      label: t('public.models.stat.vendors'),
      value: numberFormat.format(vendors.size),
    },
    {
      label: t('public.models.stat.measured'),
      value: numberFormat.format(measured),
    },
    {
      label: t('public.models.stat.capabilities'),
      value: numberFormat.format(capabilities.size),
    },
    {
      label: t('public.models.stat.fastest'),
      value: formatMs(fastestP50),
      note: fastestEntry?.card.display_name,
    },
  ]

  return (
    <section className="flex flex-col gap-5">
      <div className="max-w-3xl">
        <h1 className="text-public-title text-ink-1">
          {t('public.models.title')}
        </h1>
        <p className="mt-2 text-body text-ink-2">{t('public.models.intro')}</p>
      </div>

      <dl className="grid grid-cols-2 gap-px overflow-hidden rounded-xl border border-border-hairline bg-border-hairline sm:grid-cols-3 lg:grid-cols-5">
        {stats.map((stat) => (
          <div key={stat.label} className="bg-surface-card px-4 py-3">
            <dt className="text-meta leading-5 text-ink-3">{stat.label}</dt>
            <dd className="mt-0.5 text-stat tabular-nums text-ink-1">
              {stat.value}
              {stat.note === undefined ? null : (
                <span className="mt-0.5 block truncate text-meta font-normal leading-5 text-ink-3">
                  {stat.note}
                </span>
              )}
            </dd>
          </div>
        ))}
      </dl>

      <div
        role="group"
        aria-label={t('public.models.stat.capabilities')}
        className="flex flex-wrap gap-2"
      >
        {slots.map((slot) => {
          const active = activeCapability === slot.value
          return (
            <button
              key={slot.value}
              type="button"
              aria-pressed={active}
              onClick={() => onPickCapability(slot.value)}
              className={[
                'inline-flex h-8 items-center rounded-full border px-3 text-meta font-medium leading-5 transition-colors duration-150 motion-reduce:transition-none',
                active
                  ? 'border-brand-line bg-brand-subtle text-brand'
                  : 'border-border-default bg-surface-card text-ink-2 hover:bg-surface-hover hover:text-ink-1',
                'focus-visible:shadow-[var(--focus-ring)] focus-visible:outline-none',
              ].join(' ')}
            >
              {slot.label}
            </button>
          )
        })}
      </div>
    </section>
  )
}
