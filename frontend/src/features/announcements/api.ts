/**
 * 站点公告的传输层（D7b；契约 D7a 已入 `api/schema.d.ts`）。
 *
 * 管理面（settings:manage）与用户面（本人）共用这个模块，避免两个页面
 * 各自复制路径与错误语义。所有端点都走生成的 `apiClient`：
 * - 非 2xx 统一抛 `ApiError`（含 kind / fields / retry_after）；
 * - 写请求自动带 CSRF，失败时按冻结流程刷新会话后重放一次；
 * - 发布/下线是**幂等**动作，调用方为「同一次用户意图」生成 key 并在
 *   重试时复用（中间件的默认 key 只覆盖单次请求，无法跨重试复用）。
 */
import { apiClient } from '@/api/client'
import { requireData } from '@/api/result'
import type { components, operations } from '@/api/schema'

export type AnnouncementSummary = components['schemas']['AnnouncementSummary']
export type AnnouncementList = components['schemas']['AnnouncementList']
export type AnnouncementDetail = components['schemas']['AnnouncementDetail']
export type AnnouncementWriteInput =
  components['schemas']['AnnouncementWriteInput']
export type AnnouncementCategory = components['schemas']['AnnouncementCategory']
export type AnnouncementStatus = components['schemas']['AnnouncementStatus']
export type AnnouncementDisplayStatus =
  components['schemas']['AnnouncementDisplayStatus']
export type ActiveAnnouncement = components['schemas']['ActiveAnnouncement']
export type ActiveAnnouncementList =
  components['schemas']['ActiveAnnouncementList']

export type AnnouncementListQuery = NonNullable<
  operations['listAnnouncements']['parameters']['query']
>

/** 同一用户意图（一次点击）的幂等键；重试必须复用同一个值。 */
export function createIdempotencyKey(): string {
  if (typeof crypto !== 'undefined' && typeof crypto.randomUUID === 'function') {
    return crypto.randomUUID()
  }
  return `announcement-${Date.now()}-${Math.random().toString(36).slice(2)}`
}

export async function listAnnouncements(
  query: AnnouncementListQuery,
  signal?: AbortSignal,
): Promise<AnnouncementList> {
  return requireData(
    await apiClient.GET('/api/announcements', { params: { query }, signal }),
  )
}

export async function getAnnouncement(
  id: string,
  signal?: AbortSignal,
): Promise<AnnouncementDetail> {
  return requireData(
    await apiClient.GET('/api/announcements/{id}', {
      params: { path: { id } },
      signal,
    }),
  )
}

export async function createAnnouncement(
  body: AnnouncementWriteInput,
): Promise<AnnouncementDetail> {
  return requireData(
    await apiClient.POST('/api/announcements', { body }),
  )
}

export async function updateAnnouncement(
  id: string,
  body: AnnouncementWriteInput,
): Promise<AnnouncementDetail> {
  return requireData(
    await apiClient.PUT('/api/announcements/{id}', {
      params: { path: { id } },
      body,
    }),
  )
}

export async function deleteAnnouncement(id: string): Promise<void> {
  // 204 没有响应体；存在性由服务端 404 表达，这里只关心成败。
  await apiClient.DELETE('/api/announcements/{id}', { params: { path: { id } } })
}

export async function publishAnnouncement(
  id: string,
  idempotencyKey: string,
): Promise<AnnouncementDetail> {
  return requireData(
    await apiClient.POST('/api/announcements/{id}/publish', {
      params: { path: { id }, header: { 'Idempotency-Key': idempotencyKey } },
    }),
  )
}

export async function offlineAnnouncement(
  id: string,
  idempotencyKey: string,
): Promise<AnnouncementDetail> {
  return requireData(
    await apiClient.POST('/api/announcements/{id}/offline', {
      params: { path: { id }, header: { 'Idempotency-Key': idempotencyKey } },
    }),
  )
}

/** 用户面：生效中的公告 + 当前用户的已读标记（未登录 401）。 */
export async function listActiveAnnouncements(
  signal?: AbortSignal,
): Promise<ActiveAnnouncementList> {
  return requireData(
    await apiClient.GET('/api/announcements/active', { signal }),
  )
}

/** 标记已读是幂等 upsert：重复调用 204；非生效公告 404（不透露存在性）。 */
export async function markAnnouncementRead(id: string): Promise<void> {
  await apiClient.POST('/api/announcements/{id}/read', {
    params: { path: { id } },
  })
}
