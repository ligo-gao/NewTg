import type { Me } from '@/lib/session'

/**
 * Command palette registry (web-00 §15.1).
 *
 * The registry is deliberately static: the palette is an intent executor, not a
 * global search. Every entry either navigates with the client router or runs a
 * local side effect — command execution must never issue a network request
 * (§15.1.6). Labels are i18n keys, resolved by the component so the registry
 * itself never carries user-visible copy.
 *
 * Permission keys come from the modules-07 catalog (`users:read`, `logs:read`,
 * …). The shell must not invent new keys (§15.1.4).
 */

export type CommandKind = 'route' | 'action'
export type CommandGroupId = 'pages' | 'actions'

/** Every navigable console target, kept in sync with the static registry. */
export type CommandRoutePath =
  | '/dashboard'
  | '/tokens'
  | '/dashboard/usage-logs'
  | '/dashboard/audit-logs'
  | '/wallet'
  | '/orders'
  | '/profile'
  | '/security'
  | '/model-config'
  | '/playground'
  | '/admin/users'
  | '/admin/users/groups'
  | '/admin/pricing/model-pricing'
  | '/admin/pricing/group-pricing'
  | '/admin/channels'

export interface CommandDefinition {
  id: string
  kind: CommandKind
  group: CommandGroupId
  labelKey: string
  /** Extra ASCII aliases matched by the fuzzy filter (not rendered). */
  keywords: readonly string[]
  /** Admin-only entries must also satisfy `requiredPermission`. */
  requiredRole?: 'admin'
  requiredPermission?: string
  /** Reserved for the §15.1.5 analytics outlet; no event is emitted in M6. */
  analyticsId: string
  /** Router target for `kind: 'route'` and navigation actions. */
  to?: CommandRoutePath
  /** Local side effect executed without any request. */
  action?: 'copy-page-url'
  /**
   * Entry is only meaningful while `nav.modules.chat.playground` is on
   * (modules-02 §2.15). The `/playground` route guard is still authoritative
   * for direct links; the palette hides the entry instead of offering a
   * command that would silently bounce to `/dashboard`.
   */
  requiresPlayground?: true
}

export const COMMAND_GROUP_LABEL_KEYS: Record<CommandGroupId, string> = {
  pages: 'common.command.groups.pages',
  actions: 'common.command.groups.actions',
}

export const COMMANDS: readonly CommandDefinition[] = [
  {
    id: 'dashboard',
    kind: 'route',
    group: 'pages',
    labelKey: 'common.nav.dashboard',
    keywords: ['home', 'overview', 'stats'],
    analyticsId: 'nav.dashboard',
    to: '/dashboard',
  },
  {
    id: 'tokens',
    kind: 'route',
    group: 'pages',
    labelKey: 'common.nav.tokens',
    keywords: ['api', 'keys', 'create'],
    analyticsId: 'nav.tokens',
    to: '/tokens',
  },
  {
    id: 'usage-logs',
    kind: 'route',
    group: 'pages',
    labelKey: 'common.nav.usage_logs',
    keywords: ['logs', 'requests', 'history', 'usage'],
    analyticsId: 'nav.usage_logs',
    to: '/dashboard/usage-logs',
  },
  {
    id: 'audit-logs',
    kind: 'route',
    group: 'pages',
    labelKey: 'common.nav.audit_logs',
    keywords: ['audit', 'events', 'security'],
    // Session-group page: every authenticated user may see their own events.
    // `logs:read` widens the scope *inside* the page (admin-wide rows), it
    // does not gate the entry point. Keeping the route ungated mirrors the
    // console header nav and controller ruling (2026-10-19).
    analyticsId: 'nav.audit_logs',
    to: '/dashboard/audit-logs',
  },
  {
    id: 'wallet',
    kind: 'route',
    group: 'pages',
    labelKey: 'common.nav.wallet',
    keywords: ['balance', 'credits', 'funds', 'topup'],
    analyticsId: 'nav.wallet',
    to: '/wallet',
  },
  {
    id: 'orders',
    kind: 'route',
    group: 'pages',
    labelKey: 'common.nav.orders',
    keywords: ['payments', 'receipts', 'history'],
    analyticsId: 'nav.orders',
    to: '/orders',
  },
  {
    id: 'profile',
    kind: 'route',
    group: 'pages',
    labelKey: 'common.nav.profile',
    keywords: ['account', 'me', 'display name'],
    analyticsId: 'nav.profile',
    to: '/profile',
  },
  {
    id: 'security',
    kind: 'route',
    group: 'pages',
    labelKey: 'common.nav.security',
    keywords: ['password', '2fa', 'sessions', 'account'],
    analyticsId: 'nav.security',
    to: '/security',
  },
  {
    id: 'model-config',
    kind: 'route',
    group: 'pages',
    labelKey: 'common.nav.model_config',
    keywords: ['models', 'default model', 'limits', 'keys', 'config'],
    analyticsId: 'nav.model_config',
    to: '/model-config',
  },
  {
    id: 'playground',
    kind: 'route',
    group: 'pages',
    labelKey: 'common.nav.playground',
    keywords: ['chat', 'llm', 'prompt', 'test model'],
    analyticsId: 'nav.playground',
    requiresPlayground: true,
    to: '/playground',
  },
  {
    id: 'admin-users',
    kind: 'route',
    group: 'pages',
    labelKey: 'common.nav.users',
    keywords: ['admin', 'accounts', 'members'],
    requiredRole: 'admin',
    requiredPermission: 'users:read',
    analyticsId: 'nav.admin_users',
    to: '/admin/users',
  },
  {
    id: 'admin-groups',
    kind: 'route',
    group: 'pages',
    labelKey: 'common.nav.groups',
    keywords: ['admin', 'routing', 'groups'],
    requiredRole: 'admin',
    requiredPermission: 'groups:manage',
    analyticsId: 'nav.admin_groups',
    to: '/admin/users/groups',
  },
  {
    id: 'admin-model-pricing',
    kind: 'route',
    group: 'pages',
    labelKey: 'common.nav.model_pricing',
    keywords: ['admin', 'pricing', 'models', 'cost'],
    requiredRole: 'admin',
    requiredPermission: 'pricing:read',
    analyticsId: 'nav.admin_model_pricing',
    to: '/admin/pricing/model-pricing',
  },
  {
    id: 'admin-group-pricing',
    kind: 'route',
    group: 'pages',
    labelKey: 'common.nav.group_pricing',
    keywords: ['admin', 'pricing', 'groups', 'ratios'],
    requiredRole: 'admin',
    requiredPermission: 'pricing:read',
    analyticsId: 'nav.admin_group_pricing',
    to: '/admin/pricing/group-pricing',
  },
  {
    id: 'admin-channels',
    kind: 'route',
    group: 'pages',
    labelKey: 'common.nav.channels',
    keywords: ['admin', 'channels', 'upstream', 'providers'],
    requiredRole: 'admin',
    requiredPermission: 'channels:read',
    analyticsId: 'nav.admin_channels',
    to: '/admin/channels',
  },
  {
    id: 'copy-page-url',
    kind: 'action',
    group: 'actions',
    labelKey: 'common.command.actions.copy_url',
    keywords: ['share', 'link', 'clipboard', 'copy'],
    analyticsId: 'action.copy_page_url',
    action: 'copy-page-url',
  },
]

/**
 * Capability gate for one registry entry.
 *
 * `me` is `undefined` while `['me']` is pending or failed — exactly the window
 * in which the palette must fail closed and hide every privileged entry
 * (§15.1.4). Ordinary session entries carry no gate and stay available. With
 * `permissions` absent (older backends) we treat it as an empty set rather than
 * inferring access from `role`, because an over-shown admin entry leaks the
 * existence of an admin surface.
 */
export function isCommandAllowed(
  command: CommandDefinition,
  me: Me | undefined | null,
): boolean {
  if (me === undefined || me === null) {
    return command.requiredRole === undefined &&
      command.requiredPermission === undefined
  }

  if (command.requiredRole !== undefined && me.role !== command.requiredRole) {
    return false
  }

  if (command.requiredPermission !== undefined) {
    const permissions = me.permissions ?? []
    if (
      !permissions.includes('*') &&
      !permissions.includes(command.requiredPermission)
    ) {
      return false
    }
  }

  return true
}

/**
 * Match score for one candidate string, or `null` when it does not match.
 * Higher is better; ties keep registry order.
 *
 * Ranking mirrors §15.1.2: prefix > word start > substring > subsequence. The
 * subsequence tier is what makes short abbreviations such as `uslg` find
 * "Usage Logs" without adding a fuzzy-search dependency.
 */
export function fuzzyScore(query: string, text: string): number | null {
  const needle = query.trim().toLowerCase()
  if (needle === '') return 0

  const haystack = text.toLowerCase()
  if (haystack.startsWith(needle)) return 3
  if (haystack.split(/\s+/).some((word) => word.startsWith(needle))) return 2
  if (haystack.includes(needle)) return 1

  let cursor = 0
  for (const char of needle) {
    const found = haystack.indexOf(char, cursor)
    if (found === -1) return null
    cursor = found + 1
  }
  return 0
}

/** Best score across the resolved label and the registry's ASCII aliases. */
export function matchCommand(
  command: CommandDefinition,
  query: string,
  label: string,
): number | null {
  let best: number | null = null
  for (const candidate of [label, ...command.keywords]) {
    const score = fuzzyScore(query, candidate)
    if (score !== null && (best === null || score > best)) best = score
  }
  return best
}
