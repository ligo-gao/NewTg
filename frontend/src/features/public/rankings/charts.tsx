/**
 * 排行榜专用 SVG/图形（不引入第三方图表库，公开页首屏预算见 web-00 §3.4）。
 *
 * 改版后页面不再画时间走势，这里只保留供应商份额条：一条 100% 堆叠横条，
 * 颜色复用 `--chart-cat-*` 槽位。精确值由份额条下方的供应商列表承载，
 * 图形本身只做 overview。
 *
 * 可访问性：图形 `role="img"` + 概述标签，每个分段带 `title`。
 */

import type { RankingsProvider } from './api'
import { formatCompact, formatPercent } from './format'
import { foldTopProviders } from './share'

/** 实体稳定槽位是 1–8（1-based）；Other/未知用中性灰，绝不生成第 9 色。 */
export function seriesColor(slot: number | null): string {
  if (slot === null || !Number.isFinite(slot)) return 'var(--ink-3)'
  const clamped = Math.min(Math.max(Math.trunc(slot), 1), 8)
  return `var(--chart-cat-${clamped})`
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
 * 不伪造时间趋势；精确值由供应商列表承载。
 */
export function ShareBar({
  providers,
  locale,
  ariaLabel,
  otherLabel,
}: ShareBarProps) {
  const entries = foldTopProviders(providers, otherLabel)

  return (
    <div data-testid="share-bar" role="img" aria-label={ariaLabel} className="share-bar">
      {entries.map((entry) => (
        <div
          key={entry.key}
          className="share-segment"
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
