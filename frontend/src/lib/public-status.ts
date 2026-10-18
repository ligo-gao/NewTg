import { useQuery, type QueryClient } from '@tanstack/react-query'

import { apiClient } from '@/api/client'
import { requireData } from '@/api/result'
import type { components } from '@/api/schema'

export type PublicStatus = components['schemas']['PublicStatus']

/** Shared cache key (web-00 §5.3); every public page reads one entry. */
export const PUBLIC_STATUS_QUERY_KEY = ['public', 'status'] as const

/**
 * `GET /api/status` is served with `Cache-Control: no-store`: operator toggles
 * (registration, Turnstile, setup) must apply immediately, so the client keeps
 * no stale window either.
 */
export function publicStatusQueryOptions() {
  return {
    queryKey: PUBLIC_STATUS_QUERY_KEY,
    queryFn: async (): Promise<PublicStatus> =>
      requireData(await apiClient.GET('/api/status')),
    staleTime: 0 as const,
  }
}

export function usePublicStatus() {
  return useQuery(publicStatusQueryOptions())
}

/**
 * Upper bound for the root gate's status probe. A stalled `/api/status` (dead
 * proxy, half-open connection) would otherwise keep every route in
 * `beforeLoad` forever, blanking the whole app; after this window the gate is
 * skipped and the route renders (pages that need status show their own retry).
 */
const STATUS_PROBE_TIMEOUT_MS = 3000

async function withTimeout<T>(promise: Promise<T>, ms: number): Promise<T | null> {
  let timer: ReturnType<typeof setTimeout> | undefined
  try {
    return await Promise.race([
      promise,
      new Promise<null>((resolve) => {
        timer = setTimeout(() => resolve(null), ms)
      }),
    ])
  } finally {
    if (timer !== undefined) clearTimeout(timer)
  }
}

/**
 * Root-guard probe. A status outage must not blank the whole app (web-00 §4.4
 * allows public pages to render with safe defaults), so failures resolve to
 * `null` and the setup gate simply stays closed.
 *
 * Cached data is preferred even when stale: `staleTime: 0` is what makes a
 * mounted status consumer re-check the toggles, but running a `/api/status`
 * request on *every* client-side navigation would be wasteful. A full reload
 * (which the setup flow performs) starts with an empty cache.
 */
export async function fetchPublicStatus(
  queryClient: QueryClient,
): Promise<PublicStatus | null> {
  const cached = queryClient.getQueryData<PublicStatus>(PUBLIC_STATUS_QUERY_KEY)
  if (cached !== undefined) return cached
  try {
    return await withTimeout(
      queryClient.ensureQueryData(publicStatusQueryOptions()),
      STATUS_PROBE_TIMEOUT_MS,
    )
  } catch {
    return null
  }
}
