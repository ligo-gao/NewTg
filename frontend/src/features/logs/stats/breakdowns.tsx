import { useId, type ReactNode } from 'react'

import { formatCredits, formatTokenAmount } from '@/shared/lib/format'
import { useI18n } from '@/shared/lib/i18n-context'

import { formatSeconds } from '../format'
import { USAGE_STATUS_KEYS, UsageStatusBadge } from '../log-status-badge'
import type { UsageLogItem } from '../search'
import { EmptyState } from '../table-ui'

/** One ranked row of a distribution card; Task 5 maps API aggregates into it. */
export interface DistributionItem {
  key: string
  label: string
  cost: number
  requests: number
  tokens: number
  percent: number
}

/**
 * Share of the window, always with one decimal (`75` → `75.0%`). The value is
 * pre-computed by the caller so this component never re-derives a total; the
 * locale formatter is used instead of `toFixed` so a translated page keeps its
 * decimal separator.
 */
function formatShare(percent: number, locale: string): string {
  if (!Number.isFinite(percent)) return '—'
  const formatted = new Intl.NumberFormat(locale, {
    minimumFractionDigits: 1,
    maximumFractionDigits: 1,
  }).format(percent)
  return `${formatted}%`
}

function formatCount(value: number, locale: string): string {
  return new Intl.NumberFormat(locale, { maximumFractionDigits: 0 }).format(value)
}

/** Decorative in-row magnitude bar; the numeric share is always printed too. */
function ShareBar({ percent }: { percent: number }) {
  const width = Number.isFinite(percent)
    ? Math.min(100, Math.max(0, percent))
    : 0
  return (
    <span
      aria-hidden="true"
      className="h-1 rounded-full bg-brand"
      style={{ width: `${String(width)}%` }}
    />
  )
}

function CardHeader({
  title,
  subtitle,
  tag,
  titleId,
}: {
  title: string
  subtitle: string
  tag?: string
  titleId: string
}) {
  return (
    <div className="flex items-start justify-between gap-3 p-4 pb-2">
      <div className="min-w-0">
        <h2 id={titleId} className="text-body font-semibold text-ink-1">
          {title}
        </h2>
        <p className="text-meta text-ink-3">{subtitle}</p>
      </div>
      {tag !== undefined ? (
        <span className="shrink-0 rounded-md border border-border-hairline bg-surface-subtle px-2 py-0.5 text-badge text-ink-2">
          {tag}
        </span>
      ) : null}
    </div>
  )
}

/**
 * Ranked distribution card. `full` prints the whole comparison table (model or
 * token mix); `compact` prints the same rows as a label/bar list for narrower
 * columns (groups, users). Both derive every number from the row, so an empty
 * window only ever renders the caller's `emptyLabel`.
 */
export function DistributionCard({
  title,
  subtitle,
  tag,
  note,
  items,
  mode,
  emptyLabel,
}: {
  title: string
  subtitle: string
  tag?: string
  /** Optional muted footnote, e.g. the token card's quota-refusal hint. */
  note?: string
  items: DistributionItem[]
  mode: 'full' | 'compact'
  emptyLabel: string
}): ReactNode {
  const { t, locale } = useI18n()
  const titleId = useId()

  return (
    <section
      aria-labelledby={titleId}
      className="flex min-w-0 flex-col rounded-xl border border-border-hairline bg-surface-card"
    >
      <CardHeader title={title} subtitle={subtitle} tag={tag} titleId={titleId} />
      {items.length === 0 ? (
        <EmptyState title={emptyLabel} />
      ) : mode === 'full' ? (
        <div className="overflow-x-auto">
          <table className="w-full border-collapse text-meta">
            <caption className="sr-only">{title}</caption>
            <thead>
              <tr className="border-b border-border-hairline text-badge text-ink-3">
                <th scope="col" className="px-4 py-2 text-left font-medium">
                  {t('logs.usage.stats.columns.model')}
                </th>
                <th scope="col" className="px-2 py-2 text-right font-medium">
                  {t('logs.usage.stats.columns.cost')}
                </th>
                <th scope="col" className="px-2 py-2 text-right font-medium">
                  {t('logs.usage.stats.columns.percent')}
                </th>
                <th scope="col" className="px-2 py-2 text-right font-medium">
                  {t('logs.usage.stats.columns.tokens')}
                </th>
                <th scope="col" className="px-4 py-2 text-right font-medium">
                  {t('logs.usage.stats.columns.requests')}
                </th>
              </tr>
            </thead>
            <tbody>
              {items.map((item) => (
                <tr
                  key={item.key}
                  className="border-b border-border-hairline last:border-0"
                >
                  <td className="px-4 py-2">
                    <div className="flex min-w-0 flex-col gap-1">
                      <span className="truncate text-ink-1">{item.label}</span>
                      <ShareBar percent={item.percent} />
                    </div>
                  </td>
                  <td className="px-2 py-2 text-right font-mono tabular-nums text-ink-1">
                    {formatCredits(item.cost, locale)}
                  </td>
                  <td className="px-2 py-2 text-right font-mono tabular-nums text-ink-2">
                    {formatShare(item.percent, locale)}
                  </td>
                  <td className="px-2 py-2 text-right font-mono tabular-nums text-ink-2">
                    {formatTokenAmount(item.tokens, locale)}
                  </td>
                  <td className="px-4 py-2 text-right font-mono tabular-nums text-ink-2">
                    {formatCount(item.requests, locale)}
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      ) : (
        <ul className="flex flex-col">
          {items.map((item) => (
            <li
              key={item.key}
              className="grid grid-cols-[minmax(0,1fr)_auto] items-center gap-3 border-b border-border-hairline px-4 py-2 last:border-0"
            >
              <div className="flex min-w-0 flex-col gap-1">
                <span className="truncate text-meta text-ink-1">{item.label}</span>
                <ShareBar percent={item.percent} />
              </div>
              <div className="flex flex-col items-end gap-0.5">
                <b className="font-mono text-meta font-medium tabular-nums text-ink-1">
                  {formatCredits(item.cost, locale)}
                </b>
                <small className="text-badge tabular-nums text-ink-3">
                  {formatCount(item.requests, locale)} ·{' '}
                  {formatShare(item.percent, locale)}
                </small>
              </div>
            </li>
          ))}
        </ul>
      )}
      {note !== undefined ? (
        <p className="border-t border-border-hairline px-4 py-2 text-badge text-ink-3">
          {note}
        </p>
      ) : null}
    </section>
  )
}

/**
 * Newest-first failure list (§ review focus: `status=error` only). Rows are
 * buttons so the whole line opens the detail drawer for pointer and keyboard
 * users alike; the footer jumps to the filtered list.
 */
export function RecentFailures({
  items,
  total,
  onOpen,
  onViewAll,
}: {
  items: UsageLogItem[]
  total: number
  onOpen: (logId: number) => void
  onViewAll: () => void
}): ReactNode {
  const { t, locale } = useI18n()
  const titleId = useId()
  const timeFormat = new Intl.DateTimeFormat(locale, {
    hour: '2-digit',
    minute: '2-digit',
  })

  return (
    <section
      aria-labelledby={titleId}
      className="flex min-w-0 flex-col rounded-xl border border-border-hairline bg-surface-card"
    >
      <CardHeader
        title={t('logs.usage.stats.fail.title')}
        subtitle={t('logs.usage.stats.fail.sub')}
        tag={t('logs.usage.stats.fail.tag', { shown: items.length, total })}
        titleId={titleId}
      />
      {items.length === 0 ? (
        <EmptyState title={t('logs.usage.stats.fail.empty')} />
      ) : (
        <div className="flex flex-col px-2">
          {items.map((item) => (
            <button
              key={item.id}
              type="button"
              onClick={() => {
                onOpen(item.id)
              }}
              className="grid grid-cols-[38px_minmax(0,1fr)_auto] items-center gap-2.5 border-b border-border-hairline px-2 py-2 text-left last:border-0 hover:bg-surface-hover focus-visible:shadow-[var(--focus-ring)] focus-visible:outline-none"
            >
              <span className="text-badge tabular-nums text-ink-3">
                {timeFormat.format(new Date(item.created_at))}
              </span>
              <span className="flex min-w-0 flex-col">
                <b className="truncate font-mono text-badge text-ink-1">
                  {item.model} · {item.token_name ?? '—'}
                </b>
                <small className="truncate text-badge text-ink-3">
                  {item.error_message ?? t(USAGE_STATUS_KEYS[item.status])} ·{' '}
                  {formatSeconds(item.latency_ms)}
                </small>
              </span>
              <UsageStatusBadge status={item.status} />
            </button>
          ))}
        </div>
      )}
      <p className="border-t border-border-hairline px-4 py-2">
        <button
          type="button"
          onClick={onViewAll}
          className="text-meta font-medium text-brand hover:text-brand-hover focus-visible:shadow-[var(--focus-ring)] focus-visible:outline-none"
        >
          {t('logs.usage.stats.fail.view_all', { total })}
        </button>
      </p>
    </section>
  )
}
