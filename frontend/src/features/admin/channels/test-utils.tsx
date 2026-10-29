import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { cleanup, render } from '@testing-library/react'
import type { ReactElement, ReactNode } from 'react'
import { afterEach, vi } from 'vitest'

import common from '@/locales/en/common.json'
import { I18nContextProvider, type TranslateValues } from '@/shared/lib/i18n-context'
import { ToastViewport, useToastStore } from '@/shared/ui/toast'

import type { Channel, ChannelListResponse } from './types'

/**
 * Test-only providers and transport stub.
 *
 * Strings are resolved synchronously from the real `common.json` so assertions
 * see production copy without waiting for the lazy dictionary chunk. `fetch` is
 * stubbed rather than mocked with msw so tests can inspect the exact query
 * string, headers and JSON body the feature emits.
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

  return {
    queryClient,
    ...render(ui, { wrapper: Wrapper }),
  }
}

afterEach(() => {
  cleanup()
  useToastStore.setState({ toasts: [] })
  vi.unstubAllGlobals()
})

// ---------------------------------------------------------------------------
// fetch stub
// ---------------------------------------------------------------------------

export interface FetchRecord {
  method: string
  url: URL
  headers: Headers
  body: unknown
}

export type FetchHandler = (request: FetchRecord) => Response | Promise<Response>

/** Replaces `globalThis.fetch` and records every request the feature makes. */
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
      const method = (init?.method ?? 'GET').toUpperCase()
      const headers = new Headers(init?.headers)
      let body: unknown
      if (typeof init?.body === 'string') {
        try {
          body = JSON.parse(init.body)
        } catch {
          body = init.body
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
    errors?: Array<{ field: string; message: string }>
  },
): Response {
  return jsonResponse({ status, ...problem }, status)
}

// ---------------------------------------------------------------------------
// Fixtures
// ---------------------------------------------------------------------------

export function makeChannel(overrides: Partial<Channel> = {}): Channel {
  return {
    id: 1,
    name: 'alpha',
    type: 'openai',
    status: 1,
    priority: 10,
    weight: 1,
    supported_models: ['gpt-4', 'gpt-4o'],
    group_tags: null,
    rate_limit: null,
    auto_ban: true,
    allow_private_network: false,
    test_model: null,
    remark: null,
    tag: null,
    fail_count: 0,
    auto_disabled_reason: null,
    last_test_at: null,
    last_test_result: null,
    created_at: '2026-10-01T00:00:00Z',
    updated_at: '2026-10-01T00:00:00Z',
    version: '2026-10-01T00:00:00Z',
    ...overrides,
  }
}

export function makeListResponse(
  items: Channel[],
  overrides: Partial<ChannelListResponse> = {},
): ChannelListResponse {
  return {
    items,
    total: items.length,
    page: 1,
    page_size: 20,
    ...overrides,
  }
}

/** Last request matching a method/path pair, for assertions. */
export function findRequest(
  requests: FetchRecord[],
  method: string,
  pathname: string,
): FetchRecord | undefined {
  return [...requests]
    .reverse()
    .find((request) => request.method === method && request.url.pathname === pathname)
}
