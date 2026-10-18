import {
  MutationCache,
  QueryCache,
  QueryClient,
  QueryClientProvider,
} from '@tanstack/react-query'
import { useState, type ReactNode } from 'react'

import { ApiError, type ApiErrorKind } from '@/api/errors'

import { reportServerError, isUnauthorizedError } from './auth-failures'

/**
 * Web-00 §5.4 retry policy: one backing-off attempt for transient failures,
 * none for deterministic ones. 401/403/404/400(422) cannot succeed on replay
 * and retrying them would delay the guard/error landings by seconds.
 */
const NON_RETRYABLE_KINDS: ReadonlySet<ApiErrorKind> = new Set([
  'auth',
  'forbidden',
  'not_found',
  'validation',
])

function shouldRetryQuery(failureCount: number, error: unknown): boolean {
  if (error instanceof ApiError && NON_RETRYABLE_KINDS.has(error.kind)) {
    return false
  }
  return failureCount < 1
}

/**
 * Server-state provider (web-00 §5.4). All server data lives in the Query
 * cache; components never call `openapi-fetch` directly.
 */
export function createQueryClient(): QueryClient {
  const client: QueryClient = new QueryClient({
    // Cache-level error hooks (v5): every query/mutation failure passes through
    // here, which is where the global 401 / forced-password-change landings
    // live. They are wired on the caches rather than per-call so a forgotten
    // handler in one feature cannot leave the user staring at a generic error.
    queryCache: new QueryCache({
      onError: (error, query) => {
        if (isUnauthorizedError(error) && query.queryKey[0] !== 'me') {
          // A dead session must not stay cached as an identity: guards and
          // feature headers read `['me']` directly, and refetching it here
          // would race the navigation (the next mount re-probes anyway).
          void client.invalidateQueries({ queryKey: ['me'], refetchType: 'none' })
        }
        reportServerError(error, query.queryKey)
      },
    }),
    mutationCache: new MutationCache({
      onError: (error) => {
        if (isUnauthorizedError(error)) {
          void client.invalidateQueries({ queryKey: ['me'], refetchType: 'none' })
        }
        reportServerError(error, [])
      },
    }),
    defaultOptions: {
      queries: {
        retry: shouldRetryQuery,
        refetchOnWindowFocus: false,
        staleTime: 30_000,
        // `gcTime` is deliberately not set: TanStack Query's 5-minute default
        // already matches the `list` tier in web-00 §5.4.
      },
      mutations: {
        // Writes are never retried automatically (idempotency safety).
        retry: 0,
      },
    },
  })
  return client
}

/**
 * The application-wide client. It is a module singleton because the router
 * guards (`beforeLoad`) and the React tree must observe the same `['me']`
 * cache; a second client would make every navigation re-probe the session.
 */
export const appQueryClient = createQueryClient()

export interface QueryProviderProps {
  children: ReactNode
  /** Injected in tests; production reuses the router-visible singleton. */
  client?: QueryClient
}

export function QueryProvider({ children, client }: QueryProviderProps) {
  const [queryClient] = useState(() => client ?? appQueryClient)

  return <QueryClientProvider client={queryClient}>{children}</QueryClientProvider>
}
