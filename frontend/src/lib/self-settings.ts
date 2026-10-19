import { apiClient } from '@/api/client'
import { requireData } from '@/api/result'
import type { components } from '@/api/schema'
import type { Locale } from '@/shared/lib/i18n-context'

export type SelfSettings = components['schemas']['SelfSettings']

/**
 * 个人设置（GET /api/self）的缓存键。语言卡与 i18n 运行时共用同一条缓存，
 * 避免「读取偏好」和「写入偏好」各发一次请求，也让写入后立刻反映到运行时。
 */
export const SELF_SETTINGS_QUERY_KEY = ['self', 'settings'] as const

/**
 * 读取当前用户的个人设置。仅登录后才有意义：调用方必须以 `enabled`
 * 控制是否发出请求（匿名访问会得到 401）。
 */
export function selfSettingsQueryOptions() {
  return {
    queryKey: SELF_SETTINGS_QUERY_KEY,
    queryFn: async (): Promise<SelfSettings> =>
      requireData(await apiClient.GET('/api/self')),
    retry: false as const,
    staleTime: 30_000,
  }
}

/**
 * 保存界面语言（F-01-19）。
 *
 * 为什么必须带 `updated_at`：该端点采用乐观锁，缺失或过期的值会返回
 * 400/409（modules-01 §自设更新）。调用方从 GET /api/self 的缓存里取
 * 最新令牌，写入成功后把新响应写回同一缓存。
 */
export async function updateLanguagePreference(
  language: Locale,
  updatedAt: string,
): Promise<SelfSettings> {
  return requireData(
    await apiClient.PUT('/api/self/setting', {
      body: { language, updated_at: updatedAt },
    }),
  )
}

/**
 * 保存默认模型（F-02-18）。
 *
 * 与语言偏好共用同一条 PUT /api/self/setting：这是稀疏部分更新，未出现的键
 * 保持原值，因此这里只提交 `default_model`，不会覆盖 language/sidebar_modules。
 * 空串是契约规定的「清除」语义（服务端落 NULL），不是「无操作」。
 * 乐观锁令牌同样来自 GET /api/self 的 `updated_at`，过期会 409。
 */
export async function updateDefaultModel(
  defaultModel: string,
  updatedAt: string,
): Promise<SelfSettings> {
  return requireData(
    await apiClient.PUT('/api/self/setting', {
      body: { default_model: defaultModel, updated_at: updatedAt },
    }),
  )
}
