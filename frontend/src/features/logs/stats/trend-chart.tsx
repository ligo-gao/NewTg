import { useEffect, useId, useState } from 'react'

import { formatCredits } from '@/shared/lib/format'
import { useI18n } from '@/shared/lib/i18n-context'

import { EmptyState } from '../table-ui'
import { formatBucketTick, trendInsights, type TrendPoint } from './insights'

export type TrendMetric = 'cost' | 'requests'

export interface TrendCardProps {
  rows: TrendPoint[]
  granularity: 'hour' | 'day'
  /** Which column of the bucket this card plots; one card per measure. */
  metric: TrendMetric
  /** Peak RPM from the rates endpoint; the requests card prints it when set. */
  rpmPeak?: number
  loading?: boolean
  emptyLabel: string
}

/** Gridlines at the plot's 0/25/50/75/100 % heights. */
const GRIDLINES = [0, 25, 50, 75, 100]

function compact(value: number, locale: string): string {
  return new Intl.NumberFormat(locale, {
    notation: 'compact',
    maximumFractionDigits: 1,
  }).format(value)
}

function formatCount(value: number, locale: string): string {
  return new Intl.NumberFormat(locale, { maximumFractionDigits: 0 }).format(value)
}

function barHeight(value: number, max: number): string {
  // A zero bucket keeps its slot as a hairline stub instead of disappearing,
  // so the bucket count stays readable on the axis.
  return value === 0 ? '2px' : `${(value / max) * 100}%`
}

/**
 * One measure, one card. Spend and requests carry different units, so they are
 * separate cards with their own y-axis, x-axis, readout, footer and table —
 * never one plot frame with two scales sharing an edge.
 *
 * Hover and ← → move one bucket at a time and the reading lands in the card
 * subtitle: no floating tooltip that can cover a bar or change the card's
 * height. The `sr-only` table is the accessible equivalent of the whole chart.
 */
export function TrendCard({
  rows,
  granularity,
  metric,
  rpmPeak,
  loading = false,
  emptyLabel,
}: TrendCardProps) {
  const { t, locale } = useI18n()
  const [active, setActive] = useState<number | null>(null)
  const titleId = useId()
  const liveId = useId()

  // A new window or granularity means the old index points at a bucket that
  // may no longer exist (or means something else), so drop it.
  useEffect(() => {
    setActive(null)
  }, [rows.length, granularity])

  const isCost = metric === 'cost'
  const title = t(
    isCost
      ? 'logs.usage.stats.chart.cost_series'
      : 'logs.usage.stats.chart.request_series',
  )
  const unit = t(
    isCost
      ? 'logs.usage.credits'
      : 'logs.usage.stats.chart.requests_unit',
  )
  const formatValue = (value: number): string =>
    isCost ? formatCredits(value, locale) : formatCount(value, locale)
  const formatAxis = (value: number): string =>
    isCost ? compact(value, locale) : formatCount(value, locale)

  const insights = trendInsights(rows)
  const values = rows.map((row) => (isCost ? row.cost : row.requests))
  const total = values.reduce((sum, value) => sum + value, 0)
  const max = Math.max(...values, 1)
  const average = rows.length === 0 ? 0 : total / rows.length
  // The mean is printed with the measure's own formatter; `formatCredits`
  // rejects fractions, so the displayed value is rounded while the dashed
  // line keeps the exact position.
  const averageLabel = formatValue(Math.round(average))
  const peakIndex = values.indexOf(max)
  const peakValue = values[peakIndex] ?? 0
  const peakRow = rows[peakIndex] ?? null
  const peakShare = total > 0 ? (peakValue / total) * 100 : 0
  const hasSpend = insights.totalCost > 0
  // A refused-only window has traffic but no spend; naming a "peak" at 0 would
  // print "… · 0 credits (0.0%)", so the spend card omits that item.
  const showPeak = isCost ? hasSpend : peakValue > 0

  const step = Math.max(1, Math.ceil(rows.length / 6))
  const tickIndexes = rows
    .map((_, index) => index)
    .filter((index) => index % step === 0)
  const barColumns = {
    gridTemplateColumns: `repeat(${String(rows.length)}, minmax(0, 1fr))`,
    gap: '2px',
  }

  const readout =
    active === null || rows[active] === undefined
      ? t('logs.usage.stats.chart.readout_idle')
      : t('logs.usage.stats.chart.readout', {
          time: formatBucketTick(
            rows[active]!.bucket,
            granularity,
            locale,
          ),
          value: formatValue(values[active]!),
          unit,
          percent: (total > 0 ? (values[active]! / total) * 100 : 0).toFixed(1),
        })

  function moveActive(next: number | null): void {
    setActive(next)
  }

  function onKeyDown(event: React.KeyboardEvent<HTMLDivElement>): void {
    const last = rows.length - 1
    let next: number | null = null
    if (event.key === 'ArrowRight') {
      next = active === null ? 0 : Math.min(active + 1, last)
    } else if (event.key === 'ArrowLeft') {
      next = active === null ? last : Math.max(active - 1, 0)
    } else if (event.key === 'Home') {
      next = 0
    } else if (event.key === 'End') {
      next = last
    } else if (event.key === 'Escape') {
      moveActive(null)
      return
    }
    if (next === null) return
    event.preventDefault()
    moveActive(next)
  }

  const barColor = isCost ? 'bg-[var(--chart-cat-1)]' : 'bg-[var(--chart-cat-2)]'

  return (
    <section
      aria-labelledby={titleId}
      data-testid={`trend-card-${metric}`}
      className="flex min-w-0 flex-col rounded-xl border border-border-hairline bg-surface-card px-4 pb-3 pt-3.5"
    >
      <div className="flex items-start justify-between gap-3">
        <div className="min-w-0">
          <h2 id={titleId} className="text-body font-semibold text-ink-1">
            {title}
          </h2>
          <p
            data-testid={`trend-readout-${metric}`}
            className="mt-px truncate text-meta tabular-nums text-ink-3"
          >
            {readout}
          </p>
        </div>
        {rows.length > 0 ? (
          <span className="flex shrink-0 items-center gap-1.5 pt-0.5 text-badge tabular-nums text-ink-3">
            <i
              aria-hidden="true"
              className="w-3 border-t border-dashed border-ink-3"
            />
            {t('logs.usage.stats.chart.average', {
              value: averageLabel,
            })}
          </span>
        ) : null}
      </div>

      {loading ? (
        <div
          data-testid={`trend-loading-${metric}`}
          aria-busy="true"
          className="mt-3 h-[150px] animate-pulse rounded-lg bg-surface-hover motion-reduce:animate-none sm:h-[200px]"
        />
      ) : rows.length === 0 ? (
        <EmptyState title={emptyLabel} />
      ) : (
        <>
          <div className="grid grid-cols-[44px_minmax(0,1fr)] gap-1.5 pt-3">
            <div
              aria-hidden="true"
              data-testid={`trend-axis-${metric}`}
              className="relative h-[150px] font-mono text-badge leading-none tabular-nums text-ink-3 sm:h-[200px]"
            >
              <span className="absolute right-0 top-0 -translate-y-1/2">
                {formatAxis(max)}
              </span>
              <span className="absolute right-0 top-1/2 -translate-y-1/2">
                {formatAxis(max / 2)}
              </span>
              <span className="absolute bottom-0 right-0 translate-y-1/2">
                {formatAxis(0)}
              </span>
            </div>
            <div
              role="group"
              tabIndex={0}
              aria-label={t('logs.usage.stats.chart.chart_aria', { name: title })}
              aria-describedby={liveId}
              data-testid={`trend-plot-${metric}`}
              className="relative h-[150px] cursor-crosshair rounded-sm focus-visible:shadow-[var(--focus-ring)] focus-visible:outline-none sm:h-[200px]"
              onMouseLeave={() => {
                moveActive(null)
              }}
              onBlur={() => {
                moveActive(null)
              }}
              onKeyDown={onKeyDown}
            >
              {GRIDLINES.map((percent) => (
                <div
                  key={percent}
                  aria-hidden="true"
                  className={`absolute inset-x-0 h-px ${
                    percent === 0
                      ? 'bg-border-default'
                      : 'bg-border-hairline/70'
                  }`}
                  style={{ top: `${String(percent)}%` }}
                />
              ))}
              {/* Highlight column, so the readout has a mark on the plot. */}
              {active !== null ? (
                <div
                  aria-hidden="true"
                  className="pointer-events-none absolute inset-y-0 bg-ink-1/[0.07]"
                  style={{
                    left: `${String(((active + 0.5) / rows.length) * 100)}%`,
                    width: `min(${String(100 / rows.length)}%, 56px)`,
                    transform: 'translateX(-50%)',
                  }}
                />
              ) : null}
              <div className="absolute inset-0 grid h-full grid-rows-1" style={barColumns}>
                {rows.map((row, index) => (
                  <div
                    key={row.bucket}
                    className="flex h-full min-w-0 flex-col items-center justify-end"
                    onMouseEnter={() => {
                      moveActive(index)
                    }}
                  >
                    <div
                      data-testid={`trend-bar-${metric}`}
                      data-zero={row[isCost ? 'cost' : 'requests'] === 0 ? 'true' : 'false'}
                      title={`${formatBucketTick(row.bucket, granularity, locale)} · ${formatValue(
                        row[isCost ? 'cost' : 'requests'],
                      )} ${unit}`}
                      style={{
                        height: barHeight(row[isCost ? 'cost' : 'requests'], max),
                        minHeight: 2,
                      }}
                      className={`w-full max-w-8 rounded-t ${
                        row[isCost ? 'cost' : 'requests'] === 0
                          ? 'bg-border-hairline'
                          : barColor
                      }`}
                    />
                  </div>
                ))}
              </div>
              {average > 0 ? (
                <div
                  data-testid={`trend-avg-${metric}`}
                  aria-hidden="true"
                  className="pointer-events-none absolute inset-x-0 border-t border-dashed border-border-default"
                  style={{ top: `${String((1 - average / max) * 100)}%` }}
                />
              ) : null}
            </div>
          </div>

          <div className="mt-2 grid grid-cols-[44px_minmax(0,1fr)] gap-1.5">
            <div />
            <div
              aria-hidden="true"
              className="grid font-mono text-badge leading-none tabular-nums text-ink-3"
              style={{
                gridTemplateColumns: `repeat(${String(rows.length)}, minmax(0, 1fr))`,
                gap: '2px',
              }}
            >
              {tickIndexes.map((index, tickPosition) => {
                const row = rows[index]
                if (row === undefined) return null
                const isFirst = tickPosition === 0
                const isLast = tickPosition === tickIndexes.length - 1
                // Thin out the odd ticks on narrow viewports so the nowrap
                // labels cannot collide; first and last always stay visible.
                const isDense = !isFirst && !isLast && index % (step * 2) !== 0
                return (
                  <span
                    key={row.bucket}
                    data-testid="trend-x-tick"
                    className={`whitespace-nowrap ${
                      isFirst ? 'text-left' : isLast ? 'text-right' : 'text-center'
                    } ${isDense ? 'hidden sm:block' : ''}`}
                    style={{ gridColumn: index + 1 }}
                  >
                    {formatBucketTick(row.bucket, granularity, locale)}
                  </span>
                )
              })}
            </div>
          </div>

          <p id={liveId} aria-live="polite" className="sr-only">
            {active === null ? '' : readout}
          </p>

          <ul className="mt-3 flex flex-wrap gap-x-4 gap-y-1 border-t border-border-hairline pt-2.5 text-badge text-ink-2">
            {showPeak && peakRow !== null ? (
              <li>
                {t(
                  isCost
                    ? 'logs.usage.stats.chart.peak'
                    : 'logs.usage.stats.chart.peak_requests',
                  isCost
                    ? {
                        time: formatBucketTick(peakRow.bucket, granularity, locale),
                        cost: formatCredits(peakValue, locale),
                        percent: peakShare.toFixed(1),
                      }
                    : {
                        time: formatBucketTick(peakRow.bucket, granularity, locale),
                        count: formatCount(peakValue, locale),
                        percent: peakShare.toFixed(1),
                      },
                )}
              </li>
            ) : null}
            <li>
              {t(
                isCost
                  ? granularity === 'hour'
                    ? 'logs.usage.stats.chart.average_hour'
                    : 'logs.usage.stats.chart.average_day'
                  : granularity === 'hour'
                    ? 'logs.usage.stats.chart.average_requests_hour'
                    : 'logs.usage.stats.chart.average_requests_day',
                { value: averageLabel },
              )}
            </li>
            <li>
              {t('logs.usage.stats.chart.active', {
                active: formatCount(insights.activeBuckets, locale),
                total: formatCount(insights.bucketCount, locale),
                unit: t(
                  granularity === 'hour'
                    ? 'logs.usage.stats.chart.unit_hour'
                    : 'logs.usage.stats.chart.unit_day',
                ),
              })}
            </li>
            {isCost ? (
              <li className="text-ink-3">
                {t(
                  granularity === 'hour'
                    ? 'logs.usage.stats.chart.zero_note'
                    : 'logs.usage.stats.chart.zero_note_day',
                  { count: formatCount(insights.zeroBuckets, locale) },
                )}
              </li>
            ) : rpmPeak !== undefined && rpmPeak > 0 ? (
              <li>
                {t('logs.usage.stats.chart.rpm_peak', {
                  value: compact(rpmPeak, locale),
                })}
              </li>
            ) : null}
          </ul>

          {/* Relief rule: every chart carries a table equivalent. The
              sr-only live region is the screen-reader path; this visible
              disclosure is the sighted-reader path (UI §3.8). */}
          <details className="mt-2.5">
            <summary className="cursor-pointer text-meta font-medium text-brand focus-visible:shadow-[var(--focus-ring)] focus-visible:outline-none">
              {t('logs.usage.stats.view_table')}
            </summary>
            <div className="max-h-64 overflow-auto">
              <table
                data-testid={`trend-table-${metric}`}
                className="mt-2 w-full text-body"
              >
                <caption className="sr-only">
                  {t('logs.usage.stats.trend_table_caption')}
                </caption>
                <thead>
                  <tr className="border-b border-border-hairline text-left text-meta font-medium text-ink-2">
                    <th scope="col" className="px-2 py-2">
                      {t('logs.usage.stats.columns.time')}
                    </th>
                    <th scope="col" className="px-2 py-2 text-right">
                      {isCost
                        ? t('logs.usage.stats.columns.cost')
                        : t('logs.usage.stats.columns.requests')}
                    </th>
                  </tr>
                </thead>
                <tbody>
                  {rows.map((row) => (
                    <tr
                      key={row.bucket}
                      className="border-b border-border-hairline last:border-0"
                    >
                      <td className="px-2 py-1.5 font-mono text-meta">
                        {formatBucketTick(row.bucket, granularity, locale)}
                      </td>
                      <td className="px-2 py-1.5 text-right font-mono text-meta tabular-nums">
                        {formatValue(row[isCost ? 'cost' : 'requests'])}
                      </td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
          </details>
        </>
      )}
    </section>
  )
}
