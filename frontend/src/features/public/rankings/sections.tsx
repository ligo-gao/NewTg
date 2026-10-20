/**
 * 排行榜四个数据区块（UI §F-09-30 ~ F-09-33）。每个区块自包含视图切换，
 * 数据缺失时只降级自身，不影响其他区块。
 *
 * 图表默认形态 + 表格视图是 relief 规则的一部分：亮色槽位在浅色底上可能
 * 低于 3:1，精确值必须一键可得。移动端图表降级为卡片/纵向堆叠条列，
 * 不做横向滚动。
 */

import { useEffect, useRef, useState } from 'react'

import type { TranslateFn } from '@/shared/lib/i18n-context'
import { useI18n } from '@/shared/lib/i18n-context'

import type {
  RankingsModel,
  RankingsMover,
  RankingsPeriod,
  RankingsProvider,
  RankingsTrend,
} from './api'
import {
  ModelMagnitudeChart,
  ShareBar,
  TrendStackedChart,
  seriesColor,
} from './charts'
import {
  PERIOD_LABEL_KEYS,
  formatBucketFull,
  formatBucketLabel,
  formatCompact,
  formatExact,
  formatPercent,
  widerPeriod,
} from './format'

type ViewMode = 'chart' | 'table'

function useCopyFeedback() {
  const [copied, setCopied] = useState<string | null>(null)
  const timer = useRef<ReturnType<typeof setTimeout> | null>(null)
  useEffect(
    () => () => {
      if (timer.current !== null) clearTimeout(timer.current)
    },
    [],
  )
  const copy = (name: string) => {
    void globalThis.navigator?.clipboard?.writeText?.(name).catch(() => {})
    setCopied(name)
    if (timer.current !== null) clearTimeout(timer.current)
    timer.current = setTimeout(() => setCopied(null), 2000)
  }
  return { copied, copy }
}

function ViewToggle({
  view,
  onChange,
}: {
  view: ViewMode
  onChange: (next: ViewMode) => void
}) {
  const { t } = useI18n()
  return (
    <div
      role="group"
      aria-label={t('public.rankings.view.label')}
      className="inline-flex rounded-lg bg-surface-subtle p-1"
    >
      {(['chart', 'table'] as const).map((mode) => (
        <button
          key={mode}
          type="button"
          aria-pressed={view === mode}
          onClick={() => onChange(mode)}
          className={`h-7 rounded-md px-3 text-xs font-medium focus-visible:shadow-[var(--focus-ring)] focus-visible:outline-none ${
            view === mode
              ? 'bg-surface-card text-brand shadow-sm'
              : 'text-ink-2 hover:text-ink-1'
          }`}
        >
          {t(
            mode === 'chart'
              ? 'public.rankings.view.chart'
              : 'public.rankings.view.table',
          )}
        </button>
      ))}
    </div>
  )
}

function SectionHeader({
  titleKey,
  view,
  onViewChange,
  toggle = true,
}: {
  titleKey: string
  view?: ViewMode
  onViewChange?: (next: ViewMode) => void
  toggle?: boolean
}) {
  const { t } = useI18n()
  return (
    <div className="flex items-center justify-between gap-3">
      <h2 className="text-base font-semibold leading-6 text-ink-1">
        {t(titleKey)}
      </h2>
      {toggle && view !== undefined && onViewChange !== undefined ? (
        <ViewToggle view={view} onChange={onViewChange} />
      ) : null}
    </div>
  )
}

function EmptyLine({ textKey }: { textKey: string }) {
  const { t } = useI18n()
  return (
    <p className="rounded-lg border border-dashed border-border-hairline px-4 py-8 text-center text-sm text-ink-2">
      {t(textKey)}
    </p>
  )
}

/** 升降标记：图标（▲/▼/—）+ 文字 + 发散色三重编码，不使用状态色。 */
export function DeltaMarker({ deltaPct }: { deltaPct: number | null }) {
  const { t, locale } = useI18n()
  if (deltaPct === null) {
    return (
      <span className="text-xs text-chart-div-mid">
        {t('public.rankings.delta.new')}
      </span>
    )
  }
  if (deltaPct > 0) {
    return (
      <span className="text-xs font-medium tabular-nums text-chart-div-pos">
        ▲ {formatPercent(deltaPct, locale)}
      </span>
    )
  }
  if (deltaPct < 0) {
    return (
      <span className="text-xs font-medium tabular-nums text-chart-div-neg">
        ▼ {formatPercent(Math.abs(deltaPct), locale)}
      </span>
    )
  }
  return <span className="text-xs text-chart-div-mid">—</span>
}

function PeriodChip({
  period,
  onSelectPeriod,
}: {
  period: RankingsPeriod
  onSelectPeriod: (next: RankingsPeriod) => void
}) {
  const { t } = useI18n()
  const wider = widerPeriod(period)
  if (wider === null) return null
  return (
    <button
      type="button"
      onClick={() => onSelectPeriod(wider)}
      className="rounded-full border border-border-default px-3 py-1 text-xs font-medium text-brand hover:border-brand focus-visible:shadow-[var(--focus-ring)] focus-visible:outline-none"
    >
      {t('public.rankings.model.switch_to', { period: t(PERIOD_LABEL_KEYS[wider]) })}
    </button>
  )
}

interface ModelSectionProps {
  models: readonly RankingsModel[]
  period: RankingsPeriod
  onSelectPeriod: (next: RankingsPeriod) => void
  locale: string
  t: TranslateFn
}

export function ModelRankingsSection({
  models,
  period,
  onSelectPeriod,
  locale,
  t,
}: ModelSectionProps) {
  const [view, setView] = useState<ViewMode>('chart')
  const [expanded, setExpanded] = useState(false)
  const { copied, copy } = useCopyFeedback()
  const top = models.slice(0, 10)
  const maxTokens = Math.max(1, ...top.map((model) => model.total_tokens))
  // 对数脊线的分母；maxTokens=1 时 log10 为 0，退回线性（宽度 0）。
  const logMax = maxTokens <= 1 ? 1 : Math.log10(maxTokens)

  return (
    <section id="model-rankings" className="scroll-mt-20">
      <SectionHeader
        titleKey="public.rankings.model.title"
        view={view}
        onViewChange={setView}
      />
      {models.length === 0 ? (
        <div className="mt-4 flex flex-col items-center gap-3">
          <EmptyLine textKey="public.rankings.model.empty" />
          <PeriodChip period={period} onSelectPeriod={onSelectPeriod} />
        </div>
      ) : view === 'chart' ? (
        <div className="mt-4">
          <ModelMagnitudeChart
            models={top}
            locale={locale}
            ariaLabel={t('public.rankings.model.chart_aria', { count: top.length })}
            tokensLabel={t('public.rankings.model.tokens')}
            requestsLabel={t('public.rankings.model.requests')}
          />
          {/* 移动端卡片：名次 + 模型 + 全宽细条 + 读数，无横向滚动。 */}
          <ul className="flex flex-col gap-3 md:hidden" data-testid="model-cards">
            {top.map((model) => (
              <li
                key={`${model.rank}-${model.model}`}
                className="rounded-lg border border-border-hairline bg-surface-card p-3"
              >
                <div className="flex items-baseline justify-between gap-2">
                  <div className="flex min-w-0 items-baseline gap-2">
                    <span className="w-6 shrink-0 text-xs tabular-nums text-ink-3">
                      {model.rank}
                    </span>
                    <span className="truncate text-sm font-semibold text-ink-1">
                      {model.display_name}
                    </span>
                  </div>
                  <DeltaMarker deltaPct={model.delta_pct} />
                </div>
                <p className="ml-8 truncate text-xs text-ink-2">{model.author}</p>
                <div className="ml-8 mt-2 h-2 rounded-full bg-surface-subtle">
                  <div
                    className="h-2 rounded-full"
                    style={{
                      width: `${Math.max(
                        (Math.log10(Math.max(model.total_tokens, 1)) / logMax) *
                          100,
                        model.total_tokens > 0 ? 2 : 0,
                      )}%`,
                      backgroundColor: 'var(--chart-cat-1)',
                    }}
                  />
                </div>
                <p className="ml-8 mt-1 text-xs tabular-nums text-ink-2">
                  {formatCompact(model.total_tokens, locale)}{' '}
                  {t('public.rankings.model.tokens')} ·{' '}
                  {formatCompact(model.requests, locale)}{' '}
                  {t('public.rankings.model.requests')}
                </p>
              </li>
            ))}
          </ul>
        </div>
      ) : (
        <div className="mt-4">
          <div className="overflow-x-auto rounded-lg border border-border-hairline">
            <table data-testid="model-table" className="w-full text-left text-sm">
              <thead className="bg-surface-subtle text-xs text-ink-2">
                <tr>
                  <th scope="col" className="px-3 py-2 font-medium">
                    {t('public.rankings.model.rank')}
                  </th>
                  <th scope="col" className="px-3 py-2 font-medium">
                    {t('public.rankings.model.model')}
                  </th>
                  <th scope="col" className="px-3 py-2 text-right font-medium">
                    {t('public.rankings.model.tokens')}
                  </th>
                  <th scope="col" className="px-3 py-2 text-right font-medium">
                    {t('public.rankings.model.requests')}
                  </th>
                  <th scope="col" className="px-3 py-2 text-right font-medium">
                    {t('public.rankings.model.delta')}
                  </th>
                </tr>
              </thead>
              <tbody>
                {(expanded ? models : models.slice(0, 10)).map((model) => (
                  <tr
                    key={`${model.rank}-${model.model}`}
                    className="border-t border-border-hairline"
                  >
                    <td className="px-3 py-2 tabular-nums text-ink-3">
                      {model.rank}
                    </td>
                    <td className="px-3 py-2">
                      <button
                        type="button"
                        onClick={() => copy(model.model)}
                        aria-label={t('public.rankings.model.copy_model', {
                          name: model.model,
                        })}
                        className="text-left font-medium text-ink-1 hover:text-brand focus-visible:shadow-[var(--focus-ring)] focus-visible:outline-none"
                      >
                        {model.display_name}
                      </button>
                      <span className="ml-2 text-xs text-ink-2">
                        {model.author}
                      </span>
                    </td>
                    <td className="px-3 py-2 text-right tabular-nums text-ink-1">
                      {formatExact(model.total_tokens, locale)}
                    </td>
                    <td className="px-3 py-2 text-right tabular-nums text-ink-2">
                      {formatExact(model.requests, locale)}
                    </td>
                    <td className="px-3 py-2 text-right">
                      <DeltaMarker deltaPct={model.delta_pct} />
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
          <div className="mt-2 flex items-center justify-between text-xs text-ink-3">
            {models.length > 10 ? (
              <button
                type="button"
                disabled={expanded}
                onClick={() => setExpanded(true)}
                className="rounded-lg border border-border-default px-3 py-1.5 font-medium text-ink-1 hover:border-brand disabled:cursor-not-allowed disabled:opacity-50"
              >
                {t('public.rankings.model.show_more')}
              </button>
            ) : (
              <span />
            )}
            <span aria-live="polite">
              {copied !== null ? t('public.rankings.model.copied') : ''}
            </span>
          </div>
        </div>
      )}
    </section>
  )
}

function MoverColumn({
  direction,
  items,
  maxAbs,
  locale,
}: {
  direction: 'up' | 'down'
  items: readonly RankingsMover[]
  maxAbs: number
  locale: string
}) {
  const { t } = useI18n()
  const { copied, copy } = useCopyFeedback()
  const color =
    direction === 'up' ? 'var(--chart-div-pos)' : 'var(--chart-div-neg)'
  const emptyKey =
    direction === 'up'
      ? 'public.rankings.momentum.no_up'
      : 'public.rankings.momentum.no_down'

  return (
    <div>
      <h3 className="text-sm font-semibold text-ink-1">
        {direction === 'up'
          ? `↑ ${t('public.rankings.momentum.rising')}`
          : `↓ ${t('public.rankings.momentum.falling')}`}
      </h3>
      {items.length === 0 ? (
        <p className="mt-3 text-sm text-ink-2">{t(emptyKey)}</p>
      ) : (
        <ul className="mt-3 flex flex-col gap-2">
          {items.slice(0, 5).map((item) => (
            <li key={item.model}>
              <button
                type="button"
                onClick={() => copy(item.model)}
                title={t('public.rankings.momentum.compare', {
                  current: formatExact(item.cur_tokens, locale),
                  previous: formatExact(item.prev_tokens, locale),
                })}
                className="w-full rounded-lg px-2 py-2 text-left hover:bg-surface-hover focus-visible:shadow-[var(--focus-ring)] focus-visible:outline-none"
              >
                <div className="flex items-baseline justify-between gap-3">
                  <span className="min-w-0 truncate text-sm font-semibold text-ink-1">
                    {item.model}
                  </span>
                  <span
                    className="shrink-0 text-xs font-medium tabular-nums"
                    style={{ color }}
                  >
                    {direction === 'up' ? '▲' : '▼'}{' '}
                    {formatPercent(Math.abs(item.delta_pct), locale)}
                  </span>
                </div>
                <div className="mt-1 flex items-center gap-2">
                  <span className="truncate text-xs text-ink-2">
                    {item.author}
                  </span>
                  <span className="ml-auto h-1.5 flex-1 max-w-32 rounded-full bg-surface-subtle">
                    <span
                      className="block h-1.5 rounded-full"
                      style={{
                        width: `${Math.max(
                          maxAbs > 0
                            ? (Math.abs(item.delta_pct) / maxAbs) * 100
                            : 0,
                          4,
                        )}%`,
                        // L7：幅度微条只表达大小，使用单一蓝色阶；方向已由
                        // ▲/▼ 与百分比文字的双发散色承载，微条不再重复。
                        backgroundColor: 'var(--chart-cat-1)',
                      }}
                    />
                  </span>
                </div>
              </button>
              {copied === item.model ? (
                <span className="sr-only" aria-live="polite">
                  {t('public.rankings.model.copied')}
                </span>
              ) : null}
            </li>
          ))}
        </ul>
      )}
    </div>
  )
}

export function MomentumSection({
  movers,
  locale,
}: {
  movers: { up: readonly RankingsMover[]; down: readonly RankingsMover[] }
  locale: string
}) {
  const maxAbs = Math.max(
    1,
    ...movers.up.map((item) => Math.abs(item.delta_pct)),
    ...movers.down.map((item) => Math.abs(item.delta_pct)),
  )
  return (
    <section id="momentum" className="scroll-mt-20">
      <SectionHeader titleKey="public.rankings.momentum.title" toggle={false} />
      {movers.up.length === 0 && movers.down.length === 0 ? (
        <div className="mt-4">
          <EmptyLine textKey="public.rankings.momentum.no_change" />
        </div>
      ) : (
        <div className="mt-4 grid grid-cols-1 gap-6 md:grid-cols-2">
          <MoverColumn
            direction="up"
            items={movers.up}
            maxAbs={maxAbs}
            locale={locale}
          />
          <MoverColumn
            direction="down"
            items={movers.down}
            maxAbs={maxAbs}
            locale={locale}
          />
        </div>
      )}
    </section>
  )
}

function seriesLabel(model: string, t: TranslateFn): string {
  return model === 'Other' ? t('public.rankings.trend.other') : model
}

export function TrendSection({
  trend,
  locale,
}: {
  trend: RankingsTrend
  locale: string
}) {
  const { t } = useI18n()
  const [view, setView] = useState<ViewMode>('chart')
  const hasData = trend.buckets.length > 0 && trend.series.length > 0
  const totals = trend.buckets.map((_, bucketIndex) =>
    trend.series.reduce((sum, item) => sum + (item.tokens[bucketIndex] ?? 0), 0),
  )

  return (
    <section id="trend" className="scroll-mt-20">
      <SectionHeader
        titleKey="public.rankings.trend.title"
        view={view}
        onViewChange={setView}
      />
      {!hasData ? (
        <div className="mt-4">
          <EmptyLine textKey="public.rankings.trend.accumulating" />
        </div>
      ) : view === 'chart' ? (
        <div className="mt-4">
          <ul className="mb-3 flex flex-wrap gap-x-4 gap-y-2 text-xs text-ink-2">
            {trend.series.map((item) => (
              <li key={item.model} className="flex items-center gap-1.5">
                <span
                  aria-hidden="true"
                  className="h-2.5 w-2.5 rounded-sm"
                  style={{ backgroundColor: seriesColor(item.slot) }}
                />
                {seriesLabel(item.model, t)}
              </li>
            ))}
          </ul>
          <TrendStackedChart
            series={trend.series}
            buckets={trend.buckets}
            granularity={trend.granularity}
            locale={locale}
            ariaLabel={t('public.rankings.trend.chart_aria', {
              count: trend.buckets.length,
            })}
          />
          {/* 移动端：每个时间桶一根全宽 100% 堆叠条。 */}
          <ul className="flex flex-col gap-2 md:hidden" data-testid="trend-list">
            {trend.buckets.map((bucket, bucketIndex) => {
              const total = totals[bucketIndex] ?? 0
              return (
                <li key={bucket} className="text-xs text-ink-2">
                  <div className="mb-1 flex justify-between gap-2">
                    <span title={formatBucketFull(bucket)}>
                      {formatBucketLabel(bucket, trend.granularity)}
                    </span>
                    <span className="tabular-nums">
                      {formatCompact(total, locale)}
                    </span>
                  </div>
                  <div className="flex h-2 w-full overflow-hidden rounded-full bg-surface-subtle">
                    {trend.series.map((item) => {
                      const value = item.tokens[bucketIndex] ?? 0
                      if (value <= 0 || total <= 0) return null
                      return (
                        <span
                          key={item.model}
                          style={{
                            width: `${(value / total) * 100}%`,
                            backgroundColor: seriesColor(item.slot),
                          }}
                        />
                      )
                    })}
                  </div>
                </li>
              )
            })}
          </ul>
        </div>
      ) : (
        <div className="mt-4 max-h-[32rem] overflow-auto rounded-lg border border-border-hairline">
          <table data-testid="trend-table" className="w-full text-left text-sm">
            <thead className="sticky top-0 bg-surface-subtle text-xs text-ink-2">
              <tr>
                <th scope="col" className="px-3 py-2 font-medium">
                  {t('public.rankings.trend.time')}
                </th>
                {trend.series.map((item) => (
                  <th
                    key={item.model}
                    scope="col"
                    className="px-3 py-2 text-right font-medium"
                  >
                    {seriesLabel(item.model, t)}
                  </th>
                ))}
                <th scope="col" className="px-3 py-2 text-right font-medium">
                  {t('public.rankings.trend.total')}
                </th>
              </tr>
            </thead>
            <tbody>
              {trend.buckets.map((bucket, bucketIndex) => (
                <tr key={bucket} className="border-t border-border-hairline">
                  <td
                    className="px-3 py-2 whitespace-nowrap tabular-nums text-ink-2"
                    title={formatBucketFull(bucket)}
                  >
                    {formatBucketLabel(bucket, trend.granularity)}
                  </td>
                  {trend.series.map((item) => (
                    <td
                      key={item.model}
                      className="px-3 py-2 text-right tabular-nums text-ink-1"
                    >
                      {formatExact(item.tokens[bucketIndex] ?? 0, locale)}
                    </td>
                  ))}
                  <td className="px-3 py-2 text-right font-medium tabular-nums text-ink-1">
                    {formatExact(totals[bucketIndex] ?? 0, locale)}
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

interface ShareEntry {
  label: string
  tokens: number
  share: number
  slot: number | null
  /** L8：名次以服务端 `rank` 为准；聚合出的 Other 没有服务端名次。 */
  rank: number | null
}

function shareEntries(providers: readonly RankingsProvider[]): ShareEntry[] {
  const top = providers.slice(0, 5)
  const rest = providers.slice(5)
  return [
    ...top.map((item) => ({
      label: item.author,
      tokens: item.total_tokens,
      share: item.share_pct,
      slot: item.slot,
      rank: item.rank,
    })),
    ...(rest.length > 0
      ? [
          {
            label: 'Other',
            tokens: rest.reduce((sum, item) => sum + item.total_tokens, 0),
            share: rest.reduce((sum, item) => sum + item.share_pct, 0),
            slot: null,
            rank: null,
          },
        ]
      : []),
  ]
}

function ProviderTable({
  entries,
  locale,
  showTotal,
}: {
  entries: readonly ShareEntry[]
  locale: string
  showTotal: boolean
}) {
  const { t } = useI18n()
  const totalTokens = entries.reduce((sum, item) => sum + item.tokens, 0)
  const totalShare = entries.reduce((sum, item) => sum + item.share, 0)
  return (
    <table data-testid="provider-table" className="w-full text-left text-sm">
      <thead className="bg-surface-subtle text-xs text-ink-2">
        <tr>
          <th scope="col" className="px-3 py-2 font-medium">
            {t('public.rankings.share.rank')}
          </th>
          <th scope="col" className="px-3 py-2 font-medium">
            {t('public.rankings.share.author')}
          </th>
          <th scope="col" className="px-3 py-2 text-right font-medium">
            {t('public.rankings.model.tokens')}
          </th>
          <th scope="col" className="px-3 py-2 text-right font-medium">
            {t('public.rankings.share.share')}
          </th>
        </tr>
      </thead>
      <tbody>
        {entries.map((entry, index) => (
          <tr key={entry.label} className="border-t border-border-hairline">
            <td className="px-3 py-2 tabular-nums text-ink-3">
              {entry.rank ?? index + 1}
            </td>
            <td className="px-3 py-2">
              <span className="inline-flex items-center gap-2 text-ink-1">
                <span
                  aria-hidden="true"
                  className="h-2.5 w-2.5 rounded-sm"
                  style={{ backgroundColor: seriesColor(entry.slot) }}
                />
                {entry.label === 'Other' ? t('public.rankings.share.other') : entry.label}
              </span>
            </td>
            <td className="px-3 py-2 text-right tabular-nums text-ink-1">
              {formatExact(entry.tokens, locale)}
            </td>
            <td className="px-3 py-2 text-right tabular-nums text-ink-1">
              {formatPercent(entry.share, locale)}
            </td>
          </tr>
        ))}
        {showTotal ? (
          <tr className="border-t border-border-default font-medium">
            <td className="px-3 py-2" />
            <td className="px-3 py-2 text-ink-1">
              {t('public.rankings.share.total')}
            </td>
            <td className="px-3 py-2 text-right tabular-nums text-ink-1">
              {formatExact(totalTokens, locale)}
            </td>
            <td className="px-3 py-2 text-right tabular-nums text-ink-1">
              {formatPercent(totalShare, locale)}
            </td>
          </tr>
        ) : null}
      </tbody>
    </table>
  )
}

export function ShareSection({
  providers,
  locale,
}: {
  providers: readonly RankingsProvider[]
  locale: string
}) {
  const { t } = useI18n()
  const [view, setView] = useState<ViewMode>('chart')
  const entries = shareEntries(providers)
  return (
    <section id="market-share" className="scroll-mt-20">
      <SectionHeader
        titleKey="public.rankings.share.title"
        view={view}
        onViewChange={setView}
      />
      {providers.length === 0 ? (
        <div className="mt-4">
          <EmptyLine textKey="public.rankings.share.accumulating" />
        </div>
      ) : view === 'chart' ? (
        <div className="mt-4 grid grid-cols-1 gap-6 lg:grid-cols-3">
          <div className="lg:col-span-2">
            <ShareBar
              providers={providers}
              locale={locale}
              ariaLabel={t('public.rankings.share.chart_aria')}
              otherLabel={t('public.rankings.share.other')}
            />
            <ul className="mt-3 flex flex-wrap gap-x-4 gap-y-2 text-xs text-ink-2">
              {entries.map((entry) => (
                <li key={entry.label} className="flex items-center gap-1.5">
                  <span
                    aria-hidden="true"
                    className="h-2.5 w-2.5 rounded-sm"
                    style={{ backgroundColor: seriesColor(entry.slot) }}
                  />
                  {entry.label === 'Other'
                    ? t('public.rankings.share.other')
                    : entry.label}
                </li>
              ))}
            </ul>
            <p className="mt-6 text-xs font-medium text-ink-2">
              {t('public.rankings.share.providers')}
            </p>
            <div className="mt-2 overflow-x-auto rounded-lg border border-border-hairline">
              <ProviderTable entries={entries} locale={locale} showTotal />
            </div>
          </div>
          <div className="rounded-lg border border-border-hairline bg-surface-subtle p-4 text-sm text-ink-2">
            <p className="font-medium text-ink-1">
              {t('public.rankings.share.providers')}
            </p>
            <ol className="mt-2 flex flex-col gap-2">
              {entries.map((entry, index) => (
                <li key={entry.label} className="flex justify-between gap-2">
                  <span className="truncate">
                    {entry.rank ?? index + 1}.{' '}
                    {entry.label === 'Other'
                      ? t('public.rankings.share.other')
                      : entry.label}
                  </span>
                  <span className="shrink-0 tabular-nums">
                    {formatPercent(entry.share, locale)}
                  </span>
                </li>
              ))}
            </ol>
          </div>
        </div>
      ) : (
        <div className="mt-4 overflow-x-auto rounded-lg border border-border-hairline">
          <ProviderTable entries={entries} locale={locale} showTotal />
        </div>
      )}
    </section>
  )
}

/** 冷启动时四区块的「待累积」占位（浅色网格骨架 + 一句说明）。 */
export function SectionPlaceholder({ titleKey }: { titleKey: string }) {
  const { t } = useI18n()
  return (
    <section>
      <h2 className="text-base font-semibold leading-6 text-ink-1">
        {t(titleKey)}
      </h2>
      <div
        aria-hidden="true"
        className="mt-4 grid grid-cols-3 gap-2 opacity-40"
      >
        {Array.from({ length: 9 }, (_, index) => (
          <div key={index} className="h-8 rounded bg-surface-subtle" />
        ))}
      </div>
      <p className="mt-2 text-xs text-ink-2">
        {t('public.rankings.section.accumulating')}
      </p>
    </section>
  )
}
