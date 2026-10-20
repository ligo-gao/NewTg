/**
 * 公开公告详情传输层（契约待 OpenAPI 登记）。
 *
 * `GET /api/announcements/public/{id}` 目前尚未进入 `api/schema.d.ts`
 * （管理面的 `/api/announcements/{id}` 是鉴权端点，形状也不同：它返回
 * Markdown 源文，而公开端点返回服务端净化后的 `content_html`）。因此这里
 * 手写一份**最小**的请求/响应类型，并用与 `api/client` 相同的
 * `parseProblem`/`toApiError` 归一错误。
 *
 * 待 OpenAPI 登记后替换：删除本文件的 `PublicAnnouncement` 与手动 fetch，
 * 改用 `apiClient.GET('/api/announcements/public/{id}')` 的生成类型即可，
 * 页面与查询键无需改动。
 */

import { toApiError } from '@/api/client'
import { parseProblem } from '@/api/errors'

/** 与 `announcements.category` 枚举一致；未知值视为响应损坏。 */
export type PublicAnnouncementCategory = 'notice' | 'maintenance' | 'release'

/**
 * 公开只读响应。空标题/空正文是合法的降级输入（页面负责占位文案），
 * 因此只有类型错误才判定为契约违例。
 */
export interface PublicAnnouncement {
  id: string
  title: string
  category: PublicAnnouncementCategory
  /** 服务端已按 `SanitizeMarkdown` 净化；唯一允许注入 HTML 的字段。 */
  content_html: string
  published_at: string
  expire_at: string | null
}

const CATEGORIES: readonly PublicAnnouncementCategory[] = [
  'notice',
  'maintenance',
  'release',
]

/** Same-origin absolute URL; jsdom's Node fetch rejects relative URLs in tests. */
function apiUrl(path: string): string {
  const base = typeof location !== 'undefined' ? location.origin : 'http://localhost'
  return new URL(path, base).toString()
}

function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === 'object' && value !== null
}

function requiredString(body: Record<string, unknown>, key: string): string {
  const value = body[key]
  if (typeof value !== 'string') throw invalidResponse()
  return value
}

/**
 * 契约违例按服务端错误处理：页面显示可重试的状态，而不是把半截数据
 * 渲成 HTML。`parseProblem` 保证调用方拿到的仍是同一种 `ApiError`。
 */
function invalidResponse() {
  return parseProblem({ status: 500 })
}

export function normalizePublicAnnouncement(body: unknown): PublicAnnouncement {
  if (!isRecord(body)) throw invalidResponse()

  const category = body.category
  if (
    typeof category !== 'string' ||
    !(CATEGORIES as readonly string[]).includes(category)
  ) {
    throw invalidResponse()
  }

  const expireAt = body.expire_at
  if (expireAt !== null && expireAt !== undefined && typeof expireAt !== 'string') {
    throw invalidResponse()
  }

  return {
    id: requiredString(body, 'id'),
    title: requiredString(body, 'title'),
    category: category as PublicAnnouncementCategory,
    content_html: requiredString(body, 'content_html'),
    published_at: requiredString(body, 'published_at'),
    expire_at: typeof expireAt === 'string' ? expireAt : null,
  }
}

export function publicAnnouncementQueryKey(id: string) {
  return ['public', 'announcements', 'detail', id] as const
}

export async function fetchPublicAnnouncement(
  id: string,
  signal?: AbortSignal,
): Promise<PublicAnnouncement> {
  const path = `/api/announcements/public/${encodeURIComponent(id)}`
  let response: Response
  try {
    response = await fetch(apiUrl(path), {
      credentials: 'same-origin',
      headers: { Accept: 'application/json' },
      signal,
    })
  } catch (error) {
    // Abort is a caller cancellation, not an offline condition.
    if (error instanceof DOMException && error.name === 'AbortError') throw error
    throw parseProblem({ status: 0 })
  }
  if (!response.ok) throw await toApiError(response)
  const body: unknown = await response.json().catch(() => undefined)
  return normalizePublicAnnouncement(body)
}

export function publicAnnouncementQueryOptions(id: string) {
  return {
    queryKey: publicAnnouncementQueryKey(id),
    queryFn: ({ signal }: { signal: AbortSignal }) =>
      fetchPublicAnnouncement(id, signal),
    // 服务端冻结 `Cache-Control: public, max-age=60`；客户端在同一个窗口内
    // 不重复请求，与 B8 的 60/min 预算保持一致。
    staleTime: 60_000,
    retry: false as const,
  }
}
