/**
 * FE-08 section registry for the settings shell.
 *
 * Each entry maps one backend `section` (the GET/PUT `?section=` value) to a
 * card. Field order here is the visual order; values, types and danger flags
 * always come from the server response, so this file never becomes a second
 * source of truth for defaults or validation bounds beyond UX pre-checks.
 */

export const SETTINGS_TABS = [
  'site',
  'auth',
  'billing',
  'payment',
  'security',
  'notify',
  'policy',
] as const

export type SettingsTab = (typeof SETTINGS_TABS)[number]

export function isSettingsTab(value: unknown): value is SettingsTab {
  return typeof value === 'string' && (SETTINGS_TABS as readonly string[]).includes(value)
}

export type FieldControl =
  | 'text'
  | 'textarea'
  | 'secret'
  | 'switch'
  | 'select'
  | 'number'
  | 'tags'
  | 'lines'
  | 'number_list'
  | 'module_toggles'
  | 'multi_select'
  | 'json'

export interface FieldOption {
  value: string | number
  labelKey: string
}

export interface ToggleOption {
  key: string
  labelKey: string
}

export interface FieldConfig {
  key: string
  labelKey: string
  hintKey?: string
  control: FieldControl
  required?: boolean
  maxLength?: number
  min?: number
  max?: number
  /** Reject values below or equal to this bound (`> value`). */
  greaterThan?: number
  integer?: boolean
  options?: readonly FieldOption[]
  /**
   * Shown instead of the hint while a `multi_select` value is the empty
   * array (e.g. no webhook events subscribed). The empty array stays valid:
   * the backend treats it as "deliver nothing", which is a legitimate draft.
   */
  emptyHintKey?: string
  /** Render the secret as a multi-line textarea (PEM keys). */
  multiline?: boolean
  /** Object keys rendered as switches by the `module_toggles` control. */
  toggles?: readonly ToggleOption[]
  /** Server-enforced read-only field (e.g. `nav.console.require_auth`). */
  locked?: boolean
  /**
   * Risk direction for `dangerous` registry keys (UI design §4): only the
   * direction that loosens safety asks for confirmation; turning a switch
   * back to the safe side does not.
   */
  dangerDirection?: 'off' | 'on' | 'change'
}

export interface SectionConfig {
  /** Backend section id, used verbatim in `?section=`. */
  id: string
  titleKey: string
  descriptionKey?: string
  fields: readonly FieldConfig[]
  /**
   * Section-scoped action rendered next to Save. `webhook` posts to
   * `/api/settings/test/webhook`; keeping it declarative means one card can
   * carry the shape without the shell knowing about notify.webhook.
   */
  testKind?: 'webhook'
}

export interface TabConfig {
  id: SettingsTab
  labelKey: string
  descriptionKey: string
  sections: readonly SectionConfig[]
}

function field(
  key: string,
  labelKey: string,
  control: FieldControl,
  extra: Omit<FieldConfig, 'key' | 'labelKey' | 'control'> = {},
): FieldConfig {
  return { key, labelKey, control, ...extra }
}

function text(
  key: string,
  labelKey: string,
  extra: Omit<FieldConfig, 'key' | 'labelKey' | 'control'> = {},
): FieldConfig {
  return field(key, labelKey, 'text', extra)
}

function toggle(
  key: string,
  labelKey: string,
  extra: Omit<FieldConfig, 'key' | 'labelKey' | 'control'> = {},
): FieldConfig {
  return field(key, labelKey, 'switch', extra)
}

function number(
  key: string,
  labelKey: string,
  extra: Omit<FieldConfig, 'key' | 'labelKey' | 'control'> = {},
): FieldConfig {
  return field(key, labelKey, 'number', { integer: true, ...extra })
}

function label(key: string): string {
  return `settings.field.${key}`
}

function hint(key: string): string {
  return `settings.hint.${key}`
}

const SITE: SectionConfig = {
  id: 'site',
  titleKey: 'settings.section.site',
  descriptionKey: 'settings.section_description.site',
  fields: [
    text('site.title', label('site.title'), {
      required: true,
      maxLength: 64,
      hintKey: hint('site.title'),
    }),
    text('site.logo_url', label('site.logo_url'), { maxLength: 512 }),
    text('site.server_address', label('site.server_address'), {
      required: true,
      maxLength: 256,
      hintKey: hint('site.server_address'),
    }),
    text('site.timezone', label('site.timezone'), { required: true, maxLength: 64 }),
    text('site.docs_url', label('site.docs_url'), { maxLength: 256 }),
    text('site.status_page_url', label('site.status_page_url'), { maxLength: 256 }),
    text('site.filing_no', label('site.filing_no'), { maxLength: 64 }),
    field('site.footer_html', label('site.footer_html'), 'textarea', {
      maxLength: 4096,
      hintKey: hint('site.footer_html'),
    }),
    field('site.about_html', label('site.about_html'), 'textarea', {
      maxLength: 8192,
      hintKey: hint('site.about_html'),
    }),
    text('site.seo.description', label('site.seo.description'), { maxLength: 160 }),
    text('site.seo.og_image_url', label('site.seo.og_image_url'), { maxLength: 512 }),
    toggle('site.home_custom_enabled', label('site.home_custom_enabled'), {
      dangerDirection: 'on',
    }),
    field('site.home_custom_html', label('site.home_custom_html'), 'textarea', {
      maxLength: 65536,
      hintKey: hint('site.home_custom_html'),
      dangerDirection: 'change',
    }),
    field('site.home_blocks', label('site.home_blocks'), 'json'),
  ],
}

const NAV: SectionConfig = {
  id: 'nav',
  titleKey: 'settings.section.nav',
  descriptionKey: 'settings.section_description.nav',
  fields: [
    toggle('nav.home.enabled', label('nav.home.enabled')),
    toggle('nav.home.require_auth', label('nav.home.require_auth')),
    toggle('nav.console.enabled', label('nav.console.enabled')),
    // The server forces this to true; rendering it disabled keeps the UI
    // honest instead of offering a switch whose change would be rejected.
    toggle('nav.console.require_auth', label('nav.console.require_auth'), {
      locked: true,
      hintKey: hint('nav.console.require_auth'),
    }),
    toggle('nav.pricing.enabled', label('nav.pricing.enabled')),
    toggle('nav.pricing.require_auth', label('nav.pricing.require_auth')),
    toggle('nav.rankings.enabled', label('nav.rankings.enabled')),
    toggle('nav.rankings.require_auth', label('nav.rankings.require_auth')),
    toggle('nav.docs.enabled', label('nav.docs.enabled')),
    toggle('nav.about.enabled', label('nav.about.enabled')),
    field('nav.modules.console', label('nav.modules.console'), 'module_toggles', {
      hintKey: hint('nav.modules.console'),
      toggles: [
        { key: 'dashboard', labelKey: 'settings.shell.module_dashboard' },
        { key: 'tokens', labelKey: 'settings.shell.module_tokens' },
        { key: 'usage_logs', labelKey: 'settings.shell.module_usage_logs' },
        { key: 'wallet', labelKey: 'settings.shell.module_wallet' },
        { key: 'profile', labelKey: 'settings.shell.module_profile' },
        { key: 'security', labelKey: 'settings.shell.module_security' },
      ],
    }),
    toggle('nav.modules.chat.playground', label('nav.modules.chat.playground')),
    toggle('nav.modules.chat.presets', label('nav.modules.chat.presets')),
  ],
}

const AUTH: SectionConfig = {
  id: 'auth',
  titleKey: 'settings.section.auth',
  descriptionKey: 'settings.section_description.auth',
  fields: [
    toggle('auth.signup_enabled', label('auth.signup_enabled'), {
      hintKey: hint('auth.signup_enabled'),
      dangerDirection: 'off',
    }),
    toggle('auth.password_login_enabled', label('auth.password_login_enabled'), {
      hintKey: hint('auth.password_login_enabled'),
    }),
    toggle('auth.mfa.admin_required', label('auth.mfa.admin_required')),
    toggle('auth.mfa.admin_hardware_required', label('auth.mfa.admin_hardware_required')),
    toggle('auth.email_verification_required', label('auth.email_verification_required'), {
      hintKey: hint('auth.email_verification_required'),
    }),
    toggle(
      'auth.email_domain_restrict_enabled',
      label('auth.email_domain_restrict_enabled'),
    ),
    field('auth.email_domain_allowlist', label('auth.email_domain_allowlist'), 'tags', {
      hintKey: hint('auth.email_domain_allowlist'),
    }),
  ],
}

const AUTH_TURNSTILE: SectionConfig = {
  id: 'auth.turnstile',
  titleKey: 'settings.section.auth_turnstile',
  descriptionKey: 'settings.section_description.auth_turnstile',
  fields: [
    toggle('auth.turnstile.enabled', label('auth.turnstile.enabled')),
    text('auth.turnstile.site_key', label('auth.turnstile.site_key'), { maxLength: 64 }),
    field('auth.turnstile.secret_key', label('auth.turnstile.secret_key'), 'secret', {
      hintKey: hint('auth.turnstile.secret_key'),
    }),
  ],
}

const BILLING: SectionConfig = {
  id: 'billing',
  titleKey: 'settings.section.billing',
  descriptionKey: 'settings.section_description.billing',
  fields: [
    number('billing.quota_per_unit', label('billing.quota_per_unit'), {
      min: 1,
      max: 1_000_000,
      hintKey: hint('billing.quota_per_unit'),
      dangerDirection: 'change',
    }),
    field('billing.display_unit', label('billing.display_unit'), 'select', {
      options: [
        { value: 'credits', labelKey: 'settings.option.credits' },
        { value: 'cny', labelKey: 'settings.option.cny' },
      ],
      hintKey: hint('billing.display_unit'),
    }),
    text('billing.currency_symbol', label('billing.currency_symbol'), { maxLength: 8 }),
    number('billing.signup_grant_credits', label('billing.signup_grant_credits'), {
      min: 0,
      max: 1_000_000_000_000,
      hintKey: hint('billing.signup_grant_credits'),
    }),
    toggle(
      'billing.signup_grant_requires_verified_email',
      label('billing.signup_grant_requires_verified_email'),
    ),
    number(
      'billing.balance_alert_threshold_credits',
      label('billing.balance_alert_threshold_credits'),
      { min: 0 },
    ),
    field(
      'billing.balance_alert_channels',
      label('billing.balance_alert_channels'),
      'module_toggles',
      {
        hintKey: hint('billing.balance_alert_channels'),
        toggles: [
          { key: 'banner', labelKey: 'settings.shell.module_banner' },
          { key: 'email', labelKey: 'settings.shell.module_email' },
          { key: 'webhook', labelKey: 'settings.shell.module_webhook' },
        ],
      },
    ),
    toggle(
      'billing.free_model_preconsume_enabled',
      label('billing.free_model_preconsume_enabled'),
    ),
    number(
      'billing.free_model_preconsume_multiplier',
      label('billing.free_model_preconsume_multiplier'),
      {
        integer: false,
        greaterThan: 0,
        hintKey: hint('billing.free_model_preconsume_multiplier'),
      },
    ),
    number(
      'billing.free_model_preconsume_trust_credits',
      label('billing.free_model_preconsume_trust_credits'),
      { min: 0, max: 1_000_000_000_000, hintKey: hint('billing.free_model_preconsume_trust_credits') },
    ),
  ],
}

// Daily check-in (modules-08 §3.8; W15-C). Disabled by default; min_quota=0
// is a legitimate "record only" mode and is explained in the hint rather than
// rejected, because the design freezes that combination as valid.
const CHECKIN: SectionConfig = {
  id: 'checkin',
  titleKey: 'settings.section.checkin',
  descriptionKey: 'settings.section_description.checkin',
  fields: [
    toggle('checkin.enabled', label('checkin.enabled')),
    number('checkin.min_quota', label('checkin.min_quota'), {
      min: 0,
      max: 1_000_000_000_000,
      hintKey: hint('checkin.min_quota'),
    }),
    number('checkin.max_quota', label('checkin.max_quota'), {
      min: 0,
      max: 1_000_000_000_000,
      hintKey: hint('checkin.max_quota'),
    }),
  ],
}

const PAY: SectionConfig = {
  id: 'pay',
  titleKey: 'settings.section.pay',
  descriptionKey: 'settings.section_description.pay',
  fields: [
    toggle('pay.online_enabled', label('pay.online_enabled'), {
      hintKey: hint('pay.online_enabled'),
      dangerDirection: 'off',
    }),
    field('pay.amount_presets_cny', label('pay.amount_presets_cny'), 'number_list', {
      hintKey: hint('pay.amount_presets_cny'),
    }),
  ],
}

const PAY_STRIPE: SectionConfig = {
  id: 'pay.stripe',
  titleKey: 'settings.section.pay_stripe',
  descriptionKey: 'settings.section_description.pay_stripe',
  fields: [
    toggle('pay.stripe.enabled', label('pay.stripe.enabled')),
    field('pay.stripe.secret_key', label('pay.stripe.secret_key'), 'secret', {
      hintKey: hint('pay.stripe.secret_key'),
    }),
    field('pay.stripe.webhook_secret', label('pay.stripe.webhook_secret'), 'secret', {
      hintKey: hint('pay.stripe.webhook_secret'),
    }),
    number('pay.stripe.min_topup_cny', label('pay.stripe.min_topup_cny'), {
      min: 1,
      max: 100_000,
    }),
  ],
}

const PAY_ALIPAY: SectionConfig = {
  id: 'pay.alipay',
  titleKey: 'settings.section.pay_alipay',
  descriptionKey: 'settings.section_description.pay_alipay',
  fields: [
    toggle('pay.alipay.enabled', label('pay.alipay.enabled')),
    text('pay.alipay.app_id', label('pay.alipay.app_id'), { maxLength: 32 }),
    field('pay.alipay.private_key', label('pay.alipay.private_key'), 'secret', {
      multiline: true,
      hintKey: hint('pay.alipay.private_key'),
    }),
    field('pay.alipay.public_key', label('pay.alipay.public_key'), 'secret', {
      multiline: true,
      hintKey: hint('pay.alipay.public_key'),
    }),
    field('pay.alipay.gateway_url', label('pay.alipay.gateway_url'), 'select', {
      options: [
        {
          value: 'https://openapi.alipay.com/gateway.do',
          labelKey: 'settings.option.alipay_gateway_production',
        },
        {
          value: 'https://openapi-sandbox.dl.alipaydev.com/gateway.do',
          labelKey: 'settings.option.alipay_gateway_sandbox',
        },
      ],
    }),
    toggle('pay.alipay.sandbox', label('pay.alipay.sandbox')),
  ],
}

const PAY_EPAY: SectionConfig = {
  id: 'pay.epay',
  titleKey: 'settings.section.pay_epay',
  descriptionKey: 'settings.section_description.pay_epay',
  fields: [
    toggle('pay.epay.enabled', label('pay.epay.enabled')),
    text('pay.epay.api_url', label('pay.epay.api_url'), {
      maxLength: 1024,
      hintKey: hint('pay.epay.api_url'),
    }),
    text('pay.epay.pid', label('pay.epay.pid'), { maxLength: 128 }),
    // 签名密钥只写不读：服务端返回掩码，留空 = 不修改，清空 = 显式删除。
    field('pay.epay.key', label('pay.epay.key'), 'secret', {
      hintKey: hint('pay.epay.key'),
    }),
    field('pay.epay.sign_mode', label('pay.epay.sign_mode'), 'select', {
      options: [
        { value: 'append', labelKey: 'settings.option.epay_sign_append' },
        { value: 'amp', labelKey: 'settings.option.epay_sign_amp' },
      ],
      hintKey: hint('pay.epay.sign_mode'),
    }),
    text('pay.epay.notify_url', label('pay.epay.notify_url'), {
      maxLength: 1024,
      hintKey: hint('pay.epay.notify_url'),
    }),
    text('pay.epay.return_url', label('pay.epay.return_url'), {
      maxLength: 1024,
      hintKey: hint('pay.epay.return_url'),
    }),
  ],
}

const RL: SectionConfig = {
  id: 'rl',
  titleKey: 'settings.section.rl',
  descriptionKey: 'settings.section_description.rl',
  fields: [
    number('rl.user_model_rpm', label('rl.user_model_rpm'), {
      min: 0,
      max: 100_000,
      hintKey: hint('rl.user_model_rpm'),
    }),
    number('rl.token_rpm', label('rl.token_rpm'), {
      min: 0,
      max: 100_000,
      hintKey: hint('rl.token_rpm'),
    }),
    number('rl.user_global_rpm', label('rl.user_global_rpm'), {
      min: 0,
      max: 100_000,
      hintKey: hint('rl.user_global_rpm'),
    }),
    field('rl.group_overrides', label('rl.group_overrides'), 'json', {
      hintKey: hint('rl.group_overrides'),
    }),
  ],
}

const SEC: SectionConfig = {
  id: 'sec',
  titleKey: 'settings.section.sec',
  descriptionKey: 'settings.section_description.sec',
  fields: [
    // 三态强度取代旧布尔开关：off 会解除全部出站校验，属关闭型风险。
    field('sec.ssrf.mode', label('sec.ssrf.mode'), 'select', {
      options: [
        { value: 'off', labelKey: 'settings.option.ssrf_mode_off' },
        { value: 'basic', labelKey: 'settings.option.ssrf_mode_basic' },
        { value: 'strict', labelKey: 'settings.option.ssrf_mode_strict' },
      ],
      hintKey: hint('sec.ssrf.mode'),
      dangerDirection: 'off',
    }),
    toggle('sec.ssrf.enabled', label('sec.ssrf.enabled'), {
      hintKey: hint('sec.ssrf.enabled'),
      dangerDirection: 'off',
    }),
    toggle('sec.ssrf.allow_private_ip', label('sec.ssrf.allow_private_ip'), {
      hintKey: hint('sec.ssrf.allow_private_ip'),
      dangerDirection: 'on',
    }),
    field('sec.ssrf.allowed_ports', label('sec.ssrf.allowed_ports'), 'number_list', {
      hintKey: hint('sec.ssrf.allowed_ports'),
    }),
    field('sec.ssrf.domain_mode', label('sec.ssrf.domain_mode'), 'select', {
      options: [
        { value: 'all', labelKey: 'settings.option.domain_all' },
        { value: 'allowlist', labelKey: 'settings.option.domain_allowlist' },
        { value: 'denylist', labelKey: 'settings.option.domain_denylist' },
      ],
      // 默认 allowlist 是破坏性收紧（G-DOM-68）：提示必须写明。
      hintKey: hint('sec.ssrf.domain_mode'),
    }),
    field('sec.ssrf.domain_list', label('sec.ssrf.domain_list'), 'lines', {
      hintKey: hint('sec.ssrf.domain_list'),
    }),
    number('sec.ssrf.max_redirects', label('sec.ssrf.max_redirects'), {
      min: 0,
      max: 10,
      hintKey: hint('sec.ssrf.max_redirects'),
    }),
    number('sec.ssrf.connect_timeout_seconds', label('sec.ssrf.connect_timeout_seconds'), {
      min: 1,
      max: 120,
      hintKey: hint('sec.ssrf.connect_timeout_seconds'),
    }),
    toggle('sec.sensitive.enabled', label('sec.sensitive.enabled')),
    field('sec.sensitive.scope', label('sec.sensitive.scope'), 'module_toggles', {
      hintKey: hint('sec.sensitive.scope'),
      toggles: [
        { key: 'prompt', labelKey: 'settings.shell.module_prompt' },
        { key: 'system', labelKey: 'settings.shell.module_system' },
      ],
    }),
    field('sec.sensitive.words', label('sec.sensitive.words'), 'lines', {
      hintKey: hint('sec.sensitive.words'),
      dangerDirection: 'change',
    }),
    field('sec.sensitive.action', label('sec.sensitive.action'), 'select', {
      options: [
        { value: 'block', labelKey: 'settings.option.sensitive_block' },
        { value: 'log', labelKey: 'settings.option.sensitive_log' },
      ],
      hintKey: hint('sec.sensitive.action'),
    }),
  ],
}

const NOTIFY_SMTP: SectionConfig = {
  id: 'notify.smtp',
  titleKey: 'settings.section.notify_smtp',
  descriptionKey: 'settings.section_description.notify_smtp',
  fields: [
    text('notify.smtp.host', label('notify.smtp.host'), { maxLength: 256 }),
    number('notify.smtp.port', label('notify.smtp.port'), { min: 1, max: 65_535 }),
    field('notify.smtp.encryption', label('notify.smtp.encryption'), 'select', {
      options: [
        { value: 'ssl', labelKey: 'settings.option.ssl' },
        { value: 'starttls', labelKey: 'settings.option.starttls' },
        { value: 'none', labelKey: 'settings.option.none' },
      ],
    }),
    text('notify.smtp.username', label('notify.smtp.username'), { maxLength: 256 }),
    text('notify.smtp.from_email', label('notify.smtp.from_email'), {
      maxLength: 254,
      hintKey: hint('notify.smtp.from_email'),
    }),
    text('notify.smtp.from_name', label('notify.smtp.from_name'), {
      maxLength: 128,
      hintKey: hint('notify.smtp.from_name'),
    }),
    field('notify.smtp.password', label('notify.smtp.password'), 'secret'),
    toggle('notify.smtp.force_auth_login', label('notify.smtp.force_auth_login')),
    toggle('notify.smtp.insecure_skip_verify', label('notify.smtp.insecure_skip_verify'), {
      hintKey: hint('notify.smtp.insecure_skip_verify'),
    }),
  ],
}

const NOTIFY_WEBHOOK: SectionConfig = {
  id: 'notify.webhook',
  titleKey: 'settings.section.notify_webhook',
  descriptionKey: 'settings.section_description.notify_webhook',
  // The test action posts to the saved snapshot, so it is independent of the
  // `enabled` switch and the event subscription (backend D2-14).
  testKind: 'webhook',
  fields: [
    toggle('notify.webhook.enabled', label('notify.webhook.enabled')),
    text('notify.webhook.url', label('notify.webhook.url'), {
      maxLength: 1024,
      hintKey: hint('notify.webhook.url'),
    }),
    field('notify.webhook.secret', label('notify.webhook.secret'), 'secret', {
      hintKey: hint('notify.webhook.secret'),
    }),
    // Order mirrors the frozen backend set (registry/validate.go
    // `webhookEvents`) so the audit diff and the checkbox order agree.
    field('notify.webhook.events', label('notify.webhook.events'), 'multi_select', {
      hintKey: hint('notify.webhook.events'),
      emptyHintKey: hint('notify.webhook.events_empty'),
      options: [
        { value: 'balance_low', labelKey: 'settings.option.event.balance_low' },
        { value: 'payment_success', labelKey: 'settings.option.event.payment_success' },
        { value: 'announcement', labelKey: 'settings.option.event.announcement' },
        { value: 'user_signup', labelKey: 'settings.option.event.user_signup' },
      ],
    }),
    // Explicit low-balance threshold for the N6 scan. 0 disables the scan;
    // bounds mirror the registry (`notify.webhook.balance_low_threshold`,
    // 0–1e12 credits). It is separate from
    // `billing.balance_alert_threshold_credits` (the in-product alert), which
    // the gate intentionally does not consume as a second source of truth.
    number('notify.webhook.balance_low_threshold', label('notify.webhook.balance_low_threshold'), {
      min: 0,
      max: 1_000_000_000_000,
      hintKey: hint('notify.webhook.balance_low_threshold'),
    }),
  ],
}

// Wave 1 only ships F-08-45 (channel health); affinity and retry budgets are
// later waves, so no config entries exist for `rl.affinity` / `rl.retry`.
const RL_HEALTH: SectionConfig = {
  id: 'rl.health',
  titleKey: 'settings.section.rl_health',
  descriptionKey: 'settings.section_description.rl_health',
  fields: [
    toggle('rl.health.auto_disable_enabled', label('rl_health.auto_disable_enabled'), {
      hintKey: hint('rl_health.auto_disable_enabled'),
      dangerDirection: 'on',
    }),
    number(
      'rl.health.response_time_threshold_seconds',
      label('rl_health.response_time_threshold_seconds'),
      {
        integer: false,
        greaterThan: 0,
        hintKey: hint('rl_health.response_time_threshold_seconds'),
      },
    ),
    field('rl.health.disable_keywords', label('rl_health.disable_keywords'), 'lines', {
      hintKey: hint('rl_health.disable_keywords'),
      dangerDirection: 'change',
    }),
    toggle('rl.health.auto_enable_enabled', label('rl_health.auto_enable_enabled'), {
      hintKey: hint('rl_health.auto_enable_enabled'),
    }),
    number('rl.health.test_interval_minutes', label('rl_health.test_interval_minutes'), {
      min: 1,
      max: 1440,
      hintKey: hint('rl_health.test_interval_minutes'),
    }),
    number('rl.health.test_concurrency', label('rl_health.test_concurrency'), {
      min: 1,
      max: 50,
      hintKey: hint('rl_health.test_concurrency'),
    }),
  ],
}

export const SETTINGS_TABS_CONFIG: readonly TabConfig[] = [
  {
    id: 'site',
    labelKey: 'settings.tab.site',
    descriptionKey: 'settings.tab_description.site',
    sections: [SITE, NAV],
  },
  {
    id: 'auth',
    labelKey: 'settings.tab.auth',
    descriptionKey: 'settings.tab_description.auth',
    sections: [AUTH, AUTH_TURNSTILE],
  },
  {
    id: 'billing',
    labelKey: 'settings.tab.billing',
    descriptionKey: 'settings.tab_description.billing',
    sections: [BILLING, CHECKIN],
  },
  {
    id: 'payment',
    labelKey: 'settings.tab.payment',
    descriptionKey: 'settings.tab_description.payment',
    sections: [PAY, PAY_STRIPE, PAY_ALIPAY, PAY_EPAY],
  },
  {
    id: 'security',
    labelKey: 'settings.tab.security',
    descriptionKey: 'settings.tab_description.security',
    sections: [RL, SEC],
  },
  {
    id: 'notify',
    labelKey: 'settings.tab.notify',
    descriptionKey: 'settings.tab_description.notify',
    sections: [NOTIFY_SMTP, NOTIFY_WEBHOOK],
  },
  {
    id: 'policy',
    labelKey: 'settings.tab.policy',
    descriptionKey: 'settings.tab_description.policy',
    sections: [RL_HEALTH],
  },
]

export function tabConfig(tab: SettingsTab): TabConfig {
  const config = SETTINGS_TABS_CONFIG.find((entry) => entry.id === tab)
  // The union above is exhaustively mapped; this guards a future tab added to
  // SETTINGS_TABS without a config entry instead of rendering an empty shell.
  if (config === undefined) throw new Error(`missing settings tab config: ${tab}`)
  return config
}
