/**
 * 排行榜页面（F-09-29）：Hero + 周期 Tabs + 刷新 → 口径条 → 概要四格 →
 * token 走势 → 排名梯子 + 右栏 → 事实栏。
 *
 * 数据策略：
 * - 每个周期一条 query，`staleTime=Infinity` → 切回已加载周期不再请求；
 * - 页面不自动轮询（公开页避免无谓负载），刷新只由 Hero 按钮触发；
 * - 429 使用服务端 `Retry-After` 倒计时，归零后自动重试一次；离线恢复
 *   监听 `online` 事件自动重发当前周期。
 *
 * 所有读数都来自 `rankingsSummary` / `metricShare` / `tokensPerRequest`，
 * 页面本身不做除法；接口没给的数字（例如趋势补点、目录总数）一律不补。
 * 冷启动（status=empty）不是「暂无数据」：保留 Hero 与 Tabs，主区渲染
 * 引导卡与四个「待累积」占位，并按配置显示定价/文档/注册入口。
 */

import { useEffect, useRef, useState } from 'react'
import type { KeyboardEvent } from 'react'
import { useQuery } from '@tanstack/react-query'

import { ApiError } from '@/api/errors'
import { usePublicStatus } from '@/lib/public-status'
import { ME_QUERY_KEY, meQueryOptions } from '@/lib/session'
import { formatRelativeTime } from '@/shared/lib/datetime'
import { useI18n } from '@/shared/lib/i18n-context'
import type { TranslateFn } from '@/shared/lib/i18n-context'
import { isSafePublicHref } from '@/features/public/nav-visibility'

import {
  RANKINGS_PERIODS,
  freshnessState,
  rankingsQueryOptions,
  type RankingsPeriod,
  type RankingsResponse,
  type RankingsTrend,
} from './api'
import { TrendStackedChart, seriesColor } from './charts'
import {
  PERIOD_LABEL_KEYS,
  formatBucketFull,
  formatBucketLabel,
  formatCompact,
  formatExact,
} from './format'
import { ModelLadder } from './ladder'
import { RankingsRail } from './rail'
import { SectionPlaceholder } from './sections'
import { rankingsSummary } from './summary'

export interface RankingsPageProps {
  period: RankingsPeriod
  onPeriodChange: (next: RankingsPeriod) => void
}

function RefreshIcon() {
  return (
    <svg
      width="16"
      height="16"
      viewBox="0 0 16 16"
      fill="none"
      stroke="currentColor"
      strokeWidth="1.6"
      strokeLinecap="round"
      strokeLinejoin="round"
      aria-hidden="true"
    >
      <path d="M13.5 6.5A5.5 5.5 0 1 0 14 9" />
      <path d="M13.5 2.5v4h-4" />
    </svg>
  )
}

function PeriodTabs({
  period,
  onChange,
}: {
  period: RankingsPeriod
  onChange: (next: RankingsPeriod) => void
}) {
  const { t } = useI18n()

  const onKeyDown = (
    event: KeyboardEvent<HTMLButtonElement>,
    index: number,
  ) => {
    const last = RANKINGS_PERIODS.length - 1
    let nextIndex: number | null = null
    if (event.key === 'ArrowRight') nextIndex = index === last ? 0 : index + 1
    else if (event.key === 'ArrowLeft') nextIndex = index === 0 ? last : index - 1
    else if (event.key === 'Home') nextIndex = 0
    else if (event.key === 'End') nextIndex = last
    if (nextIndex === null) return
    event.preventDefault()
    const next = RANKINGS_PERIODS[nextIndex]
    if (next === undefined) return
    onChange(next)
    const target = event.currentTarget.parentElement?.querySelector<HTMLElement>(
      `[data-period="${next}"]`,
    )
    target?.focus()
  }

  return (
    <div
      role="tablist"
      aria-label={t('public.rankings.period.label')}
      className="rank-seg"
    >
      {RANKINGS_PERIODS.map((entry, index) => (
        <button
          key={entry}
          type="button"
          role="tab"
          data-period={entry}
          id={`rankings-tab-${entry}`}
          aria-selected={period === entry}
          aria-controls="rankings-panel"
          tabIndex={period === entry ? 0 : -1}
          onClick={() => onChange(entry)}
          onKeyDown={(event) => onKeyDown(event, index)}
        >
          {t(PERIOD_LABEL_KEYS[entry])}
        </button>
      ))}
    </div>
  )
}

function RankingsSkeleton() {
  return (
    <div className="flex flex-col gap-6" data-testid="rankings-skeleton">
      <div className="h-[76px] rounded-lg bg-surface-subtle" />
      <div className="h-[172px] rounded-lg bg-surface-subtle/70" />
      <div className="flex flex-col gap-3">
        {[0, 1, 2].map((row) => (
          <div key={row} className="h-16 rounded bg-surface-subtle/70" />
        ))}
      </div>
    </div>
  )
}

/**
 * 窗口时间必须按响应里的站点时区展示（R-D6：服务端按站点时区聚合）。
 * 非法 IANA 名不应让整页崩溃：`Intl.DateTimeFormat` 会抛 RangeError，
 * 此时回退浏览器本地时区，读数仍可读。
 */
function windowDate(value: string, locale: string, timeZone?: string): string {
  const ms = Date.parse(value)
  if (Number.isNaN(ms)) return value
  const options: Intl.DateTimeFormatOptions = { dateStyle: 'medium' }
  try {
    return new Intl.DateTimeFormat(
      locale,
      timeZone === undefined ? options : { ...options, timeZone },
    ).format(new Date(ms))
  } catch {
    return new Intl.DateTimeFormat(locale, options).format(new Date(ms))
  }
}

function RateLimitBanner({ seconds }: { seconds: number }) {
  const { t } = useI18n()
  return (
    <div
      role="status"
      className="mt-4 rounded-lg border border-warning-line bg-warning-bg px-4 py-3 text-body text-ink-1"
    >
      {t('public.rankings.rate_limited', { seconds })}
    </div>
  )
}

function RankingsError({
  error,
  onRetry,
}: {
  error: ApiError | null
  onRetry: () => void
}) {
  const { t } = useI18n()
  const message =
    error?.kind === 'offline'
      ? t('public.rankings.offline')
      : error?.kind === 'server'
        ? t('public.rankings.service_unavailable')
        : t('public.rankings.failed')
  return (
    <div
      role="alert"
      className="rounded-lg border border-border-hairline bg-surface-card px-4 py-8 text-center"
    >
      <p className="text-body text-ink-1">{message}</p>
      <button
        type="button"
        onClick={onRetry}
        className="mt-4 rounded-lg border border-border-default px-4 py-2 text-body font-medium text-ink-1 hover:border-brand focus-visible:shadow-[var(--focus-ring)] focus-visible:outline-none"
      >
        {t('public.rankings.retry')}
      </button>
    </div>
  )
}

function ColdStartGuide({
  data,
  isLoggedIn,
  docsUrl,
  signupEnabled,
}: {
  data: RankingsResponse
  isLoggedIn: boolean
  docsUrl: string
  signupEnabled: boolean
}) {
  const { t } = useI18n()
  return (
    <div
      data-testid="rankings-cold-start"
      className="rounded-xl border border-border-hairline bg-surface-card p-6 md:p-8"
    >
      <h2 className="text-section font-semibold text-ink-1">
        {t('public.rankings.cold_start.title')}
      </h2>
      <p className="mt-2 max-w-[640px] text-body leading-6 text-ink-2">
        {t('public.rankings.cold_start.body', { count: data.sample_threshold })}
      </p>
      <div className="mt-5 flex flex-wrap gap-3">
        <a
          href="/pricing"
          className="inline-flex h-10 items-center rounded-lg bg-brand px-4 text-body font-medium text-on-brand hover:bg-brand-hover focus-visible:shadow-[var(--focus-ring)] focus-visible:outline-none"
        >
          {t('public.rankings.cold_start.pricing')}
        </a>
        {docsUrl !== '' ? (
          <a
            href={docsUrl}
            target={/^https?:\/\//i.test(docsUrl) ? '_blank' : undefined}
            rel={/^https?:\/\//i.test(docsUrl) ? 'noopener noreferrer' : undefined}
            className="inline-flex h-10 items-center rounded-lg border border-border-default px-4 text-body font-medium text-ink-1 hover:border-brand focus-visible:shadow-[var(--focus-ring)] focus-visible:outline-none"
          >
            {t('public.rankings.cold_start.quick_start')}
          </a>
        ) : null}
        {signupEnabled && !isLoggedIn ? (
          <a
            href="/sign-up"
            className="inline-flex h-10 items-center rounded-lg border border-border-default px-4 text-body font-medium text-ink-1 hover:border-brand focus-visible:shadow-[var(--focus-ring)] focus-visible:outline-none"
          >
            {t('public.rankings.cold_start.sign_up')}
          </a>
        ) : null}
      </div>
    </div>
  )
}

function WindowBar({
  data,
  locale,
  t,
}: {
  data: RankingsResponse
  locale: string
  t: TranslateFn
}) {
  return (
    <div className="rank-window">
      <span>
        {t('public.rankings.window.label')}{' '}
        <b>
          {t('public.rankings.window.value', {
            start: windowDate(data.window.start, locale, data.timezone),
            end: windowDate(data.window.end, locale, data.timezone),
          })}
        </b>
      </span>
      <span>
        {t('public.rankings.window.timezone')} <b>{data.timezone}</b>
      </span>
      <span>
        {t('public.rankings.window.threshold')}{' '}
        <b>
          {t('public.rankings.window.threshold_value', {
            count: data.sample_threshold,
          })}
        </b>
      </span>
      <span>
        {t('public.rankings.window.basis')}{' '}
        <b>{t('public.rankings.window.basis_value')}</b>
      </span>
    </div>
  )
}

function SummaryFourUp({
  data,
  locale,
  period,
  t,
}: {
  data: RankingsResponse
  locale: string
  period: RankingsPeriod
  t: TranslateFn
}) {
  const summary = rankingsSummary(data.models)
  return (
    <div className="rank-sum">
      <div>
        <div className="rank-k">{t('public.rankings.summary.tokens')}</div>
        <div className="rank-v">
          {formatCompact(summary.totalTokens, locale)}
        </div>
        <div className="rank-s">
          {formatExact(summary.totalTokens, locale)}
        </div>
      </div>
      <div>
        <div className="rank-k">{t('public.rankings.summary.requests')}</div>
        <div className="rank-v">
          {formatExact(summary.totalRequests, locale)}
        </div>
        <div className="rank-s">{t(PERIOD_LABEL_KEYS[period])}</div>
      </div>
      <div>
        <div className="rank-k">{t('public.rankings.summary.models')}</div>
        <div className="rank-v">{summary.listedModels}</div>
      </div>
      <div>
        <div className="rank-k">{t('public.rankings.summary.providers')}</div>
        <div className="rank-v">{summary.providerCount}</div>
      </div>
    </div>
  )
}

/**
 * 趋势面板只画响应里的桶：接口返回 1 个桶就画 1 个，绝不补齐到 7 天，
 * 因此也不需要「示意数据」标注。移动端图表隐藏时用每桶一根全宽堆叠条
 * 承载同样的读数。
 */
function TrendPanel({
  trend,
  locale,
  t,
}: {
  trend: RankingsTrend
  locale: string
  t: TranslateFn
}) {
  const hasTrend = trend.buckets.length > 0 && trend.series.length > 0
  const totals = trend.buckets.map((_, bucketIndex) =>
    trend.series.reduce(
      (sum, item) => sum + (item.tokens[bucketIndex] ?? 0),
      0,
    ),
  )
  const seriesLabel = (model: string) =>
    model === 'Other' ? t('public.rankings.trend.other') : model

  return (
    <section className="rank-panel rank-trend" aria-labelledby="rank-trend-title">
      <div className="rank-panel-head">
        <h2 id="rank-trend-title">{t('public.rankings.trend.title')}</h2>
        {hasTrend ? (
          <ul className="rank-legend">
            {trend.series.map((item) => (
              <li key={item.model}>
                <i
                  aria-hidden="true"
                  style={{ backgroundColor: seriesColor(item.slot) }}
                />
                <span>{seriesLabel(item.model)}</span>
              </li>
            ))}
          </ul>
        ) : null}
      </div>
      {hasTrend ? (
        <>
          <div className="rank-chart">
            <TrendStackedChart
              series={trend.series}
              buckets={trend.buckets}
              granularity={trend.granularity}
              locale={locale}
              ariaLabel={t('public.rankings.trend.chart_aria', {
                count: trend.buckets.length,
              })}
            />
          </div>
          <ul className="rank-trend-list">
            {trend.buckets.map((bucket, bucketIndex) => {
              const total = totals[bucketIndex] ?? 0
              return (
                <li key={bucket}>
                  <div className="rank-trend-label">
                    <span title={formatBucketFull(bucket)}>
                      {formatBucketLabel(bucket, trend.granularity)}
                    </span>
                    <span>{formatCompact(total, locale)}</span>
                  </div>
                  <div className="rank-trend-track" aria-hidden="true">
                    {trend.series.map((item) => {
                      const value = item.tokens[bucketIndex] ?? 0
                      if (value <= 0 || total <= 0) return null
                      return (
                        <i
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
        </>
      ) : (
        <p className="rank-empty">{t('public.rankings.trend.accumulating')}</p>
      )}
    </section>
  )
}

/** 事实栏：窗口 / 样本 / 阈值 / 计算于 / 时区，全部来自响应。 */
function FactsPanel({
  data,
  locale,
  t,
}: {
  data: RankingsResponse
  locale: string
  t: TranslateFn
}) {
  return (
    <div className="rank-facts">
      <h2>{t('public.rankings.facts.title')}</h2>
      <dl>
        <dt>{t('public.rankings.window.label')}</dt>
        <dd className="mono">
          {t('public.rankings.window.value', {
            start: windowDate(data.window.start, locale, data.timezone),
            end: windowDate(data.window.end, locale, data.timezone),
          })}
        </dd>
        <dt>{t('public.rankings.facts.samples')}</dt>
        <dd className="mono">
          {formatExact(rankingsSummary(data.models).totalRequests, locale)}{' '}
          {t('public.rankings.ladder.requests')}
        </dd>
        <dt>{t('public.rankings.window.threshold')}</dt>
        <dd className="mono">
          {t('public.rankings.window.threshold_value', {
            count: data.sample_threshold,
          })}
        </dd>
        <dt>{t('public.rankings.facts.computed')}</dt>
        <dd>
          {data.computed_at === null
            ? '—'
            : formatRelativeTime(data.computed_at, locale)}
        </dd>
        <dt>{t('public.rankings.window.timezone')}</dt>
        <dd className="mono">{data.timezone}</dd>
      </dl>
    </div>
  )
}

export function RankingsPage({ period, onPeriodChange }: RankingsPageProps) {
  const { t, locale } = useI18n()
  const status = usePublicStatus()
  const me = useQuery({ ...meQueryOptions(), queryKey: ME_QUERY_KEY })
  const isLoggedIn = me.isSuccess
  const query = useQuery(rankingsQueryOptions(period))
  const data = query.data
  const error = query.error instanceof ApiError ? query.error : null

  const rateLimited = error?.kind === 'rate_limited'
  const [retryAt, setRetryAt] = useState<number | null>(null)
  const [countdown, setCountdown] = useState(0)
  const [manualRetry, setManualRetry] = useState(false)
  // 只自动重试一次：连续第二个 429 说明限流仍在，继续倒计时会让页面
  // 永远停在「0 秒」；此时交还可手动重试的错误态（M2）。
  const autoRetried = useRef(false)

  // 服务端 Retry-After 是倒计时的唯一来源；缺省时用契约冻结的 5s。
  const retryAfter = error?.retry_after
  useEffect(() => {
    if (!rateLimited) {
      autoRetried.current = false
      setManualRetry(false)
      setRetryAt(null)
      return
    }
    if (autoRetried.current) {
      // error 身份在每次失败后变化，effect 会随第二个 429 再次进入这里。
      setManualRetry(true)
      setRetryAt(null)
      return
    }
    const seconds = retryAfter !== undefined && retryAfter > 0 ? retryAfter : 5
    setRetryAt(Date.now() + seconds * 1000)
    setCountdown(seconds)
  }, [rateLimited, retryAfter, error])

  const refetch = query.refetch
  useEffect(() => {
    if (retryAt === null) return
    const tick = () => {
      const left = Math.max(0, Math.ceil((retryAt - Date.now()) / 1000))
      setCountdown(left)
      if (left === 0) {
        setRetryAt(null)
        if (!autoRetried.current) {
          autoRetried.current = true
          void refetch()
        } else {
          setManualRetry(true)
        }
      }
    }
    tick()
    const timer = setInterval(tick, 1000)
    return () => clearInterval(timer)
  }, [retryAt, refetch])

  // 离线恢复自动重发当前周期（保留 period，不重置视图）。
  const errorKind = error?.kind
  useEffect(() => {
    if (errorKind !== 'offline') return
    const handler = () => void refetch()
    window.addEventListener('online', handler)
    return () => window.removeEventListener('online', handler)
  }, [errorKind, refetch])

  const site = status.data?.site
  const docsUrlRaw = (site?.docs_url ?? '').trim()
  const docsUrl = isSafePublicHref(docsUrlRaw) ? docsUrlRaw : ''
  const signupEnabled = status.data?.auth?.signup_enabled === true
  const freshness = data === null || data === undefined
    ? null
    : freshnessState(data.freshness_seconds, period)
  const stale = freshness === 'stale' || freshness === 'expired'
  // M1：normalizeRankings 抛的是普通 Error，不是 ApiError。只看
  // `error !== null` 会把这类失败渲染成空白；`isError` 才是查询失败的
  // 唯一判据，文案按通用失败处理（error 传 null）。
  const showError = (query.isError && !rateLimited) || (rateLimited && manualRetry)
  const retryRankings = () => {
    autoRetried.current = false
    setManualRetry(false)
    void query.refetch()
  }

  return (
    <div>
      <header className="rank-head">
        <div>
          <h1>{t('public.rankings.title')}</h1>
          <p>{t('public.rankings.subtitle')}</p>
        </div>
        <div className="rank-actions">
          <PeriodTabs period={period} onChange={onPeriodChange} />
          <button
            type="button"
            onClick={() => void query.refetch()}
            disabled={query.isFetching}
            className="rank-refresh"
          >
            <RefreshIcon />
            {t('public.rankings.refresh')}
          </button>
        </div>
      </header>

      {rateLimited && !manualRetry ? <RateLimitBanner seconds={countdown} /> : null}
      {stale ? (
        <div
          role="status"
          className="mt-4 rounded-lg border border-warning-line bg-warning-bg px-4 py-3 text-body text-ink-1"
        >
          {t('public.rankings.delayed')}
        </div>
      ) : null}

      <div
        id="rankings-panel"
        role="tabpanel"
        aria-labelledby={`rankings-tab-${period}`}
        className="rank-panel-body"
      >
        {query.isPending && !rateLimited ? (
          <RankingsSkeleton />
        ) : showError ? (
          <RankingsError error={error} onRetry={retryRankings} />
        ) : data === undefined ? null : data.status === 'empty' ? (
          <>
            <ColdStartGuide
              data={data}
              isLoggedIn={isLoggedIn}
              docsUrl={docsUrl}
              signupEnabled={signupEnabled}
            />
            <SectionPlaceholder titleKey="public.rankings.trend.title" />
            <SectionPlaceholder titleKey="public.rankings.ladder.title" />
            <SectionPlaceholder titleKey="public.rankings.rail.providers" />
            <SectionPlaceholder titleKey="public.rankings.rail.efficiency" />
          </>
        ) : (
          <>
            <WindowBar data={data} locale={locale} t={t} />
            <SummaryFourUp
              data={data}
              locale={locale}
              period={period}
              t={t}
            />
            <TrendPanel trend={data.trend} locale={locale} t={t} />
            <div className="rank-body">
              <ModelLadder
                models={data.models}
                metric="tokens"
                locale={locale}
                t={t}
              />
              <aside className="rank-rail">
                <RankingsRail
                  providers={data.providers}
                  models={data.models}
                  movers={data.movers}
                  locale={locale}
                  t={t}
                />
                <FactsPanel data={data} locale={locale} t={t} />
              </aside>
            </div>
          </>
        )}
      </div>
    </div>
  )
}
