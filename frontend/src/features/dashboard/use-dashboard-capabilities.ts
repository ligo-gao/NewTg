import { useQuery } from '@tanstack/react-query'

import { apiClient } from '@/api/client'
import { requireData } from '@/api/result'
import { meQueryOptions } from '@/lib/session'

export interface DashboardCapabilities {
  /** True only for admin sessions with logs:read (or a full grant). */
  canFilterByUsername: boolean
  /** Capability probing still in flight; callers hide the control meanwhile. */
  loading: boolean
}

/**
 * Resolves whether the current viewer may use the admin-only `username`
 * parameter (F-05-06 / R-M6-12). `MeView` intentionally omits the permission
 * matrix, so the existing M1/M6 pattern applies: fetch the actor's own row
 * from `GET /api/users` and inspect `admin_permissions` (`null` = FULL,
 * `["*"]` = full, `[]` = explicitly none).
 *
 * This is a UX gate only. The server independently narrows non-admin viewers
 * to their own rows and returns 403 when an admin-only parameter is present
 * on a non-admin request (web-00 §9.1). Fail-closed while unknown: a filter
 * that might 403 the whole dashboard is worse than one rendered a moment later.
 */
export function useDashboardCapabilities(): DashboardCapabilities {
  const meQuery = useQuery(meQueryOptions())
  const username = meQuery.data?.username
  const isAdmin = meQuery.data?.role === 'admin'

  const actorQuery = useQuery({
    queryKey: ['dashboard-actor', username],
    // Non-admins have no permission matrix to read; calling /api/users would
    // only produce a 403.
    enabled: isAdmin && username !== undefined,
    staleTime: 0,
    queryFn: async () => {
      const data = requireData(
        await apiClient.GET('/api/users', {
          params: { query: { keyword: username ?? '', p: 1, page_size: 100 } },
        }),
      )
      return (
        (data.items ?? []).find((candidate) => candidate.username === username) ??
        null
      )
    },
  })

  if (meQuery.isPending) return { canFilterByUsername: false, loading: true }
  if (!isAdmin) return { canFilterByUsername: false, loading: false }
  if (actorQuery.isPending) return { canFilterByUsername: false, loading: true }
  // Unreadable user list or a page that does not contain the actor: the
  // capability is unknown, so keep the admin-only control hidden.
  if (actorQuery.data === undefined || actorQuery.data === null) {
    return { canFilterByUsername: false, loading: false }
  }

  const permissions = actorQuery.data.admin_permissions
  return {
    canFilterByUsername:
      permissions === null || permissions.includes('*') || permissions.includes('logs:read'),
    loading: false,
  }
}
