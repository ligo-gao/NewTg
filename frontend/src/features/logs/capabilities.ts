import { useQuery } from '@tanstack/react-query'

import { meQueryOptions } from '@/lib/session'

export interface LogsCapabilities {
  /** Session probe still in flight; callers keep admin controls hidden. */
  loading: boolean
  /** Admin role (channel columns stay admin-only). */
  isAdmin: boolean
  /** `role=admin AND permissions include "*" or logs:read` (R-M6-13). */
  canFilterByUsername: boolean
  /** Prompt plaintext gate: effective permission `log.prompt.view` or `*`. */
  canViewPrompt: boolean
  /**
   * Sensitive payload gate for F-05-06. The switch is only rendered with
   * `log.prompt.view`; without it the UI never asks the server to reveal and
   * stored error text stays masked. The server re-checks the same permission
   * on every request, so this flag is a UX gate, not the authority.
   */
  canViewSensitive: boolean
}

const NONE: LogsCapabilities = {
  loading: false,
  isAdmin: false,
  canFilterByUsername: false,
  canViewPrompt: false,
  canViewSensitive: false,
}

/**
 * Capability gate for the usage/audit log pages (F-05-01, F-05-05, F-05-06).
 *
 * `GET /api/me` carries the caller's effective permission keys (M6 R-17), so
 * no extra `/api/users` probe is needed. Fail-closed while the session probe
 * is pending: rendering an admin-only `username` parameter that 403s the whole
 * page is worse than showing the control a moment later. The server remains
 * the authority for both the data scope and the prompt permission.
 */
export function useLogsCapabilities(): LogsCapabilities {
  const meQuery = useQuery(meQueryOptions())

  if (meQuery.isPending) {
    return { ...NONE, loading: true }
  }

  const me = meQuery.data
  if (me === undefined) return NONE

  const permissions = me.permissions ?? []
  const fullAccess = permissions.includes('*')
  return {
    loading: false,
    isAdmin: me.role === 'admin',
    canFilterByUsername:
      me.role === 'admin' && (fullAccess || permissions.includes('logs:read')),
    canViewPrompt: fullAccess || permissions.includes('log.prompt.view'),
    // Same key as the switch: reveal is exactly the prompt-view permission
    // (backend `sensitiveRevealAllowed` re-checks `log.prompt.view`).
    canViewSensitive: fullAccess || permissions.includes('log.prompt.view'),
  }
}
