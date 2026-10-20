/**
 * 排行榜页面框架（F-09-29）：Hero + 周期 Tabs + 四个数据区块。
 *
 * 数据策略：
 * - 每个周期一条 query，`staleTime=Infinity` → 切回已加载周期不再请求；
 * - 页面不自动轮询（公开页避免无谓负载），刷新只由 Hero 按钮触发；
 * - 429 使用服务端 `Retry-After` 倒计时，归零后自动重试一次；离线恢复
 *   监听 `online` 事件自动重发当前周期。
 *
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
import { isSafePublicHref } from '@/features/public/nav-visibility'

import {
  RANKINGS_PERIODS,
  freshnessState,
  rankingsQueryOptions,
  type RankingsPeriod,
  type RankingsResponse,
} from './api'
import { PERIOD_LABEL_KEYS } from './format'
import {
  ModelRankingsSection,
  MomentumSection,
  SectionPlaceholder,
  ShareSection,
  TrendSection,
} from './sections'

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
      className="flex gap-1 overflow-x-auto rounded-lg bg-surface-subtle p-1"
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
          className={`h-9 shrink-0 rounded-md px-4 text-sm font-medium focus-visible:shadow-[var(--focus-ring)] focus-visible:outline-none ${
            period === entry
              ? 'bg-surface-card text-brand shadow-sm'
              : 'text-ink-2 hover:bg-surface-hover hover:text-ink-1'
          }`}
        >
          {t(PERIOD_LABEL_KEYS[entry])}
        </button>
      ))}
    </div>
  )
}

function RankingsSkeleton() {
  return (
    <div className="flex flex-col gap-10" data-testid="rankings-skeleton">
      {[0, 1, 2, 3].map((section) => (
        <div key={section}>
          <div className="h-6 w-40 rounded bg-surface-subtle" />
          <div className="mt-4 flex flex-col gap-3">
            {[0, 1, 2].map((row) => (
              <div key={row} className="h-10 rounded bg-surface-subtle/70" />
            ))}
          </div>
        </div>
      ))}
    </div>
  )
}

/**
 * 窗口时间必须按响应里的站点时区展示（R-D6：服务端按站点时区聚合）。
 * 非法 IANA 名不应让整页崩溃：`Intl.DateTimeFormat` 会抛 RangeError，
 * 此时回退浏览器本地时区，读数仍可读。
 */
function absoluteTime(
  value: string,
  locale: string,
  timeZone?: string,
): string {
  const ms = Date.parse(value)
  if (Number.isNaN(ms)) return value
  const options: Intl.DateTimeFormatOptions = {
    dateStyle: 'medium',
    timeStyle: 'short',
  }
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
      className="mt-4 rounded-lg border border-warning-line bg-warning-bg px-4 py-3 text-sm text-ink-1"
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
      <p className="text-sm text-ink-1">{message}</p>
      <button
        type="button"
        onClick={onRetry}
        className="mt-4 rounded-lg border border-border-default px-4 py-2 text-sm font-medium text-ink-1 hover:border-brand focus-visible:shadow-[var(--focus-ring)] focus-visible:outline-none"
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
      <h2 className="text-lg font-semibold text-ink-1">
        {t('public.rankings.cold_start.title')}
      </h2>
      <p className="mt-2 max-w-[640px] text-sm leading-6 text-ink-2">
        {t('public.rankings.cold_start.body', { count: data.sample_threshold })}
      </p>
      <div className="mt-5 flex flex-wrap gap-3">
        <a
          href="/pricing"
          className="inline-flex h-10 items-center rounded-lg bg-brand px-4 text-sm font-medium text-on-brand hover:bg-brand-hover focus-visible:shadow-[var(--focus-ring)] focus-visible:outline-none"
        >
          {t('public.rankings.cold_start.pricing')}
        </a>
        {docsUrl !== '' ? (
          <a
            href={docsUrl}
            target={/^https?:\/\//i.test(docsUrl) ? '_blank' : undefined}
            rel={/^https?:\/\//i.test(docsUrl) ? 'noopener noreferrer' : undefined}
            className="inline-flex h-10 items-center rounded-lg border border-border-default px-4 text-sm font-medium text-ink-1 hover:border-brand focus-visible:shadow-[var(--focus-ring)] focus-visible:outline-none"
          >
            {t('public.rankings.cold_start.quick_start')}
          </a>
        ) : null}
        {signupEnabled && !isLoggedIn ? (
          <a
            href="/sign-up"
            className="inline-flex h-10 items-center rounded-lg border border-border-default px-4 text-sm font-medium text-ink-1 hover:border-brand focus-visible:shadow-[var(--focus-ring)] focus-visible:outline-none"
          >
            {t('public.rankings.cold_start.sign_up')}
          </a>
        ) : null}
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
      <header className="border-b border-border-hairline pb-6 pt-10">
        <div className="flex flex-wrap items-start justify-between gap-4">
          <div>
            <h1 className="text-2xl font-semibold leading-8 text-ink-1 md:text-[32px] md:leading-10">
              {t('public.rankings.title')}
            </h1>
            <p className="mt-2 max-w-[640px] text-sm leading-6 text-ink-2">
              {t('public.rankings.subtitle')}
            </p>
          </div>
          <button
            type="button"
            onClick={() => void query.refetch()}
            disabled={query.isFetching}
            className="inline-flex h-9 items-center gap-2 rounded-lg border border-border-default px-3 text-sm font-medium text-ink-1 hover:border-brand disabled:cursor-not-allowed disabled:opacity-50 focus-visible:shadow-[var(--focus-ring)] focus-visible:outline-none"
          >
            <RefreshIcon />
            {t('public.rankings.refresh')}
          </button>
        </div>

        <div className="mt-5 flex flex-col gap-3 lg:flex-row lg:items-center lg:justify-between">
          <PeriodTabs period={period} onChange={onPeriodChange} />
          {data !== null && data !== undefined && data.status !== 'empty' ? (
            <p
              className="text-xs leading-5 text-ink-3"
              title={
                data.computed_at !== null
                  ? absoluteTime(data.computed_at, locale, data.timezone)
                  : undefined
              }
            >
              {t('public.rankings.methodology')} ·{' '}
              {t('public.rankings.window', {
                start: absoluteTime(data.window.start, locale, data.timezone),
                end: absoluteTime(data.window.end, locale, data.timezone),
              })}{' '}
              · {t('public.rankings.min_samples', { count: data.sample_threshold })}{' '}
              {data.computed_at !== null ? (
                <>
                  ·{' '}
                  {t('public.rankings.updated', {
                    relative: formatRelativeTime(data.computed_at, locale),
                  })}
                </>
              ) : null}
            </p>
          ) : null}
        </div>
      </header>

      {rateLimited && !manualRetry ? <RateLimitBanner seconds={countdown} /> : null}
      {stale ? (
        <div
          role="status"
          className="mt-4 rounded-lg border border-warning-line bg-warning-bg px-4 py-3 text-sm text-ink-1"
        >
          {t('public.rankings.delayed')}
        </div>
      ) : null}

      <div
        id="rankings-panel"
        role="tabpanel"
        aria-labelledby={`rankings-tab-${period}`}
        className="mt-8 flex flex-col gap-10"
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
            <SectionPlaceholder titleKey="public.rankings.model.title" />
            <SectionPlaceholder titleKey="public.rankings.momentum.title" />
            <SectionPlaceholder titleKey="public.rankings.trend.title" />
            <SectionPlaceholder titleKey="public.rankings.share.title" />
          </>
        ) : (
          <>
            <ModelRankingsSection
              models={data.models}
              period={period}
              onSelectPeriod={onPeriodChange}
              locale={locale}
              t={t}
            />
            <MomentumSection movers={data.movers} locale={locale} />
            <TrendSection trend={data.trend} locale={locale} />
            <ShareSection providers={data.providers} locale={locale} />
          </>
        )}
      </div>
    </div>
  )
}
