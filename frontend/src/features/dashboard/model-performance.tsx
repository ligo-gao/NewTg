import { useMemo, useState } from 'react'

import { useI18n } from '@/shared/lib/i18n-context'
import { handleRadioGroupKeyDown } from '@/shared/lib/roving-focus'
import type { ChartConfig, ChartSeries } from '@/shared/lib/chart-data'
import { AnalyticsChart } from '@/shared/ui/analytics-chart'
import { Tag } from '@/shared/ui/tag'

import {
  type ModelPerfBucketPoint,
  type ModelPerfDimension,
  type ModelPerfRange,
  type ModelPerfResponse,
} from './model-perf-api'
import { ModelPerfBadge } from './model-perf-badge'
import { StatCard } from './stat-card'
import { useModelPerf } from './use-model-perf'

/** UI §8.2：无样本一律显示 `—`，绝不显示 0（0 是真实值，语义不同）。 */
const NONE = '—'
const LOW_SAMPLE_SIZE = 20

const RANGES: readonly ModelPerfRange[] = ['24h', '7d']

interface AggregatedPoint {
  start: string
  requests: number
  ttft50: number | null
  ttft95: number | null
  latency95: number | null
  success: number | null
  uptime: number | null
}

function weighted(
  buckets: ModelPerfBucketPoint[],
  pick: (bucket: ModelPerfBucketPoint) => number | null,
): number | null {
  let sum = 0
  let weight = 0
  for (const bucket of buckets) {
    const value = pick(bucket)
    if (value === null) continue
    // 以请求数加权：全模型视图必须把各模型桶折成一条线，低流量模型不能
    // 和高流量模型等权，否则一条冷门模型的抖动会主导整条曲线。
    const w = bucket.requests > 0 ? bucket.requests : 1
    sum += value * w
    weight += w
  }
  return weight === 0 ? null : sum / weight
}

/**
 * 看板接口按 (start, model) 返回桶；KPI 是全模型汇总，所以图表层必须把
 * 同一 start 的多个模型折成一个点。折算是查询期近似（UI §8.2 允许），
 * 且只用于曲线形状，KPI 数值始终直接取后端 `summary`。
 */
function aggregateBuckets(
  buckets: ModelPerfBucketPoint[],
): AggregatedPoint[] {
  const byStart = new Map<string, ModelPerfBucketPoint[]>()
  for (const bucket of buckets) {
    const list = byStart.get(bucket.start)
    if (list === undefined) byStart.set(bucket.start, [bucket])
    else list.push(bucket)
  }

  return [...byStart.entries()]
    .sort(([left], [right]) => Date.parse(left) - Date.parse(right))
    .map(([start, group]) => ({
      start,
      requests: group.reduce((sum, bucket) => sum + bucket.requests, 0),
      ttft50: weighted(group, (bucket) => bucket.ttft_p50_ms),
      ttft95: weighted(group, (bucket) => bucket.ttft_p95_ms),
      latency95: weighted(group, (bucket) => bucket.latency_p95_ms),
      success: weighted(group, (bucket) => bucket.success_rate),
      uptime: weighted(group, (bucket) => bucket.uptime),
    }))
}

function seriesFrom(
  points: AggregatedPoint[],
  pick: (point: AggregatedPoint) => number | null,
): { ts: string; value: number }[] {
  return points.flatMap((point) => {
    const value = pick(point)
    if (value === null || !Number.isFinite(value)) return []
    const ms = Date.parse(point.start)
    if (Number.isNaN(ms)) return []
    return [{ ts: new Date(ms).toISOString(), value }]
  })
}

function formatMs(value: number | null, over30s = false): string {
  if (value === null || !Number.isFinite(value)) return NONE
  // +Inf 桶只能表达下界，伪造一个有限分位数比留白更危险（UI §8.2）。
  if (over30s) return '>30s'
  if (value >= 1000) return `${(value / 1000).toFixed(1)}s`
  return `${Math.round(value)}ms`
}

function formatPercent(value: number | null, digits = 1): string {
  if (value === null || !Number.isFinite(value)) return NONE
  return `${(value * 100).toFixed(digits)}%`
}

function formatCount(value: number | null, locale: string): string {
  if (value === null || !Number.isFinite(value)) return NONE
  return new Intl.NumberFormat(locale, { maximumFractionDigits: 0 }).format(
    value,
  )
}

function formatTps(value: number | null, locale: string): string {
  if (value === null || !Number.isFinite(value)) return NONE
  return new Intl.NumberFormat(locale, {
    minimumFractionDigits: 1,
    maximumFractionDigits: 1,
  }).format(value)
}

function formatDuration(seconds: number, locale: string): string {
  if (!Number.isFinite(seconds) || seconds <= 0) return NONE
  const minutes = Math.round(seconds / 60)
  if (minutes >= 60) {
    const hours = Math.floor(minutes / 60)
    const rest = minutes % 60
    return rest === 0
      ? `${new Intl.NumberFormat(locale).format(hours)}h`
      : `${new Intl.NumberFormat(locale).format(hours)}h ${rest}m`
  }
  return `${new Intl.NumberFormat(locale).format(minutes)}m`
}

function formatInstant(value: string, locale: string): string {
  const ms = Date.parse(value)
  if (Number.isNaN(ms)) return value
  return new Intl.DateTimeFormat(locale, {
    month: 'short',
    day: 'numeric',
    hour: '2-digit',
    minute: '2-digit',
  }).format(new Date(ms))
}

export interface ModelPerformanceProps {
  /** Admin gate for the channel/upstream breakdown (UI §8.1 / §8.7). */
  canViewDimension: boolean
}

/**
 * F-05-08 模型性能看板（modules-05 UI §8）。
 *
 * 硬约束：只展示采集管道数据；`pipeline.enabled === false` 时整块替换为
 * 采集未启用说明，绝不用 `usage_logs` 数字伪造 TTFT / Uptime / Incidents。
 * 空样本显示 `—`（不是 0%），<20 样本加「样本不足」徽章。
 */
export function ModelPerformance({ canViewDimension }: ModelPerformanceProps) {
  const { t, locale } = useI18n()
  const [range, setRange] = useState<ModelPerfRange>('24h')
  const [model, setModel] = useState<string | undefined>(undefined)
  const [dimensionChoice, setDimensionChoice] = useState<
    ModelPerfDimension | 'none'
  >('none')

  const dimension = canViewDimension && dimensionChoice !== 'none'
    ? dimensionChoice
    : undefined
  const query = useModelPerf({ range, model, dimension })
  const data = query.data

  const aggregated = useMemo(
    () => (data === undefined ? [] : aggregateBuckets(data.buckets)),
    [data],
  )

  const ttftSeries: ChartSeries[] = useMemo(
    () => [
      {
        id: 'ttft-p50',
        label: t('common.dashboard.performance.series.ttft_p50'),
        points: seriesFrom(aggregated, (point) => point.ttft50),
      },
      {
        id: 'ttft-p95',
        label: t('common.dashboard.performance.series.ttft_p95'),
        points: seriesFrom(aggregated, (point) => point.ttft95),
      },
    ],
    [aggregated, t],
  )

  const latencySeries: ChartSeries[] = useMemo(
    () => [
      {
        id: 'latency-p95',
        label: t('common.dashboard.performance.series.latency_p95'),
        points: seriesFrom(aggregated, (point) => point.latency95),
      },
    ],
    [aggregated, t],
  )

  const rateSeries: ChartSeries[] = useMemo(
    () => [
      {
        id: 'success-rate',
        label: t('common.dashboard.performance.series.success_rate'),
        points: seriesFrom(aggregated, (point) => point.success),
      },
      {
        id: 'uptime',
        label: t('common.dashboard.performance.series.uptime'),
        points: seriesFrom(aggregated, (point) => point.uptime),
      },
    ],
    [aggregated, t],
  )

  const summary = data?.summary
  const sampleSize = summary?.sample_size ?? null
  const lowSample = sampleSize !== null && sampleSize < LOW_SAMPLE_SIZE
  const emptyWindow =
    data !== undefined &&
    data.pipeline.enabled &&
    (sampleSize === null || sampleSize === 0)

  const comparisonDiscrepancy =
    data?.comparison !== null &&
    data?.comparison !== undefined &&
    data.comparison.delta !== null &&
    Math.abs(data.comparison.delta) > 0.01

  return (
    <section
      aria-labelledby="model-performance-title"
      data-testid="model-performance"
      className="mt-8 rounded-xl border border-border-hairline bg-surface-card p-4"
    >
      <div className="flex flex-wrap items-end justify-between gap-3">
        <div>
          <h2
            id="model-performance-title"
            className="text-lg font-semibold text-ink-1"
          >
            {t('common.dashboard.performance.title')}
          </h2>
          <p className="mt-1 text-[13px] text-ink-3">
            {t('common.dashboard.performance.subtitle')}
          </p>
        </div>

        <div className="flex flex-wrap items-end gap-3">
          <div
            role="radiogroup"
            aria-label={t('common.dashboard.performance.range_label')}
            onKeyDown={handleRadioGroupKeyDown}
            className="inline-flex rounded-lg border border-border-default bg-surface-card p-0.5"
          >
            {RANGES.map((value) => {
              const active = value === range
              return (
                <button
                  key={value}
                  type="button"
                  role="radio"
                  aria-checked={active}
                  tabIndex={active ? 0 : -1}
                  data-testid={`perf-range-${value}`}
                  onClick={() => {
                    setRange(value)
                  }}
                  className={[
                    'rounded-md px-2.5 py-1 text-[13px] focus-visible:shadow-[var(--focus-ring)] focus-visible:outline-none',
                    active
                      ? 'bg-surface-hover font-medium text-ink-1'
                      : 'text-ink-2 hover:text-ink-1',
                  ].join(' ')}
                >
                  {t(`common.dashboard.range.${value}`)}
                </button>
              )
            })}
          </div>

          <label className="flex flex-col gap-1 text-[13px] font-medium text-ink-1">
            {t('common.dashboard.performance.model')}
            <select
              data-testid="perf-model-select"
              aria-label={t('common.dashboard.performance.model')}
              value={model ?? ''}
              onChange={(event) => {
                const value = event.target.value
                setModel(value === '' ? undefined : value)
              }}
              className="h-9 min-w-40 rounded-lg border border-border-default bg-surface-card px-2 text-sm text-ink-1"
            >
              <option value="">
                {t('common.dashboard.performance.model_all')}
              </option>
              {(data?.models ?? []).map((row) => (
                <option key={row.model} value={row.model}>
                  {row.model}
                </option>
              ))}
            </select>
          </label>

          {canViewDimension ? (
            <div
              role="radiogroup"
              aria-label={t('common.dashboard.performance.dimension.label')}
              onKeyDown={handleRadioGroupKeyDown}
              className="inline-flex rounded-lg border border-border-default bg-surface-card p-0.5"
            >
              {(['none', 'channel', 'upstream'] as const).map((value) => {
                const active = dimensionChoice === value
                return (
                  <button
                    key={value}
                    type="button"
                    role="radio"
                    aria-checked={active}
                    tabIndex={active ? 0 : -1}
                    data-testid={`perf-dimension-${value}`}
                    onClick={() => {
                      setDimensionChoice(value)
                    }}
                    className={[
                      'rounded-md px-2.5 py-1 text-[13px] focus-visible:shadow-[var(--focus-ring)] focus-visible:outline-none',
                      active
                        ? 'bg-surface-hover font-medium text-ink-1'
                        : 'text-ink-2 hover:text-ink-1',
                    ].join(' ')}
                  >
                    {t(`common.dashboard.performance.dimension.${value}`)}
                  </button>
                )
              })}
            </div>
          ) : null}
        </div>
      </div>

      {query.isPending ? (
        <PerformanceSkeleton />
      ) : query.isError ? (
        <div
          data-testid="perf-error"
          role="alert"
          className="mt-4 rounded-lg border border-danger-line bg-danger-bg p-3 text-sm text-danger"
        >
          <p>{t('common.dashboard.performance.error')}</p>
          <button
            type="button"
            data-testid="perf-retry"
            onClick={() => void query.refetch()}
            className="mt-2 rounded border border-danger-line px-2 py-0.5 text-xs focus-visible:shadow-[var(--focus-ring)] focus-visible:outline-none"
          >
            {t('common.action.retry')}
          </button>
        </div>
      ) : data === undefined || !data.pipeline.enabled ? (
        // 管道闸门：独立说明，不显示任何历史数字（UI §8.7）。
        <div
          data-testid="perf-pipeline-disabled"
          className="mt-4 rounded-lg border border-warning-line bg-warning-bg p-4 text-sm text-ink-1"
        >
          <p className="font-medium">
            {t('common.dashboard.performance.pipeline_disabled')}
          </p>
          <p className="mt-1 text-ink-2">
            {t('common.dashboard.performance.pipeline_hint')}
          </p>
        </div>
      ) : (
        <div className="mt-4 flex flex-col gap-4">
          {data.pipeline.stale ? (
            <p data-testid="perf-stale" className="text-xs text-warning">
              {t('common.dashboard.performance.stale')}
            </p>
          ) : null}

          {comparisonDiscrepancy && data.comparison !== null ? (
            <div
              data-testid="perf-comparison"
              role="status"
              className="rounded-lg border border-warning-line bg-warning-bg p-3 text-sm text-ink-1"
            >
              <p className="font-medium">
                {t('common.dashboard.performance.comparison.title')}
              </p>
              <p className="mt-1 text-xs text-ink-2">
                {t('common.dashboard.performance.comparison.body', {
                  pipeline: formatPercent(
                    data.comparison.pipeline_success_rate,
                  ),
                  usage: formatPercent(
                    data.comparison.usage_logs_success_rate,
                  ),
                  delta: formatPercent(data.comparison.delta),
                })}
              </p>
            </div>
          ) : null}

          <div className="flex flex-wrap items-center gap-2 text-[13px] text-ink-2">
            <span>
              {t('common.dashboard.performance.sample_size', {
                count: formatCount(sampleSize, locale),
              })}
            </span>
            {lowSample ? (
              <span data-testid="perf-low-sample">
                <Tag variant="warning">
                  {t('common.dashboard.performance.low_sample')}
                </Tag>
              </span>
            ) : null}
            {data.pipeline.updated_at !== null ? (
              <span className="text-ink-3">
                {t('common.dashboard.performance.updated_at', {
                  time: formatInstant(data.pipeline.updated_at, locale),
                })}
              </span>
            ) : null}
          </div>

          {emptyWindow ? (
            <p data-testid="perf-empty" className="text-sm text-ink-2">
              {t('common.dashboard.performance.empty')}{' '}
              <a href="/tokens" className="text-brand underline">
                {t('common.dashboard.performance.empty_action')}
              </a>
            </p>
          ) : null}

          <div className="grid grid-cols-2 gap-4 md:grid-cols-3 xl:grid-cols-6">
            <StatCard
              testId="perf-card-ttft"
              label={t('common.dashboard.performance.cards.ttft_p95')}
              value={formatMs(summary?.ttft_p95_ms ?? null, summary?.ttft_p95_over_30s)}
              loading={false}
              retryLabel={t('common.action.retry')}
            />
            <StatCard
              testId="perf-card-latency"
              label={t('common.dashboard.performance.cards.latency_p95')}
              value={formatMs(
                summary?.latency_p95_ms ?? null,
                summary?.latency_p95_over_30s,
              )}
              loading={false}
              retryLabel={t('common.action.retry')}
            />
            <StatCard
              testId="perf-card-success"
              label={t('common.dashboard.performance.cards.success_rate')}
              value={formatPercent(summary?.success_rate ?? null)}
              // refused 计入成功率分母但不算失败，必须在 KPI 区单独可见
              //（UI §8.2 / §8.4）。
              subline={t('common.dashboard.performance.refused', {
                count: formatCount(summary?.refused ?? 0, locale),
              })}
              loading={false}
              retryLabel={t('common.action.retry')}
            />
            <StatCard
              testId="perf-card-uptime"
              label={t('common.dashboard.performance.cards.uptime')}
              value={formatPercent(summary?.uptime ?? null, 2)}
              loading={false}
              retryLabel={t('common.action.retry')}
            />
            <StatCard
              testId="perf-card-incidents"
              label={t('common.dashboard.performance.cards.incidents')}
              value={formatCount(summary?.incidents ?? null, locale)}
              loading={false}
              retryLabel={t('common.action.retry')}
            />
            <StatCard
              testId="perf-card-tps"
              label={t('common.dashboard.performance.cards.tps')}
              value={formatTps(summary?.tps ?? null, locale)}
              unit={t('common.dashboard.performance.units.tps')}
              loading={false}
              retryLabel={t('common.action.retry')}
            />
          </div>

          <div className="grid gap-4 xl:grid-cols-2">
            <ChartPanel
              testId="perf-chart-ttft"
              title={t('common.dashboard.performance.charts.ttft')}
              series={ttftSeries}
              config={{ kind: 'line', unit: 'ms' }}
            />
            <ChartPanel
              testId="perf-chart-latency"
              title={t('common.dashboard.performance.charts.latency')}
              series={latencySeries}
              config={{ kind: 'line', unit: 'ms' }}
            />
          </div>

          <ChartPanel
            testId="perf-chart-rates"
            title={t('common.dashboard.performance.charts.rates')}
            series={rateSeries}
            config={{ kind: 'line', unit: '%' }}
          />

          <IncidentList incidents={data.incidents} />

          {canViewDimension && dimension !== undefined ? (
            <DimensionTable data={data} locale={locale} />
          ) : null}

          {data.models.length > 0 ? (
            <ModelTable
              data={data}
              locale={locale}
              selectedModel={model}
              onSelectModel={setModel}
            />
          ) : null}
        </div>
      )}
    </section>
  )
}

function PerformanceSkeleton() {
  return (
    <div
      data-testid="perf-skeleton"
      aria-busy="true"
      className="mt-4 flex flex-col gap-4"
    >
      <div className="grid grid-cols-2 gap-4 md:grid-cols-3 xl:grid-cols-6">
        {Array.from({ length: 6 }, (_, index) => (
          <div
            key={index}
            className="h-24 animate-pulse rounded-xl bg-surface-hover motion-reduce:animate-none"
          />
        ))}
      </div>
      <div className="h-60 animate-pulse rounded-lg bg-surface-hover motion-reduce:animate-none" />
      <div className="h-40 animate-pulse rounded-lg bg-surface-hover motion-reduce:animate-none" />
    </div>
  )
}

function ChartPanel({
  testId,
  title,
  series,
  config,
}: {
  testId: string
  title: string
  series: ChartSeries[]
  config: ChartConfig
}) {
  const { t } = useI18n()
  const hasData = series.some((item) => item.points.length > 0)

  return (
    <section
      data-testid={testId}
      className="rounded-lg border border-border-hairline p-3"
    >
      <h3 className="text-sm font-semibold text-ink-1">{title}</h3>
      {hasData ? (
        <>
          <div className="mt-2">
            <AnalyticsChart series={series} config={config} />
          </div>
          {/* UI §8.5：每个图表都提供可展开的数据表等价文本。 */}
          <details className="mt-2 text-xs text-ink-2">
            <summary className="cursor-pointer text-brand">
              {t('common.dashboard.performance.view_data_table')}
            </summary>
            <table className="mt-2 w-full text-left">
              <thead>
                <tr>
                  <th scope="col" className="py-1 font-medium">
                    {t('common.dashboard.performance.table.series')}
                  </th>
                  <th scope="col" className="py-1 font-medium">
                    {t('common.dashboard.performance.table.time')}
                  </th>
                  <th scope="col" className="py-1 text-right font-medium">
                    {t('common.dashboard.performance.table.value')} ({config.unit})
                  </th>
                </tr>
              </thead>
              <tbody>
                {series.flatMap((item) =>
                  item.points.map((point) => (
                    <tr key={`${item.id}-${point.ts}`}>
                      <td className="py-0.5">{item.label}</td>
                      <td className="py-0.5">{point.ts}</td>
                      <td className="py-0.5 text-right tabular-nums">
                        {point.value}
                      </td>
                    </tr>
                  )),
                )}
              </tbody>
            </table>
          </details>
        </>
      ) : (
        <p className="mt-2 text-sm text-ink-3">
          {t('common.dashboard.performance.chart_empty')}
        </p>
      )}
    </section>
  )
}

function IncidentList({
  incidents,
}: {
  incidents: ModelPerfResponse['incidents']
}) {
  const { t, locale } = useI18n()

  return (
    <section
      data-testid="perf-incidents"
      aria-labelledby="perf-incidents-title"
      className="rounded-lg border border-border-hairline p-3"
    >
      <h3
        id="perf-incidents-title"
        className="text-sm font-semibold text-ink-1"
      >
        {t('common.dashboard.performance.incidents.title')}
      </h3>
      {incidents.length === 0 ? (
        <p className="mt-2 text-sm text-ink-3">
          {t('common.dashboard.performance.incidents.none')}
        </p>
      ) : (
        <ul className="mt-2 flex flex-col gap-2">
          {incidents.map((incident) => (
            <li
              key={`${incident.start}-${incident.end}`}
              data-testid="perf-incident"
              tabIndex={0}
              className="flex flex-wrap items-center gap-2 rounded-lg bg-surface-subtle px-3 py-2 text-[13px] text-ink-1 focus-visible:shadow-[var(--focus-ring)] focus-visible:outline-none"
            >
              <span aria-hidden="true" className="text-danger">
                ▲
              </span>
              <span className="font-medium">
                {formatInstant(incident.start, locale)} –{' '}
                {formatInstant(incident.end, locale)}
              </span>
              <span className="text-ink-2">
                {formatDuration(incident.duration_seconds, locale)}
              </span>
              <span className="text-ink-2">
                {t('common.dashboard.performance.incidents.availability')}
              </span>
            </li>
          ))}
        </ul>
      )}
    </section>
  )
}

function DimensionTable({
  data,
  locale,
}: {
  data: ModelPerfResponse
  locale: string
}) {
  const { t } = useI18n()
  const rows = data.dimensions ?? []
  const dimension = rows[0]?.dimension ?? 'channel'

  return (
    <section
      data-testid="perf-dimensions"
      aria-labelledby="perf-dimensions-title"
      className="rounded-lg border border-border-hairline p-3"
    >
      <h3
        id="perf-dimensions-title"
        className="text-sm font-semibold text-ink-1"
      >
        {t('common.dashboard.performance.dimension.title', {
          dimension: t(
            `common.dashboard.performance.dimension.${dimension}`,
          ),
        })}
      </h3>
      {rows.length === 0 ? (
        <p className="mt-2 text-sm text-ink-3">
          {t('common.dashboard.performance.chart_empty')}
        </p>
      ) : (
        <div className="mt-2 overflow-x-auto">
          <table className="w-full text-sm">
            <thead>
              <tr className="border-b border-border-hairline text-left text-xs text-ink-2">
                <th scope="col" className="py-2 font-medium">
                  {t('common.dashboard.performance.dimension.key')}
                </th>
                <th scope="col" className="py-2 text-right font-medium">
                  {t('common.dashboard.performance.table.requests')}
                </th>
                <th scope="col" className="py-2 text-right font-medium">
                  {t('common.dashboard.performance.cards.success_rate')}
                </th>
                <th scope="col" className="py-2 text-right font-medium">
                  {t('common.dashboard.performance.cards.latency_p95')}
                </th>
                <th scope="col" className="py-2 text-right font-medium">
                  {t('common.dashboard.performance.cards.uptime')}
                </th>
              </tr>
            </thead>
            <tbody>
              {rows.map((row) => (
                <tr
                  key={`${row.dimension}-${row.key}`}
                  className="border-b border-border-hairline last:border-0"
                >
                  <td className="py-2 font-mono text-[13px]">{row.key}</td>
                  <td className="py-2 text-right tabular-nums">
                    {formatCount(row.requests, locale)}
                  </td>
                  <td className="py-2 text-right tabular-nums">
                    {formatPercent(row.success_rate)}
                  </td>
                  <td className="py-2 text-right tabular-nums">
                    {formatMs(row.latency_p95_ms)}
                  </td>
                  <td className="py-2 text-right tabular-nums">
                    {formatPercent(row.uptime, 2)}
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
    </section>
  )
}

function ModelTable({
  data,
  locale,
  selectedModel,
  onSelectModel,
}: {
  data: ModelPerfResponse
  locale: string
  selectedModel: string | undefined
  onSelectModel: (model: string | undefined) => void
}) {
  const { t } = useI18n()

  return (
    <section
      data-testid="perf-models"
      aria-labelledby="perf-models-title"
      className="rounded-lg border border-border-hairline p-3"
    >
      <h3 id="perf-models-title" className="text-sm font-semibold text-ink-1">
        {t('common.dashboard.performance.models.title')}
      </h3>
      <div className="mt-2 overflow-x-auto">
        <table className="w-full text-sm">
          <thead>
            <tr className="border-b border-border-hairline text-left text-xs text-ink-2">
              <th scope="col" className="py-2 font-medium">
                {t('common.dashboard.performance.models.model')}
              </th>
              <th scope="col" className="py-2 text-right font-medium">
                {t('common.dashboard.performance.models.ttft')}
              </th>
              <th scope="col" className="py-2 text-right font-medium">
                {t('common.dashboard.performance.cards.latency_p95')}
              </th>
              <th scope="col" className="py-2 text-right font-medium">
                {t('common.dashboard.performance.cards.success_rate')}
              </th>
              <th scope="col" className="py-2 text-right font-medium">
                {t('common.dashboard.performance.cards.uptime')}
              </th>
              <th scope="col" className="py-2 text-right font-medium">
                {t('common.dashboard.performance.cards.incidents')}
              </th>
              <th scope="col" className="py-2 text-right font-medium">
                {t('common.dashboard.performance.table.requests')}
              </th>
              <th scope="col" className="py-2 font-medium">
                {t('common.dashboard.performance.models.badge')}
              </th>
            </tr>
          </thead>
          <tbody>
            {data.models.map((row) => (
              <tr
                key={row.model}
                data-testid="perf-model-row"
                className={[
                  'border-b border-border-hairline last:border-0',
                  row.model === selectedModel ? 'bg-surface-hover' : '',
                ].join(' ')}
              >
                <th scope="row" className="py-2 text-left font-normal">
                  <button
                    type="button"
                    data-testid={`perf-model-${row.model}`}
                    onClick={() => {
                      onSelectModel(
                        row.model === selectedModel ? undefined : row.model,
                      )
                    }}
                    className="font-mono text-[13px] text-brand focus-visible:shadow-[var(--focus-ring)] focus-visible:outline-none"
                  >
                    {row.model}
                  </button>
                </th>
                <td className="py-2 text-right font-mono text-[13px] tabular-nums">
                  {formatMs(row.ttft_p50_ms)} / {formatMs(row.ttft_p95_ms)}
                </td>
                <td className="py-2 text-right tabular-nums">
                  {formatMs(row.latency_p95_ms)}
                </td>
                <td className="py-2 text-right tabular-nums">
                  {formatPercent(row.success_rate)}
                </td>
                <td className="py-2 text-right tabular-nums">
                  {formatPercent(row.uptime, 2)}
                </td>
                <td className="py-2 text-right tabular-nums">
                  {formatCount(row.incidents, locale)}
                </td>
                <td className="py-2 text-right tabular-nums">
                  {formatCount(row.sample_size, locale)}
                </td>
                <td className="py-2">
                  <ModelPerfBadge
                    badge={row.badge}
                    onSelect={() => {
                      onSelectModel(row.model)
                    }}
                  />
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      </div>
    </section>
  )
}
