/**
 * 排行榜专用 SVG 图表（不引入第三方图表库，公开页首屏预算见 web-00 §3.4）。
 *
 * 【为什么没有复用 shared/ui/analytics-chart】现有组件的数据契约是
 * `ChartSeries{points:[{ts,value}]}` + line/bar/area，且坐标按时间点对齐。
 * 排行榜需要的是三种不同形状：横向对数条（按模型名次）、按桶堆叠柱
 * （`tokens: number[]` 与 `buckets` 索引对齐）、以及供应链的当前份额
 * 单条。把三种形态塞进现有 props 会迫使其改动公开契约（M6 明确要求
 * props 兼容），成本高于收益；因此这里独立实现，但复用同一套
 * `--chart-cat-*` 槽位与 `--ink-*` 文本色，视觉体系保持一致。
 *
 * 可访问性：每个图表 `role="img"` + 概述标签；精确值由同一面板里视觉隐藏
 * 的 `.rank-trend-list` 提供（移动端直接显示，≥621 保留在无障碍树中）。所有
 * 动画均为 CSS 过渡，`prefers-reduced-motion` 由 tokens.css 的全局媒体查询归零。
 */

import type { RankingsModel, RankingsProvider, RankingsTrendSeries } from './api'
import { formatBucketFull, formatBucketLabel, formatCompact, formatPercent } from './format'

/** 实体稳定槽位是 1–8（1-based）；Other/未知用中性灰，绝不生成第 9 色。 */
export function seriesColor(slot: number | null): string {
  if (slot === null || !Number.isFinite(slot)) return 'var(--ink-3)'
  const clamped = Math.min(Math.max(Math.trunc(slot), 1), 8)
  return `var(--chart-cat-${clamped})`
}

interface ModelMagnitudeChartProps {
  models: readonly RankingsModel[]
  locale: string
  ariaLabel: string
  /** tooltip 的字段名走 i18n，不在 SVG 里硬编码英文。 */
  tokensLabel: string
  requestsLabel: string
}

const MODEL_ROW_HEIGHT = 56
const MODEL_VIEW_WIDTH = 640
const MODEL_LEFT = 176
const MODEL_RIGHT_READOUT = 64
const MODEL_PLOT_WIDTH = MODEL_VIEW_WIDTH - MODEL_LEFT - MODEL_RIGHT_READOUT
const MODEL_TOP = 26

/**
 * 模型榜量级脊线：对数刻度（十进），条从共同左基线出发，数据端 4px 圆角。
 * 单色第 1 槽——Top10 模型数量超过 8 个分类槽，分类色会迫使循环，而
 * 「排名」本身就是顺序信息，颜色不承担身份。
 */
export function ModelMagnitudeChart({
  models,
  locale,
  ariaLabel,
  tokensLabel,
  requestsLabel,
}: ModelMagnitudeChartProps) {
  const top = models.slice(0, 10)
  const maxTokens = Math.max(1, ...top.map((model) => model.total_tokens))
  const maxExponent = Math.max(1, Math.ceil(Math.log10(maxTokens)))
  const height = MODEL_TOP + top.length * MODEL_ROW_HEIGHT + 8

  /**
   * L4：刻度按 `exponent / maxExponent` 归一，而不是除以 `log10(maxTokens)`。
   * 后者在 maxTokens 不是 10 的整数幂时会让最大十进制刻度（10^maxExponent）
   * 落到绘图区之外，轴线与标签越界；按整数指数归一后 0..maxExponent 恰好
   * 铺满 0..PLOT_WIDTH。
   */
  const xForTokens = (tokens: number): number => {
    if (tokens <= 0) return 0
    return (Math.log10(tokens) / maxExponent) * MODEL_PLOT_WIDTH
  }

  return (
    <div data-testid="model-magnitude-chart" className="hidden w-full md:block">
      <svg
        role="img"
        aria-label={ariaLabel}
        viewBox={`0 0 ${MODEL_VIEW_WIDTH} ${height}`}
        className="h-auto w-full"
      >
        {/* 十进刻度脊线（发丝线，仅装饰，数值由读数/表格承载）。 */}
        {Array.from({ length: maxExponent + 1 }, (_, exponent) => exponent).map(
          (exponent) => {
            const x = MODEL_LEFT + xForTokens(10 ** exponent)
            return (
              <g key={exponent}>
                <line
                  x1={x}
                  y1={MODEL_TOP - 8}
                  x2={x}
                  y2={height - 8}
                  stroke="var(--border-hairline)"
                />
                <text
                  x={x}
                  y={12}
                  textAnchor={exponent === maxExponent ? 'end' : 'middle'}
                  className="fill-ink-3 text-badge tabular-nums"
                >
                  {formatCompact(10 ** exponent, locale)}
                </text>
              </g>
            )
          },
        )}

        {top.map((model, index) => {
          const top_y = MODEL_TOP + index * MODEL_ROW_HEIGHT
          const barWidth = xForTokens(model.total_tokens)
          return (
            <g
              key={`${model.rank}-${model.model}`}
              data-model={model.model}
              className="text-ink-1"
            >
              <title>
                {`${model.display_name} · ${formatCompact(
                  model.total_tokens,
                  locale,
                )} ${tokensLabel} · ${formatCompact(
                  model.requests,
                  locale,
                )} ${requestsLabel}`}
              </title>
              <text
                x={4}
                y={top_y + 22}
                className="fill-ink-3 text-badge tabular-nums"
              >
                {model.rank}
              </text>
              <text
                x={26}
                y={top_y + 22}
                className="fill-ink-1 text-badge font-semibold"
              >
                {model.display_name}
              </text>
              <text x={26} y={top_y + 38} className="fill-ink-2 text-badge">
                {model.author}
              </text>
              <rect
                data-magnitude-bar="true"
                x={MODEL_LEFT}
                y={top_y + 12}
                width={Math.max(barWidth, model.total_tokens > 0 ? 2 : 0)}
                height={20}
                rx={4}
                fill="var(--chart-cat-1)"
              />
              <text
                x={MODEL_VIEW_WIDTH - 4}
                y={top_y + 27}
                textAnchor="end"
                className="fill-ink-1 text-badge font-semibold tabular-nums"
              >
                {formatCompact(model.total_tokens, locale)}
              </text>
            </g>
          )
        })}
      </svg>
    </div>
  )
}

interface TrendChartProps {
  series: readonly RankingsTrendSeries[]
  buckets: readonly string[]
  granularity: 'hour' | 'day' | 'week'
  locale: string
  ariaLabel: string
}

const TREND_WIDTH = 640
const TREND_HEIGHT = 320
const TREND_LEFT = 54
const TREND_TOP = 14
const TREND_BOTTOM = 34
const TREND_PLOT_WIDTH = TREND_WIDTH - TREND_LEFT - 10
const TREND_PLOT_HEIGHT = TREND_HEIGHT - TREND_TOP - TREND_BOTTOM

/** 堆叠柱：每个时间桶一根，段间留 2px 承载面色缝；Y 轴为单一 token 轴。 */
export function TrendStackedChart({
  series,
  buckets,
  granularity,
  locale,
  ariaLabel,
}: TrendChartProps) {
  const totals = buckets.map((_, bucketIndex) =>
    series.reduce((sum, item) => sum + (item.tokens[bucketIndex] ?? 0), 0),
  )
  const maxTotal = Math.max(1, ...totals)
  const step = TREND_PLOT_WIDTH / Math.max(buckets.length, 1)
  const barWidth = Math.max(Math.min(step * 0.6, 40), 4)
  const tickIndexes = [
    ...new Set([0, Math.floor((buckets.length - 1) / 2), buckets.length - 1]),
  ].filter((index) => index >= 0)

  return (
    <div data-testid="trend-chart" className="hidden w-full md:block">
      <svg
        role="img"
        aria-label={ariaLabel}
        viewBox={`0 0 ${TREND_WIDTH} ${TREND_HEIGHT}`}
        className="h-auto w-full"
      >
        {[0, 0.5, 1].map((fraction) => {
          const y = TREND_TOP + fraction * TREND_PLOT_HEIGHT
          return (
            <g key={fraction}>
              <line
                x1={TREND_LEFT}
                y1={y}
                x2={TREND_LEFT + TREND_PLOT_WIDTH}
                y2={y}
                stroke="var(--border-hairline)"
              />
              <text
                x={TREND_LEFT - 6}
                y={y + 3}
                textAnchor="end"
                className="fill-ink-3 text-badge tabular-nums"
              >
                {formatCompact(maxTotal * (1 - fraction), locale)}
              </text>
            </g>
          )
        })}

        {buckets.map((bucket, bucketIndex) => {
          const total = totals[bucketIndex] ?? 0
          const x = TREND_LEFT + bucketIndex * step + (step - barWidth) / 2
          let cursor = TREND_TOP + TREND_PLOT_HEIGHT
          return (
            <g key={bucket} data-bucket={bucket}>
              <title>
                {`${formatBucketFull(bucket)} · ${formatCompact(total, locale)}`}
              </title>
              {series.map((item) => {
                const value = item.tokens[bucketIndex] ?? 0
                if (value <= 0 || total <= 0) return null
                const rawHeight = (value / maxTotal) * TREND_PLOT_HEIGHT
                // 2px 色缝：段高不足 2px 时不再扣除，否则负高度会让柱形倒挂。
                const segmentHeight = Math.max(rawHeight - 2, 0.5)
                cursor -= rawHeight
                return (
                  <rect
                    key={`${item.model}-${bucket}`}
                    data-series={item.model}
                    x={x}
                    y={cursor + 2}
                    width={barWidth}
                    height={segmentHeight}
                    rx={1}
                    fill={seriesColor(item.slot)}
                  />
                )
              })}
            </g>
          )
        })}

        {tickIndexes.map((index) => {
          const bucket = buckets[index]
          if (bucket === undefined) return null
          return (
            <text
              key={bucket}
              x={TREND_LEFT + index * step + step / 2}
              y={TREND_HEIGHT - 10}
              textAnchor={
                index === 0
                  ? 'start'
                  : index === buckets.length - 1
                    ? 'end'
                    : 'middle'
              }
              className="fill-ink-3 text-badge tabular-nums"
            >
              {formatBucketLabel(bucket, granularity)}
            </text>
          )
        })}
      </svg>
    </div>
  )
}

interface ShareBarProps {
  providers: readonly RankingsProvider[]
  locale: string
  ariaLabel: string
  /** 溢出实体的显示名走 i18n；不硬编码英文 `Other`。 */
  otherLabel: string
}

/**
 * 供应商当前份额的 100% 堆叠条。契约明确 `RankingsProvider` 不含系列
 * （R-D6-05：供应商时间序列无存储），因此这里只画当前窗口的份额构成，
 * 不伪造时间趋势；精确值由右侧供应商排名列表与表格视图承载。
 */
export function ShareBar({
  providers,
  locale,
  ariaLabel,
  otherLabel,
}: ShareBarProps) {
  const top = providers.slice(0, 5)
  const rest = providers.slice(5)
  const otherTokens = rest.reduce((sum, item) => sum + item.total_tokens, 0)
  const otherShare = rest.reduce((sum, item) => sum + item.share_pct, 0)
  const entries = [
    ...top.map((item) => ({
      label: item.author,
      tokens: item.total_tokens,
      share: item.share_pct,
      slot: item.slot,
    })),
    ...(rest.length > 0
      ? [{ label: otherLabel, tokens: otherTokens, share: otherShare, slot: null }]
      : []),
  ]

  return (
    <div
      data-testid="share-bar"
      role="img"
      aria-label={ariaLabel}
      className="flex h-4 w-full overflow-hidden rounded-full"
    >
      {entries.map((entry) => (
        <div
          key={entry.label}
          data-series={entry.label}
          title={`${entry.label} · ${formatPercent(entry.share, locale)} · ${formatCompact(
            entry.tokens,
            locale,
          )}`}
          style={{
            width: `${Math.max(entry.share, 0)}%`,
            backgroundColor: seriesColor(entry.slot),
          }}
        />
      ))}
    </div>
  )
}
