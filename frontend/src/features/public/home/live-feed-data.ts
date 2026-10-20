/**
 * 首页 live_feed 的数据通道（F-09-02 / D6b）。
 *
 * 两个端点共享同一份脱敏事件：SSE 优先，断流/不支持时降级为 5s 轮询
 * `GET /api/home/live-feed?limit=20`。事件字段白名单：
 * id/ts/model/upstream/tokens_in/tokens_out/latency_ms/status —— 不含费用、
 * 用户、渠道或上游凭据（裁决 A-1）。本模块在边界处丢弃不合约的条目，
 * 一条坏数据不会让整块动态消失。
 */

import { toApiError } from '@/api/client'
import { parseProblem } from '@/api/errors'
import type { components } from '@/api/schema'

export type LiveFeedEvent = components['schemas']['LiveFeedEvent']
export type LiveFeedUpstream = components['schemas']['LiveFeedUpstream']

export const LIVE_FEED_LIMIT = 20
export const LIVE_FEED_SSE_PATH = '/api/home/live-feed/stream'
export const LIVE_FEED_POLL_PATH = '/api/home/live-feed'

const UPSTREAMS: readonly LiveFeedUpstream[] = [
  'openai',
  'anthropic',
  'gemini',
  'unknown',
]

function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === 'object' && value !== null
}

function isFiniteNumber(value: unknown): value is number {
  return typeof value === 'number' && Number.isFinite(value)
}

export function normalizeLiveFeedEvent(value: unknown): LiveFeedEvent | null {
  if (!isRecord(value)) return null
  const upstream = value.upstream
  const status = value.status
  if (
    typeof value.id !== 'string' ||
    typeof value.ts !== 'string' ||
    typeof value.model !== 'string' ||
    !isFiniteNumber(value.tokens_in) ||
    !isFiniteNumber(value.tokens_out) ||
    !isFiniteNumber(value.latency_ms) ||
    (status !== 'ok' && status !== 'error') ||
    !(UPSTREAMS as readonly unknown[]).includes(upstream)
  ) {
    return null
  }
  return {
    id: value.id,
    ts: value.ts,
    model: value.model,
    upstream: upstream as LiveFeedUpstream,
    tokens_in: value.tokens_in,
    tokens_out: value.tokens_out,
    latency_ms: value.latency_ms,
    status,
  }
}

export function normalizeLiveFeedList(body: unknown): LiveFeedEvent[] {
  if (!isRecord(body) || !Array.isArray(body.events)) {
    throw new Error('invalid live feed envelope')
  }
  return body.events.flatMap((entry) => {
    const event = normalizeLiveFeedEvent(entry)
    return event === null ? [] : [event]
  })
}

function apiBase(): string {
  return typeof location !== 'undefined' ? location.origin : 'http://localhost'
}

export async function fetchLiveFeedEvents(
  limit: number = LIVE_FEED_LIMIT,
  signal?: AbortSignal,
): Promise<LiveFeedEvent[]> {
  let response: Response
  try {
    response = await fetch(
      new URL(`${LIVE_FEED_POLL_PATH}?limit=${limit}`, apiBase()).toString(),
      {
        credentials: 'same-origin',
        headers: { Accept: 'application/json' },
        signal,
      },
    )
  } catch (error) {
    if (error instanceof DOMException && error.name === 'AbortError') throw error
    throw parseProblem({ status: 0 })
  }
  if (!response.ok) throw await toApiError(response)
  return normalizeLiveFeedList(await response.json().catch(() => undefined))
}

/** SSE `event: call` 帧的 data 解析；损坏帧返回 null 而不是抛错断流。 */
export function parseLiveFeedSseData(data: string): LiveFeedEvent | null {
  try {
    return normalizeLiveFeedEvent(JSON.parse(data))
  } catch {
    return null
  }
}

/** `HH:mm:ss`，等宽显示由调用方设置；解析失败原样返回。 */
export function formatClock(iso: string, locale: string): string {
  const ms = Date.parse(iso)
  if (Number.isNaN(ms)) return iso
  return new Intl.DateTimeFormat(locale, {
    hour: '2-digit',
    minute: '2-digit',
    second: '2-digit',
    hour12: false,
  }).format(new Date(ms))
}

export function formatInteger(value: number, locale: string): string {
  return new Intl.NumberFormat(locale, { maximumFractionDigits: 0 }).format(value)
}
