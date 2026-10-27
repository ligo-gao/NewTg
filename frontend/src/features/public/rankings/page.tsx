/**
 * 排行榜页面（F-09-29）：Hero + 周期 Tabs + 刷新 → 窗口条 → 概要五格 →
 * 双榜并列（实际调用排行 + 全球性能测评）→ 底部三栏（供应商分布 /
 * 本周动态 / 统计详情）。测评行可点开详情弹窗。
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
 *
 * 语言：所有文案走 `public.rankings.*`，随 `useI18n` 的全局语言切换即时
 * 更新；本文件不出现任何语言字面量（CJK lint 门禁的一部分）。
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
  refreshCadence,
  type RankingsPeriod,
  type RankingsResponse,
} from './api'
import { getBenchmarkData } from './benchmarks'
import { BenchmarkModal } from './benchmark-modal'
import { PERIOD_LABEL_KEYS, formatCompact, formatExact } from './format'
import { GlobalBenchmarkLadder } from './global-ladder'
import { ModelLadder } from './ladder'
import { RankingsRail } from './rail'
import { SectionPlaceholder } from './sections'
import { rankingsSummary, tokensPerRequest } from './summary'

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
function formatWindowDate(
  ms: number,
  locale: string,
  timeZone: string | undefined,
  options: Intl.DateTimeFormatOptions,
): string {
  try {
    return new Intl.DateTimeFormat(
      locale,
      timeZone === undefined ? options : { ...options, timeZone },
    ).format(new Date(ms))
  } catch {
    return new Intl.DateTimeFormat(locale, options).format(new Date(ms))
  }
}

/** 窗口端点的日期；无法解析的值原样返回，不让整页失败。 */
function windowDate(value: string, locale: string, timeZone?: string): string {
  const ms = Date.parse(value)
  if (Number.isNaN(ms)) return value
  return formatWindowDate(ms, locale, timeZone, { dateStyle: 'medium' })
}

/**
 * 两端日历日期相同时（`period=today`）纯日期会打印成同一个日子，看起来像
 * 空窗口；此时补上时间以露出真实跨度。跨天窗口保持原来的日期输出。
 */
function windowRange(
  start: string,
  end: string,
  locale: string,
  timeZone?: string,
): { start: string; end: string } {
  const startDate = windowDate(start, locale, timeZone)
  const endDate = windowDate(end, locale, timeZone)
  const startMs = Date.parse(start)
  const endMs = Date.parse(end)
  if (startDate !== endDate || Number.isNaN(startMs) || Number.isNaN(endMs)) {
    return { start: startDate, end: endDate }
  }
  const options: Intl.DateTimeFormatOptions = {
    dateStyle: 'medium',
    timeStyle: 'short',
  }
  return {
    start: formatWindowDate(startMs, locale, timeZone, options),
    end: formatWindowDate(endMs, locale, timeZone, options),
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

/** 窗口 / 时区 / 样本阈值 / 测评数据来源，四项都来自响应或本模块常量。 */
function WindowBar({
  data,
  locale,
  t,
}: {
  data: RankingsResponse
  locale: string
  t: TranslateFn
}) {
  const range = windowRange(
    data.window.start,
    data.window.end,
    locale,
    data.timezone,
  )
  return (
    <div className="rank-window">
      <span>
        {t('public.rankings.window.label')}{' '}
        <b>
          {t('public.rankings.window.value', {
            start: range.start,
            end: range.end,
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
        {t('public.rankings.window.benchmark')}{' '}
        <b>{t('public.rankings.window.benchmark_value')}</b>
      </span>
    </div>
  )
}

/** 概要五格：前四格来自上榜模型，第五格是「总 token ÷ 总请求」。 */
function SummaryFiveUp({
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
  const average = tokensPerRequest(summary.totalTokens, summary.totalRequests)
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
      <div data-testid="rank-efficiency">
        <div className="rank-k">{t('public.rankings.summary.efficiency')}</div>
        <div className="rank-v">
          {average === null ? '—' : formatExact(average, locale)}
        </div>
        <div className="rank-s">
          {t('public.rankings.summary.efficiency_unit')}
        </div>
      </div>
    </div>
  )
}

/**
 * 统计详情：每一行都必须能从响应里算出来——样本量、计算时间、新鲜度、
 * 刷新节奏都来自接口或本模块的冻结常量；没有一项是写死的营销文案。
 * 新鲜度按周期各自的刷新间隔判定（与页面顶部延迟提示同一判据），因此
 * 「实时」不会与「数据更新延迟」同时出现。
 */
const FRESHNESS_LABEL_KEYS: Record<string, string> = {
  fresh: 'public.rankings.facts.freshness_fresh',
  stale: 'public.rankings.facts.freshness_stale',
  expired: 'public.rankings.facts.freshness_expired',
}

function FactsPanel({
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
  const freshness = freshnessState(data.freshness_seconds, period)
  const freshnessLabel =
    freshness === null
      ? '—'
      : t(
          FRESHNESS_LABEL_KEYS[freshness] ??
            'public.rankings.facts.freshness_stale',
        )
  const cadence = refreshCadence(period)
  const cadenceLabel = t(
    cadence.unit === 'hours'
      ? 'public.rankings.facts.frequency_hours'
      : 'public.rankings.facts.frequency_minutes',
    { count: cadence.count },
  )
  return (
    <div className="info-panel" data-testid="rank-facts">
      <h3>{t('public.rankings.facts.title')}</h3>
      <div className="facts-list">
        <div className="fact-row">
          <span className="fact-label">{t('public.rankings.facts.samples')}</span>
          <span className="fact-value">
            {formatExact(rankingsSummary(data.models).totalRequests, locale)}
          </span>
        </div>
        <div className="fact-row">
          <span className="fact-label">{t('public.rankings.facts.computed')}</span>
          <span className="fact-value">
            {data.computed_at === null
              ? '—'
              : formatRelativeTime(data.computed_at, locale)}
          </span>
        </div>
        <div className="fact-row">
          <span className="fact-label">{t('public.rankings.facts.freshness')}</span>
          <span className="fact-value">{freshnessLabel}</span>
        </div>
        <div className="fact-row">
          <span className="fact-label">{t('public.rankings.facts.channels')}</span>
          <span className="fact-value">
            {t('public.rankings.facts.channels_value')}
          </span>
        </div>
        <div className="fact-row">
          <span className="fact-label">{t('public.rankings.facts.frequency')}</span>
          <span className="fact-value">{cadenceLabel}</span>
        </div>
      </div>
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
  const [benchmarkId, setBenchmarkId] = useState<string | null>(null)

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
  const freshness =
    data === null || data === undefined
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
  const benchmarkData =
    benchmarkId === null ? null : getBenchmarkData(benchmarkId)

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
          <div className="rank-cold">
            <ColdStartGuide
              data={data}
              isLoggedIn={isLoggedIn}
              docsUrl={docsUrl}
              signupEnabled={signupEnabled}
            />
            <SectionPlaceholder titleKey="public.rankings.ladder.title" />
            <SectionPlaceholder titleKey="public.rankings.benchmark.title" />
            <SectionPlaceholder titleKey="public.rankings.rail.providers" />
            <SectionPlaceholder titleKey="public.rankings.rail.movers" />
          </div>
        ) : (
          <>
            <WindowBar data={data} locale={locale} t={t} />
            <SummaryFiveUp
              data={data}
              locale={locale}
              period={period}
              t={t}
            />
            {/* 双榜并列布局：实际调用排行 + 全球性能测评 */}
            <div className="dual-rankings">
              <ModelLadder
                models={data.models}
                metric="tokens"
                locale={locale}
                t={t}
              />
              <GlobalBenchmarkLadder
                locale={locale}
                t={t}
                onSelect={setBenchmarkId}
              />
            </div>
            {/* 底部信息栏（三栏横向布局）*/}
            <div className="info-grid">
              <RankingsRail
                providers={data.providers}
                movers={data.movers}
                locale={locale}
                t={t}
              />
              <FactsPanel
                data={data}
                locale={locale}
                period={period}
                t={t}
              />
            </div>
          </>
        )}
      </div>

      <BenchmarkModal
        isOpen={benchmarkId !== null}
        data={benchmarkData}
        onClose={() => setBenchmarkId(null)}
      />
    </div>
  )
}
