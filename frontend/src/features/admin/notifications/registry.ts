/**
 * 通知模板的**前端冻结注册表**（注册表真源在 backend/internal/notify/registry.go）。
 *
 * 为什么前端也要存一份：冻结的 HTTP 契约（GET/PUT /api/notification-templates）
 * 只返回模板行，不暴露变量白名单、锁定集合与分类清单。编辑器要渲染变量 chip、
 * 判断「未登记变量」并在锁定模板上隐藏开关，就必须在客户端持有一份**只读副本**。
 * 服务端仍是唯一授权方：未知变量/停用锁定模板依然由 422 兜底，本文件只做
 * 提前提示，绝不作为校验边界。
 *
 * 同步要求：backend 注册表变化时必须同步本文件；两边的 16 类 key 与 18 个
 * (key, channel) 变量组逐条对应。
 */

import type { NotificationTemplateChannel } from './types'

export interface RegistryVariable {
  name: string
  required: boolean
}

export interface RegistryCategory {
  key: string
  channels: readonly NotificationTemplateChannel[]
}

/**
 * §3.9.1 的 16 个 category，顺序与注册表一致（auth → security → billing →
 * system → ops）。key 与 category 一一对应。
 */
export const NOTIFICATION_CATEGORIES: readonly RegistryCategory[] = [
  { key: 'auth.verify_code', channels: ['email'] },
  { key: 'auth.password_reset', channels: ['email'] },
  { key: 'auth.welcome', channels: ['email'] },
  { key: 'security.alert', channels: ['email'] },
  { key: 'billing.balance_low', channels: ['email', 'inapp'] },
  { key: 'billing.quota_exhausted', channels: ['email', 'inapp'] },
  { key: 'billing.usage_digest', channels: ['email'] },
  { key: 'billing.payment_success', channels: ['email'] },
  { key: 'billing.payment_failed', channels: ['email'] },
  { key: 'billing.order_expired', channels: ['email'] },
  { key: 'billing.redemption_success', channels: ['email'] },
  { key: 'billing.violation_fee', channels: ['email'] },
  { key: 'system.announcement', channels: ['inapp'] },
  { key: 'system.smtp_test', channels: ['email'] },
  { key: 'ops.channel_auto_disabled', channels: ['email'] },
  { key: 'ops.redemption_expiring', channels: ['email'] },
] as const

export const NOTIFICATION_CATEGORY_KEYS: readonly string[] =
  NOTIFICATION_CATEGORIES.map((category) => category.key)

export function isNotificationCategory(value: unknown): value is string {
  return typeof value === 'string' && NOTIFICATION_CATEGORY_KEYS.includes(value)
}

/**
 * P4 不可关集合（§3.9 / 注册表 `Locked`）。`auth.*` 是通配，因此
 * auth.welcome 也在内；另加 security.alert、system.smtp_test、
 * system.announcement。服务端仍会以 422 locked_template 拒绝停用。
 */
const LOCKED_TEMPLATE_KEYS: ReadonlySet<string> = new Set([
  'auth.verify_code',
  'auth.password_reset',
  'auth.welcome',
  'security.alert',
  'system.smtp_test',
  'system.announcement',
])

export function isLockedTemplate(key: string): boolean {
  return LOCKED_TEMPLATE_KEYS.has(key)
}

/**
 * §3.9.3 的封闭白名单，按 (key, channel) 分组。变量只做文本替换，因此
 * 只记录名称与必填性；类型与样例值由服务端持有（预览接口会合并样例值）。
 */
const VARIABLES: Readonly<Record<string, readonly RegistryVariable[]>> = {
  'auth.verify_code@email': [
    { name: 'username', required: false },
    { name: 'code', required: true },
    { name: 'expires_minutes', required: true },
    { name: 'purpose_label', required: true },
  ],
  'auth.password_reset@email': [
    { name: 'username', required: false },
    { name: 'reset_url', required: true },
    { name: 'expires_minutes', required: true },
    { name: 'ip', required: false },
  ],
  'auth.welcome@email': [
    { name: 'username', required: false },
    { name: 'site_title', required: true },
    { name: 'welcome_bonus', required: false },
    { name: 'dashboard_url', required: true },
  ],
  'security.alert@email': [
    { name: 'username', required: false },
    { name: 'event_title', required: true },
    { name: 'event_detail', required: true },
    { name: 'event_time', required: true },
    { name: 'ip', required: false },
    { name: 'device', required: false },
    { name: 'action_hint', required: true },
    { name: 'security_url', required: true },
  ],
  'billing.balance_low@email': [
    { name: 'username', required: false },
    { name: 'balance', required: true },
    { name: 'threshold', required: true },
    { name: 'topup_url', required: true },
    { name: 'site_title', required: true },
  ],
  'billing.balance_low@inapp': [
    { name: 'balance', required: true },
    { name: 'threshold', required: true },
    { name: 'topup_url', required: true },
  ],
  'billing.quota_exhausted@email': [
    { name: 'username', required: false },
    { name: 'balance', required: true },
    { name: 'topup_url', required: true },
    { name: 'site_title', required: true },
  ],
  'billing.quota_exhausted@inapp': [
    { name: 'balance', required: true },
    { name: 'topup_url', required: true },
  ],
  'billing.usage_digest@email': [
    { name: 'username', required: false },
    { name: 'period_label', required: true },
    { name: 'total_credits', required: true },
    { name: 'request_count', required: true },
    { name: 'top_models', required: true },
    { name: 'dashboard_url', required: true },
  ],
  'billing.payment_success@email': [
    { name: 'username', required: false },
    { name: 'amount', required: true },
    { name: 'credits', required: true },
    { name: 'order_no', required: true },
    { name: 'paid_at', required: true },
    { name: 'wallet_url', required: true },
  ],
  'billing.payment_failed@email': [
    { name: 'username', required: false },
    { name: 'amount', required: true },
    { name: 'order_no', required: true },
    { name: 'reason_label', required: true },
    { name: 'failed_at', required: true },
    { name: 'retry_url', required: true },
  ],
  'billing.order_expired@email': [
    { name: 'username', required: false },
    { name: 'order_no', required: true },
    { name: 'amount', required: true },
    { name: 'expired_at', required: true },
    { name: 'topup_url', required: true },
  ],
  'billing.redemption_success@email': [
    { name: 'username', required: false },
    { name: 'credits', required: true },
    { name: 'balance', required: true },
    { name: 'code_hint', required: false },
    { name: 'redeemed_at', required: true },
    { name: 'wallet_url', required: true },
  ],
  'billing.violation_fee@email': [
    { name: 'username', required: false },
    { name: 'fee', required: true },
    { name: 'reason_label', required: true },
    { name: 'occurred_at', required: true },
    { name: 'balance', required: true },
    { name: 'policy_url', required: true },
  ],
  'system.announcement@inapp': [
    { name: 'title', required: true },
    { name: 'summary', required: true },
    { name: 'published_at', required: true },
    { name: 'link_url', required: false },
  ],
  'system.smtp_test@email': [
    { name: 'site_title', required: true },
    { name: 'sent_at', required: true },
    { name: 'admin_username', required: true },
  ],
  'ops.channel_auto_disabled@email': [
    { name: 'admin_username', required: false },
    { name: 'channel_name', required: true },
    { name: 'channel_id', required: true },
    { name: 'reason_label', required: true },
    { name: 'disabled_at', required: true },
    { name: 'channels_url', required: true },
  ],
  'ops.redemption_expiring@email': [
    { name: 'admin_username', required: false },
    { name: 'batch_name', required: true },
    { name: 'remaining_count', required: true },
    { name: 'expires_at', required: true },
    { name: 'codes_url', required: true },
  ],
}

export function variablesFor(
  key: string,
  channel: NotificationTemplateChannel,
): readonly RegistryVariable[] {
  return VARIABLES[`${key}@${channel}`] ?? []
}

/** 分类展示名的 i18n 键：点号换成下划线，避免词典层级与 key 的点号混淆。 */
export function categoryLabelKey(category: string): string {
  return `settings.notification_templates.categories.${category.replaceAll('.', '_')}`
}

/** §3.9.2 的正文上限（服务端按 rune 计数，前端同样按码点计数）。 */
export const SUBJECT_MAX_LENGTH = 256
export const BODY_MAX_LENGTH = 16_384
export const CTA_TEXT_MAX_LENGTH = 32

export interface VariableIssue {
  /** 白名单外的变量名（`{{.foo}}` 形式）。 */
  unknown: string[]
  /** 非字段访问构造（管道、函数、条件、未闭合括号等）。 */
  invalid: string[]
}

const FIELD_CONSTRUCT = /^\{\{\s*\.([a-z][a-z0-9_]*)\s*\}\}$/
const CONSTRUCT = /\{\{(?:[^{}]|\{[^{}]*\})*\}\}/g

/**
 * 扫描模板文本里的构造，镜像后端 validateVariables 的口径：
 * - `{{.name}}` 且 name 在白名单内 → 合法；
 * - `{{.name}}` 但 name 不在白名单 → unknown；
 * - 其它 `{{...}}` → invalid；
 * - 剥离合法字段后仍残留花括号 → invalid（未闭合/嵌套）。
 */
export function scanTemplateVariables(
  text: string,
  key: string,
  channel: NotificationTemplateChannel,
): VariableIssue {
  const allowed = new Set(variablesFor(key, channel).map((variable) => variable.name))
  const unknown: string[] = []
  const invalid: string[] = []

  const constructs = text.match(CONSTRUCT) ?? []
  for (const construct of constructs) {
    const match = FIELD_CONSTRUCT.exec(construct)
    if (match === null) {
      if (!invalid.includes(construct)) invalid.push(construct)
      continue
    }
    const name = match[1] ?? ''
    if (!allowed.has(name) && !unknown.includes(name)) unknown.push(name)
  }

  // 与后端一致：只剥离字段构造，残留的 `{`/`}` 都是非法构造。
  // 已经命中非法构造时不再追加整段残留，避免同一条错误出现两次。
  const stripped = text.replace(/\{\{\s*\.[a-z][a-z0-9_]*\s*\}\}/g, '')
  if (invalid.length === 0 && /[{}]/.test(stripped)) {
    const residue = stripped.trim()
    if (residue !== '' && !invalid.includes(residue)) invalid.push(residue)
  }

  return { unknown, invalid }
}
