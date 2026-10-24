import type { ReactNode } from 'react'

import { useI18n } from '@/shared/lib/i18n-context'
import { Button } from '@/shared/ui/button'

import { PAGE_SIZE_OPTIONS } from './ui-constants'

/** Five-row table skeleton (modules-05 §1.6: never a full-page spinner). */
export function SkeletonRows({
  columns,
  rows = 5,
}: {
  columns: number
  rows?: number
}) {
  return (
    <div data-testid="table-skeleton" aria-busy="true" className="flex flex-col">
      {Array.from({ length: rows }, (_, rowIndex) => (
        <div
          key={rowIndex}
          className="flex h-11 items-center gap-4 border-b border-border-hairline px-4 last:border-0"
        >
          {Array.from({ length: columns }, (_, columnIndex) => (
            <span
              key={columnIndex}
              aria-hidden="true"
              className="h-3 flex-1 rounded bg-ink-4/30"
            />
          ))}
        </div>
      ))}
    </div>
  )
}

export function InlineError({
  message,
  onRetry,
  retryLabel,
}: {
  message: string
  onRetry: () => void
  retryLabel: string
}) {
  return (
    <div
      role="alert"
      className="flex flex-wrap items-center gap-3 p-5 text-body text-ink-2"
    >
      <span>{message}</span>
      <Button variant="secondary" onClick={onRetry}>
        {retryLabel}
      </Button>
    </div>
  )
}

export function EmptyState({
  title,
  hint,
  action,
}: {
  title: string
  hint?: string
  action?: ReactNode
}) {
  return (
    <div className="flex flex-col items-center gap-2 p-10 text-center text-body text-ink-2">
      <p className="font-medium text-ink-1">{title}</p>
      {hint !== undefined ? <p className="text-meta text-ink-3">{hint}</p> : null}
      {action}
    </div>
  )
}

export interface PaginationFooterProps {
  /** Dictionary namespace under `logs.` (`usage` or `audit`). */
  namespace: 'usage' | 'audit'
  page: number
  pageSize: number
  total: number
  onPageChange: (page: number) => void
  onPageSizeChange: (pageSize: number) => void
}

/**
 * Offset pagination shared by both log tables. The page count is derived from
 * the exact server total; callers handle the out-of-range fallback because it
 * requires a toast in addition to navigation.
 */
export function PaginationFooter({
  namespace,
  page,
  pageSize,
  total,
  onPageChange,
  onPageSizeChange,
}: PaginationFooterProps) {
  const { t } = useI18n()
  const totalPages = Math.max(1, Math.ceil(total / pageSize))

  return (
    <div className="flex flex-wrap items-center justify-end gap-3 border-t border-border-hairline px-4 py-3 text-body text-ink-2">
      <span data-testid="pagination-page">
        {t(`logs.${namespace}.page`, { page, pages: totalPages })}
      </span>
      <Button
        variant="secondary"
        aria-label={t(`logs.${namespace}.previous`)}
        disabled={page <= 1}
        onClick={() => {
          onPageChange(Math.max(1, page - 1))
        }}
      >
        ‹
      </Button>
      <Button
        variant="secondary"
        aria-label={t(`logs.${namespace}.next`)}
        disabled={page >= totalPages}
        onClick={() => {
          onPageChange(page + 1)
        }}
      >
        ›
      </Button>
      <label className="flex items-center gap-2">
        {t(`logs.${namespace}.page_size`)}
        <select
          aria-label={t(`logs.${namespace}.page_size`)}
          value={pageSize}
          onChange={(event) => {
            onPageSizeChange(Number(event.target.value))
          }}
          className="h-9 rounded-lg border border-border-default bg-surface-card px-2 text-body text-ink-1"
        >
          {PAGE_SIZE_OPTIONS.map((size) => (
            <option key={size} value={size}>
              {size}
            </option>
          ))}
        </select>
      </label>
    </div>
  )
}
