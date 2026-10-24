import { Button } from '@/shared/ui/button'
import { useI18n } from '@/shared/lib/i18n-context'

const PAGE_SIZES = [10, 20, 50] as const

export interface PricingPaginationProps {
  page: number
  pages: number
  pageSize: number
  onPageChange: (page: number) => void
  onPageSizeChange: (size: number) => void
  disabled?: boolean
}

/**
 * 定价表格共用的 offset 分页（web-00 §5.5）：p 从 1 开始，翻页时
 * page_size 保持不变；筛选变化由调用方把 p 重置为 1。
 */
export function PricingPagination({
  page,
  pages,
  pageSize,
  onPageChange,
  onPageSizeChange,
  disabled = false,
}: PricingPaginationProps) {
  const { t } = useI18n()
  const safePages = Math.max(1, pages)

  return (
    <div className="flex items-center justify-end gap-3 border-t border-border-hairline px-5 py-3 text-meta text-ink-2">
      <label htmlFor="pricing-page-size" className="sr-only">
        {t('admin.pricing.pagination_page_size')}
      </label>
      <select
        id="pricing-page-size"
        className="h-8 rounded-md border border-border-default bg-surface-card px-2 text-meta text-ink-1"
        value={pageSize}
        disabled={disabled}
        onChange={(event) => {
          onPageSizeChange(Number(event.target.value))
        }}
      >
        {PAGE_SIZES.map((size) => (
          <option key={size} value={size}>
            {size}
          </option>
        ))}
      </select>
      <Button
        variant="secondary"
        className="h-8 px-2 text-meta"
        disabled={disabled || page <= 1}
        onClick={() => {
          onPageChange(Math.max(1, page - 1))
        }}
      >
        {t('admin.pricing.pagination_previous')}
      </Button>
      <span className="font-mono tabular-nums">
        {t('admin.pricing.pagination_page', { page, pages: safePages })}
      </span>
      <Button
        variant="secondary"
        className="h-8 px-2 text-meta"
        disabled={disabled || page >= safePages}
        onClick={() => {
          onPageChange(page + 1)
        }}
      >
        {t('admin.pricing.pagination_next')}
      </Button>
    </div>
  )
}
