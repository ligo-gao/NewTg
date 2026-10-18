import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { useState, type ReactNode } from 'react'

/**
 * Server-state provider (web-00 §5.4). All server data lives in the Query
 * cache; components never call `openapi-fetch` directly.
 */
export function createQueryClient(): QueryClient {
  return new QueryClient({
    defaultOptions: {
      queries: {
        // Query retries are one backing-off attempt; deterministic failures
        // (auth/forbidden/not_found/validation) are excluded by the callers.
        retry: 1,
        refetchOnWindowFocus: false,
        staleTime: 30_000,
        // `gcTime` is deliberately not set: TanStack Query's 5-minute default
        // already matches the `list` tier in web-00 §5.4. (The M0 draft kept an
        // explicit 300_000 with a comment claiming it was carried over from a
        // prior task — it was simply the library default, so the noise is gone.)
      },
      mutations: {
        // Writes are never retried automatically (idempotency safety).
        retry: 0,
      },
    },
  })
}

export interface QueryProviderProps {
  children: ReactNode
  /** Injected in tests; production derives one client per app instance. */
  client?: QueryClient
}

export function QueryProvider({ children, client }: QueryProviderProps) {
  const [queryClient] = useState(() => client ?? createQueryClient())

  return <QueryClientProvider client={queryClient}>{children}</QueryClientProvider>
}
