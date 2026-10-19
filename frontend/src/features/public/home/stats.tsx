import { useQuery } from '@tanstack/react-query'
import { useEffect, useMemo, useRef, useState } from 'react'

import { toApiError } from '@/api/client'
import { parseProblem } from '@/api/errors'
import { useI18n } from '@/shared/lib/i18n-context'

export interface HomeStats {
  upstreams: number
  models: number
  endpoints: number
  routing_dims: number
}

export const HOME_STATS_QUERY_KEY = ['public', 'home', 'stats'] as const

/**
 * Cache key includes the schema version: a future shape change must not read
 * yesterday's object and render `undefined` as a count (F-09-04 异常流程).
 */
export const HOME_STATS_CACHE_KEY = 'newtg.home.stats.v1'
export const HOME_STATS_CACHE_TTL_MS = 24 * 60 * 60 * 1000

const STAT_KEYS = ['upstreams', 'models', 'endpoints', 'routing_dims'] as const

function isCount(value: unknown): value is number {
  return typeof value === 'number' && Number.isInteger(value) && value >= 0
}

/**
 * Validates the four frozen counters at the network boundary (R-PUB-08
 * semantics). A malformed body is treated as a failed request so the component
 * falls back to cache/hidden instead of rendering a bogus number.
 */
export function normalizeHomeStats(body: unknown): HomeStats {
  if (typeof body !== 'object' || body === null) throw new Error('invalid stats envelope')
  const record = body as Record<string, unknown>
  const result = {} as HomeStats
  for (const key of STAT_KEYS) {
    const value = record[key]
    if (!isCount(value)) throw new Error(`invalid stats field: ${key}`)
    result[key] = value
  }
  return result
}

export function readHomeStatsCache(now = Date.now()): HomeStats | null {
  try {
    const raw = globalThis.localStorage?.getItem(HOME_STATS_CACHE_KEY)
    if (raw === null || raw === undefined) return null
    const parsed: unknown = JSON.parse(raw)
    if (typeof parsed !== 'object' || parsed === null) return null
    const { cached_at: cachedAt, stats } = parsed as Record<string, unknown>
    if (typeof cachedAt !== 'number' || !Number.isFinite(cachedAt)) return null
    if (now - cachedAt > HOME_STATS_CACHE_TTL_MS) return null
    return normalizeHomeStats(stats)
  } catch {
    // Private mode / quota errors / corrupt JSON are all "no cache".
    return null
  }
}

export function writeHomeStatsCache(stats: HomeStats, now = Date.now()): void {
  try {
    globalThis.localStorage?.setItem(
      HOME_STATS_CACHE_KEY,
      JSON.stringify({ cached_at: now, stats }),
    )
  } catch {
    // Losing the fallback cache never breaks the current render.
  }
}

function apiUrl(path: string): string {
  const base = typeof location !== 'undefined' ? location.origin : 'http://localhost'
  return new URL(path, base).toString()
}

async function fetchHomeStats(signal?: AbortSignal): Promise<HomeStats> {
  let response: Response
  try {
    response = await fetch(apiUrl('/api/home/stats'), {
      credentials: 'same-origin',
      headers: { Accept: 'application/json' },
      signal,
    })
  } catch (error) {
    if (error instanceof DOMException && error.name === 'AbortError') throw error
    throw parseProblem({ status: 0 })
  }
  if (!response.ok) throw await toApiError(response)
  return normalizeHomeStats(await response.json().catch(() => undefined))
}

/**
 * IntersectionObserver gate: the counters are below the fold and must not cost
 * a request on first paint. jsdom and very old browsers have no observer; in
 * that case loading starts immediately (progressive enhancement, not a blank
 * section).
 */
function useInView() {
  const ref = useRef<HTMLElement>(null)
  const [inView, setInView] = useState(
    () => typeof IntersectionObserver === 'undefined',
  )

  useEffect(() => {
    if (inView || typeof IntersectionObserver === 'undefined') return
    const node = ref.current
    if (node === null) return
    const observer = new IntersectionObserver(
      (entries) => {
        if (entries.some((entry) => entry.isIntersecting)) {
          setInView(true)
          observer.disconnect()
        }
      },
      { threshold: 0.1 },
    )
    observer.observe(node)
    return () => observer.disconnect()
  }, [inView])

  return { ref, inView }
}

/**
 * F-09-04 stats band.
 *
 * Non-negotiable rules encoded here:
 * - render final values immediately — no count-up, no `requestAnimationFrame`
 *   (UI design §0.3 rule 5);
 * - a zero counter is removed item by item; an all-zero snapshot hides the
 *   whole band;
 * - on failure show the 24h cache; with no cache hide the band. Never render
 *   `0` or `–` as a stand-in for "unknown".
 */
export function StatsSection() {
  const { t } = useI18n()
  const { ref, inView } = useInView()
  const query = useQuery({
    queryKey: HOME_STATS_QUERY_KEY,
    queryFn: ({ signal }) => fetchHomeStats(signal),
    enabled: inView,
    retry: false,
    staleTime: 5 * 60 * 1000,
  })

  // Cache is read lazily so an error state does not touch storage on success.
  const cached = useMemo(
    () => (query.isError ? readHomeStatsCache() : null),
    [query.isError],
  )

  useEffect(() => {
    if (query.data !== undefined) writeHomeStatsCache(query.data)
  }, [query.data])

  const stats = query.data ?? cached
  const visible = stats === null ? [] : STAT_KEYS.filter((key) => stats[key] > 0)

  if (stats !== null && visible.length === 0) return null
  if (query.isError && cached === null) return null

  return (
    <section ref={ref} className="border-y border-border-hairline bg-surface-subtle">
      <div className="mx-auto w-full max-w-[1200px] px-4 py-10 md:px-6">
        <ul className="grid grid-cols-2 gap-6 md:grid-cols-4">
          {(stats === null ? STAT_KEYS : visible).map((key) => (
            <li key={key}>
              {stats === null ? (
                <span
                  aria-hidden="true"
                  className="block h-8 w-16 rounded bg-ink-4/30"
                />
              ) : (
                <span className="block text-2xl font-semibold leading-8 text-ink-1">
                  {new Intl.NumberFormat().format(stats[key])}
                </span>
              )}
              <span className="mt-1 block text-xs font-medium leading-[18px] text-ink-2">
                {t(`public.home.stats.${key}`)}
              </span>
            </li>
          ))}
        </ul>
      </div>
    </section>
  )
}
