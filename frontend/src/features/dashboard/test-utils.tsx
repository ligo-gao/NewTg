import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { cleanup, render } from '@testing-library/react'
import type { ReactElement, ReactNode } from 'react'
import { afterEach, vi } from 'vitest'

import common from '@/locales/en/common.json'
import { I18nContextProvider, type TranslateValues } from '@/shared/lib/i18n-context'

/**
 * Dashboard test harness. Mirrors the tokens feature harness: a fresh
 * QueryClient per test (query caches are module state otherwise) and the real
 * English `common` dictionary so assertions use final copy rather than keys.
 */

function flatten(prefix: string, value: unknown, out: Record<string, string>): void {
  if (typeof value === 'string') {
    out[prefix] = value
    return
  }
  if (typeof value === 'object' && value !== null) {
    for (const [key, child] of Object.entries(value)) {
      flatten(`${prefix}.${key}`, child, out)
    }
  }
}

const MESSAGES: Record<string, string> = (() => {
  const out: Record<string, string> = {}
  flatten('common', common, out)
  return out
})()

function translate(key: string, values?: TranslateValues): string {
  const template = MESSAGES[key]
  if (template === undefined) return key
  if (values === undefined) return template
  return template.replace(/\{([a-zA-Z0-9_]+)\}/g, (match, name: string) => {
    const value = values[name]
    return value === undefined ? match : String(value)
  })
}

const I18N_VALUE = { locale: 'en' as const, t: translate }

export function renderWithProviders(ui: ReactElement) {
  const queryClient = new QueryClient({
    defaultOptions: {
      queries: { retry: false, gcTime: 0, staleTime: 0 },
      mutations: { retry: false },
    },
  })

  function Wrapper({ children }: { children: ReactNode }) {
    return (
      <QueryClientProvider client={queryClient}>
        <I18nContextProvider value={I18N_VALUE}>{children}</I18nContextProvider>
      </QueryClientProvider>
    )
  }

  return { queryClient, ...render(ui, { wrapper: Wrapper }) }
}

afterEach(() => {
  cleanup()
  vi.unstubAllGlobals()
})

export interface FetchRecord {
  method: string
  url: URL
  headers: Headers
  body: unknown
}

export type FetchHandler = (request: FetchRecord) => Response | Promise<Response>

export function installFetch(handler: FetchHandler): FetchRecord[] {
  const requests: FetchRecord[] = []

  vi.stubGlobal(
    'fetch',
    vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      const raw =
        typeof input === 'string'
          ? input
          : input instanceof URL
            ? input.toString()
            : input.url
      const request = input instanceof Request ? input.clone() : new Request(raw, init)
      const method = request.method.toUpperCase()
      const headers = new Headers(request.headers)
      const text = await request.text()
      let body: unknown
      if (text !== '') {
        try {
          body = JSON.parse(text)
        } catch {
          body = text
        }
      }
      const record: FetchRecord = { method, url: new URL(raw), headers, body }
      requests.push(record)
      return handler(record)
    }),
  )

  return requests
}

export function jsonResponse(body: unknown, status = 200): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: {
      'Content-Type':
        status >= 400 ? 'application/problem+json' : 'application/json',
    },
  })
}

export function problemResponse(
  status: number,
  problem: { type: string; title: string; detail?: string },
): Response {
  return jsonResponse({ status, ...problem }, status)
}

export function requestsFor(
  requests: FetchRecord[],
  pathname: string,
): FetchRecord[] {
  return requests.filter(
    (request) => request.method === 'GET' && request.url.pathname === pathname,
  )
}

export function latestRequest(
  requests: FetchRecord[],
  pathname: string,
): FetchRecord | undefined {
  return requestsFor(requests, pathname).at(-1)
}
