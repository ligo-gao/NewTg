/**
 * F-09-02 首页实时调用动态。
 *
 * 数据通道优先级：SSE（`/api/home/live-feed/stream`）→ 事件源不支持/断流时
 * 降级为 5s 轮询 `?limit=20`，失败按 5s→15s→60s 退避。暂停开关与页面
 * 隐藏都会关闭连接；恢复可见/继续时重新建立。
 *
 * 【聚合条】服务端尚未下发 `window:{calls,cost_credits}`（G-DOM-21）。
 * 页面只展示区块标题与连接状态，绝不编造调用次数或费用（裁决 A-1 的
 * 信任边界：真实读数只在服务端提供时出现）。
 *
 * 逐行字段不含费用：时间 / 模型 / 上游 / token(in/out) / 耗时 / 状态。
 * 桌面表格 + 移动端卡片；状态用图标 + 文字双编码，不用彩色徽章。
 */

import { useEffect, useState } from 'react'

import { ApiError } from '@/api/errors'
import { useI18n } from '@/shared/lib/i18n-context'

import {
  LIVE_FEED_LIMIT,
  LIVE_FEED_SSE_PATH,
  fetchLiveFeedEvents,
  formatClock,
  formatInteger,
  parseLiveFeedSseData,
  type LiveFeedEvent,
} from './live-feed-data'

type ConnectionState = 'connecting' | 'live' | 'paused' | 'reconnecting'

function PauseIcon({ paused }: { paused: boolean }) {
  return (
    <svg
      width="18"
      height="18"
      viewBox="0 0 18 18"
      fill="none"
      stroke="currentColor"
      strokeWidth="1.6"
      strokeLinecap="round"
      strokeLinejoin="round"
      aria-hidden="true"
    >
      {paused ? (
        <path d="M5 3.5v11l8-5.5-8-5.5Z" />
      ) : (
        <>
          <path d="M6 3.5v11M12 3.5v11" />
        </>
      )}
    </svg>
  )
}

function RedactionMask() {
  return (
    <span className="inline-flex items-center gap-1" aria-hidden="true">
      {[0, 1, 2, 3].map((index) => (
        <span key={index} className="h-3 w-1.5 rounded-sm bg-ink-4" />
      ))}
    </span>
  )
}

function StatusCell({ status }: { status: LiveFeedEvent['status'] }) {
  const { t } = useI18n()
  return status === 'ok' ? (
    <span className="inline-flex items-center gap-1 text-success">
      <span aria-hidden="true">✓</span>
      {t('public.live_feed.status.ok')}
    </span>
  ) : (
    <span className="inline-flex items-center gap-1 text-danger">
      <span aria-hidden="true">▲</span>
      {t('public.live_feed.status.error')}
    </span>
  )
}

function FeedSkeleton() {
  return (
    <div className="mt-4 flex flex-col gap-2" data-testid="live-feed-skeleton">
      {[0, 1, 2, 3, 4].map((row) => (
        <div key={row} className="h-10 rounded bg-ink-4/15" />
      ))}
    </div>
  )
}

function ConnectionBadge({ connection }: { connection: ConnectionState }) {
  const { t } = useI18n()
  const label =
    connection === 'live'
      ? t('public.live_feed.live')
      : connection === 'paused'
        ? t('public.live_feed.paused')
        : connection === 'reconnecting'
          ? t('public.live_feed.reconnecting')
          : t('public.live_feed.connecting')
  const dot =
    connection === 'live'
      ? 'bg-brand animate-pulse motion-reduce:animate-none'
      : connection === 'reconnecting'
        ? 'bg-warning'
        : 'bg-ink-4'
  return (
    <span
      role="status"
      aria-live="polite"
      className="inline-flex items-center gap-2 text-xs font-medium text-ink-2"
    >
      <span aria-hidden="true" className={`h-1.5 w-1.5 rounded-full ${dot}`} />
      {label}
    </span>
  )
}

function DesktopTable({ events, locale }: { events: LiveFeedEvent[]; locale: string }) {
  const { t } = useI18n()
  return (
    <table className="mt-4 hidden w-full table-fixed text-left text-[13px] md:table">
      <thead>
        <tr className="h-9 border-b border-brand-line text-xs text-ink-2">
          <th scope="col" className="w-[88px] font-medium">
            {t('public.live_feed.col.time')}
          </th>
          <th scope="col" className="font-medium">
            {t('public.live_feed.col.model')}
          </th>
          <th scope="col" className="w-[150px] font-medium">
            {t('public.live_feed.col.upstream')}
          </th>
          <th scope="col" className="w-[140px] text-right font-medium">
            {t('public.live_feed.col.tokens')}
          </th>
          <th scope="col" className="w-[96px] text-right font-medium">
            {t('public.live_feed.col.latency')}
          </th>
          <th scope="col" className="w-[96px] text-right font-medium">
            {t('public.live_feed.col.status')}
          </th>
        </tr>
      </thead>
      <tbody>
        {events.slice(0, 12).map((event) => (
          <tr
            key={event.id}
            data-event-id={event.id}
            className="h-11 border-b border-border-hairline/60 hover:bg-surface-hover"
          >
            <td className="pr-4 font-mono tabular-nums text-ink-2">
              {formatClock(event.ts, locale)}
            </td>
            <td className="truncate pr-4 text-ink-1" title={event.model}>
              {event.model}
            </td>
            <td className="pr-4">
              <span className="inline-flex items-center gap-2">
                <span className="font-mono text-xs text-ink-2">{event.upstream}</span>
                <RedactionMask />
              </span>
            </td>
            <td className="pr-4 text-right font-mono tabular-nums text-ink-2">
              {formatInteger(event.tokens_in, locale)}
              <span className="px-1 text-ink-4">/</span>
              {formatInteger(event.tokens_out, locale)}
            </td>
            <td className="pr-4 text-right font-mono tabular-nums text-ink-2">
              {formatInteger(event.latency_ms, locale)} ms
            </td>
            <td className="text-right text-xs">
              <StatusCell status={event.status} />
            </td>
          </tr>
        ))}
      </tbody>
    </table>
  )
}

function MobileCards({ events, locale }: { events: LiveFeedEvent[]; locale: string }) {
  const { t } = useI18n()
  return (
    <ul className="mt-4 flex flex-col gap-3 md:hidden">
      {events.slice(0, 8).map((event) => (
        <li
          key={event.id}
          data-event-id={event.id}
          className="rounded-lg border border-border-hairline bg-surface-card p-4"
        >
          <div className="flex items-center justify-between">
            <span className="font-mono text-xs tabular-nums text-ink-2">
              {formatClock(event.ts, locale)}
            </span>
            <StatusCell status={event.status} />
          </div>
          <dl className="mt-2 grid grid-cols-[auto_1fr] gap-x-3 gap-y-1 text-xs">
            <dt className="text-ink-2">{t('public.live_feed.col.model')}</dt>
            <dd className="truncate text-ink-1">{event.model}</dd>
            <dt className="text-ink-2">{t('public.live_feed.col.upstream')}</dt>
            <dd className="font-mono text-ink-2">{event.upstream}</dd>
            <dt className="text-ink-2">{t('public.live_feed.col.tokens')}</dt>
            <dd className="font-mono tabular-nums text-ink-2">
              {formatInteger(event.tokens_in, locale)} /{' '}
              {formatInteger(event.tokens_out, locale)}
            </dd>
            <dt className="text-ink-2">{t('public.live_feed.col.latency')}</dt>
            <dd className="font-mono tabular-nums text-ink-2">
              {formatInteger(event.latency_ms, locale)} ms
            </dd>
          </dl>
        </li>
      ))}
    </ul>
  )
}

export function LiveFeedSection() {
  const { t, locale } = useI18n()
  const [events, setEvents] = useState<LiveFeedEvent[]>([])
  const [paused, setPaused] = useState(false)
  const [hidden, setHidden] = useState(
    () => typeof document !== 'undefined' && document.visibilityState === 'hidden',
  )
  const [connection, setConnection] = useState<ConnectionState>('connecting')
  // 服务端关闭区块（404）时整块隐藏：轮询/SSE 都不该对已关闭端点无限重试。
  const [disabled, setDisabled] = useState(false)

  useEffect(() => {
    if (typeof document === 'undefined') return
    const onChange = () => setHidden(document.visibilityState === 'hidden')
    document.addEventListener('visibilitychange', onChange)
    return () => document.removeEventListener('visibilitychange', onChange)
  }, [])

  useEffect(() => {
    if (paused || hidden) {
      setConnection('paused')
      return
    }

    let disposed = false
    let source: EventSource | undefined
    let timer: ReturnType<typeof setTimeout> | undefined
    let failures = 0

    const appendEvent = (event: LiveFeedEvent) => {
      setEvents((previous) => {
        if (previous.some((item) => item.id === event.id)) return previous
        return [event, ...previous].slice(0, LIVE_FEED_LIMIT)
      })
    }

    const schedule = (delayMs: number) => {
      if (disposed) return
      timer = setTimeout(() => {
        void poll()
      }, delayMs)
    }

    const poll = async () => {
      try {
        const next = await fetchLiveFeedEvents(LIVE_FEED_LIMIT)
        if (disposed) return
        failures = 0
        setEvents(next)
        setConnection('live')
        schedule(5000)
      } catch (error) {
        if (disposed) return
        if (error instanceof ApiError && error.kind === 'not_found') {
          setDisabled(true)
          return
        }
        failures += 1
        setConnection('reconnecting')
        const retryAfter =
          error instanceof ApiError && error.retry_after !== undefined
            ? error.retry_after
            : undefined
        const seconds =
          retryAfter !== undefined && retryAfter > 0
            ? retryAfter
            : failures === 1
              ? 5
              : failures === 2
                ? 15
                : 60
        schedule(seconds * 1000)
      }
    }

    if (typeof EventSource === 'undefined') {
      // 不支持 SSE：静默降级轮询（F-09-02 分支流程）。
      setConnection('connecting')
      void poll()
    } else {
      try {
        setConnection('connecting')
        source = new EventSource(LIVE_FEED_SSE_PATH)
        source.onopen = () => {
          if (disposed) return
          failures = 0
          setConnection('live')
        }
        source.addEventListener('call', (raw) => {
          const event = parseLiveFeedSseData((raw as MessageEvent<string>).data)
          if (event !== null) appendEvent(event)
        })
        source.onerror = () => {
          if (disposed) return
          // 浏览器会无限重连；这里主动关闭并降级为轮询，由轮询退避兜底。
          source?.close()
          source = undefined
          failures = 0
          setConnection('reconnecting')
          void poll()
        }
      } catch {
        void poll()
      }
    }

    return () => {
      disposed = true
      source?.close()
      if (timer !== undefined) clearTimeout(timer)
    }
  }, [paused, hidden])

  const showEmpty = events.length === 0

  if (disabled) return null

  return (
    <section data-testid="live-feed-section" className="bg-brand-subtle">
      <div className="mx-auto w-full max-w-[1200px] px-4 py-16 md:px-6 md:py-20">
        <div className="flex flex-wrap items-start justify-between gap-4">
          <div>
            <h2 className="text-base font-semibold leading-6 text-ink-1">
              {t('public.live_feed.title')}
            </h2>
            <p className="mt-2 text-[13px] leading-5 text-ink-2">
              {t('public.live_feed.anonymized')}
            </p>
          </div>
          <div className="flex items-center gap-4">
            <ConnectionBadge connection={connection} />
            <button
              type="button"
              role="switch"
              aria-checked={paused}
              aria-label={
                paused
                  ? t('public.live_feed.resume')
                  : t('public.live_feed.pause')
              }
              onClick={() => setPaused((value) => !value)}
              className="inline-flex h-8 w-8 items-center justify-center rounded-lg text-ink-2 hover:bg-surface-hover hover:text-ink-1 focus-visible:shadow-[var(--focus-ring)] focus-visible:outline-none"
            >
              <PauseIcon paused={paused} />
            </button>
          </div>
        </div>

        {connection === 'reconnecting' ? (
          <p className="mt-3 text-xs text-warning" role="status">
            {t('public.live_feed.reconnecting_hint')}
          </p>
        ) : null}

        {showEmpty ? (
          connection === 'live' ? (
            <p className="mt-6 rounded-lg border border-dashed border-brand-line px-4 py-8 text-center text-sm text-ink-2">
              {t('public.live_feed.empty')}
            </p>
          ) : connection === 'paused' ? (
            <p className="mt-6 text-center text-sm text-ink-2">
              {t('public.live_feed.paused_hint')}
            </p>
          ) : (
            <FeedSkeleton />
          )
        ) : (
          <>
            <DesktopTable events={events} locale={locale} />
            <MobileCards events={events} locale={locale} />
          </>
        )}
      </div>
    </section>
  )
}
