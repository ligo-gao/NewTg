import type { QueryClient } from '@tanstack/react-query'

import { apiClient } from '@/api/client'
import { ApiError } from '@/api/errors'
import { requireData } from '@/api/result'
import type { components } from '@/api/schema'

export type Me = components['schemas']['MeView']

export const ME_QUERY_KEY = ['me'] as const

/**
 * The session source of truth (web-00 §9.2): `GET /api/me`, cached under
 * `['me']`, retried never — a 401 is a stable answer, not a transient fault.
 */
export function meQueryOptions() {
  return {
    queryKey: ME_QUERY_KEY,
    queryFn: async (): Promise<Me> =>
      requireData(await apiClient.GET('/api/me')),
    retry: false as const,
    staleTime: 30_000,
  }
}

/**
 * Route-guard probe. A 401 means "guest" and resolves to `null` (web-00 §9.2:
 * the probe must stay silent); every other failure propagates so the route
 * error boundary renders instead of misclassifying an outage as not-logged-in.
 */
export async function fetchMe(queryClient: QueryClient): Promise<Me | null> {
  try {
    return await queryClient.ensureQueryData(meQueryOptions())
  } catch (error) {
    if (error instanceof ApiError && error.kind === 'auth') return null
    throw error
  }
}
