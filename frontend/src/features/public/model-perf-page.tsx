import { useQuery } from '@tanstack/react-query'
import { useId, useState } from 'react'

import { ApiError } from '@/api/errors'
import { Button } from '@/shared/ui/base/button'
import { Tag } from '@/shared/ui/tag'
import { useI18n } from '@/shared/lib/i18n-context'

import { fetchModelMetrics, type ModelMetrics, type PerfRange } from './api'

const RANGES: readonly PerfRange[] = ['24h', '7d']

/** modules-09 F-09-25: below 20 calls the section must flag low confidence. */
const LOW_SAMPLE_THRESHOLD = 20

export interface ModelPerfPageProps {
  modelId: string
}

function noValue() {
  return '—'
}

/**
 * F-09-25 model performance section.
 *
 * Every value shown is aggregated by the backend from `usage_logs`; the page
 * never derives or invents a number. `null` is rendered as an em dash (not 0)
 * per the shared no-data rule, and a low sample size is surfaced rather than
 * hidden behind a precise-looking percentage.
 */
export function ModelPerfPage({ modelId }: ModelPerfPageProps) {
  const { t, locale } = useI18n()
  const [range, setRange] = useState<PerfRange>('24h')
  // Two ModelPerfPage instances (e.g. side-by-side variants) must not share a
  // radio name, or selecting a range in one would uncheck the other.
  const rangeName = useId()

  const query = useQuery({
    queryKey: ['public', 'model-metrics', modelId, range],
    queryFn: ({ signal }) => fetchModelMetrics(modelId, range, signal),
    retry: false,
  })

  return (
    <section className="flex flex-col gap-6">
      <div className="flex flex-wrap items-start justify-between gap-4">
        <div>
          <h1 className="text-public-title font-semibold leading-7 text-ink-1">
            {t('public.perf.title')}
          </h1>
          <p className="mt-1 text-body text-ink-2">{modelId}</p>
        </div>
        <fieldset className="flex items-center gap-1 rounded-lg border border-border-hairline bg-surface-subtle p-1">
          <legend className="sr-only">{t('public.perf.range_label')}</legend>
          {RANGES.map((option) => (
            <label
              key={option}
              className={[
                'cursor-pointer rounded-md px-3 py-1 text-meta font-medium',
                range === option
                  ? 'bg-surface-card text-ink-1 shadow-sm'
                  : 'text-ink-2 hover:text-ink-1',
                'has-[:focus-visible]:shadow-[var(--focus-ring)]',
              ].join(' ')}
            >
              <input
                type="radio"
                name={rangeName}
                value={option}
                checked={range === option}
                onChange={() => setRange(option)}
                className="sr-only"
              />
              {t(`public.perf.range.${option}`)}
            </label>
          ))}
        </fieldset>
      </div>

      {query.isPending ? (
        <PerformanceSkeleton label={t('public.perf.loading')} />
      ) : query.isError ? (
        <PerformanceError
          message={
            query.error instanceof ApiError && query.error.kind === 'rate_limited'
              ? t('public.perf.rate_limited')
              : t('public.perf.unavailable')
          }
          retryLabel={t('public.perf.retry')}
          onRetry={() => void query.refetch()}
        />
      ) : (
        <PerformanceBody data={query.data} locale={locale} />
      )}
    </section>
  )
}

function PerformanceSkeleton({ label }: { label: string }) {
  return (
    <div aria-busy="true" className="flex flex-col gap-6">
      <span className="sr-only">{label}</span>
      <div className="grid grid-cols-1 gap-4 sm:grid-cols-2 xl:grid-cols-4">
        {[0, 1, 2, 3].map((index) => (
          <div
            key={index}
            className="h-[88px] rounded-xl border border-border-hairline bg-surface-card"
          />
        ))}
      </div>
      <div className="h-[132px] rounded-xl border border-border-hairline bg-surface-card" />
    </div>
  )
}

function PerformanceError({
  message,
  retryLabel,
  onRetry,
}: {
  message: string
  retryLabel: string
  onRetry: () => void
}) {
  return (
    <div
      role="alert"
      className="flex flex-wrap items-center justify-between gap-3 rounded-xl border border-danger-line bg-danger-bg p-4 text-body text-danger"
    >
      <span>{message}</span>
      <Button variant="secondary" onClick={onRetry}>
        {retryLabel}
      </Button>
    </div>
  )
}

function PerformanceBody({ data, locale }: { data: ModelMetrics; locale: string }) {
  const { t } = useI18n()
  const summary = data.summary
  const isEmpty = summary.sample_size === 0
  const lowSample = !isEmpty && summary.sample_size < LOW_SAMPLE_THRESHOLD
  const showTtft = data.per_group.some((group) => group.avg_ttft_ms !== null)

  const formatCount = new Intl.NumberFormat(locale)
  const formatPercent = new Intl.NumberFormat(locale, {
    style: 'percent',
    maximumFractionDigits: 1,
  })
  const formatDecimal = new Intl.NumberFormat(locale, { maximumFractionDigits: 1 })

  const stats = [
    {
      key: 'avg_latency',
      label: t('public.perf.avg_latency'),
      value:
        summary.avg_latency_ms === null
          ? noValue()
          : `${formatCount.format(summary.avg_latency_ms)} ms`,
    },
    {
      key: 'success_rate',
      label: t('public.perf.success_rate'),
      value:
        summary.success_rate === null ? noValue() : formatPercent.format(summary.success_rate),
    },
    {
      key: 'tps',
      label: t('public.perf.tps'),
      value: summary.tps === null ? noValue() : formatDecimal.format(summary.tps),
    },
    {
      key: 'failed_24h',
      label: t('public.perf.failed_24h'),
      value: summary.failed_24h === null ? noValue() : formatCount.format(summary.failed_24h),
    },
  ]

  return (
    <div className="flex flex-col gap-6">
      {isEmpty ? (
        <p className="text-body text-ink-2">{t('public.perf.empty')}</p>
      ) : null}

      <dl className="grid grid-cols-1 gap-4 sm:grid-cols-2 xl:grid-cols-4">
        {stats.map((stat) => (
          <div
            key={stat.key}
            className="flex flex-col gap-1 rounded-xl border border-border-hairline bg-surface-card p-4"
          >
            <dt className="text-meta leading-[18px] text-ink-2">{stat.label}</dt>
            <dd className="text-stat font-semibold leading-8 tabular-nums text-ink-1">
              {stat.value}
            </dd>
            {stat.key === 'tps' ? (
              <span className="text-meta leading-5 text-ink-2">
                {t('public.perf.approximate')}
              </span>
            ) : null}
          </div>
        ))}
      </dl>

      {lowSample ? (
        <div role="status">
          <Tag variant="warning">{t('public.perf.low_sample')}</Tag>
        </div>
      ) : null}

      <div className="overflow-x-auto">
        <table
          aria-label={t('public.perf.group_table')}
          className="w-full min-w-[520px] border-collapse text-body"
        >
          <thead>
            <tr className="border-b border-border-hairline bg-surface-subtle text-left text-meta text-ink-2">
              <th scope="col" className="px-3 py-2 font-medium">
                {t('public.perf.group')}
              </th>
              {showTtft ? (
                <th scope="col" className="px-3 py-2 text-right font-medium">
                  {t('public.perf.avg_ttft')}
                </th>
              ) : null}
              <th scope="col" className="px-3 py-2 text-right font-medium">
                {t('public.perf.avg_latency')}
              </th>
              <th scope="col" className="px-3 py-2 text-right font-medium">
                {t('public.perf.success_rate')}
              </th>
            </tr>
          </thead>
          <tbody>
            {data.per_group.map((group) => (
              <tr key={group.group} className="border-b border-border-hairline last:border-0">
                <th scope="row" className="px-3 py-3 text-left font-medium text-ink-1">
                  {group.group}
                </th>
                {showTtft ? (
                  <td className="px-3 py-3 text-right tabular-nums text-ink-1">
                    {group.avg_ttft_ms === null
                      ? noValue()
                      : `${formatCount.format(group.avg_ttft_ms)} ms`}
                  </td>
                ) : null}
                <td className="px-3 py-3 text-right tabular-nums text-ink-1">
                  {group.avg_latency_ms === null
                    ? noValue()
                    : `${formatCount.format(group.avg_latency_ms)} ms`}
                </td>
                <td className="px-3 py-3 text-right tabular-nums text-ink-1">
                  {group.success_rate === null ? noValue() : formatPercent.format(group.success_rate)}
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      </div>
    </div>
  )
}
