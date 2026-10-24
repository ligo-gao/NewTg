/**
 * 表格视图（F-09-17）。与卡片消费同一份 `PublicModelCard`，不额外请求。
 * 数字列右对齐并启用 tabular-nums；模型/计费类型两列在窄屏横向滚动时
 * sticky，保证扫读时始终知道自己在看哪一行。
 */

import type { PublicModelCard } from './api'
import { convertCreditsPerK, formatCredit, formatRatio } from './format'
import { HighlightedText } from './highlight'
import { endpointLabel } from './metadata'
import type { PricingSort, PricingTokenUnit } from './params'
import { useI18n } from '@/shared/lib/i18n-context'
import { Tag } from '@/shared/ui/tag'

export interface PricingTableProps {
  items: readonly PublicModelCard[]
  tokenUnit: PricingTokenUnit
  search: string
  sort: PricingSort
}

function cellValue(
  raw: string | null,
  tokenUnit: PricingTokenUnit,
  locale: string,
): string {
  if (tokenUnit === 'K') return convertCreditsPerK(raw, locale) ?? '—'
  return formatCredit(raw, locale)
}

export function PricingTable({
  items,
  tokenUnit,
  search,
  sort,
}: PricingTableProps) {
  const { t, locale } = useI18n()
  const nameSort = sort === 'name' ? 'ascending' : 'none'
  const priceSort =
    sort === 'price_asc' ? 'ascending' : sort === 'price_desc' ? 'descending' : 'none'

  return (
    <div className="w-full overflow-x-auto rounded-xl border border-border-default bg-surface-card">
      <table className="w-full min-w-[1080px] border-collapse text-meta leading-5">
        <thead className="bg-surface-subtle text-left text-meta text-ink-2">
          <tr className="h-9">
            <th scope="col" aria-sort={nameSort} className="sticky left-0 z-10 bg-surface-subtle px-4 py-2">
              {t('public.pricing.table_model')}
            </th>
            <th scope="col" className="sticky left-[200px] z-10 bg-surface-subtle px-4 py-2">
              {t('public.pricing.table_mode')}
            </th>
            <th scope="col" aria-sort={priceSort} className="px-4 py-2 text-right">
              {t('public.pricing.price_input')}
            </th>
            <th scope="col" className="px-4 py-2 text-right">
              {t('public.pricing.price_output')}
            </th>
            <th scope="col" className="px-4 py-2 text-right">
              {t('public.pricing.price_cache')}
            </th>
            <th scope="col" className="px-4 py-2">{t('public.pricing.table_vendor')}</th>
            <th scope="col" className="px-4 py-2">{t('public.pricing.table_tags')}</th>
            <th scope="col" className="px-4 py-2">{t('public.pricing.table_endpoints')}</th>
            <th scope="col" className="px-4 py-2">{t('public.pricing.table_groups')}</th>
          </tr>
        </thead>
        <tbody>
          {items.map((card) => {
            const perRequest = card.mode === 'per_request'
            return (
              <tr key={card.model_id} className="h-11 border-t border-border-hairline hover:bg-surface-hover">
                <td className="sticky left-0 z-10 bg-surface-card px-4 py-2">
                  <a
                    href={`/pricing/${encodeURIComponent(card.model_id)}`}
                    className="inline-flex max-w-[180px] items-center truncate font-medium text-brand hover:underline focus-visible:shadow-[var(--focus-ring)] focus-visible:outline-none"
                    title={card.display_name === '' ? card.model_id : card.display_name}
                  >
                    <HighlightedText
                      text={card.display_name === '' ? card.model_id : card.display_name}
                      query={search}
                    />
                  </a>
                </td>
                <td className="sticky left-[200px] z-10 bg-surface-card px-4 py-2">
                  <Tag variant={perRequest ? 'neutral' : 'info'}>
                    {perRequest
                      ? t('public.pricing.mode_request')
                      : t('public.pricing.mode_token')}
                  </Tag>
                </td>
                <td className="px-4 py-2 text-right font-mono tabular-nums text-ink-1">
                  {cellValue(card.prompt_price, tokenUnit, locale)}
                </td>
                <td className="px-4 py-2 text-right font-mono tabular-nums text-ink-1">
                  {perRequest ? '—' : cellValue(card.completion_price, tokenUnit, locale)}
                </td>
                <td className="px-4 py-2 text-right font-mono tabular-nums text-ink-1">
                  {perRequest
                    ? '—'
                    : `${cellValue(card.cache_read_price, tokenUnit, locale)} / ${cellValue(
                        card.cache_write_price,
                        tokenUnit,
                        locale,
                      )}`}
                </td>
                <td className="px-4 py-2 text-ink-2">{card.vendor === '' ? '—' : card.vendor}</td>
                <td className="max-w-[140px] px-4 py-2 text-ink-2">
                  <span className="line-clamp-1" title={card.tags.join(', ')}>
                    {card.tags.length > 0 ? card.tags.join(', ') : '—'}
                  </span>
                </td>
                <td className="max-w-[160px] px-4 py-2 text-ink-2">
                  <span
                    className="line-clamp-1"
                    title={card.endpoints.map((entry) => endpointLabel(entry, t)).join(', ')}
                  >
                    {card.endpoints.length > 0
                      ? card.endpoints.map((entry) => endpointLabel(entry, t)).join(', ')
                      : '—'}
                  </span>
                </td>
                <td className="px-4 py-2 text-ink-2">
                  {card.group_price === null
                    ? '—'
                    : `${card.group_price.group} ${formatRatio(card.group_price.ratio) ?? ''}`.trim()}
                </td>
              </tr>
            )
          })}
        </tbody>
      </table>
    </div>
  )
}
