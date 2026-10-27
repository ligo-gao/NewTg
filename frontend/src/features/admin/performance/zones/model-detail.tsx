import { useQuery } from '@tanstack/react-query'

import { useI18n } from '@/shared/lib/i18n-context'

import { fetchModelPerfSeries, type ModelPerfBucket } from '../api'
import { LineChart, type LineChartSeries } from '../charts/line-chart'
import { formatMs } from '../utils'

/**
 * 单模型下钻曲线（Task 16 / adjudications §2）。
 *
 * 这个组件**只在行展开时挂载**：折叠态不渲染它，也就不会建立 `useQuery`，
 * 自然没有预取（模型的 24h 分桶序列粒度是分钟级，提前拉会白烧后端）。
 *
 * x 轴必须是 `Date.parse(bucket.start)` 的 epoch 毫秒：LineChart 的 x 是数值
 * 时间戳，传桶序号会把 1970 年当日期渲染。越过 30s 的桶没有有限值，点位
 * 写成 null——LineChart 用 `Number.isFinite` 过滤 y，折线不经过该点，数据表
 * 该格显示 `—`（与后端 spark 的既有口径一致，不插值、不补 30000）。
 */

export interface ModelDetailProps {
  model: string
}

type NumericField = 'ttft_p50_ms' | 'ttft_p95_ms'
type OverField = 'ttft_p50_over_30s' | 'ttft_p95_over_30s'

/**
 * 桶 → `[epoch 毫秒, 值]`。`over_30s` 的点写成 null：LineChart 的序列类型
 * 声明为 `[number, number][]`，但运行时按 `Number.isFinite` 丢弃 y，所以
 * 这里对 null 做一次有意的断言，换取「该点无有限值」而不是伪造 30000。
 */
function pointsFor(
  buckets: readonly ModelPerfBucket[],
  valueField: NumericField,
  overField: OverField,
): [number, number][] {
  return buckets.map((bucket) => {
    const value = bucket[overField] ? null : bucket[valueField]
    return [Date.parse(bucket.start), value as number]
  })
}

export function ModelDetail({ model }: ModelDetailProps) {
  const { t } = useI18n()

  const query = useQuery({
    queryKey: ['perf-model-series', model],
    queryFn: ({ signal }) => fetchModelPerfSeries(model, signal),
    // 管理观测页不自动重试：失败就在展开区显示空态，刷新由页面负责。
    retry: false,
  })

  // 首次请求中不渲染「无样本」：那会把加载态误报成空数据。
  if (query.isPending) return null

  const buckets = query.data?.buckets ?? []
  const pipeline = query.data?.pipeline

  // 闸门（adjudications §2）：采集管道未启用或该模型没有任何桶时不画图。
  // 查询失败同样落在这里——没有数据就显示空态，不伪造成一条平线。
  if (pipeline === undefined || !pipeline.enabled || buckets.length === 0) {
    return (
      <div
        data-testid="model-detail-empty"
        className="rounded-lg border border-dashed border-border-default bg-surface-subtle px-4 py-3.5"
      >
        <p className="font-semibold text-ink-1">
          {t('admin.performance.models.empty_title')}
        </p>
        <p className="mt-1 text-meta text-ink-2">
          {t('admin.performance.models.empty_note')}
        </p>
      </div>
    )
  }

  const samples = buckets.map((bucket) => bucket.requests)
  const series: LineChartSeries[] = [
    {
      key: 'ttft_p50',
      label: t('admin.performance.detail.legend_p50'),
      color: 'var(--chart-cat-1)',
      tableLabel: t('admin.performance.detail.table_p50'),
      points: pointsFor(buckets, 'ttft_p50_ms', 'ttft_p50_over_30s'),
      samples,
    },
    {
      key: 'ttft_p95',
      label: t('admin.performance.detail.legend_p95'),
      color: 'var(--chart-cat-3)',
      tableLabel: t('admin.performance.detail.table_p95'),
      points: pointsFor(buckets, 'ttft_p95_ms', 'ttft_p95_over_30s'),
      samples,
    },
  ]

  return (
    <div data-testid="model-detail">
      <div className="mb-3">
        <h3 className="text-meta font-semibold text-ink-1">
          {t('admin.performance.detail.title', { model })}
        </h3>
        <p className="mt-0.5 text-badge text-ink-3">
          {t('admin.performance.detail.sub')}
        </p>
      </div>
      <LineChart
        series={series}
        title={t('admin.performance.detail.title', { model })}
        unit="ms"
        tickFormatter={formatMs}
      />
    </div>
  )
}
