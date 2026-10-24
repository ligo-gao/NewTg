import { useQuery } from '@tanstack/react-query'
import { useMemo } from 'react'

import { ApiError } from '@/api/errors'
import { StatCard } from '@/features/dashboard/stat-card'
import type { ChartConfig, ChartSeries } from '@/shared/lib/chart-data'
import { useI18n } from '@/shared/lib/i18n-context'
import { AnalyticsChart } from '@/shared/ui/analytics-chart'
import { FormAlert } from '@/shared/ui/form'

import { getSummary, listRuns, type ReconcileSummary } from './api'

/** `api.ts` 只导出汇总信封；按天元素类型从它派生，不改动传输层。 */
type ReconcileSummaryDay = ReconcileSummary['days'][number]

/**
 * 概览 Tab（任务 15）。
 *
 * 汇总按天返回，卡片代表整个运行窗口：金额求和、差异率取「绝对值最大的
 * 一天」。没有按天 token 权重可用，所以真实的加权平均不可计算；显式的
 * 「最差一天」比一个悄悄算错的均值更诚实。
 *
 * 运行列表只用来解析 `token_tolerance_bp`（契约里没有单次运行的 GET）。
 * 运行不在第一页时宁可不画容差带，也不猜一个阈值。
 */

/** 与结果 Tab 的列表同前缀但带自己的窗口参数：两者参数不同，不能共享缓存。 */
const RUNS_QUERY_KEY = ['upstream-reconciliations', 'runs', 'overview'] as const
const SUMMARY_QUERY_KEY = ['upstream-reconciliations', 'summary'] as const

/** 容差查询只读第一页：目标是最近运行的快照值，不是全量遍历。 */
const RUNS_PAGE = 1
const RUNS_PAGE_SIZE = 20

export interface OverviewTabProps {
  runId: number | null
}

interface KpiWindow {
  revenue: number | null
  upstreamCost: number | null
  margin: number | null
  /** 绝对差异最大的一天；null = 整个窗口都没有可比对的差异率。 */
  delta: { value: number; date: string } | null
}

function problemMessage(error: unknown): string {
  if (error instanceof ApiError) return error.detail ?? error.title
  return error instanceof Error ? error.message : String(error)
}

/** 分 → 元是纯展示换算；`toFixed` 不参与求和，NULL 保留为长破折号。 */
function formatMinor(value: number | null): string {
  return value === null ? '—' : `¥${(value / 100).toFixed(2)}`
}

function formatBp(value: number | null): string {
  return value === null ? '—' : String(Math.round(value))
}

/** 只对非 NULL 天求和；全部为 NULL 时返回 NULL（未知 ≠ ¥0.00）。 */
function sumMinor(
  days: readonly ReconcileSummaryDay[],
  field: 'revenue_minor' | 'upstream_cost_minor' | 'margin_minor',
): number | null {
  let total = 0
  let seen = false
  for (const day of days) {
    const value = day[field]
    if (value === null || value === undefined) continue
    total += value
    seen = true
  }
  return seen ? total : null
}

/** `usage_date` 是 ISO 日历日：字典序即时间序，且不受词典排序规则影响。 */
function compareUsageDate(a: ReconcileSummaryDay, b: ReconcileSummaryDay): number {
  if (a.usage_date === b.usage_date) return 0
  return a.usage_date < b.usage_date ? -1 : 1
}

/**
 * 先按 `usage_date` 升序，绝对值相同的最差日取更早一天：服务端排序对
 * 汇总端点不作承诺，平手也需要一个确定答案。
 */
function aggregate(days: readonly ReconcileSummaryDay[]): KpiWindow {
  const ordered = [...days].sort(compareUsageDate)
  let delta: { value: number; date: string } | null = null
  for (const day of ordered) {
    const value = day.delta_token_bp
    if (value === null || value === undefined) continue
    if (delta === null || Math.abs(value) > Math.abs(delta.value)) {
      delta = { value, date: day.usage_date }
    }
  }
  return {
    revenue: sumMinor(ordered, 'revenue_minor'),
    upstreamCost: sumMinor(ordered, 'upstream_cost_minor'),
    margin: sumMinor(ordered, 'margin_minor'),
    delta,
  }
}

/**
 * 值域同时覆盖数据点与 ±容差，并留 10% 余量：参考线不能贴在边框上，否则
 * 与坐标轴混在一起。没有数据也没有容差时交给图表自己的空态。
 */
function buildDomain(
  values: readonly number[],
  tolerance: number | null,
): readonly [number, number] | undefined {
  if (values.length === 0 && tolerance === null) return undefined
  let min = values.length === 0 ? 0 : Math.min(...values)
  let max = values.length === 0 ? 0 : Math.max(...values)
  if (tolerance !== null) {
    min = Math.min(min, -tolerance)
    max = Math.max(max, tolerance)
  }
  const span = max - min
  const padding = span === 0 ? Math.max(Math.abs(max), 1) : span * 0.1
  return [min - padding, max + padding]
}

export function OverviewTab({ runId }: OverviewTabProps) {
  const { t } = useI18n()

  // 运行列表与汇总并行读取：容差带晚到不影响卡片与趋势线先渲染。
  const runsQuery = useQuery({
    queryKey: [...RUNS_QUERY_KEY, RUNS_PAGE, RUNS_PAGE_SIZE] as const,
    queryFn: ({ signal }) =>
      listRuns({ p: RUNS_PAGE, page_size: RUNS_PAGE_SIZE }, signal),
    enabled: runId !== null,
    retry: false,
    staleTime: 30_000,
  })

  const summaryQuery = useQuery({
    queryKey: [...SUMMARY_QUERY_KEY, runId] as const,
    queryFn: ({ signal }) => {
      if (runId === null) throw new Error('summary query without a run id')
      return getSummary(runId, signal)
    },
    enabled: runId !== null,
    retry: false,
    staleTime: 30_000,
  })

  const tolerance =
    runId === null
      ? null
      : (runsQuery.data?.items.find((run) => run.id === runId)
          ?.token_tolerance_bp ?? null)

  const days = summaryQuery.data?.days
  const kpis = useMemo(() => aggregate(days ?? []), [days])

  const chart = useMemo((): {
    series: ChartSeries[]
    config: ChartConfig
    referenceLines: { value: number; label: string }[] | undefined
  } => {
    const points = [...(days ?? [])]
      .sort(compareUsageDate)
      .flatMap((day) => {
        const value = day.delta_token_bp
        if (value === null || value === undefined) return []
        // 契约里的 usage_date 是 UTC 日历日：固定 T00:00:00Z，避免宿主时区
        // 把某一天挪到前/后一格。
        return [{ ts: `${day.usage_date}T00:00:00Z`, value }]
      })

    const toleranceLabel =
      tolerance === null
        ? ''
        : t('admin.upstream_reconciliation.overview.tolerance', {
            bp: tolerance,
          })

    return {
      // 单一序列：容差带是参考线，不进入 series（否则会被画成第二条曲线）。
      series: [
        {
          id: 'delta_token_bp',
          label: t('admin.upstream_reconciliation.overview.trend_series'),
          points,
        },
      ],
      config: {
        kind: 'line',
        unit: 'bp',
        domain: buildDomain(
          points.map((point) => point.value),
          tolerance,
        ),
      },
      referenceLines:
        tolerance === null
          ? undefined
          : [
              { value: tolerance, label: toleranceLabel },
              { value: -tolerance, label: toleranceLabel },
            ],
    }
  }, [days, tolerance, t])

  if (runId === null) {
    return (
      <div className="flex flex-col gap-4" data-testid="overview-tab">
        <FormAlert tone="info">
          {t('admin.upstream_reconciliation.overview.no_run')}
        </FormAlert>
      </div>
    )
  }

  const retry = (): void => {
    void summaryQuery.refetch()
  }
  // 每个卡片的错误重试按钮必须可区分：读屏/键盘用户听到的是「Retry 收入」，
  // 而不是四个一模一样的「Retry」。
  const retryLabelFor = (metric: string): string =>
    t('admin.upstream_reconciliation.overview.retry_for', { metric })
  const revenueLabel = t('admin.upstream_reconciliation.overview.revenue')
  const upstreamCostLabel = t(
    'admin.upstream_reconciliation.overview.upstream_cost',
  )
  const marginLabel = t('admin.upstream_reconciliation.overview.margin')
  const deltaLabel = t('admin.upstream_reconciliation.overview.delta_bp')
  const loading = summaryQuery.isPending
  const failed = summaryQuery.isError
  const hasDays = days !== undefined && days.length > 0

  return (
    <div className="flex flex-col gap-4" data-testid="overview-tab">
      {failed ? (
        <FormAlert tone="danger">
          {problemMessage(summaryQuery.error)}
        </FormAlert>
      ) : null}

      {summaryQuery.isSuccess && !hasDays ? (
        <FormAlert tone="info">
          {t('admin.upstream_reconciliation.overview.empty')}
        </FormAlert>
      ) : (
        <>
          <div className="grid grid-cols-2 gap-4 lg:grid-cols-4">
            <StatCard
              testId="overview-revenue"
              label={revenueLabel}
              value={formatMinor(kpis.revenue)}
              loading={loading}
              error={failed}
              onRetry={retry}
              retryLabel={retryLabelFor(revenueLabel)}
            />
            <StatCard
              testId="overview-upstream-cost"
              label={upstreamCostLabel}
              value={formatMinor(kpis.upstreamCost)}
              loading={loading}
              error={failed}
              onRetry={retry}
              retryLabel={retryLabelFor(upstreamCostLabel)}
            />
            <StatCard
              testId="overview-margin"
              label={marginLabel}
              value={formatMinor(kpis.margin)}
              loading={loading}
              error={failed}
              onRetry={retry}
              retryLabel={retryLabelFor(marginLabel)}
            />
            <StatCard
              testId="overview-delta-bp"
              label={deltaLabel}
              value={formatBp(kpis.delta?.value ?? null)}
              unit="bp"
              subline={
                kpis.delta === null
                  ? undefined
                  : t('admin.upstream_reconciliation.overview.delta_worst_day', {
                      date: kpis.delta.date,
                    })
              }
              loading={loading}
              error={failed}
              onRetry={retry}
              retryLabel={retryLabelFor(deltaLabel)}
            />
          </div>

          {hasDays ? (
            <section className="rounded-xl border border-border-hairline bg-surface-card p-4">
              <h2 className="text-body font-medium text-ink-1">
                {t('admin.upstream_reconciliation.overview.trend_title')}
              </h2>
              <AnalyticsChart
                series={chart.series}
                config={chart.config}
                referenceLines={chart.referenceLines}
                tooltip
              />
            </section>
          ) : null}
        </>
      )}
    </div>
  )
}
