/**
 * 模型库页头：标题、介绍与四项目录统计。
 *
 * 统计必须来自**完整目录**（`entries`），不是当前筛选结果。最快首字 P50
 * 只在 `measure === 'ttft'` 且有读数的模型里取最小——端到端延迟不能冒充首字
 * 延迟；没有读数时如实显示 `—`，副标题是 `model_id`。输入价取全目录
 * 按 token 计价条目的 `price.input` 区间（按次价格不是每百万 token，不能
 * 混进同一区间），任一端缺失即 `—`，不把未知价格伪装成免费。
 *
 * 能力筛选只属于左侧筛选栏：页头不再重复渲染 chip 行。
 */

import { useI18n } from '@/shared/lib/i18n-context'

import { formatMs } from './format'
import type { ModelEntry } from './performance'
import { formatRmbRange } from './price'

export interface ModelHeroProps {
  entries: readonly ModelEntry[]
}

interface Stat {
  label: string
  value: string
  /** 最快首字的模型 id；其它统计没有副标题。 */
  note?: string
}

export function ModelHero({ entries }: ModelHeroProps) {
  const { t, locale } = useI18n()
  const numberFormat = new Intl.NumberFormat(locale)

  let measured = 0
  let fastestEntry: ModelEntry | null = null
  let fastestP50: number | null = null
  let minInputPrice: number | null = null
  let maxInputPrice: number | null = null

  for (const entry of entries) {
    if (entry.perf.hasData) measured += 1

    if (entry.perf.measure === 'ttft') {
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

    // 输入价区间只统计「按 token 且输入价已知」的条目：按次计价的价格是
    // 「元 / 次」，混进「元 / 百万 token」的区间会把两种单位相加；输入价缺
    // 失的条目也不参与，它既不是 ¥0 也不是免费，放进去只会把下沿拉到零。
    const input = entry.price.perCall ? null : entry.price.input
    if (input !== null && Number.isFinite(input)) {
      if (minInputPrice === null || input < minInputPrice) minInputPrice = input
      if (maxInputPrice === null || input > maxInputPrice) maxInputPrice = input
    }
  }

  const stats: Stat[] = [
    {
      label: t('public.models.stat.models'),
      value: numberFormat.format(entries.length),
    },
    {
      label: t('public.models.stat.measured'),
      value: numberFormat.format(measured),
    },
    {
      label: t('public.models.stat.fastest'),
      value: formatMs(fastestP50, locale),
      note: fastestEntry?.card.model_id,
    },
    {
      label: t('public.models.stat.input_price'),
      value: formatRmbRange(minInputPrice, maxInputPrice, locale) ?? '—',
    },
  ]

  return (
    <section className="flex flex-col">
      <div className="max-w-3xl">
        <h1 className="text-public-title text-ink-1">
          {t('public.models.title')}
        </h1>
        <p className="mt-2 max-w-[40em] text-public-body text-ink-2">
          {t('public.models.intro')}
        </p>
      </div>

      <dl className="public-page-body grid grid-cols-2 gap-px overflow-hidden rounded-xl border border-border-hairline bg-border-hairline sm:grid-cols-4">
        {stats.map((stat) => (
          <div key={stat.label} className="bg-surface-card px-4 py-3">
            <dt className="text-public-caption text-ink-3">{stat.label}</dt>
            <dd className="mt-0.5 text-public-figure tabular-nums text-ink-1">
              {stat.value}
              {stat.note === undefined ? null : (
                <span className="mt-0.5 block truncate text-public-caption font-normal text-ink-3">
                  {stat.note}
                </span>
              )}
            </dd>
          </div>
        ))}
      </dl>
    </section>
  )
}
