/**
 * 列表页的加载/错误/空态（F-09-22）。
 *
 * 空态刻意分两种：有筛选（给「清除筛选」出口）与全局无模型（没有可执行
 * 的写操作，公开页也不该出现「去创建模型」）。错误态是行内重试，而不是
 * 把已经渲染的数据整页替换掉。
 */

import { useI18n } from '@/shared/lib/i18n-context'

export function PricingSkeleton({ view }: { view: 'card' | 'table' }) {
  const { t } = useI18n()
  return (
    <div aria-busy="true" className="flex flex-col gap-4">
      <span className="sr-only">{t('public.pricing.loading')}</span>
      {view === 'table' ? (
        <div className="overflow-hidden rounded-xl border border-border-hairline bg-surface-card">
          {[0, 1, 2, 3, 4].map((row) => (
            <div key={row} className="h-11 border-b border-border-hairline last:border-b-0" />
          ))}
        </div>
      ) : (
        <div className="grid grid-cols-1 gap-4 md:grid-cols-2 xl:grid-cols-3">
          {[0, 1, 2, 3, 4, 5].map((card) => (
            <div
              key={card}
              className="flex h-[280px] flex-col gap-3 rounded-xl border border-border-hairline bg-surface-card p-4"
            >
              <div className="h-4 w-2/3 rounded bg-surface-subtle" />
              <div className="h-3 w-1/2 rounded bg-surface-subtle" />
              <div className="mt-4 h-3 w-full rounded bg-surface-subtle" />
              <div className="h-3 w-5/6 rounded bg-surface-subtle" />
              <div className="mt-auto h-3 w-1/3 rounded bg-surface-subtle" />
            </div>
          ))}
        </div>
      )}
    </div>
  )
}

export function InlineError({
  message,
  onRetry,
}: {
  message: string
  onRetry: () => void
}) {
  const { t } = useI18n()
  return (
    <div role="alert" className="flex flex-wrap items-center gap-3 rounded-lg border border-danger-line bg-danger-bg px-4 py-3 text-public-body text-danger">
      <span>{message}</span>
      <button
        type="button"
        onClick={onRetry}
        className="rounded-md border border-danger-line px-2 py-0.5 text-public-caption font-medium hover:bg-surface-card focus-visible:shadow-[var(--focus-ring)] focus-visible:outline-none"
      >
        {t('public.pricing.retry')}
      </button>
    </div>
  )
}

export function EmptyState({
  filtered,
  onClearFilters,
}: {
  filtered: boolean
  onClearFilters: () => void
}) {
  const { t } = useI18n()
  return (
    <div
      role="status"
      className="flex min-h-[320px] flex-col items-center justify-center gap-3 rounded-xl border border-border-hairline bg-surface-card px-6 text-center"
    >
      <svg
        aria-hidden="true"
        viewBox="0 0 24 24"
        className="size-6 text-ink-4"
        fill="none"
        stroke="currentColor"
        strokeWidth="1.5"
      >
        {filtered ? (
          <>
            <circle cx="10" cy="10" r="6" />
            <path d="m15 15 5 5M8 8l4 4M12 8l-4 4" strokeLinecap="round" />
          </>
        ) : (
          <>
            <path d="M4 8.5 12 4l8 4.5V19l-8-4.5L4 19z" />
            <path d="M12 4v10.5" />
          </>
        )}
      </svg>
      <h2 className="text-public-section text-ink-1">
        {filtered
          ? t('public.pricing.empty')
          : t('public.pricing.global_empty')}
      </h2>
      <p className="max-w-[480px] text-public-body text-ink-2">
        {filtered
          ? t('public.pricing.empty_hint')
          : t('public.pricing.global_empty_hint')}
      </p>
      {filtered ? (
        <button
          type="button"
          onClick={onClearFilters}
          className="mt-1 inline-flex h-9 items-center rounded-lg bg-brand px-4 text-public-body font-medium text-white hover:bg-brand-hover focus-visible:shadow-[var(--focus-ring)] focus-visible:outline-none"
        >
          {t('public.pricing.clear_filters')}
        </button>
      ) : null}
    </div>
  )
}

export function DetailNotFound() {
  const { t } = useI18n()
  return (
    <div
      role="alert"
      className="flex min-h-[320px] flex-col items-center justify-center gap-3 rounded-xl border border-border-hairline bg-surface-card px-6 text-center"
    >
      <svg
        aria-hidden="true"
        viewBox="0 0 24 24"
        className="size-6 text-ink-4"
        fill="none"
        stroke="currentColor"
        strokeWidth="1.5"
      >
        <path d="M6 3h8l4 4v14H6z" />
        <path d="M14 3v4h4M12 11v4" strokeLinecap="round" />
      </svg>
      <h1 className="text-public-title text-ink-1">
        {t('public.pricing.detail.not_found')}
      </h1>
      <p className="max-w-[480px] text-public-body text-ink-2">
        {t('public.pricing.detail.not_found_hint')}
      </p>
      <a
        href="/pricing"
        className="mt-1 inline-flex h-9 items-center rounded-lg bg-brand px-4 text-public-body font-medium text-white hover:bg-brand-hover focus-visible:shadow-[var(--focus-ring)] focus-visible:outline-none"
      >
        {t('public.pricing.detail.back')}
      </a>
    </div>
  )
}
