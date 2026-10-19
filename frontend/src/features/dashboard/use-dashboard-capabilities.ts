import { useQuery } from '@tanstack/react-query'

import { meQueryOptions } from '@/lib/session'

export interface DashboardCapabilities {
  /** True only for admin sessions whose effective permissions include logs:read. */
  canFilterByUsername: boolean
  /** Capability probing still in flight; callers hide the control meanwhile. */
  loading: boolean
}

/**
 * Resolves whether the current viewer may use the admin-only `username`
 * parameter (F-05-06 / R-M6-12).
 *
 * `GET /api/me` now carries the caller's effective permission keys (M6 R-17),
 * so the gate is exactly `role=admin AND (permissions contains "*" or
 * "logs:read")`. Probing `/api/users` would have required `users:read` and
 * silently denied a logs-only admin the filter they are entitled to.
 *
 * This is a UX gate only. The server independently narrows non-admin viewers
 * to their own rows and returns 403 when an admin-only parameter is present
 * on a non-admin request (web-00 §9.1). Fail-closed while the session probe is
 * pending: a filter that might 403 the whole dashboard is worse than one
 * rendered a moment later.
 */
export function useDashboardCapabilities(): DashboardCapabilities {
  const meQuery = useQuery(meQueryOptions())

  if (meQuery.isPending) return { canFilterByUsername: false, loading: true }

  const me = meQuery.data
  if (me === undefined || me.role !== 'admin') {
    return { canFilterByUsername: false, loading: false }
  }

  const permissions = me.permissions ?? []
  return {
    canFilterByUsername:
      permissions.includes('*') || permissions.includes('logs:read'),
    loading: false,
  }
}
