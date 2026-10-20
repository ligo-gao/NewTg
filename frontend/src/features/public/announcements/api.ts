/**
 * 公开公告详情传输层（D7b；契约见 OpenAPI `getPublicAnnouncement`）。
 *
 * 只用生成客户端的类型化 `apiClient`：路径、UUID 参数与响应字段都由
 * `api/schema.d.ts` 校验。服务端返回的 `content_html` 已经过
 * `markup.MarkdownToHTML` 净化，是页面唯一允许注入 HTML 的字段。
 *
 * 错误语义：不可见/不存在统一 404（服务端不区分），429 带 Retry-After，
 * 传输失败由 `apiClient` 归一为 `ApiError`（offline），页面无需自行解析。
 */

import { apiClient } from '@/api/client'
import { requireData } from '@/api/result'
import type { components } from '@/api/schema'

/** 与 `announcements.category` 枚举一致；未知值由生成的联合类型拦截。 */
export type PublicAnnouncementCategory = components['schemas']['PublicAnnouncement']['category']

/**
 * 公开只读响应。空标题/空正文是合法的降级输入（页面负责占位文案），
 * `content_html` 是服务端净化后的 HTML。
 */
export type PublicAnnouncement = components['schemas']['PublicAnnouncement']

export function publicAnnouncementQueryKey(id: string) {
  return ['public', 'announcements', 'detail', id] as const
}

/**
 * 读取公告详情。`id` 由 openapi-fetch 的 path 序列化器统一
 * `encodeURIComponent`；不要在调用处二次编码。
 */
export async function fetchPublicAnnouncement(
  id: string,
  signal?: AbortSignal,
): Promise<PublicAnnouncement> {
  const result = await apiClient.GET('/api/announcements/public/{id}', {
    params: { path: { id } },
    signal,
  })
  return requireData(result)
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
