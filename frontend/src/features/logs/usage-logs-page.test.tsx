import { screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { useState } from 'react'
import { beforeEach, describe, expect, it, vi } from 'vitest'

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
  summary?: (request: FetchRecord) => Response
  byModel?: (request: FetchRecord) => Response
  timeseries?: (request: FetchRecord) => Response
  rates?: (request: FetchRecord) => Response
}

const EMPTY_SUMMARY = {
  cost_total: 0,
  request_count: 0,
  success_rate: 0,
  prompt_tokens_total: 0,
  completion_tokens_total: 0,
  pre_deducted_total: 0,
  refunded_total: 0,
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
    if (path === '/api/usage-stats/summary') {
      if (scenario.summary !== undefined) return scenario.summary(request)
      return jsonResponse(EMPTY_SUMMARY)
    }
    if (path === '/api/usage-stats/by-model') {
      if (scenario.byModel !== undefined) return scenario.byModel(request)
      return jsonResponse([])
    }
    if (path === '/api/usage-stats/timeseries') {
      if (scenario.timeseries !== undefined) return scenario.timeseries(request)
      return jsonResponse([])
    }
    if (path === '/api/usage-stats/rates') {
      if (scenario.rates !== undefined) return scenario.rates(request)
      return jsonResponse({
        rpm_avg: 0,
        rpm_peak: 0,
        tpm_avg: 0,
        cost_total: 0,
        pre_deducted_total: 0,
        refunded_total: 0,
      })
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

  it('lets Enter on a copy button copy without opening the drawer', async () => {
    const user = userEvent.setup()
    const writeText = vi.fn(async () => undefined)
    Object.defineProperty(navigator, 'clipboard', {
      configurable: true,
      value: { writeText },
    })
    const requests = installScenario({
      list: () => jsonResponse(page([USER_ROW])),
    })
    renderWithProviders(<PageHarness />)

    const copyButton = await screen.findByRole('button', {
      name: 'Copy request ID',
    })
    copyButton.focus()
    await user.keyboard('{Enter}')

    expect(writeText).toHaveBeenCalledWith(USER_ROW.request_id)
    await waitFor(() => {
      expect(
        useToastStore.getState().toasts.some((toast) => toast.message === 'Copied'),
      ).toBe(true)
    })
    // The row's own Enter handler must not fire for a key pressed on a child.
    expect(screen.queryByRole('dialog')).toBeNull()
    expect(
      requestsFor(requests, `/api/usage-logs/${String(USER_ROW.id)}`),
    ).toHaveLength(0)
  })

  it('reports a failed copy instead of claiming success', async () => {
    const user = userEvent.setup()
    const writeText = vi.fn(async () => {
      throw new Error('clipboard denied')
    })
    Object.defineProperty(navigator, 'clipboard', {
      configurable: true,
      value: { writeText },
    })
    installScenario({ list: () => jsonResponse(page([USER_ROW])) })
    renderWithProviders(<PageHarness />)

    await user.click(
      await screen.findByRole('button', { name: 'Copy request ID' }),
    )

    await waitFor(() => {
      expect(
        useToastStore
          .getState()
          .toasts.some(
            (toast) =>
              toast.message === 'Copy failed. Please select the text manually.',
          ),
      ).toBe(true)
    })
    expect(
      useToastStore.getState().toasts.some((toast) => toast.message === 'Copied'),
    ).toBe(false)
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
      // 敏感开关默认关闭：列表请求保持打码口径，绝不默认请求原文。
      expect(request?.url.searchParams.has('reveal_sensitive')).toBe(false)
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

  it('shows the request id column to every role and hides the group filter from users', async () => {
    installScenario({ list: () => jsonResponse(page([USER_ROW])) })
    renderWithProviders(<PageHarness />)

    await screen.findByText('gpt-4o')
    expect(screen.getByRole('columnheader', { name: 'Request ID' })).toBeTruthy()
    expect(screen.getByText('9f3a8d2e…')).toBeTruthy()
    expect(screen.queryByLabelText('Group')).toBeNull()
  })

  it('keeps a user-supplied group filter visible as a removable chip', async () => {
    const user = userEvent.setup()
    const requests = installScenario({
      list: () => jsonResponse(page([USER_ROW])),
    })
    renderWithProviders(<PageHarness initial={{ group: 'vip' }} />)

    await screen.findByText('gpt-4o')
    await user.click(screen.getByRole('button', { name: 'Group: vip ×' }))
    await waitFor(() => {
      expect(
        latestRequest(requests, '/api/usage-logs')?.url.searchParams.get('group'),
      ).toBeNull()
    })
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

  it('debounces the group and token filters by 300ms (admin)', async () => {
    const user = userEvent.setup()
    const requests = installScenario({
      me: ME_ADMIN,
      list: () => jsonResponse(page([USER_ROW])),
    })
    renderWithProviders(<PageHarness />)

    await screen.findByText('gpt-4o')
    await user.type(screen.getByLabelText('Group'), 'default')
    expect(
      requestsFor(requests, '/api/usage-logs').every(
        (request) => request.url.searchParams.get('group') !== 'default',
      ),
    ).toBe(true)
    await waitFor(
      () => {
        expect(
          latestRequest(requests, '/api/usage-logs')?.url.searchParams.get(
            'group',
          ),
        ).toBe('default')
      },
      { timeout: 2000 },
    )

    await user.type(screen.getByLabelText('Token ID'), '12')
    await waitFor(
      () => {
        expect(
          latestRequest(requests, '/api/usage-logs')?.url.searchParams.get(
            'token_id',
          ),
        ).toBe('12')
      },
      { timeout: 2000 },
    )
  })

  it('keeps in-flight username text when another filter changes', async () => {
    const user = userEvent.setup()
    const requests = installScenario({
      me: ME_ADMIN,
      list: () => jsonResponse(page([{ ...USER_ROW, username: 'alice' }])),
    })
    renderWithProviders(<PageHarness />)

    await screen.findByText('gpt-4o')
    const input = screen.getByLabelText('Username') as HTMLInputElement
    await user.type(input, 'alice')
    // Change another filter inside the 300ms debounce window. The external
    // search sync must not wipe the in-flight username draft.
    await user.selectOptions(screen.getByLabelText('Status'), 'error')
    expect(input.value).toBe('alice')

    await waitFor(
      () => {
        const request = latestRequest(requests, '/api/usage-logs')
        expect(request?.url.searchParams.get('username')).toBe('alice')
        expect(request?.url.searchParams.get('status')).toBe('error')
      },
      { timeout: 2500 },
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
        ).toBe('9f3a8d2e1b2c4d5e8f90112233445566')
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
    // The filter toolbar stays available so the user can correct the range.
    expect(screen.getByLabelText('Status')).toBeTruthy()
    expect(screen.getByRole('radio', { name: 'Custom' })).toBeTruthy()
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
    // Filters remain editable while rate-limited.
    expect(screen.getByLabelText('Status')).toBeTruthy()
  })

  it('keeps the 429 countdown at zero instead of snapping back to Retry-After', async () => {
    installScenario({
      list: () =>
        problemResponse(
          429,
          { type: 'rate_limited', title: 'Too many requests' },
          { 'Retry-After': '1' },
        ),
    })
    renderWithProviders(<PageHarness />)

    expect(
      await screen.findByText('Too many requests, retry in 1s'),
    ).toBeTruthy()
    expect(
      await screen.findByText('Too many requests, retry in 0s', undefined, {
        timeout: 3000,
      }),
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
    expect(within(dialog).getByText('2 retries')).toBeTruthy()
    expect(within(dialog).getByText('9f3a8d2e-1b2c-4d5e-8f90-112233445566')).toBeTruthy()
  })

  it('never shows raw retry decision keys to a non-admin', async () => {
    const user = userEvent.setup()
    installScenario({
      list: () => jsonResponse(page([USER_ROW])),
      detail: () =>
        jsonResponse({
          ...USER_ROW,
          retry_chain: [
            {
              channel_name: 'OpenRouter-A',
              decision: 'priority',
              error_class: 'upstream_rate_limited',
              latency_ms: 800,
            },
            {
              channel_name: 'OpenRouter-B',
              decision: 'fallback',
              error_class: null,
              latency_ms: 1200,
            },
          ],
        }),
    })
    renderWithProviders(<PageHarness />)

    await user.click(await screen.findByText('gpt-4o'))
    const dialog = await screen.findByRole('dialog')
    expect(within(dialog).getByText('OpenRouter-A')).toBeTruthy()
    expect(within(dialog).queryByText('priority')).toBeNull()
    expect(within(dialog).queryByText('fallback')).toBeNull()
  })

  it('masks error messages for users by default', async () => {
    const user = userEvent.setup()
    installScenario({
      list: () => jsonResponse(page([ERROR_ROW])),
      detail: () =>
        jsonResponse({
          ...ERROR_ROW,
          error_message: 'upstream exploded at host db.internal',
        }),
    })
    renderWithProviders(<PageHarness />)

    await user.click(await screen.findByText('gpt-4o-mini'))
    const dialog = await screen.findByRole('dialog')
    expect(within(dialog).getByText('Failure Reason')).toBeTruthy()
    expect(within(dialog).getAllByText('••••••').length).toBeGreaterThan(0)
    expect(dialog.textContent).not.toContain('upstream exploded')
  })

  it('masks error messages until an admin with log.prompt.view enables the switch', async () => {
    const user = userEvent.setup()
    const requests = installScenario({
      me: { ...ME_ADMIN, permissions: ['logs:read', 'log.prompt.view'] },
      list: () => jsonResponse(page([{ ...ERROR_ROW, username: 'alice' }])),
      detail: () =>
        jsonResponse({
          ...ERROR_ROW,
          error_message: 'upstream exploded at host db.internal',
        }),
    })
    renderWithProviders(<PageHarness />)

    await user.click(await screen.findByText('gpt-4o-mini'))
    let dialog = await screen.findByRole('dialog')
    // F-05-06：默认打码，即使有权限也必须由用户显式开启。
    expect(dialog.textContent).not.toContain('upstream exploded')
    expect(within(dialog).getAllByText('••••••').length).toBeGreaterThan(0)

    await user.click(within(dialog).getByRole('button', { name: 'Close' }))
    await user.click(screen.getByRole('switch', { name: 'Show Sensitive Content' }))
    await user.click(screen.getByTestId('sensitive-confirm'))

    await user.click(await screen.findByText('gpt-4o-mini'))
    dialog = await screen.findByRole('dialog')
    expect(
      within(dialog).getByText('upstream exploded at host db.internal'),
    ).toBeTruthy()
    expect(
      latestRequest(requests, '/api/usage-logs')?.url.searchParams.get(
        'reveal_sensitive',
      ),
    ).toBe('1')
  })

  it('hides View Prompt without log.prompt.view and masks prompt-like fields', async () => {
    const user = userEvent.setup()
    installScenario({
      list: () => jsonResponse(page([USER_ROW])),
      detail: () => jsonResponse({ ...USER_ROW, other: { prompt: 'secret text' } }),
    })
    renderWithProviders(<PageHarness />)

    // UI §6.4/§6.5：无 log.prompt.view 时静默隐藏入口，不渲染禁用态或
    // 「没有权限」提示（入口本身不得暴露敏感面存在）。
    await screen.findByText('gpt-4o')
    expect(
      screen.queryByRole('switch', { name: 'Show Sensitive Content' }),
    ).toBeNull()

    await user.click(await screen.findByText('gpt-4o'))
    const dialog = await screen.findByRole('dialog')
    // §8.2：无权限时连打码占位与引导文案都不渲染，避免暴露记录存在性。
    expect(within(dialog).queryByText('Prompt')).toBeNull()
    expect(within(dialog).queryByTestId('prompt-masked')).toBeNull()
    expect(within(dialog).queryByText('••••••')).toBeNull()
    expect(within(dialog).queryByText('secret text')).toBeNull()
    expect(within(dialog).queryByRole('button', { name: 'View Prompt' })).toBeNull()
  })

  it('hides the prompt action when no prompt carrier exists', async () => {
    const user = userEvent.setup()
    installScenario({ list: () => jsonResponse(page([USER_ROW])) })
    renderWithProviders(<PageHarness />)

    await user.click(await screen.findByText('gpt-4o'))
    const dialog = await screen.findByRole('dialog')
    expect(within(dialog).queryByRole('button', { name: 'View Prompt' })).toBeNull()
    expect(within(dialog).queryByText('••••••')).toBeNull()
  })
})

describe('usage logs tabs and request_id deep links', () => {
  it('switches to the stats tab without resetting the filters', async () => {
    const user = userEvent.setup()
    const changes: UsageLogSearch[] = []
    const requests = installScenario({
      summary: () =>
        jsonResponse({
          ...EMPTY_SUMMARY,
          cost_total: 1234,
          request_count: 3,
          success_rate: 0.6667,
          prompt_tokens_total: 1200,
          completion_tokens_total: 300,
        }),
    })
    renderWithProviders(
      <PageHarness
        initial={{ model: 'gpt-4o', range: '7d' }}
        onSearch={(next) => changes.push(next)}
      />,
    )

    await screen.findByRole('tab', { name: 'Logs' })
    await user.click(screen.getByRole('tab', { name: 'Stats' }))

    await waitFor(() => {
      expect(changes.at(-1)?.tab).toBe('stats')
      // The tag: filters are view state, not tab state.
      expect(changes.at(-1)?.model).toBe('gpt-4o')
      expect(changes.at(-1)?.range).toBe('7d')
    })
    expect(
      await screen.findByRole('tab', { name: 'Stats', selected: true }),
    ).toBeTruthy()
    expect(screen.getByText('Total spent')).toBeTruthy()

    const statsRequest = latestRequest(requests, '/api/usage-stats/summary')
    expect(statsRequest?.url.searchParams.get('model')).toBe('gpt-4o')
    expect(statsRequest?.url.searchParams.get('range')).toBe('7d')
    // The list is not refetched while the stats view is active.
    const listCalls = requestsFor(requests, '/api/usage-logs').filter(
      (request) => request.url.searchParams.get('model') === 'gpt-4o',
    )
    expect(listCalls.length).toBeLessThanOrEqual(1)
  })

  it('opens the detail drawer from ?request_id= and strips it on close', async () => {
    const user = userEvent.setup()
    const changes: UsageLogSearch[] = []
    const requestId = USER_ROW.request_id
    installScenario({
      list: () => jsonResponse(page([USER_ROW])),
      detail: () => jsonResponse({ ...USER_ROW, other: {} }),
    })
    renderWithProviders(
      <PageHarness
        initial={{ request_id: requestId }}
        onSearch={(next) => changes.push(next)}
      />,
    )

    const dialog = await screen.findByRole('dialog')
    expect(
      await within(dialog).findByText(
        '9f3a8d2e-1b2c-4d5e-8f90-112233445566',
      ),
    ).toBeTruthy()

    await user.click(within(dialog).getByRole('button', { name: 'Close' }))
    await waitFor(() => {
      expect(changes.at(-1)?.request_id).toBeUndefined()
    })
  })

  it('opens the drawer for a compact 32-hex request_id (gateway format)', async () => {
    const compact = '9f3a8d2e1b2c4d5e8f90112233445566'
    const requests = installScenario({
      // The row comes back hyphenated while the link is compact: matching is
      // hyphen-insensitive, so both spellings address the same request.
      list: () => jsonResponse(page([USER_ROW])),
      detail: () => jsonResponse({ ...USER_ROW, other: {} }),
    })
    renderWithProviders(<PageHarness initial={{ request_id: compact }} />)

    expect(await screen.findByRole('dialog')).toBeTruthy()
    expect(
      latestRequest(requests, '/api/usage-logs')?.url.searchParams.get(
        'request_id',
      ),
    ).toBe(compact)
  })

  it('accepts a hyphenated ID without RFC version/variant bits', async () => {
    const nonV4 = '9f3a8d2e-1b2c-0d5e-0f90-112233445566'
    installScenario({
      list: () => jsonResponse(page([{ ...USER_ROW, request_id: nonV4 }])),
      detail: () => jsonResponse({ ...USER_ROW, request_id: nonV4, other: {} }),
    })
    renderWithProviders(<PageHarness initial={{ request_id: nonV4 }} />)

    expect(await screen.findByRole('dialog')).toBeTruthy()
  })

  it('corrects a stale page number before resolving the deep link', async () => {
    const changes: UsageLogSearch[] = []
    const requests = installScenario({
      list: () => jsonResponse(page([USER_ROW])),
      detail: () => jsonResponse({ ...USER_ROW, other: {} }),
    })
    renderWithProviders(
      <PageHarness
        initial={{ request_id: USER_ROW.request_id, p: 5 }}
        onSearch={(next) => changes.push(next)}
      />,
    )

    expect(await screen.findByRole('dialog')).toBeTruthy()
    expect(changes.some((next) => next.p === 1)).toBe(true)
    expect(
      latestRequest(requests, '/api/usage-logs')?.url.searchParams.get('p'),
    ).toBe('1')
  })

  it('reports a malformed request_id without replacing the page', async () => {
    const changes: UsageLogSearch[] = []
    const requests = installScenario({
      list: () => jsonResponse(page([USER_ROW])),
    })
    renderWithProviders(
      <PageHarness initial={{ request_id: 'bad-id' }} onSearch={(next) => changes.push(next)} />,
    )

    await screen.findByText('gpt-4o')
    await waitFor(() => {
      expect(changes.length).toBeGreaterThan(0)
      expect(changes.at(-1)?.request_id).toBeUndefined()
    })
    expect(
      useToastStore.getState().toasts.some((toast) => toast.message === 'Invalid request ID'),
    ).toBe(true)
    expect(
      requestsFor(requests, '/api/usage-logs').every(
        (request) => !request.url.searchParams.has('request_id'),
      ),
    ).toBe(true)
  })

  it('reports an unknown request_id and returns to the list', async () => {
    const changes: UsageLogSearch[] = []
    installScenario({ list: () => jsonResponse(page([])) })
    renderWithProviders(
      <PageHarness
        initial={{ request_id: USER_ROW.request_id }}
        onSearch={(next) => changes.push(next)}
      />,
    )

    await waitFor(() => {
      expect(changes.length).toBeGreaterThan(0)
      expect(changes.at(-1)?.request_id).toBeUndefined()
    })
    expect(
      useToastStore.getState().toasts.some(
        (toast) => toast.message === 'Log archived or not found',
      ),
    ).toBe(true)
    expect(screen.queryByRole('dialog')).toBeNull()
  })
})
