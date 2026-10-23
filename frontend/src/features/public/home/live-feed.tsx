/**
 * F-09-02 首页实时调用动态。
 *
 * 数据通道优先级：SSE（`/api/home/live-feed/stream`）→ 事件源不支持/断流时
 * 降级为 5s 轮询 `?limit=20`，失败按 5s→15s→60s 退避。暂停开关与页面
 * 隐藏都会关闭连接；恢复可见/继续时重新建立。
 *
 * 【聚合条】`window:{calls,cost_credits}` 由轮询端点返回（A6 冻结：SSE 帧
 * 不附带聚合读数）。SSE 模式下没有轮询循环，因此每 30s 轻量拉一次快照
 * 只刷新 window；否则聚合条会永远停在 onopen 播种时的那一刻。读数为
 * 近 60 秒（当前分钟桶近似，F-09-02），费用只以整数积分出现在这里，
 * 逐行事件不含费用。
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
  fetchLiveFeedSnapshot,
  formatClock,
  formatInteger,
  parseLiveFeedSseData,
  type LiveFeedEvent,
  type LiveFeedWindow,
} from './live-feed-data'

type ConnectionState = 'connecting' | 'live' | 'paused' | 'reconnecting'

/**
 * SSE 模式下聚合条的刷新间隔。轮询降级模式由 5s 轮询顺带刷新，不需要
 * 这个计时器。取 30s：60 秒窗口下滞后上界约 30 秒（半个窗口），读数
 * 仍然足够实时，同时把公开只读限流（60 次/分钟/IP+path）的占用压到很低。
 */
const LIVE_FEED_WINDOW_REFRESH_MS = 30_000

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

function StatusCell({ status }: { status: LiveFeedEvent['status'] }) {
  const { t } = useI18n()
  return status === 'ok' ? (
    <span className="status-ok">
      <span aria-hidden="true">✓</span> {t('public.live_feed.status.ok')}
    </span>
  ) : (
    <span className="status-error">
      <span aria-hidden="true">▲</span> {t('public.live_feed.status.error')}
    </span>
  )
}

function FeedSkeleton() {
  return (
    <div className="feed-skeleton" data-testid="live-feed-skeleton" aria-hidden="true">
      {[0, 1, 2, 3, 4].map((row) => (
        <span key={row} />
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
  const tone =
    connection === 'paused'
      ? ' is-paused'
      : connection === 'reconnecting'
        ? ' is-warning'
        : connection === 'connecting'
          ? ' is-idle'
          : ''
  return (
    <span role="status" aria-live="polite" className={`live-badge${tone}`}>
      <i aria-hidden="true" />
      {label}
    </span>
  )
}

function DesktopTable({ events, locale }: { events: LiveFeedEvent[]; locale: string }) {
  const { t } = useI18n()
  return (
    <table className="ledger-table">
      <caption>{t('public.live_feed.anonymized')}</caption>
      <colgroup>
        <col style={{ width: '10%' }} />
        <col style={{ width: '27%' }} />
        <col style={{ width: '15%' }} />
        <col style={{ width: '18%' }} />
        <col style={{ width: '16%' }} />
        <col style={{ width: '14%' }} />
      </colgroup>
      <thead>
        <tr>
          <th scope="col">{t('public.live_feed.col.time')}</th>
          <th scope="col">{t('public.live_feed.col.model')}</th>
          <th scope="col">{t('public.live_feed.col.upstream')}</th>
          <th scope="col" className="num">
            {t('public.live_feed.col.tokens')}
          </th>
          <th scope="col" className="num">
            {t('public.live_feed.col.latency')}
          </th>
          <th scope="col">{t('public.live_feed.col.status')}</th>
        </tr>
      </thead>
      <tbody>
        {events.slice(0, 12).map((event) => (
          <tr key={event.id} data-event-id={event.id}>
            <td className="time">{formatClock(event.ts, locale)}</td>
            <td className="model" title={event.model}>
              {event.model}
            </td>
            <td className="data">{event.upstream}</td>
            <td className="data num">
              {formatInteger(event.tokens_in, locale)}
              <span aria-hidden="true"> / </span>
              {formatInteger(event.tokens_out, locale)}
            </td>
            <td className="data num">
              {formatInteger(event.latency_ms, locale)}{' '}
              {t('public.live_feed.unit.ms')}
            </td>
            <td className="status-cell">
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
    <ul className="ledger-cards">
      {events.slice(0, 8).map((event) => (
        <li key={event.id} data-event-id={event.id} className="ledger-card">
          <div className="top">
            <span className="time">{formatClock(event.ts, locale)}</span>
            <StatusCell status={event.status} />
          </div>
          <div className="model">{event.model}</div>
          <div className="meta">
            <span>{event.upstream}</span>
            <span>
              {formatInteger(event.tokens_in, locale)} /{' '}
              {formatInteger(event.tokens_out, locale)}
            </span>
            <span>
              {formatInteger(event.latency_ms, locale)}{' '}
              {t('public.live_feed.unit.ms')}
            </span>
          </div>
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
  // null = 服务端未提供或窗口数据不可信 → 隐藏聚合条（不伪造 0）。
  const [aggregate, setAggregate] = useState<LiveFeedWindow | null>(null)

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
    let windowTimer: ReturnType<typeof setInterval> | undefined
    let failures = 0
    let polling = false

    /** SSE 与播种/轮询可能同时到达同一事件：按 id 去重、按时间倒序并保持 20 条上限。 */
    const mergeEvents = (
      previous: LiveFeedEvent[],
      incoming: readonly LiveFeedEvent[],
    ): LiveFeedEvent[] => {
      const seen = new Set(previous.map((item) => item.id))
      const merged = [...previous]
      for (const item of incoming) {
        if (seen.has(item.id)) continue
        seen.add(item.id)
        merged.push(item)
      }
      // 快照是「最新在前」，SSE 事件是「随后到达」；统一按 ts 倒序，
      // 避免播种后新事件被排到快照末尾而在前 12 行之外。
      merged.sort((a, b) => Date.parse(b.ts) - Date.parse(a.ts))
      return merged.slice(0, LIVE_FEED_LIMIT)
    }

    const appendEvent = (event: LiveFeedEvent) => {
      setEvents((previous) => mergeEvents(previous, [event]))
    }

    /**
     * L1：SSE onopen 只证明通道建立，不代表已有事件。并行拉一次快照播种，
     * 让首屏立刻有内容；首事件到达前统一显示「等待新调用」。播种失败不
     * 影响 SSE（429/网络错误静默，404 仍按关闭区块处理）。
     */
    const seed = async () => {
      try {
        const snapshot = await fetchLiveFeedSnapshot(LIVE_FEED_LIMIT)
        if (disposed) return
        setEvents((previous) => mergeEvents(previous, snapshot.events))
        setAggregate(snapshot.window)
      } catch (error) {
        if (disposed) return
        if (error instanceof ApiError && error.kind === 'not_found') {
          setDisabled(true)
        }
      }
    }

    /**
     * SSE 模式下的聚合条刷新：只取窗口读数，不覆盖事件列表（事件由 SSE
     * 实时插入，快照可能比本地列表旧）。失败静默——聚合条缺失不等于
     * 通道断流，下一轮再试。
     */
    const refreshAggregate = async () => {
      try {
        const snapshot = await fetchLiveFeedSnapshot(1)
        if (disposed) return
        setAggregate(snapshot.window)
      } catch {
        // 限流/网络抖动都静默：这条读数不是内容主路径。
      }
    }

    const schedule = (delayMs: number) => {
      if (disposed) return
      timer = setTimeout(() => {
        timer = undefined
        void poll()
      }, delayMs)
    }

    const poll = async () => {
      if (disposed || polling) return
      polling = true
      try {
        const snapshot = await fetchLiveFeedSnapshot(LIVE_FEED_LIMIT)
        if (disposed) return
        failures = 0
        setEvents(snapshot.events)
        setAggregate(snapshot.window)
        setConnection('live')
        schedule(5000)
      } catch (error) {
        if (disposed) return
        if (error instanceof ApiError && error.kind === 'not_found') {
          setDisabled(true)
          return
        }
        failures += 1
        const limited =
          error instanceof ApiError && error.kind === 'rate_limited'
        // 429 是「限流退避」而不是断线：不退到 reconnecting 徽标/提示，
        // 避免把服务端主动节流误报成连接中断（L5）。
        if (!limited) setConnection('reconnecting')
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
      } finally {
        polling = false
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
          void seed()
          // SSE 帧不携带聚合读数：用低频轮询保持 window 新鲜（见文件头）。
          if (windowTimer === undefined) {
            windowTimer = setInterval(() => {
              void refreshAggregate()
            }, LIVE_FEED_WINDOW_REFRESH_MS)
          }
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
          // 轮询每 5s 自带 window，保留独立计时器会变成双请求。
          if (windowTimer !== undefined) {
            clearInterval(windowTimer)
            windowTimer = undefined
          }
          failures = 0
          setConnection('reconnecting')
          void poll()
        }
      } catch {
        void poll()
      }
    }

    /**
     * L5：网络恢复时立即补拉一次，不再等 5/15/60s 的退避窗口。仅降级
     * 轮询时需要（SSE 由浏览器自行重连）；清理未触发的退避计时器避免
     * 双请求。
     */
    const onOnline = () => {
      if (disposed) return
      if (timer !== undefined) {
        clearTimeout(timer)
        timer = undefined
      }
      failures = 0
      if (source === undefined) void poll()
    }
    window.addEventListener('online', onOnline)

    return () => {
      disposed = true
      window.removeEventListener('online', onOnline)
      source?.close()
      if (timer !== undefined) clearTimeout(timer)
      if (windowTimer !== undefined) clearInterval(windowTimer)
    }
  }, [paused, hidden])

  const showEmpty = events.length === 0

  if (disabled) return null

  return (
    <section data-testid="live-feed-section" className="ledger">
      <div className="wrap">
        <div className="ledger-head">
          <div>
            <h2 className="section-title">{t('public.live_feed.title')}</h2>
            <p className="section-note">{t('public.live_feed.anonymized')}</p>
          </div>
          <div className="ledger-tools">
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
              className="icon-btn"
            >
              <PauseIcon paused={paused} />
            </button>
          </div>
        </div>

        {aggregate !== null ? (
          <p
            data-testid="live-feed-window"
            className="ledger-window"
            // 完整句子只给读屏/无障碍名称；可见文本拆成三段，才能按 UI
            // 设计给「−金额」或零值单独上色。
            aria-label={
              aggregate.cost_credits === 0
                ? t('public.live_feed.window.cost_zero', {
                    calls: formatInteger(aggregate.calls, locale),
                  })
                : t('public.live_feed.window', {
                    calls: formatInteger(aggregate.calls, locale),
                    credits: formatInteger(aggregate.cost_credits, locale),
                  })
            }
          >
            <span>
              {t('public.live_feed.window.calls', {
                calls: formatInteger(aggregate.calls, locale),
              })}
            </span>
            {/* 支出前缀用 Unicode 减号 U+2212（UI 设计 §F-09-02）；零值用
                --amount-zero 渲染 0，非零用 --amount-out。 */}
            <span
              className={
                aggregate.cost_credits === 0
                  ? 'text-amount-zero'
                  : 'text-amount-out'
              }
            >
              {aggregate.cost_credits === 0
                ? '0'
                : `−${formatInteger(aggregate.cost_credits, locale)}`}
            </span>
            <span> {t('public.live_feed.window.credit_unit')}</span>
          </p>
        ) : null}

        {connection === 'reconnecting' ? (
          <p className="ledger-hint" role="status">
            {t('public.live_feed.reconnecting_hint')}
          </p>
        ) : null}

        {showEmpty ? (
          connection === 'live' ? (
            <div className="ledger-empty">
              <span className="wait">
                <span className="pulse-dot" aria-hidden="true" />
                {t('public.live_feed.waiting')}
              </span>
              <p className="hint">{t('public.live_feed.waiting_hint')}</p>
            </div>
          ) : connection === 'paused' ? (
            <div className="ledger-empty">
              <span className="wait">{t('public.live_feed.paused_hint')}</span>
            </div>
          ) : (
            <FeedSkeleton />
          )
        ) : (
          <>
            <div className="ledger-table-wrap">
              <DesktopTable events={events} locale={locale} />
            </div>
            <MobileCards events={events} locale={locale} />
          </>
        )}
      </div>
    </section>
  )
}
