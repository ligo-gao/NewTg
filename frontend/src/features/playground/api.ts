import { apiClient } from '@/api/client'
import { requireData } from '@/api/result'
import { csrfFetch, REQUEST_ID_HEADER } from '@/lib/csrf'

export interface PlaygroundChatMessage {
  role: 'user' | 'assistant'
  content: string
}

export interface PlaygroundUsageTokens {
  prompt_tokens: number
  completion_tokens: number
}

export interface PlaygroundStreamResult {
  content: string
  usage?: PlaygroundUsageTokens
  requestId?: string
  finishReason?: string
}

/**
 * `/pg/*` 的错误信封（relay-01 §3.6）：OpenAI 形状，不是管理面的 RFC 7807。
 * 请求 ID 只从 `X-Request-ID` 头读取（web-00 §13.3 C-2）。
 */
export class PlaygroundError extends Error {
  readonly status: number
  readonly code?: string
  readonly requestId?: string

  constructor(status: number, message: string, code?: string, requestId?: string) {
    super(message)
    this.name = 'PlaygroundError'
    this.status = status
    if (code !== undefined) this.code = code
    if (requestId !== undefined) this.requestId = requestId
  }
}

interface StreamChunk {
  id?: unknown
  model?: unknown
  choices?: unknown
  usage?: unknown
}

function readUsage(value: unknown): PlaygroundUsageTokens | undefined {
  if (typeof value !== 'object' || value === null) return undefined
  const record = value as Record<string, unknown>
  const prompt = record.prompt_tokens
  const completion = record.completion_tokens
  if (typeof prompt !== 'number' || typeof completion !== 'number') {
    return undefined
  }
  return { prompt_tokens: prompt, completion_tokens: completion }
}

function readDelta(value: unknown): string {
  if (!Array.isArray(value)) return ''
  let text = ''
  for (const choice of value) {
    if (typeof choice !== 'object' || choice === null) continue
    const delta = (choice as Record<string, unknown>).delta
    if (typeof delta !== 'object' || delta === null) continue
    const content = (delta as Record<string, unknown>).content
    if (typeof content === 'string') text += content
  }
  return text
}

async function readError(response: Response): Promise<PlaygroundError> {
  const requestId = response.headers.get(REQUEST_ID_HEADER) ?? undefined
  let code: string | undefined
  let message = `HTTP ${response.status}`
  try {
    const body: unknown = await response.clone().json()
    if (typeof body === 'object' && body !== null) {
      const error = (body as Record<string, unknown>).error
      if (typeof error === 'object' && error !== null) {
        const record = error as Record<string, unknown>
        if (typeof record.code === 'string') code = record.code
        if (typeof record.message === 'string') message = record.message
      }
    }
  } catch {
    // 非 JSON 错误体：保留状态码文案。
  }
  return new PlaygroundError(response.status, message, code, requestId)
}

function isAbortError(error: unknown): boolean {
  return (
    typeof error === 'object' &&
    error !== null &&
    (error as { name?: unknown }).name === 'AbortError'
  )
}

export { isAbortError }

/**
 * 会话身份调用 `POST /pg/chat/completions`（OpenAI 协议，SSE）。
 *
 * 关键约束（relay-01 §1.5）：
 * - **不带 `Authorization`**：Playground 只接受浏览器会话，带令牌会被 403；
 * - 写请求带 CSRF 头，`csrf_failed` 走 `csrfFetch` 与契约客户端完全相同的
 *   「GET /api/me 刷新 → 原请求重放一次」路径，不另造一套；
 * - `stream_options.include_usage` 显式打开：中继会在 `[DONE]` 前合成末块
 *   usage（relay-03 §2.3），页面的用量摘要只信服务端事实，不做本地估算。
 */
export async function streamChatCompletion(options: {
  model: string
  messages: PlaygroundChatMessage[]
  signal: AbortSignal
  onToken: (delta: string) => void
}): Promise<PlaygroundStreamResult> {
  const response = await csrfFetch('/pg/chat/completions', {
    method: 'POST',
    credentials: 'include',
    headers: {
      'Content-Type': 'application/json',
      Accept: 'text/event-stream',
    },
    body: JSON.stringify({
      model: options.model,
      messages: options.messages,
      stream: true,
      stream_options: { include_usage: true },
    }),
    signal: options.signal,
  })

  if (!response.ok) throw await readError(response)

  const reader = response.body?.getReader()
  if (reader === undefined || reader === null) {
    throw new PlaygroundError(
      response.status,
      'stream body missing',
      undefined,
      response.headers.get(REQUEST_ID_HEADER) ?? undefined,
    )
  }

  const decoder = new TextDecoder()
  let buffer = ''
  let content = ''
  let usage: PlaygroundUsageTokens | undefined
  let finishReason: string | undefined
  let requestId = response.headers.get(REQUEST_ID_HEADER) ?? undefined

  function consumeFrame(frame: string): boolean {
    // 心跳帧是 `: PING` 注释；只处理 data 行。
    const dataLines = frame
      .split('\n')
      .filter((line) => line.startsWith('data:'))
      .map((line) => line.slice(5).trim())
    if (dataLines.length === 0) return false
    const payload = dataLines.join('\n')
    if (payload === '[DONE]') return true
    let chunk: StreamChunk
    try {
      chunk = JSON.parse(payload) as StreamChunk
    } catch {
      return false
    }
    const delta = readDelta(chunk.choices)
    if (delta !== '') {
      content += delta
      options.onToken(delta)
    }
    const parsedUsage = readUsage(chunk.usage)
    if (parsedUsage !== undefined) usage = parsedUsage
    if (Array.isArray(chunk.choices)) {
      for (const choice of chunk.choices) {
        if (typeof choice !== 'object' || choice === null) continue
        const reason = (choice as Record<string, unknown>).finish_reason
        if (typeof reason === 'string') finishReason = reason
      }
    }
    if (typeof chunk.id === 'string' && chunk.id.startsWith('chatcmpl-')) {
      requestId = chunk.id.slice('chatcmpl-'.length)
    }
    return false
  }

  try {
    for (;;) {
      const { done, value } = await reader.read()
      if (done) break
      buffer += decoder.decode(value, { stream: true })
      let separator = buffer.indexOf('\n\n')
      while (separator !== -1) {
        const frame = buffer.slice(0, separator)
        buffer = buffer.slice(separator + 2)
        if (consumeFrame(frame)) {
          await reader.cancel()
          return { content, usage, requestId, finishReason }
        }
        separator = buffer.indexOf('\n\n')
      }
    }
    if (buffer.trim() !== '') consumeFrame(buffer)
  } finally {
    reader.releaseLock()
  }

  return { content, usage, requestId, finishReason }
}

/**
 * 完成后按 request_id 回读 `usage_logs.cost`（本轮 credits）。
 *
 * 为什么再查一次：SSE 末块只带 token 数，credits 是计费管线的结算结果，
 * 本地用价格表估算会与「实际扣费」分叉。查不到（日志尚未落库/无权限）时
 * 返回 null，界面显示待定而不是编造数字。
 */
export async function fetchTurnCredits(
  requestId: string,
): Promise<number | null> {
  try {
    const data = requireData(
      await apiClient.GET('/api/usage-logs', {
        params: { query: { request_id: requestId, page_size: 1 } },
      }),
    )
    const item = (data.items ?? [])[0] as
      | { cost?: unknown; token_id?: unknown }
      | undefined
    return typeof item?.cost === 'number' ? item.cost : null
  } catch {
    return null
  }
}
