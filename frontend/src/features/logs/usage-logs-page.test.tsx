import { screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { useState } from 'react'
import { beforeEach, describe, expect, it } from 'vitest'

import { useToastStore } from '@/shared/ui/toast'

import type { UsageLogSearch } from './search'
import { DEFAULT_USAGE_LOG_SEARCH } from './search'
import { UsageLogsPage } from './usage-logs-page'
import {
  type FetchRecord,
  installFetch,
  jsonResponse,
  latestRequest,
  problemResponse,
  renderWithProviders,
  requestsFor,
} from './test-utils'

const ME_USER = {
  username: 'alice',
  role: 'user' as const,
  status: 'enabled' as const,
  permissions: [] as string[],
}

const ME_ADMIN = {
  ...ME_USER,
  username: 'root',
  role: 'admin' as const,
  permissions: ['logs:read'] as string[],
}

const ME_PSEUDO_ADMIN = {
  ...ME_USER,
  username: 'root',
  role: 'admin' as const,
  permissions: ['users:read'] as string[],
}

const USER_ROW = {
  id: 1,
  request_id: '9f3a8d2e-1b2c-4d5e-8f90-112233445566',
  token_id: 1,
  token_name: 'prod-key-1',
  channel_id: 12,
  upstream: 'OpenRouter-GPT',
  model: 'gpt-4o',
  group: 'default',
  prompt_tokens: 1204,
  completion_tokens: 356,
  cache_read_tokens: 812,
  cache_write_tokens: 0,
  pre_deducted: 512,
  cost: 45,
  refunded: 467,
  latency_ms: 1200,
  ttft: 300,
  stream: true,
  status: 'success' as const,
  error_class: null,
  error_message: null,
  retry_chain: null,
  created_at: '2026-10-14T09:41:22Z',
}

const ERROR_ROW = {
  ...USER_ROW,
  id: 2,
  model: 'gpt-4o-mini',
  status: 'error' as const,
  cost: 0,
  refunded: 512,
  error_class: 'upstream_5xx',
  error_message: 'upstream failed',
}

const REFUNDED_ROW = {
  ...USER_ROW,
  id: 3,
  model: 'claude-sonnet',
  status: 'refunded' as const,
  refunded: 12,
}

const REFUSED_ROW = {
  ...USER_ROW,
  id: 4,
  model: 'gemini-2.5-pro',
  status: 'refused' as const,
  cost: 0,
  refunded: 0,
  pre_deducted: null,
}

function page(
  items: unknown[],
  total = items.length,
  pageNumber = 1,
  pageSize = 20,
) {
  return { items, total, page: pageNumber, page_size: pageSize }
}

function PageHarness({
  initial,
  onSearch,
}: {
  initial?: Partial<UsageLogSearch>
  onSearch?: (next: UsageLogSearch) => void
}) {
  const [search, setSearch] = useState<UsageLogSearch>({
    ...DEFAULT_USAGE_LOG_SEARCH,
    ...initial,
  })
  return (
    <UsageLogsPage
      search={search}
      onSearchChange={(next) => {
        onSearch?.(next)
        setSearch(next)
      }}
    />
  )
}

interface Scenario {
  me?: object
  list?: (request: FetchRecord) => Response
  detail?: (request: FetchRecord) => Response
}

function installScenario(scenario: Scenario): FetchRecord[] {
  return installFetch((request) => {
    const path = request.url.pathname
    if (path === '/api/me') {
      return jsonResponse(scenario.me ?? ME_USER)
    }
    if (path === '/api/usage-logs') {
      if (scenario.list !== undefined) return scenario.list(request)
      return jsonResponse(page([]))
    }
    if (/^\/api\/usage-logs\/\d+$/.test(path)) {
      if (scenario.detail !== undefined) return scenario.detail(request)
      return jsonResponse({ ...USER_ROW, other: {} })
    }
    if (path.endsWith('/prompt')) {
      return problemResponse(404, { type: 'about:blank', title: 'not found' })
    }
    return problemResponse(404, { type: 'about:blank', title: 'not found' })
  })
}

beforeEach(() => {
  useToastStore.setState({ toasts: [] })
})

describe('usage logs list', () => {
  it('renders rows with the four status badges and token snapshots', async () => {
    installScenario({
      list: () =>
        jsonResponse(page([USER_ROW, ERROR_ROW, REFUNDED_ROW, REFUSED_ROW], 4)),
    })
    renderWithProviders(<PageHarness />)

    await screen.findByText('gpt-4o')
    const badges = screen.getAllByTestId('status-badge')
    expect(badges.map((badge) => badge.getAttribute('data-variant'))).toEqual([
      'success',
      'danger',
      'warning',
      'danger-outline',
    ])
    expect(screen.getAllByText('prod-key-1')).toHaveLength(4)
    expect(screen.getAllByText('1,204').length).toBeGreaterThan(0)
    expect(screen.getAllByText('refunded 467')).toHaveLength(1)
  })

  it('defaults to a 24h range and never sends a username for a user', async () => {
    const requests = installScenario({
      list: () => jsonResponse(page([USER_ROW])),
    })
    renderWithProviders(
      <PageHarness initial={{ username: 'someone-else' }} />,
    )

    await screen.findByText('gpt-4o')
    await waitFor(() => {
      const request = latestRequest(requests, '/api/usage-logs')
      expect(request).toBeDefined()
      expect(request?.url.searchParams.get('range')).toBe('24h')
      expect(request?.url.searchParams.has('username')).toBe(false)
    })
    expect(requestsFor(requests, '/api/usage-logs').every((request) => !request.url.searchParams.has('username'))).toBe(true)
    expect(screen.queryByRole('columnheader', { name: 'User' })).toBeNull()
    expect(screen.queryByRole('columnheader', { name: 'Channel' })).toBeNull()
    expect(screen.queryByLabelText('Username')).toBeNull()
  })

  it('shows admin-only columns and sends the username filter for logs:read admins', async () => {
    const requests = installScenario({
      me: ME_ADMIN,
      list: () => jsonResponse(page([{ ...USER_ROW, username: 'alice' }])),
    })
    renderWithProviders(
      <PageHarness initial={{ username: 'alice' }} />,
    )

    await screen.findByText('gpt-4o')
    expect(await screen.findByRole('columnheader', { name: 'User' })).toBeTruthy()
    expect(screen.getByRole('columnheader', { name: 'Channel' })).toBeTruthy()
    expect(await screen.findByLabelText('Username')).toBeTruthy()
    await waitFor(() => {
      expect(
        latestRequest(requests, '/api/usage-logs')?.url.searchParams.get(
          'username',
        ),
      ).toBe('alice')
    })
  })

  it('hides admin columns from an admin without logs:read', async () => {
    installScenario({
      me: ME_PSEUDO_ADMIN,
      list: () => jsonResponse(page([USER_ROW])),
    })
    renderWithProviders(<PageHarness />)

    await screen.findByText('gpt-4o')
    // Channel is an admin column; username needs logs:read, which this admin
    // does not hold.
    expect(await screen.findByRole('columnheader', { name: 'Channel' })).toBeTruthy()
    expect(screen.queryByRole('columnheader', { name: 'User' })).toBeNull()
  })

  it('syncs the status filter to the URL and resets to page 1', async () => {
    const user = userEvent.setup()
    const changes: UsageLogSearch[] = []
    const requests = installScenario({
      list: () => jsonResponse(page([USER_ROW])),
    })
    renderWithProviders(
      <PageHarness initial={{ p: 3 }} onSearch={(next) => changes.push(next)} />,
    )

    await screen.findByText('gpt-4o')
    await user.selectOptions(screen.getByLabelText('Status'), 'error')

    await waitFor(() => {
      expect(changes.at(-1)?.status).toBe('error')
      expect(changes.at(-1)?.p).toBe(1)
      expect(
        latestRequest(requests, '/api/usage-logs')?.url.searchParams.get(
          'status',
        ),
      ).toBe('error')
    })
  })

  it('debounces the model input by 300ms before querying', async () => {
    const user = userEvent.setup()
    const requests = installScenario({
      list: () => jsonResponse(page([USER_ROW])),
    })
    renderWithProviders(<PageHarness />)

    await screen.findByText('gpt-4o')
    await user.type(screen.getByLabelText('Model'), 'gpt-4o')

    // The first fetch happens on mount; while typing no query carries the
    // unfinished model.
    expect(
      requestsFor(requests, '/api/usage-logs').every(
        (request) => request.url.searchParams.get('model') !== 'gpt-4o',
      ),
    ).toBe(true)

    await waitFor(
      () => {
        expect(
          latestRequest(requests, '/api/usage-logs')?.url.searchParams.get(
            'model',
          ),
        ).toBe('gpt-4o')
      },
      { timeout: 2000 },
    )
  })

  it('blocks an invalid request ID and shows a field error', async () => {
    const user = userEvent.setup()
    const requests = installScenario({
      list: () => jsonResponse(page([USER_ROW])),
    })
    renderWithProviders(<PageHarness />)

    await screen.findByText('gpt-4o')
    await user.type(screen.getByLabelText('Request ID'), 'not-a-uuid')

    expect(await screen.findByText('Invalid request ID')).toBeTruthy()
    await new Promise((resolve) => setTimeout(resolve, 600))
    expect(
      requestsFor(requests, '/api/usage-logs').some((request) =>
        request.url.searchParams.has('request_id'),
      ),
    ).toBe(false)
  })

  it('queries a valid request ID after the 500ms debounce', async () => {
    const user = userEvent.setup()
    const requests = installScenario({
      list: () => jsonResponse(page([USER_ROW])),
    })
    renderWithProviders(<PageHarness />)

    await screen.findByText('gpt-4o')
    await user.type(
      screen.getByLabelText('Request ID'),
      '9f3a8d2e-1b2c-4d5e-8f90-112233445566',
    )

    await waitFor(
      () => {
        expect(
          latestRequest(requests, '/api/usage-logs')?.url.searchParams.get(
            'request_id',
          ),
        ).toBe('9f3a8d2e-1b2c-4d5e-8f90-112233445566')
      },
      { timeout: 2500 },
    )
  })

  it('rejects a custom range longer than 90 days without querying it', async () => {
    const user = userEvent.setup()
    const requests = installScenario({
      list: () => jsonResponse(page([USER_ROW])),
    })
    renderWithProviders(<PageHarness />)

    await screen.findByText('gpt-4o')
    expect(screen.getByText('Details are kept for the last 3 months')).toBeTruthy()

    await user.click(screen.getByRole('radio', { name: 'Custom' }))
    await user.type(screen.getByLabelText('Start'), '2026-01-01T00:00')
    await user.type(screen.getByLabelText('End'), '2026-04-02T00:00')

    expect(await screen.findByText('Range cannot exceed 90 days')).toBeTruthy()
    expect(
      requestsFor(requests, '/api/usage-logs').some((request) =>
        request.url.searchParams.has('start'),
      ),
    ).toBe(false)
  })

  it('renders a 422 invalid-range message', async () => {
    installScenario({
      list: () =>
        problemResponse(422, {
          type: 'invalid_range',
          title: 'Invalid time range',
          detail: 'start is after end',
        }),
    })
    renderWithProviders(<PageHarness />)

    expect(await screen.findByText(/Invalid time range/)).toBeTruthy()
    expect(screen.getByRole('button', { name: 'Retry' })).toBeTruthy()
  })

  it('renders a 429 message with the Retry-After countdown', async () => {
    installScenario({
      list: () =>
        problemResponse(
          429,
          { type: 'rate_limited', title: 'Too many requests' },
          { 'Retry-After': '30' },
        ),
    })
    renderWithProviders(<PageHarness />)

    expect(
      await screen.findByText('Too many requests, retry in 30s'),
    ).toBeTruthy()
  })

  it('renders the unfiltered and filtered empty states', async () => {
    const user = userEvent.setup()
    installScenario({ list: () => jsonResponse(page([])) })
    renderWithProviders(<PageHarness />)

    expect(await screen.findByText('No usage logs yet')).toBeTruthy()

    await user.selectOptions(screen.getByLabelText('Status'), 'error')
    expect(await screen.findByText('No logs match your filters')).toBeTruthy()

    await user.click(screen.getByRole('button', { name: 'Clear filters' }))
    await waitFor(() => {
      expect((screen.getByLabelText('Status') as HTMLSelectElement).value).toBe(
        'all',
      )
    })
    expect(await screen.findByText('No usage logs yet')).toBeTruthy()
  })

  it('shows an inline error and retries', async () => {
    let calls = 0
    installScenario({
      list: () => {
        calls += 1
        if (calls === 1) {
          return problemResponse(500, { type: 'server_error', title: 'boom' })
        }
        return jsonResponse(page([USER_ROW]))
      },
    })
    const user = userEvent.setup()
    renderWithProviders(<PageHarness />)

    expect(await screen.findByText('Failed to load logs.')).toBeTruthy()
    await user.click(screen.getByRole('button', { name: 'Retry' }))
    expect(await screen.findByText('gpt-4o')).toBeTruthy()
  })

  it('falls back to the last valid page and announces the list change', async () => {
    const requests = installScenario({
      list: () => jsonResponse(page([], 42, 5, 20)),
    })
    const changes: UsageLogSearch[] = []
    renderWithProviders(
      <PageHarness initial={{ p: 5 }} onSearch={(next) => changes.push(next)} />,
    )

    await waitFor(() => {
      expect(changes.at(-1)?.p).toBe(3)
    })
    await waitFor(() => {
      expect(
        latestRequest(requests, '/api/usage-logs')?.url.searchParams.get('p'),
      ).toBe('3')
    })
    expect(
      useToastStore.getState().toasts.some((toast) => toast.message === 'List updated'),
    ).toBe(true)
  })

  it('paginates with the page size', async () => {
    const user = userEvent.setup()
    const requests = installScenario({
      list: () => jsonResponse(page([USER_ROW], 42, 1, 20)),
    })
    renderWithProviders(<PageHarness />)

    await screen.findByText('gpt-4o')
    expect(screen.getByText('Page 1 of 3')).toBeTruthy()

    await user.click(screen.getByRole('button', { name: 'Next page' }))
    await waitFor(() => {
      expect(
        latestRequest(requests, '/api/usage-logs')?.url.searchParams.get('p'),
      ).toBe('2')
    })
  })

  it('opens the detail drawer with sections A-D and the retry chain', async () => {
    const user = userEvent.setup()
    const retryChain = [
      {
        channel_name: 'OpenRouter-A',
        decision: 'priority',
        decision_label: 'Priority match',
        error_class: 'upstream_rate_limited',
        latency_ms: 800,
      },
      {
        channel_name: 'OpenRouter-B',
        decision: 'fallback',
        decision_label: 'Fallback retry',
        error_class: 'network_error',
        latency_ms: 1100,
      },
      {
        channel_name: 'OpenRouter-GPT',
        decision: 'fallback',
        decision_label: 'Fallback retry',
        error_class: null,
        latency_ms: 1200,
      },
    ]
    installScenario({
      me: ME_ADMIN,
      list: () => jsonResponse(page([{ ...USER_ROW, username: 'alice' }])),
      detail: () =>
        jsonResponse({
          ...USER_ROW,
          other: {
            unit_prices: { input: 2, output: 8, cache_read: 0.5 },
            group_ratio: 1,
          },
          retry_chain: retryChain,
        }),
    })
    renderWithProviders(<PageHarness />)

    await user.click(await screen.findByText('gpt-4o'))

    const dialog = await screen.findByRole('dialog')
    expect(within(dialog).getByText('General')).toBeTruthy()
    expect(within(dialog).getByText('Usage Parameters')).toBeTruthy()
    expect(within(dialog).getByText('Billing Details')).toBeTruthy()
    expect(within(dialog).getByText('Timing & Failure')).toBeTruthy()
    expect(within(dialog).getByText('Retry Chain')).toBeTruthy()
    expect(within(dialog).getByText('OpenRouter-A')).toBeTruthy()
    expect(within(dialog).getByText('OpenRouter-GPT')).toBeTruthy()
    expect(within(dialog).getByText(/3 attempts and succeeded/)).toBeTruthy()
    expect(within(dialog).getByText('9f3a8d2e-1b2c-4d5e-8f90-112233445566')).toBeTruthy()
  })

  it('hides View Prompt without log.prompt.view and masks prompt-like fields', async () => {
    const user = userEvent.setup()
    installScenario({
      list: () => jsonResponse(page([USER_ROW])),
      detail: () => jsonResponse({ ...USER_ROW, other: { prompt: 'secret text' } }),
    })
    renderWithProviders(<PageHarness />)

    await user.click(await screen.findByText('gpt-4o'))
    const dialog = await screen.findByRole('dialog')
    expect(within(dialog).getByText('****')).toBeTruthy()
    expect(within(dialog).queryByRole('button', { name: 'View Prompt' })).toBeNull()
  })

  it('hides the prompt action when no prompt carrier exists', async () => {
    const user = userEvent.setup()
    installScenario({ list: () => jsonResponse(page([USER_ROW])) })
    renderWithProviders(<PageHarness />)

    await user.click(await screen.findByText('gpt-4o'))
    const dialog = await screen.findByRole('dialog')
    expect(within(dialog).queryByRole('button', { name: 'View Prompt' })).toBeNull()
    expect(within(dialog).queryByText('****')).toBeNull()
  })
})
