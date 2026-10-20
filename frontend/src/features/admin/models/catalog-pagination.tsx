import { useId } from 'react'

import { Button } from '@/shared/ui/button'
import { useI18n } from '@/shared/lib/i18n-context'

import { MODEL_PAGE_SIZES } from './model-catalog-search'

export interface CatalogPaginationProps {
  page: number
  pages: number
  pageSize: number
  disabled?: boolean
  onPageChange: (page: number) => void
  onPageSizeChange: (size: number) => void
}

/**
 * 目录表格共用的 offset 分页（web-00 §5.5）：p 从 1 开始，筛选变化由
 * 调用方把 p 重置为 1。两个 Tab 各实例化一次，因此 id 里带 `useId`
 * 前缀，避免同一文档内出现重复的 label 目标。
 */
export function CatalogPagination({
  page,
  pages,
  pageSize,
  disabled = false,
  onPageChange,
  onPageSizeChange,
}: CatalogPaginationProps) {
  const { t } = useI18n()
  const selectId = useId()
  const safePages = Math.max(1, pages)

  return (
    <div className="flex items-center justify-end gap-3 border-t border-border-hairline px-5 py-3 text-[13px] text-ink-2">
      <label htmlFor={selectId} className="sr-only">
        {t('admin.models.pagination_page_size')}
      </label>
      <select
        id={selectId}
        className="h-8 rounded-md border border-border-default bg-surface-card px-2 text-xs text-ink-1"
        value={pageSize}
        disabled={disabled}
        onChange={(event) => {
          onPageSizeChange(Number(event.target.value))
        }}
      >
        {MODEL_PAGE_SIZES.map((size) => (
          <option key={size} value={size}>
            {size}
          </option>
        ))}
      </select>
      <Button
        variant="secondary"
        className="h-8 px-2 text-[13px]"
        disabled={disabled || page <= 1}
        onClick={() => {
          onPageChange(Math.max(1, page - 1))
        }}
      >
        {t('admin.models.pagination_previous')}
      </Button>
      <span className="font-mono tabular-nums">
        {t('admin.models.pagination_page', { page, pages: safePages })}
      </span>
      <Button
        variant="secondary"
        className="h-8 px-2 text-[13px]"
        disabled={disabled || page >= safePages}
        onClick={() => {
          onPageChange(page + 1)
        }}
      >
        {t('admin.models.pagination_next')}
      </Button>
    </div>
  )
}
