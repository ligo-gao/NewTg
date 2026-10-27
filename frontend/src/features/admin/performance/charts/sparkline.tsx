import { sparkPath } from '../utils'

/**
 * 行内迷你折线：纯装饰，数值由相邻单元格的文本承载，所以整张 SVG 对
 * 辅助技术隐藏。`values` 允许 null（模型表的 24 点 spark 用 null 表示空小时
 * 或 p95 越过 30s 的小时）；null 处断线，而不是连成一条假趋势。
 *
 * `stroke` 固定 `currentColor`，颜色只由外层 tone 类决定（default 用分类色
 * cat-1、warning/danger 用状态色），这样暗色主题不需要第二套内联颜色。
 */

const TONE_CLASS = {
  default: 'text-chart-cat-1',
  warning: 'text-warning',
  danger: 'text-danger',
} as const

export type SparklineTone = keyof typeof TONE_CLASS

export interface SparklineProps {
  values: readonly (number | null)[]
  tone?: SparklineTone
  testId?: string
  /** 逻辑坐标宽高，只进 `viewBox`；实际尺寸由调用方 CSS 决定。 */
  width?: number
  height?: number
  className?: string
}

export function Sparkline({
  values,
  tone = 'default',
  testId,
  width = 72,
  height = 22,
  className,
}: SparklineProps) {
  const path = sparkPath(values, width, height)
  const svgClassName = ['block', TONE_CLASS[tone], className]
    .filter((value): value is string => Boolean(value))
    .join(' ')

  return (
    <svg
      aria-hidden="true"
      data-testid={testId}
      viewBox={`0 0 ${width} ${height}`}
      preserveAspectRatio="none"
      className={svgClassName}
    >
      {path === '' ? null : (
        <path
          d={path}
          fill="none"
          stroke="currentColor"
          strokeWidth={1.5}
          strokeLinecap="round"
          strokeLinejoin="round"
          vectorEffect="non-scaling-stroke"
        />
      )}
    </svg>
  )
}
