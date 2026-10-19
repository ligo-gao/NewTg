import type { TranslateFn } from '@/shared/lib/i18n-context'

/**
 * 审计动作枚举的文案键映射。
 *
 * 真源是 `backend/internal/audit/audit.go` 的冻结动作枚举：调用侧不得自造
 * 动作名（裁决 R-43），所以前端也把“枚举 → 文案键”集中在这张表里，列表与
 * 详情弹窗共用同一份，避免两处各写一套 switch 而漂移。用
 * `satisfies Record<AuditAction, string>` 让后端新增枚举时编译期就报错，
 * 强制同步词条；运行时仍按字符串查找，故未知动作不会抛错。
 */
export type AuditAction =
  | 'auth.login'
  | 'auth.logout'
  | 'auth.register'
  | 'auth.password_change'
  | 'auth.password_reset'
  | 'auth.2fa_enable'
  | 'auth.2fa_disable'
  | 'auth.2fa_backup_codes_regenerate'
  | 'token.create'
  | 'token.update'
  | 'token.delete'
  | 'token.reset_key'
  | 'token.enable'
  | 'token.disable'
  | 'token.batch_enable'
  | 'token.batch_disable'
  | 'token.batch_delete'
  | 'user.create'
  | 'user.update'
  | 'user.disable'
  | 'user.enable'
  | 'user.delete'
  | 'user.quota_adjust'
  | 'user.reset_2fa'
  | 'user.reset_password'
  | 'group.create'
  | 'group.update'
  | 'group.delete'
  | 'system_initialized'
  | 'authz.denied'

export const AUDIT_ACTION_KEYS = {
  'auth.login': 'logs.audit.actions.auth_login',
  'auth.logout': 'logs.audit.actions.auth_logout',
  'auth.register': 'logs.audit.actions.auth_register',
  'auth.password_change': 'logs.audit.actions.auth_password_change',
  'auth.password_reset': 'logs.audit.actions.auth_password_reset',
  'auth.2fa_enable': 'logs.audit.actions.auth_2fa_enable',
  'auth.2fa_disable': 'logs.audit.actions.auth_2fa_disable',
  'auth.2fa_backup_codes_regenerate':
    'logs.audit.actions.auth_2fa_backup_codes_regenerate',
  'token.create': 'logs.audit.actions.token_create',
  'token.update': 'logs.audit.actions.token_update',
  'token.delete': 'logs.audit.actions.token_delete',
  'token.reset_key': 'logs.audit.actions.token_reset_key',
  'token.enable': 'logs.audit.actions.token_enable',
  'token.disable': 'logs.audit.actions.token_disable',
  'token.batch_enable': 'logs.audit.actions.token_batch_enable',
  'token.batch_disable': 'logs.audit.actions.token_batch_disable',
  'token.batch_delete': 'logs.audit.actions.token_batch_delete',
  'user.create': 'logs.audit.actions.user_create',
  'user.update': 'logs.audit.actions.user_update',
  'user.disable': 'logs.audit.actions.user_disable',
  'user.enable': 'logs.audit.actions.user_enable',
  'user.delete': 'logs.audit.actions.user_delete',
  'user.quota_adjust': 'logs.audit.actions.user_quota_adjust',
  'user.reset_2fa': 'logs.audit.actions.user_reset_2fa',
  'user.reset_password': 'logs.audit.actions.user_reset_password',
  'group.create': 'logs.audit.actions.group_create',
  'group.update': 'logs.audit.actions.group_update',
  'group.delete': 'logs.audit.actions.group_delete',
  system_initialized: 'logs.audit.actions.system_initialized',
  'authz.denied': 'logs.audit.actions.authz_denied',
} satisfies Record<AuditAction, string>

/**
 * 未知动作（或词条缺失）的安全回退：把 `future.event_type` / `legacy_action`
 * 这类机器名转成可读文本。审计日志是排障入口，宁可显示不完美的可读串，
 * 也不能白屏或抛异常；这里只做展示，不参与任何权限/分类判断。
 */
export function humanizeAuditAction(action: string): string {
  const words = action.split(/[._\-\s]+/).filter((word) => word !== '')
  if (words.length === 0) return action
  const joined = words.join(' ').toLowerCase()
  return joined.charAt(0).toUpperCase() + joined.slice(1)
}

/**
 * 把后端动作枚举渲染为本地化文案。`t` 在词条缺失时会原样返回键名，
 * 因此这里把“返回键名”也当作缺失，走 humanize 回退而不是把键名露给用户。
 */
export function auditActionLabel(
  t: TranslateFn,
  action: string | undefined,
): string {
  if (action === undefined || action === '') return '—'
  const key = (AUDIT_ACTION_KEYS as Record<string, string | undefined>)[action]
  if (key !== undefined) {
    const label = t(key)
    if (label !== key) return label
  }
  return humanizeAuditAction(action)
}
