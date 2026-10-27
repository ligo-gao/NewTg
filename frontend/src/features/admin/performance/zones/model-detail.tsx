import { useQuery } from '@tanstack/react-query'

import { useI18n } from '@/shared/lib/i18n-context'
import { Button } from '@/shared/ui/button'

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
 * 写成 null——LineChart 在 null 处断开子路径，数据表该格显示 `—`（与后端
 * spark 的既有口径一致，不插值、不补 30000）。
 */

export interface ModelDetailProps {
  model: string
}

type NumericField = 'ttft_p50_ms' | 'ttft_p95_ms'
type OverField = 'ttft_p50_over_30s' | 'ttft_p95_over_30s'

/**
 * 桶 → `[epoch 毫秒, 值]`。`over_30s` 的点直接写 null：LineChart 现在按
 * null 断线（独立子路径）、数据表显示 `—`，tooltip 跳过该点——不伪造
 * 30000，也不跨空洞连线。
 */
function pointsFor(
  buckets: readonly ModelPerfBucket[],
  valueField: NumericField,
  overField: OverField,
): [number, number | null][] {
  return buckets.map((bucket) => [
    Date.parse(bucket.start),
    bucket[overField] ? null : bucket[valueField],
  ])
}

export function ModelDetail({ model }: ModelDetailProps) {
  const { t } = useI18n()

  const query = useQuery({
    queryKey: ['perf-model-series', model],
    queryFn: ({ signal }) => fetchModelPerfSeries(model, signal),
    // 管理观测页不自动重试：失败就在展开区显示可重试的错误态。
    retry: false,
  })

  // 请求中：给展开格一个稳定的占位，避免手风琴高度先塌后跳。骨架对辅助
  // 技术隐藏，外层 `role="status"` 只播报 busy 状态。
  if (query.isPending) {
    return (
      <div role="status" aria-busy="true" data-testid="model-detail-loading">
        <div
          aria-hidden="true"
          className="h-[220px] rounded-lg border border-border-hairline bg-surface-subtle"
        />
      </div>
    )
  }

  // 请求失败不能显示「无样本」：那是两种不同的结论。这里保留可重试的错误态，
  // 文案复用既有的性能错误键，不新增语言键。
  if (query.isError) {
    return (
      <div
        role="alert"
        data-testid="model-detail-error"
        className="rounded-lg border border-danger-line bg-danger-bg px-4 py-3.5"
      >
        <p className="text-meta font-semibold text-ink-1">
          {t('common.dashboard.performance.error')}
        </p>
        <Button
          variant="secondary"
          className="mt-2"
          onClick={() => void query.refetch()}
        >
          {t('common.action.retry')}
        </Button>
      </div>
    )
  }

  const buckets = query.data?.buckets ?? []
  const pipeline = query.data?.pipeline

  // 闸门（adjudications §2）：采集管道未启用或该模型没有任何桶时不画图。
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
