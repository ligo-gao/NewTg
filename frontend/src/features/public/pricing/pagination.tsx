/**
 * offset 分页（F-09-21）。不提供无限滚动/游标加载：`p` 与 `page_size`
 * 都入 URL，刷新与分享可复现；翻页数据由查询层的 `keepPreviousData`
 * 承接，按钮 loading 时不整表塌成骨架。
 */

import { formatTotal } from './format'
import { PAGE_SIZE_OPTIONS, type PricingView } from './params'
import { useI18n } from '@/shared/lib/i18n-context'

export interface PaginationProps {
  page: number
  pageSize: number
  total: string
  view: PricingView
  pending: boolean
  onPageChange: (page: number) => void
  onPageSizeChange: (size: number) => void
}

export function Pagination({
  page,
  pageSize,
  total,
  view,
  pending,
  onPageChange,
  onPageSizeChange,
}: PaginationProps) {
  const { t, locale } = useI18n()
  const totalCount = (() => {
    try {
      return BigInt(total)
    } catch {
      return 0n
    }
  })()
  const size = BigInt(pageSize)
  // 页数也留在 BigInt 域：int64 total 超过 2^53 时 Number() 会先丢精度。
  const lastPageNumber =
    totalCount === 0n ? 1n : (totalCount + size - 1n) / size
  const options = PAGE_SIZE_OPTIONS[view].includes(pageSize)
    ? PAGE_SIZE_OPTIONS[view]
    : [...PAGE_SIZE_OPTIONS[view], pageSize].sort((a, b) => a - b)

  return (
    <nav
      aria-label={t('public.pricing.pagination_label')}
      className="flex flex-col items-center justify-center gap-3 py-4 text-[13px] text-ink-2 md:flex-row"
    >
      <button
        type="button"
        disabled={page <= 1 || pending}
        onClick={() => onPageChange(page - 1)}
        className="h-9 rounded-lg border border-border-default px-3 font-medium text-ink-1 hover:bg-surface-hover disabled:cursor-not-allowed disabled:opacity-50 focus-visible:shadow-[var(--focus-ring)] focus-visible:outline-none"
      >
        {t('public.pricing.page_prev')}
      </button>

      <span role="status" className="px-2 font-mono tabular-nums">
        {t('public.pricing.page_info', {
          page,
          total: formatTotal(total, locale),
        })}
      </span>

      <label className="inline-flex items-center gap-1.5">
        <span>{t('public.pricing.page_size')}</span>
        <select
          value={pageSize}
          onChange={(event) => onPageSizeChange(Number(event.target.value))}
          aria-label={t('public.pricing.page_size')}
          className="h-9 rounded-lg border border-border-default bg-surface-card px-2 font-mono text-[13px] tabular-nums text-ink-1 focus:border-brand focus-visible:shadow-[var(--focus-ring)] focus-visible:outline-none"
        >
          {options.map((option) => (
            <option key={option} value={option}>
              {option}
            </option>
          ))}
        </select>
      </label>

      <button
        type="button"
        disabled={BigInt(page) >= lastPageNumber || pending}
        onClick={() => onPageChange(page + 1)}
        className="h-9 rounded-lg border border-border-default px-3 font-medium text-ink-1 hover:bg-surface-hover disabled:cursor-not-allowed disabled:opacity-50 focus-visible:shadow-[var(--focus-ring)] focus-visible:outline-none"
      >
        {t('public.pricing.page_next')}
      </button>
    </nav>
  )
}
