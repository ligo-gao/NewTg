import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { cleanup, render } from '@testing-library/react'
import type { ReactElement, ReactNode } from 'react'
import { afterEach, vi } from 'vitest'

import type { components } from '@/api/schema'
import common from '@/locales/en/common.json'
import { I18nContextProvider, type TranslateValues } from '@/shared/lib/i18n-context'
import { ToastViewport, useToastStore } from '@/shared/ui/toast'

export type Token = components['schemas']['Token']
export type TokenList = components['schemas']['TokenList']
export type CreatedToken = components['schemas']['CreatedToken']

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
      queries: { retry: false, gcTime: 0 },
      mutations: { retry: false },
    },
  })

  function Wrapper({ children }: { children: ReactNode }) {
    return (
      <QueryClientProvider client={queryClient}>
        <I18nContextProvider value={I18N_VALUE}>
          {children}
          <ToastViewport />
        </I18nContextProvider>
      </QueryClientProvider>
    )
  }

  return { queryClient, ...render(ui, { wrapper: Wrapper }) }
}

afterEach(() => {
  cleanup()
  useToastStore.setState({ toasts: [] })
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
      // openapi-fetch hands the wrapped client a `Request`, so the method,
      // headers and body live on the Request rather than on `init`.
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
  problem: {
    type: string
    title: string
    detail?: string
    fields?: string[]
  },
): Response {
  return jsonResponse({ status, ...problem }, status)
}

export function makeToken(overrides: Partial<Token> = {}): Token {
  return {
    id: 1,
    name: 'alpha',
    status: 'enabled',
    key_masked: `nkt-${'•'.repeat(24)}`,
    remain_quota: null,
    unlimited_quota: true,
    quota_percent: null,
    group: 'default',
    model_limits: null,
    allow_ips: null,
    auto_groups: [],
    cross_group_retry: false,
    created_at: '2026-10-01T00:00:00Z',
    updated_at: '2026-10-01T00:00:00Z',
    last_used_at: null,
    expires_at: null,
    ...overrides,
  }
}

export function makeListResponse(
  items: Token[],
  overrides: Partial<TokenList> = {},
): TokenList {
  return {
    items,
    total: items.length,
    page: 1,
    page_size: 20,
    ...overrides,
  }
}

export function findRequest(
  requests: FetchRecord[],
  method: string,
  pathname: string,
): FetchRecord | undefined {
  return [...requests]
    .reverse()
    .find((request) => request.method === method && request.url.pathname === pathname)
}

export function allRequests(
  requests: FetchRecord[],
  method: string,
  pathname: string,
): FetchRecord[] {
  return requests.filter(
    (request) => request.method === method && request.url.pathname === pathname,
  )
}
