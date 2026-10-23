import type { Me } from '@/lib/session'

import type { NavIconKey } from './nav-icon'

export type { NavIconKey } from './nav-icon'

export interface NavItem {
  id: string
  labelKey: string
  to: string
  icon: NavIconKey
  adminOnly?: boolean
  permission?: string
  /** Hidden when the site-level Playground module is off. */
  requiresPlayground?: true
  badge?: string
}

export interface NavGroup {
  id: string
  labelKey: string
  items: NavItem[]
}

/**
 * Single source of truth for the console navigation. The sidebar, the page
 * title and the command palette all read this list, so a route can never be
 * reachable from one surface but missing from another.
 *
 * Permission gates mirror the page read surfaces; an entry is hidden before
 * the user can hit a 403. The server remains the authority.
 */
export const NAV_GROUPS: readonly NavGroup[] = [
  {
    id: 'workspace',
    labelKey: 'nav.group.workspace',
    items: [
      {
        id: 'overview',
        labelKey: 'nav.dashboard',
        to: '/dashboard',
        icon: 'grid',
      },
    ],
  },
  {
    id: 'build',
    labelKey: 'nav.group.build',
    items: [
      { id: 'tokens', labelKey: 'nav.tokens', to: '/tokens', icon: 'key' },
      {
        id: 'model-config',
        labelKey: 'nav.model_config',
        to: '/model-config',
        icon: 'sliders',
      },
      {
        id: 'playground',
        labelKey: 'nav.playground',
        to: '/playground',
        icon: 'flask',
        requiresPlayground: true,
      },
    ],
  },
  {
    id: 'usage',
    labelKey: 'nav.group.billing',
    items: [
      {
        id: 'usage-logs',
        labelKey: 'nav.usage_logs',
        to: '/dashboard/usage-logs',
        icon: 'list',
      },
      {
        id: 'audit-logs',
        labelKey: 'nav.audit_logs',
        to: '/dashboard/audit-logs',
        icon: 'clipboard',
      },
      { id: 'wallet', labelKey: 'nav.wallet', to: '/wallet', icon: 'wallet' },
      { id: 'orders', labelKey: 'nav.orders', to: '/orders', icon: 'receipt' },
    ],
  },
  {
    id: 'admin',
    labelKey: 'nav.group.admin',
    items: [
      {
        id: 'redemption',
        labelKey: 'nav.redemption_codes',
        to: '/redemption-codes',
        icon: 'ticket',
        adminOnly: true,
        permission: 'redemption:manage',
      },
      {
        id: 'channels',
        labelKey: 'nav.channels',
        to: '/admin/channels',
        icon: 'radio',
        adminOnly: true,
        permission: 'channels:read',
      },
      {
        id: 'models',
        labelKey: 'nav.models',
        to: '/admin/models',
        icon: 'box',
        adminOnly: true,
        permission: 'channels:read',
      },
      {
        id: 'users',
        labelKey: 'nav.users',
        to: '/admin/users',
        icon: 'users',
        adminOnly: true,
        permission: 'users:read',
      },
      {
        id: 'groups',
        labelKey: 'nav.groups',
        to: '/admin/users/groups',
        icon: 'group',
        adminOnly: true,
        permission: 'groups:manage',
      },
      {
        id: 'subscriptions',
        labelKey: 'nav.subscriptions',
        to: '/admin/subscriptions',
        icon: 'card',
        adminOnly: true,
        permission: 'settings:manage',
      },
      {
        id: 'model-pricing',
        labelKey: 'nav.model_pricing',
        to: '/admin/pricing/model-pricing',
        icon: 'tag',
        adminOnly: true,
        permission: 'pricing:read',
      },
      {
        id: 'group-pricing',
        labelKey: 'nav.group_pricing',
        to: '/admin/pricing/group-pricing',
        icon: 'tag',
        adminOnly: true,
        permission: 'pricing:read',
      },
    ],
  },
  {
    id: 'system',
    labelKey: 'nav.group.system',
    items: [
      {
        id: 'settings',
        labelKey: 'nav.settings',
        to: '/admin/settings',
        icon: 'settings',
        adminOnly: true,
        permission: 'settings:manage',
      },
      {
        id: 'status',
        labelKey: 'nav.system_status',
        to: '/admin/system/status',
        icon: 'activity',
        adminOnly: true,
      },
      {
        id: 'operation-logs',
        labelKey: 'nav.operation_logs',
        to: '/admin/system/audit-logs',
        icon: 'clipboard',
        adminOnly: true,
      },
      {
        id: 'deployments',
        labelKey: 'nav.model_deployments',
        to: '/admin/system/deployments',
        icon: 'server',
        adminOnly: true,
        permission: 'settings:manage',
      },
      {
        id: 'task-plugins',
        labelKey: 'nav.task_plugins',
        to: '/admin/system/task-plugins',
        icon: 'puzzle',
        adminOnly: true,
        permission: 'settings:manage',
      },
      {
        id: 'performance',
        labelKey: 'nav.performance',
        to: '/admin/system/performance',
        icon: 'gauge',
        adminOnly: true,
        permission: 'settings:manage',
      },
    ],
  },
  {
    id: 'account',
    labelKey: 'nav.group.account',
    items: [
      { id: 'profile', labelKey: 'nav.profile', to: '/profile', icon: 'user' },
      {
        id: 'security',
        labelKey: 'nav.security',
        to: '/security',
        icon: 'shield',
      },
    ],
  },
  {
    id: 'help',
    labelKey: 'nav.group.help',
    items: [
      { id: 'about', labelKey: 'nav.about', to: '/about', icon: 'help' },
      { id: 'pricing', labelKey: 'nav.pricing', to: '/pricing', icon: 'tag' },
      { id: 'docs', labelKey: 'nav.docs', to: '/docs', icon: 'book' },
    ],
  },
]

export interface VisibleNavOptions {
  /** `nav.modules.chat.playground`; defaults to true. */
  playgroundEnabled?: boolean
}

function canUse(
  permission: string | undefined,
  permissions: readonly string[],
): boolean {
  if (permission === undefined) return true
  return permissions.includes('*') || permissions.includes(permission)
}

export function visibleNavGroups(
  me: Me | null | undefined,
  options: VisibleNavOptions = {},
): NavGroup[] {
  const role = me?.role
  const permissions = me?.permissions ?? []

  return NAV_GROUPS.map((group) => ({
    ...group,
    items: group.items.filter((item) => {
      if (item.adminOnly === true && role !== 'admin') return false
      if (!canUse(item.permission, permissions)) return false
      if (item.requiresPlayground === true && options.playgroundEnabled === false) {
        return false
      }
      return true
    }),
  })).filter((group) => group.items.length > 0)
}

function matches(pathname: string, to: string): boolean {
  return pathname === to || pathname.startsWith(`${to}/`)
}

/**
 * Longest-prefix match. `/dashboard/usage-logs` must win over `/dashboard`;
 * `/admin/users/groups` must win over `/admin/users`.
 */
export function activeNavItem(pathname: string): NavItem | undefined {
  let best: NavItem | undefined
  for (const group of NAV_GROUPS) {
    for (const item of group.items) {
      if (!matches(pathname, item.to)) continue
      if (best === undefined || item.to.length > best.to.length) best = item
    }
  }
  return best
}

export function findNavItem(pathname: string): NavItem | undefined {
  return activeNavItem(pathname)
}
