import {
  updateLanguagePreference,
  type SelfSettings,
} from '@/lib/self-settings'
import type { Locale } from '@/shared/lib/i18n-context'

/**
 * 界面语言偏好的浏览器端存储（单一来源）。
 *
 * 三个键的语义必须严格区分，否则「用户刚选的语言」和「账号保存的偏好」会
 * 互相覆盖：
 * - `newtg.language`：当前浏览器记住的语言，首屏同步读取（本地缓存）；
 * - `newtg.language.explicit`：`"1"` 表示用户在本浏览器显式选过语言；
 * - `newtg.language.pending`：`"1"` 表示这次显式选择尚未成功写回服务端。
 *
 * 只有用户手势（下拉 change）能写 `explicit` / `pending`；服务端偏好落地、
 * F5 归一化和营销站守卫只写 `newtg.language`。
 *
 * 所有读写都包在 `try/catch` 里：隐私模式或站点数据被清理时，存储访问可能
 * 直接抛错，此时语言切换必须照常工作，只是没有跨刷新的记忆。
 */

export const LOCALE_STORAGE_KEY = 'newtg.language'
export const LANGUAGE_PENDING_KEY = 'newtg.language.pending'
export const LANGUAGE_EXPLICIT_KEY = 'newtg.language.explicit'
export const LANGUAGE_SCHEMA_KEY = 'newtg.language.schema'
export const LANGUAGE_SCHEMA_VERSION = '2'

/**
 * 随包发布的 7 语。与 `shared/ui/language-switch.tsx` 一样在此重复声明，
 * 而不是从 `app/providers/i18n` 导入：后者的 `isLocale` 反过来依赖本模块，
 * 静态导入会形成环。
 */
const BUNDLED_LOCALES: readonly Locale[] = [
  'en',
  'zh-CN',
  'zh-TW',
  'fr',
  'ru',
  'ja',
  'vi',
]

export function isLocale(value: string): value is Locale {
  return (BUNDLED_LOCALES as readonly string[]).includes(value)
}

function read(key: string): string | null {
  try {
    return globalThis.localStorage?.getItem(key) ?? null
  } catch {
    return null
  }
}

function write(key: string, value: string): void {
  try {
    globalThis.localStorage?.setItem(key, value)
  } catch {
    // 存储不可用不影响当前会话：语言已在内存状态中生效。
  }
}

function remove(key: string): void {
  try {
    globalThis.localStorage?.removeItem(key)
  } catch {
    // 同 write：标记清不掉也不能让调用方失败。
  }
}

/** 读取本地记忆；未知值（旧版本/外部写入）当作没有记忆。 */
export function readStoredLocale(): Locale | null {
  const value = read(LOCALE_STORAGE_KEY)
  return value !== null && isLocale(value) ? value : null
}

export function writeStoredLocale(locale: Locale): void {
  write(LOCALE_STORAGE_KEY, locale)
}

export function hasPendingSync(): boolean {
  return read(LANGUAGE_PENDING_KEY) === '1'
}

export function hasExplicitChoice(): boolean {
  return read(LANGUAGE_EXPLICIT_KEY) === '1'
}

export function markPendingSync(): void {
  write(LANGUAGE_PENDING_KEY, '1')
}

export function clearPendingSync(): void {
  remove(LANGUAGE_PENDING_KEY)
}

/**
 * 用户手势：同时写语言、显式标记与待同步标记。三者是一个不可分割的动作，
 * 拆开写会让中途刷新出现「语言变了但不算显式选择」之类的半状态。
 */
export function markLanguageChosen(locale: Locale): void {
  write(LOCALE_STORAGE_KEY, locale)
  write(LANGUAGE_EXPLICIT_KEY, '1')
  write(LANGUAGE_PENDING_KEY, '1')
}

/**
 * 退出登录：只清同步标记，保留语言本身，匿名页面仍显示用户选过的语言。
 * （服务端偏好已被账号带走；下次登录由服务端值重新生效。）
 */
export function clearLanguageChoice(): void {
  remove(LANGUAGE_PENDING_KEY)
  remove(LANGUAGE_EXPLICIT_KEY)
}

/**
 * 一次性迁移标记。存量浏览器只补 `schema`，**不**回填 `explicit`：旧版本
 * 写 `newtg.language` 时并不知道用户是否显式选过，伪造 `explicit` 会让
 * 营销站把匿名访客错误地重定向到 `/en/` 之类的静态路径。
 */
export function ensureLanguageSchema(): void {
  if (read(LANGUAGE_SCHEMA_KEY) !== LANGUAGE_SCHEMA_VERSION) {
    write(LANGUAGE_SCHEMA_KEY, LANGUAGE_SCHEMA_VERSION)
  }
}

/**
 * 把本地选择写回服务端。
 *
 * `PUT /api/self/setting` 使用乐观锁，必须带回 `GET /api/self` 的
 * `updated_at`；调用方拿不到 `current` 时无法安全写入，返回 `null`。
 * 成功时清掉 `pending` 并返回新设置；失败时**保留** `pending`，让下一次
 * 启动/联网继续补写。服务端已是同一语言时无需请求。
 */
export async function persistLanguagePreference(
  locale: Locale,
  current: SelfSettings | undefined,
): Promise<SelfSettings | null> {
  if (current === undefined) return null
  if (current.language === locale) {
    clearPendingSync()
    return current
  }
  try {
    const updated = await updateLanguagePreference(locale, current.updated_at)
    clearPendingSync()
    return updated
  } catch {
    return null
  }
}
