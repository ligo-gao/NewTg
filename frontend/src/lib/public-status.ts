import { useQuery, type QueryClient } from '@tanstack/react-query'

import { apiClient } from '@/api/client'
import { requireData } from '@/api/result'
import type { components } from '@/api/schema'

type GeneratedPublicStatus = components['schemas']['PublicStatus']

/**
 * Narrow local mirror of the `site` / `nav` / `billing` branches of
 * `GET /api/status` (modules-09 F-09-06 field table, frozen by the M6c backend
 * task; registered in `docs/plans` as R-PUB-06/07/09).
 *
 * Why a local type instead of `components['schemas']`: the generated
 * `api/schema.d.ts` is produced from the checked-in OpenAPI document and the
 * backend task that adds these branches updates that document separately. This
 * task must not hand-edit the generated file (the drift check would then fail
 * against the backend and the next regeneration would silently drop the edit).
 * The shape below is therefore a hand-transcribed copy of the frozen contract;
 * every field is optional because a mixed-version deploy (new frontend, older
 * backend) must still render with the documented defaults instead of crashing.
 * When the generated schema catches up, this interface is the single place to
 * delete in favour of `components['schemas']['PublicStatus']`.
 */
export interface PublicHomeBlocks {
  hero?: boolean
  live_feed?: boolean
  features?: boolean
  stats?: boolean
  onboarding?: boolean
}

export interface PublicSiteSeo {
  description?: string
  og_image_url?: string
}

export interface PublicSite {
  title?: string
  logo_url?: string
  server_address?: string
  docs_url?: string
  footer_html?: string
  about_html?: string
  filing_no?: string
  status_page_url?: string
  oss_credits?: unknown
  seo?: PublicSiteSeo
  home_custom_enabled?: boolean
  /** Only sent when `home_custom_enabled=true` (R-PUB-07). */
  home_custom_html?: string
  home_blocks?: PublicHomeBlocks
}

export interface PublicNavEntry {
  enabled?: boolean
  require_auth?: boolean
}

export interface PublicNav {
  home?: PublicNavEntry
  /** Server locks `require_auth=true`; the mirror keeps it optional for old responses. */
  console?: PublicNavEntry
  pricing?: PublicNavEntry
  rankings?: PublicNavEntry
  docs?: PublicNavEntry
  about?: PublicNavEntry
}

export interface PublicBilling {
  quota_per_unit?: number
  /** `credits` | `cny` (backend enum); kept as string for forward compatibility. */
  display_unit?: string
  currency_symbol?: string
}

export interface PublicStatus extends GeneratedPublicStatus {
  site?: PublicSite
  nav?: PublicNav
  billing?: PublicBilling
}

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
