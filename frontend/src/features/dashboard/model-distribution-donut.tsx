import { useMemo } from 'react'

import { useI18n } from '@/shared/lib/i18n-context'

import type { UsageStatsByModelItem } from './api'
import { normalizeModelDistribution } from './distribution'
import { formatCreditsValue } from './format'

export interface ModelDistributionDonutProps {
  items: UsageStatsByModelItem[]
  loading: boolean
  error: boolean
  onRetry: () => void
  selectedModel?: string
  onSelectModel: (model: string | undefined) => void
}

const SIZE = 180
const CENTER = SIZE / 2
const RADIUS = 68
const STROKE = 22
/** 2px seam between sectors, expressed in pathLength units (100 = full ring). */
const GAP = 0.7

/**
 * Accessible SVG donut (no chart library, R-01). The arcs are a visual
 * summary; the legend carries the same values and percentages as text, and
 * named slices are buttons so the trend drill-down is keyboard reachable.
 * `other` is neutral, never a ninth categorical colour, and is not drillable
 * because there is no single model behind it.
 */
export function ModelDistributionDonut({
  items,
  loading,
  error,
  onRetry,
  selectedModel,
  onSelectModel,
}: ModelDistributionDonutProps) {
  const { t, locale } = useI18n()
  const slices = useMemo(() => normalizeModelDistribution(items), [items])
  const total = slices.reduce((sum, slice) => sum + slice.cost, 0)

  return (
    <section className="rounded-xl border border-border-hairline bg-surface-card p-4">
      <h3 className="text-sm font-semibold text-ink-1">
        {t('common.dashboard.donut.title')}
      </h3>

      {loading ? (
        <div
          data-testid="donut-loading"
          aria-busy="true"
          className="mt-4 h-60 animate-pulse rounded-lg bg-surface-hover motion-reduce:animate-none"
        />
      ) : error ? (
        <div data-testid="donut-error" className="mt-4 rounded-lg border border-danger-line bg-danger-bg p-3 text-sm text-danger">
          <p>{t('common.dashboard.error')}</p>
          <button
            type="button"
            data-testid="donut-retry"
            onClick={onRetry}
            className="mt-2 rounded border border-danger-line px-2 py-0.5 text-xs focus-visible:shadow-[var(--focus-ring)] focus-visible:outline-none"
          >
            {t('common.action.retry')}
          </button>
        </div>
      ) : slices.length === 0 ? (
        <div
          data-testid="donut-empty"
          className="mt-4 flex min-h-60 items-center justify-center rounded-lg border border-dashed border-neutral-line text-sm text-neutral"
        >
          {t('common.dashboard.donut.empty')}
        </div>
      ) : (
        <div data-testid="model-donut" className="mt-2 flex flex-col items-center gap-4 lg:flex-row lg:items-start">
          <svg
            role="img"
            aria-label={t('common.dashboard.donut.aria', {
              count: slices.length,
              total: formatCreditsValue(total, locale),
            })}
            viewBox={`0 0 ${SIZE} ${SIZE}`}
            className="h-48 w-48 shrink-0"
          >
            <circle
              cx={CENTER}
              cy={CENTER}
              r={RADIUS}
              fill="none"
              stroke="var(--border-hairline)"
              strokeWidth={STROKE}
            />
            {slices.map((slice, index) => {
              const offset = slices
                .slice(0, index)
                .reduce((sum, previous) => sum + previous.percent, 0)
              const visible = Math.max(slice.percent - GAP, 0)
              return (
                <circle
                  key={slice.key}
                  data-testid="donut-segment"
                  data-model={slice.key}
                  data-percent={slice.percent}
                  cx={CENTER}
                  cy={CENTER}
                  r={RADIUS}
                  fill="none"
                  stroke={slice.color}
                  strokeWidth={STROKE}
                  pathLength={100}
                  strokeDasharray={`${visible} ${100 - visible}`}
                  strokeDashoffset={-offset}
                  transform={`rotate(-90 ${CENTER} ${CENTER})`}
                  onClick={
                    slice.isOther
                      ? undefined
                      : () =>
                          onSelectModel(
                            selectedModel === slice.model ? undefined : slice.model,
                          )
                  }
                  className={slice.isOther ? undefined : 'cursor-pointer'}
                />
              )
            })}
            <text
              x={CENTER}
              y={CENTER - 2}
              textAnchor="middle"
              className="fill-ink-1 text-sm font-semibold"
            >
              {formatCreditsValue(total, locale)}
            </text>
            <text
              x={CENTER}
              y={CENTER + 14}
              textAnchor="middle"
              className="fill-ink-2 text-[8px]"
            >
              {t('common.dashboard.rates.credits')}
            </text>
          </svg>

          <ul className="flex min-w-0 flex-1 flex-col gap-0.5">
            {slices.map((slice) => {
              const label = slice.isOther
                ? t('common.dashboard.donut.other')
                : slice.model
              const content = (
                <>
                  <span
                    aria-hidden="true"
                    className="h-2.5 w-2.5 shrink-0 rounded-full"
                    style={{ backgroundColor: slice.color }}
                  />
                  <span className="min-w-0 flex-1 truncate text-left text-[13px] text-ink-1">
                    {label}
                  </span>
                  <span className="tabular-nums text-[13px] text-ink-2">
                    {formatCreditsValue(slice.cost, locale)}
                  </span>
                  <span className="w-14 text-right tabular-nums text-[13px] text-ink-2">
                    {slice.percent.toFixed(1)}%
                  </span>
                </>
              )
              return (
                <li
                  key={slice.key}
                  data-testid={`donut-legend-${slice.key}`}
                  data-percent={slice.percent}
                >
                  {slice.isOther ? (
                    <div className="flex items-center gap-2 px-1 py-1">{content}</div>
                  ) : (
                    <button
                      type="button"
                      aria-pressed={selectedModel === slice.model}
                      aria-label={t('common.dashboard.donut.legend_aria', {
                        model: slice.model,
                        cost: formatCreditsValue(slice.cost, locale),
                        percent: slice.percent.toFixed(1),
                      })}
                      onClick={() =>
                        onSelectModel(
                          selectedModel === slice.model ? undefined : slice.model,
                        )
                      }
                      className={[
                        'flex w-full items-center gap-2 rounded px-1 py-1 hover:bg-surface-hover focus-visible:shadow-[var(--focus-ring)] focus-visible:outline-none',
                        selectedModel === slice.model ? 'bg-surface-hover' : '',
                      ].join(' ')}
                    >
                      {content}
                    </button>
                  )}
                </li>
              )
            })}
          </ul>
        </div>
      )}
    </section>
  )
}
